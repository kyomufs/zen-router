package zen

// SSE parsing and stream translation (plan Task 8): a pure line-oriented
// Server-Sent-Events frame parser plus the two stream lanes the gateway
// composes in Task 12 — raw chat passthrough with cost filtering and a single
// synthesized [DONE], and Responses-vocabulary → chat.completion.chunk
// translation. Semantics are ported from the golden plugin
// (dsh-opencode-zen lib/index.js): parseSse 860-893, translateStream
// finish/usage yields 982-983, mapResponsesUsage 995-1003,
// translateResponsesStream 1090-1212. No http, no goroutines, no flushing —
// every frame is written to w as it is parsed (Task 12 wraps w with the
// http.Flusher).
//
// This comment is deliberately not the package doc: models.go owns it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// sseFrame is one parsed SSE frame: the fields needed for lane decisions
// plus the exact input bytes for byte-for-byte passthrough.
type sseFrame struct {
	event string // event: field value, "" when absent
	data  string // data: lines joined with "\n", "" when absent
	raw   []byte // exact input bytes of the frame, terminators included

	// terminated reports whether raw ends with the blank line that closes a
	// frame; false for the final frame of a stream that ended without one.
	terminated bool
}

// readSSEFrame reads the next frame from r. The blank line ends a frame; a
// stream that ends mid-frame flushes what accumulated (terminated = false);
// a stream with no pending frame yields io.EOF. Lines tolerate CRLF, `:`
// comments are skipped (their bytes stay in raw for passthrough), and data
// lines are joined per the SSE spec with "\n". Field values are trimmed of
// surrounding whitespace, mirroring the plugin's line.slice(5).trim()
// (index.js:877) — which also makes CRLF streams parse identically.
func readSSEFrame(r *bufio.Reader) (*sseFrame, error) {
	var (
		raw       []byte
		event     string
		dataLines []string
	)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			raw = append(raw, line...)
			body := strings.TrimSuffix(line, "\n")
			body = strings.TrimSuffix(body, "\r")
			if body == "" {
				// Blank line: frame boundary.
				return &sseFrame{
					event:      event,
					data:       strings.Join(dataLines, "\n"),
					raw:        raw,
					terminated: strings.HasSuffix(line, "\n"),
				}, nil
			}
			if body[0] != ':' {
				if i := strings.IndexByte(body, ':'); i >= 0 {
					value := strings.TrimSpace(body[i+1:])
					switch body[:i] {
					case "event":
						event = value
					case "data":
						dataLines = append(dataLines, value)
					}
					// id: and unknown fields need no lane decision; their
					// bytes live on in raw.
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(raw) == 0 {
					return nil, io.EOF
				}
				return &sseFrame{
					event:      event,
					data:       strings.Join(dataLines, "\n"),
					raw:        raw,
					terminated: false,
				}, nil
			}
			return nil, err
		}
	}
}

// isCostChunk reports whether data is one of the gateway's custom cost
// frames (spec §4): a JSON object carrying a top-level "cost" field with no
// non-empty choices — `{"choices":[],"cost":…}` on the oa-compat lanes and
// `{"type":"ping","cost":…}` behind `event: ping`.
func isCostChunk(data string) bool {
	if !strings.Contains(data, "cost") {
		return false
	}
	var probe struct {
		Cost    json.RawMessage   `json:"cost"`
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &probe) != nil {
		return false
	}
	return probe.Cost != nil && len(probe.Choices) == 0
}

// writeRawFrame forwards a passthrough frame byte-for-byte. An
// EOF-flushed frame gets its closing blank line synthesized so the
// terminator written after it starts a fresh frame.
func writeRawFrame(w io.Writer, f *sseFrame) error {
	if _, err := w.Write(f.raw); err != nil {
		return err
	}
	if f.terminated {
		return nil
	}
	if n := len(f.raw); n > 0 && f.raw[n-1] == '\n' {
		_, err := w.Write([]byte("\n"))
		return err
	}
	_, err := w.Write([]byte("\n\n"))
	return err
}

