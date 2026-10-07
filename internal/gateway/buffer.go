package gateway

// buffer.go: the non-streaming client path for spec §5:179 (standard
// OpenAI chat — `stream` defaults to FALSE, so an omitted flag must NOT
// receive SSE). The upstream always streams: relay framing, the
// first-event/stall watchdogs, staged rotation and stream translation are
// unchanged, and the watchdogs still govern the UPSTREAM read. Only the
// CLIENT write is deferred: translated chat.completion.chunk frames land
// here instead of on the wire, and after a clean relay (terminator seen)
// ONE chat.completion JSON body is written with Content-Type
// application/json — no [DONE], no SSE markers.
//
// Because nothing touches the ResponseWriter before flush, stream.started
// (the no-reissue latch in clientStream) stays false on this path: the
// full rotation budget applies pre-flush, and a failure before flush
// surfaces as the standard OpenAI error envelope (handler.go writeError)
// — never a partial JSON body. A failed attempt's frames are dropped by
// reset() so a re-issue starts from a clean accumulator.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"zen-router/internal/zen"
)

// relaySink is one attempt's frame destination: the SSE clientStream or
// the buffered completionBuffer. Both buffer to frame granularity; only
// completionBuffer defers the client write.
type relaySink interface {
	io.Writer
	close() error
}

// completionBuffer accumulates the translated SSE frames of one client
// request and assembles them into a single OpenAI chat.completion.
type completionBuffer struct {
	buf []byte

	fallbackModel string // request model: used when no frame stamps one
	id            string
	model         string
	created       int64
	content       strings.Builder
	reasoning     strings.Builder
	tools         map[int]*bufferedTool
	toolOrder     []int
	finish        string
	usage         json.RawMessage
}

// bufferedTool is one assembled tool call. Index (from the deltas) is
// carried through so multi-tool ordering stays unambiguous; Arguments
// concatenates across deltas like OpenAI's streaming tool arguments.
type bufferedTool struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function bufferedToolFn `json:"function"`
}

type bufferedToolFn struct {
	Name string `json:"name,omitempty"`
	// Arguments is never omitted: OpenAI's first tool delta carries
	// function.arguments alongside the name.
	Arguments string `json:"arguments"`
}

// bufferedCompletion is the single body written for a non-streaming
// client. Usage is json.RawMessage so a captured usage frame passes
// through verbatim; flush() substitutes the documented zero default when
// the stream ended without one.
type bufferedCompletion struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []bufferedChoice `json:"choices"`
	Usage   json.RawMessage  `json:"usage"`
}

type bufferedChoice struct {
	Index        int             `json:"index"`
	Message      bufferedMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type bufferedMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []bufferedTool `json:"tool_calls,omitempty"`
}

// bufferChunk is the subset of a chat.completion.chunk frame the assembly
// consults. Delta is a pointer so a frame without one (the usage frame)
// merges cleanly.
type bufferChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta *struct {
			Content          *string           `json:"content"`
			ReasoningContent *string           `json:"reasoning_content"`
			ToolCalls        []bufferToolDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

type bufferToolDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// defaultUsageJSON is the documented fallback when a clean stream ends
// without a usage frame: a zeroed usage object rather than a missing one,
// so clients can always read the counters (spec §5 makes usage advisory
// for non-streaming completions).
const defaultUsageJSON = `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`

func newCompletionBuffer(fallbackModel string) *completionBuffer {
	return &completionBuffer{fallbackModel: fallbackModel}
}

// reset clears everything buffered: invoked at the start of EVERY attempt
// so a failed attempt's partial frames never leak into a re-issued one.
func (b *completionBuffer) reset() {
	b.buf = b.buf[:0]
	b.id, b.model, b.finish = "", "", ""
	b.created = 0
	b.content.Reset()
	b.reasoning.Reset()
	b.tools = nil
	b.toolOrder = nil
	b.usage = nil
}

// Write buffers bytes and merges every complete SSE frame (one flush of
// assembly per frame, mirroring clientStream's frame granularity).
func (b *completionBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	for {
		end, ok := frameEnd(b.buf)
		if !ok {
			break
		}
		b.feed(b.buf[:end])
		b.buf = b.buf[:copy(b.buf, b.buf[end:])]
	}
	return len(p), nil
}

// close feeds any trailing unterminated frame (a clean relay always ends
// with the [DONE] frame, so this is normally a no-op) and NEVER writes to
// the client — the handler flushes exactly once after a clean relay.
func (b *completionBuffer) close() error {
	if len(b.buf) == 0 {
		return nil
	}
	tail := b.buf
	b.buf = nil
	b.feed(tail)
	return nil
}

// dataPrefix is the SSE data field marker.
var dataPrefix = []byte("data:")

// feed extracts data lines from one frame (joined per the SSE spec) and
// merges them; frames without data (comments, bare event:/id: lines) carry
// nothing to assemble.
func (b *completionBuffer) feed(frame []byte) {
	var data []string
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}
		v := bytes.TrimPrefix(line, dataPrefix)
		v = bytes.TrimPrefix(v, []byte(" ")) // SSE strips ONE leading space/tab
		v = bytes.TrimPrefix(v, []byte("\t"))
		data = append(data, string(v))
	}
	if len(data) == 0 {
		return
	}
	b.merge(strings.Join(data, "\n"))
}