// FilterChatStream reads an OpenAI chat SSE stream from r and writes it to w
// with (spec §4/§5):
//
//   - frames forwarded byte-for-byte, including `usage` and error frames;
//   - custom cost frames dropped — `event: ping`, and any JSON object with a
//     top-level "cost" field and no non-empty choices;
//   - an upstream `data: [DONE]` dropped and treated as end of input
//     (mirrors the plugin parser stopping at index.js:879), so the output
//     can never carry two terminators;
//   - exactly one synthesized `data: [DONE]` appended when the input ends
//     cleanly — including for empty input (spec §5: the daemon emits the
//     terminator the gateway never does).
//
// On a read or write error the error is returned as-is (wrapped with
// errors.Is support) and no terminator is written: a broken stream must not
// look complete.
func FilterChatStream(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		f, err := readSSEFrame(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if f.event == "ping" || isCostChunk(f.data) {
			continue
		}
		if f.data == "[DONE]" {
			break
		}
		if err := writeRawFrame(w, f); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("zen: write terminator: %w", err)
	}
	return nil
}

// --- responses → chat translation ------------------------------------------

// responsesEvent is the Responses SSE data envelope; only the fields the
// translation consults. Delta is typed string so a non-string delta fails
// unmarshalling and the frame is dropped — the same net effect as the
// plugin's typeof guard (index.js:1123/1134/1145).
type responsesEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	OutputIndex int             `json:"output_index"`
	Item        *responsesItem  `json:"item"`
	Error       *responsesError `json:"error"`
	Response    *responsesResp  `json:"response"`
}

type responsesItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesError struct {
	Message string `json:"message"`
}

type responsesResp struct {
	Status            string          `json:"status"`
	Usage             *responsesUsage `json:"usage"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *responsesError `json:"error"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// chatChunk is a generated chat.completion.chunk frame. Object is stamped by
// emit; Choices is omitted on the usage frame, Usage on all others.
type chatChunk struct {
	Object  string       `json:"object"`
	Choices []chatChoice `json:"choices,omitempty"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type chatDelta struct {
	Content          *string        `json:"content,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

type chatToolCall struct {
	Index    int         `json:"index"`
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Function *chatToolFn `json:"function,omitempty"`
}

type chatToolFn struct {
	Name string `json:"name,omitempty"`
	// Arguments is never omitted: OpenAI's first tool delta carries
	// function.arguments alongside the name.
	Arguments string `json:"arguments"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

type promptTokensDetails = struct {
	CachedTokens int `json:"cached_tokens"`
}

// responsesTool tracks one function_call output item across its lifecycle
// (added → arguments deltas → done), keyed by the event's output_index.
type responsesTool struct {
	seq      int    // chat tool_calls[].index, assigned in open order
	id       string // call_id, from output_item.added or .done
	name     string
	metaSent bool // id/name chunk already emitted
	argsSent bool // argument deltas already emitted (done must not repeat)
}

// responsesTranslator accumulates Responses-lane state and emits chat
// frames; one per stream.
type responsesTranslator struct {
	w io.Writer

	textOpen      bool // a message output item (or its deltas) was seen
	reasoningOpen bool
	blocks        int // plugin "order" entries — EMPTY_RESPONSE gauge
	tools         map[int]*responsesTool
	toolSeq       int
	cutoff        bool // completed with an incomplete_details max_output_tokens
	usage         *chatUsage
}

func strPtr(s string) *string { return &s }

func (tr *responsesTranslator) ensureText() {
	if !tr.textOpen {
		tr.textOpen = true
		tr.blocks++
	}
}

func (tr *responsesTranslator) ensureReasoning() {
	if !tr.reasoningOpen {
		tr.reasoningOpen = true
		tr.blocks++
	}
}

// emit writes one generated chat frame as a complete SSE frame.
func (tr *responsesTranslator) emit(c chatChunk) error {
	if c.Object == "" {
		c.Object = "chat.completion.chunk"
	}
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("zen: marshal chat chunk: %w", err)
	}
	if _, err := fmt.Fprintf(tr.w, "data: %s\n\n", b); err != nil {
		return fmt.Errorf("zen: write chat chunk: %w", err)
	}
	return nil
}

// TranslateResponsesStream reads a Responses-vocabulary SSE stream from r
// and writes the equivalent chat.completion.chunk stream to w, terminated by
// exactly one `data: [DONE]` when the input ends cleanly.
//
// Ported from the plugin translateResponsesStream (index.js:1090-1212):
//
//   - event selection uses the data payload's JSON "type" only — the
//     plugin's parser ignores `event:` lines (index.js:876);
//   - response.output_text.delta → delta.content;
//     response.reasoning_text.delta / response.reasoning_summary_text.delta
//     → delta.reasoning_content; empty deltas skipped;
//   - function_call items → delta.tool_calls (metadata chunk when the item
//     opens, argument fragments per delta, full arguments from
//     output_item.done only when no delta carried them);
//   - unknown events are dropped (the plugin has no fallback branch);
//   - an upstream `data: [DONE]` ends input, as in the plugin parser;
//   - response.failed / response.error / error abort with an error carrying
//     the upstream detail (index.js:1179-1182) — no terminator is written;
//   - after a clean EOF: a stream that opened zero output blocks is an
//     "empty response" error (index.js:1186), otherwise a finish frame
//     (finish_reason "length" for an incomplete max_output_tokens cutoff,
//     else the plugin's default "stop", index.js:1174/1212), then the
//     translated usage frame when response.completed carried usage, then
//     the single [DONE].
//
// The function is frame-granular: each output frame is written as soon as
// its input frame is parsed.
func TranslateResponsesStream(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	tr := &responsesTranslator{w: w, tools: make(map[int]*responsesTool)}
	for {
		f, err := readSSEFrame(br)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if f.data == "[DONE]" {
			break
		}
		if f.data == "" {
			continue
		}
		var ev responsesEvent
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			continue // non-JSON payloads ignored (index.js:888-891)
		}
		if err := tr.handle(ev); err != nil {
			return err
		}
	}
	return tr.finish()
}

// handle dispatches one Responses event; nil means "handled, keep going".
func (tr *responsesTranslator) handle(ev responsesEvent) error {
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta == "" {
			return nil
		}
		tr.ensureText()
		return tr.emit(chatChunk{Choices: []chatChoice{{
			Delta: chatDelta{Content: strPtr(ev.Delta)},
		}}})

	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if ev.Delta == "" {
			return nil
		}
		tr.ensureReasoning()
		return tr.emit(chatChunk{Choices: []chatChoice{{
			Delta: chatDelta{ReasoningContent: strPtr(ev.Delta)},
		}}})

	case "response.output_item.added":
		return tr.onItemAdded(ev)

	case "response.function_call_arguments.delta":
		return tr.onArgsDelta(ev)

	case "response.output_item.done":
		return tr.onItemDone(ev)

	case "response.completed":
		return tr.onCompleted(ev)

	case "response.failed", "response.error", "error":
		detail := "Responses stream reported an error"
		switch {
		case ev.Error != nil && ev.Error.Message != "":
			detail = ev.Error.Message
		case ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "":
			detail = ev.Response.Error.Message
		}
		return fmt.Errorf("zen: responses stream error: %s", detail)

	default:
		return nil // unknown events dropped (plugin has no fallback)
	}
}

func (tr *responsesTranslator) onItemAdded(ev responsesEvent) error {
	if ev.Item == nil {
		return nil
	}
	switch ev.Item.Type {
	case "message":
		tr.ensureText()
	case "reasoning":
		tr.ensureReasoning()
	case "function_call":
		tool := &responsesTool{
			seq:      tr.toolSeq,
			id:       ev.Item.CallID,
			name:     ev.Item.Name,
			metaSent: true,
		}
		tr.toolSeq++
		tr.tools[ev.OutputIndex] = tool
		tr.blocks++
		return tr.emit(toolMetaChunk(tool))
	}
	return nil
}

func (tr *responsesTranslator) onArgsDelta(ev responsesEvent) error {
	tool := tr.tools[ev.OutputIndex]
	if tool == nil {
		// Lazy open mirrors index.js:1147-1153 (a delta without added).
		tool = &responsesTool{seq: tr.toolSeq}
		tr.toolSeq++
		tr.tools[ev.OutputIndex] = tool
		tr.blocks++
	}
	if ev.Delta == "" {
		return nil
	}
	tool.argsSent = true
	return tr.emit(chatChunk{Choices: []chatChoice{{
		Delta: chatDelta{ToolCalls: []chatToolCall{{
			Index:    tool.seq,
			Function: &chatToolFn{Arguments: ev.Delta},
		}}},
	}}})
}

func (tr *responsesTranslator) onItemDone(ev responsesEvent) error {
	if ev.Item == nil || ev.Item.Type != "function_call" {
		return nil
	}
	tool := tr.tools[ev.OutputIndex]
	if tool == nil {
		return nil // plugin only completes an already-open block (index.js:1160)
	}
	if !tool.metaSent {
		if ev.Item.CallID != "" {
			tool.id = ev.Item.CallID
		}
		if ev.Item.Name != "" {
			tool.name = ev.Item.Name
		}
		tool.metaSent = true
		if err := tr.emit(toolMetaChunk(tool)); err != nil {
			return err
		}
	}
	if !tool.argsSent && ev.Item.Arguments != "" {
		tool.argsSent = true
		return tr.emit(chatChunk{Choices: []chatChoice{{
			Delta: chatDelta{ToolCalls: []chatToolCall{{
				Index:    tool.seq,
				Function: &chatToolFn{Arguments: ev.Item.Arguments},
			}}},
		}}})
	}
	return nil
}

func (tr *responsesTranslator) onCompleted(ev responsesEvent) error {
	resp := ev.Response
	if resp == nil {
		return nil
	}
	if resp.Usage != nil {
		tr.usage = mapResponsesUsageToChat(resp.Usage)
	}
	if resp.Status == "incomplete" &&
		resp.IncompleteDetails != nil &&
		resp.IncompleteDetails.Reason == "max_output_tokens" {
		tr.cutoff = true
	}
	return nil
}

// toolMetaChunk is the first tool_calls delta for an item: id, type and
// function name (OpenAI clients merge later fragments by index).
func toolMetaChunk(t *responsesTool) chatChunk {
	return chatChunk{Choices: []chatChoice{{
		Delta: chatDelta{ToolCalls: []chatToolCall{{
			Index:    t.seq,
			ID:       t.id,
			Type:     "function",
			Function: &chatToolFn{Name: t.name},
		}}},
	}}}
}

// mapResponsesUsageToChat converts Responses usage into the chat wire
// shape. Unlike the plugin's mapResponsesUsage (index.js:995-1003), which
// feeds the harness vocabulary (inputTokens net of the cache), the chat wire
// reports prompt_tokens inclusive of cached tokens and lists them under
// prompt_tokens_details.cached_tokens — the OpenAI convention the
// passthrough lane already forwards untouched.
func mapResponsesUsageToChat(u *responsesUsage) *chatUsage {
	out := &chatUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		out.PromptTokensDetails = &promptTokensDetails{
			CachedTokens: u.InputTokensDetails.CachedTokens,
		}
	}
	return out
}

// finish emits the stream tail: finish frame, optional usage frame, single
// [DONE] — or the EMPTY_RESPONSE error when no output block ever opened.
func (tr *responsesTranslator) finish() error {
	if tr.blocks == 0 {
		return errors.New("zen: responses stream returned an empty response")
	}
	reason := "stop"
	if tr.cutoff {
		reason = "length"
	}
	if err := tr.emit(chatChunk{Choices: []chatChoice{{
		Delta:        chatDelta{},
		FinishReason: &reason,
	}}}); err != nil {
		return err
	}
	if tr.usage != nil {
		if err := tr.emit(chatChunk{Usage: tr.usage}); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(tr.w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("zen: write terminator: %w", err)
	}
	return nil
}