// merge folds one data payload into the accumulated completion.
func (b *completionBuffer) merge(data string) {
	if data == "[DONE]" {
		return // terminator seen: relay success is the flush signal
	}
	var ch bufferChunk
	if err := json.Unmarshal([]byte(data), &ch); err != nil {
		return // non-JSON payloads are dropped (translator typeof-guard parity)
	}
	if ch.ID != "" && b.id == "" {
		b.id = ch.ID
	}
	if ch.Model != "" && b.model == "" {
		b.model = ch.Model
	}
	if ch.Created > 0 && b.created == 0 {
		b.created = ch.Created
	}
	if len(ch.Usage) > 0 && !bytes.Equal(ch.Usage, []byte("null")) {
		b.usage = append(b.usage[:0], ch.Usage...)
	}
	for _, c := range ch.Choices {
		// DM-8, first-wins: the flushed body carries exactly one choice
		// (OpenAI non-streaming semantics, n=1), so anything beyond index 0
		// must not concatenate into that one message. The upstream is
		// Zen/Anthropic-backed (no n>1 today) — defended anyway. Buffer
		// only: the SSE path never goes through merge.
		if c.Index > 0 {
			continue
		}
		if c.Delta != nil {
			if c.Delta.Content != nil {
				b.content.WriteString(*c.Delta.Content)
			}
			if c.Delta.ReasoningContent != nil {
				b.reasoning.WriteString(*c.Delta.ReasoningContent)
			}
			for _, tc := range c.Delta.ToolCalls {
				b.mergeTool(tc)
			}
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			b.finish = *c.FinishReason
		}
	}
}

// mergeTool assembles one tool-call delta: id/type/name on first sight,
// arguments concatenated across deltas, keyed and ordered by delta index.
func (b *completionBuffer) mergeTool(tc bufferToolDelta) {
	if b.tools == nil {
		b.tools = map[int]*bufferedTool{}
	}
	t, ok := b.tools[tc.Index]
	if !ok {
		t = &bufferedTool{Index: tc.Index}
		b.tools[tc.Index] = t
		b.toolOrder = append(b.toolOrder, tc.Index)
	}
	if tc.ID != "" {
		t.ID = tc.ID
	}
	if tc.Type != "" {
		t.Type = tc.Type
	}
	if tc.Function != nil {
		if tc.Function.Name != "" {
			t.Function.Name = tc.Function.Name
		}
		t.Function.Arguments += tc.Function.Arguments
	}
}

// flush writes the ONE buffered body: 200, application/json, no SSE
// markers. Documented fallback choices: model falls back to the request
// model and created to now when no frame stamped them (stampWriter covers
// the Responses lane; chat frames usually carry their own); finish_reason
// defaults to "stop" when the stream never carried one; usage defaults to
// the zero object above (see defaultUsageJSON).
func (b *completionBuffer) flush(w http.ResponseWriter) {
	id := b.id
	if id == "" {
		id = zen.RandomID("chatcmpl", 16)
	}
	model := b.model
	if model == "" {
		model = b.fallbackModel
	}
	created := b.created
	if created == 0 {
		created = time.Now().Unix()
	}
	finish := b.finish
	if finish == "" {
		finish = "stop"
	}
	usage := b.usage
	if len(usage) == 0 || !json.Valid(usage) {
		usage = json.RawMessage(defaultUsageJSON)
	}
	msg := bufferedMessage{Role: "assistant", Content: b.content.String()}
	if b.reasoning.Len() > 0 {
		msg.ReasoningContent = b.reasoning.String()
	}
	if len(b.toolOrder) > 0 {
		idx := append([]int(nil), b.toolOrder...)
		sort.Ints(idx)
		tools := make([]bufferedTool, 0, len(idx))
		for _, i := range idx {
			tools = append(tools, *b.tools[i])
		}
		msg.ToolCalls = tools
	}
	writeJSON(w, http.StatusOK, bufferedCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []bufferedChoice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   usage,
	})
}
