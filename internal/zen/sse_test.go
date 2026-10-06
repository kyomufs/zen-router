package zen

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

// --- helpers ---------------------------------------------------------------

// filterChat runs FilterChatStream over in and returns everything written.
func filterChat(t *testing.T, in string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := FilterChatStream(strings.NewReader(in), &buf); err != nil {
		t.Fatalf("FilterChatStream: %v", err)
	}
	return buf.String()
}

// translate runs TranslateResponsesStream over in; out is valid even when
// err != nil (frames emitted before the failure).
func translate(t *testing.T, in string) (out string, err error) {
	t.Helper()
	var buf bytes.Buffer
	err = TranslateResponsesStream(strings.NewReader(in), &buf)
	return buf.String(), err
}

// payloads splits translated (LF-framed) output into `data: ` payloads in
// order, failing on anything that is not a data frame.
func payloads(t *testing.T, out string) []string {
	t.Helper()
	var got []string
	for _, frame := range strings.Split(out, "\n\n") {
		if frame == "" {
			continue
		}
		if !strings.HasPrefix(frame, "data: ") {
			t.Fatalf("frame without data: prefix: %q", frame)
		}
		got = append(got, strings.TrimPrefix(frame, "data: "))
	}
	return got
}

// chunk is the subset of a generated chat.completion.chunk the tests assert.
type chunk struct {
	Object  string   `json:"object"`
	Choices []choice `json:"choices"`
	Usage   *useInfo `json:"usage"`
}

type choice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Content          *string    `json:"content"`
	ReasoningContent *string    `json:"reasoning_content"`
	ToolCalls        []toolCall `json:"tool_calls"`
}

type toolCall struct {
	Index    int     `json:"index"`
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Function *toolFn `json:"function"`
}

type toolFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type useInfo struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// decodeChunk parses one generated payload and asserts its object type.
func decodeChunk(t *testing.T, payload string) chunk {
	t.Helper()
	var c chunk
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		t.Fatalf("bad chunk %q: %v", payload, err)
	}
	if c.Object != "chat.completion.chunk" {
		t.Errorf("object = %q, want chat.completion.chunk (payload %q)", c.Object, payload)
	}
	return c
}

// --- chat passthrough filter ------------------------------------------------

// Plan Task 8: TestChatPassthroughFilter — chat frames forwarded verbatim;
// cost chunk and ping dropped; upstream [DONE] deduplicated (never two
// terminators).
func TestChatPassthroughFilter(t *testing.T) {
	roleFrame := `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`
	contentFrame := `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`
	finishFrame := `data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	in := roleFrame + "\n\n" +
		contentFrame + "\n\n" +
		`data: {"choices":[],"cost":{"total":1}}` + "\n\n" +
		"event: ping\n" + `data: {"type":"ping","cost":{"total":1}}` + "\n\n" +
		finishFrame + "\n\n" +
		"data: [DONE]\n\n"

	out := filterChat(t, in)

	for _, want := range []string{roleFrame, contentFrame, finishFrame} {
		if !strings.Contains(out, want+"\n\n") {
			t.Errorf("frame not forwarded verbatim:\n want %q\n got  %q", want, out)
		}
	}
	if strings.Contains(out, "cost") {
		t.Errorf("cost chunk leaked into output: %q", out)
	}
	if strings.Contains(out, "ping") {
		t.Errorf("ping frame leaked into output: %q", out)
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want exactly 1: %q", n, out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("output must end with exactly one terminator, got %q", out)
	}
}

// Plan Task 8: TestChatPassthroughUsageFrame — the final usage chunk is
// preserved and still precedes the synthesized terminator.
func TestChatPassthroughUsageFrame(t *testing.T) {
	contentFrame := `data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}`
	usageFrame := `data: {"id":"u1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
	in := contentFrame + "\n\n" + usageFrame + "\n\n" + "data: [DONE]\n\n"

	out := filterChat(t, in)

	if !strings.Contains(out, usageFrame+"\n\n") {
		t.Errorf("usage frame not preserved: %q", out)
	}
	if strings.Index(out, usageFrame) < strings.Index(out, contentFrame) {
		t.Errorf("usage frame must follow the content frame: %q", out)
	}
	if idxUsage, idxDone := strings.Index(out, usageFrame), strings.Index(out, "data: [DONE]"); idxUsage > idxDone {
		t.Errorf("usage frame must precede [DONE]: %q", out)
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1: %q", n, out)
	}
}

// Prompt minimum: TestFilterChatDropsCostAndPing — every gateway cost shape
// is dropped: the oa-compat cost chunk, the ping event frame, and a ping data
// payload without its event line (cost field alone).
func TestFilterChatDropsCostAndPing(t *testing.T) {
	keepA := `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`
	keepB := `data: {"choices":[{"index":0,"delta":{"content":"b"}}]}`
	in := keepA + "\n\n" +
		`data: {"choices":[],"cost":{"total":1}}` + "\n\n" +
		"event: ping\n" + `data: {"type":"ping","cost":{"total":1}}` + "\n\n" +
		`data: {"type":"ping","cost":{"total":1}}` + "\n\n" +
		keepB + "\n\n"

	out := filterChat(t, in)

	if !strings.Contains(out, keepA+"\n\n") || !strings.Contains(out, keepB+"\n\n") {
		t.Errorf("legit frames dropped: %q", out)
	}
	if strings.Contains(out, "cost") || strings.Contains(out, "ping") {
		t.Errorf("cost/ping frame leaked: %q", out)
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1: %q", n, out)
	}
}

// Prompt minimum: TestFilterChatAppendsDoneOnce — exactly one synthesized
// terminator regardless of input; empty input still yields [DONE] (spec §5:
// the daemon emits data: [DONE] at stream end unconditionally); upstream
// terminators are dropped, and input after an upstream [DONE] is ignored
// (mirrors the plugin parseSse stop at index.js:879).
func TestFilterChatAppendsDoneOnce(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		out := filterChat(t, "")
		if out != "data: [DONE]\n\n" {
			t.Errorf("empty input output = %q, want exactly %q", out, "data: [DONE]\n\n")
		}
	})

	t.Run("upstream done deduplicated", func(t *testing.T) {
		frame := `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`
		in := frame + "\n\n" +
			"data: [DONE]\n\n" +
			"data: [DONE]\n\n"
		out := filterChat(t, in)
		if n := strings.Count(out, "[DONE]"); n != 1 {
			t.Errorf("[DONE] count = %d, want 1: %q", n, out)
		}
		if !strings.HasSuffix(out, "data: [DONE]\n\n") {
			t.Errorf("missing final terminator: %q", out)
		}
	})

	t.Run("frames after upstream done are not forwarded", func(t *testing.T) {
		in := `data: {"choices":[{"index":0,"delta":{"content":"early"}}]}` + "\n\n" +
			"data: [DONE]\n\n" +
			`data: {"choices":[{"index":0,"delta":{"content":"late"}}]}` + "\n\n"
		out := filterChat(t, in)
		if strings.Contains(out, "late") {
			t.Errorf("frame after upstream [DONE] was forwarded: %q", out)
		}
		if n := strings.Count(out, "[DONE]"); n != 1 {
			t.Errorf("[DONE] count = %d, want 1: %q", n, out)
		}
	})

	t.Run("no upstream done", func(t *testing.T) {
		in := `data: {"choices":[{"index":0,"delta":{"content":"y"}}]}` + "\n\n"
		out := filterChat(t, in)
		if n := strings.Count(out, "[DONE]"); n != 1 {
			t.Errorf("[DONE] count = %d, want 1: %q", n, out)
		}
		if !strings.HasSuffix(out, "data: [DONE]\n\n") {
			t.Errorf("missing final terminator: %q", out)
		}
	})
}

// Prompt minimum: TestFilterChatPreservesUsage — a usage-only stream comes
// out byte-for-byte with the terminator appended (no cost frame, no loss).
func TestFilterChatPreservesUsage(t *testing.T) {
	usageFrame := `data: {"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`
	in := usageFrame + "\n\n" +
		`data: {"choices":[],"cost":{"total":0.001}}` + "\n\n"

	out := filterChat(t, in)

	want := usageFrame + "\n\n" + "data: [DONE]\n\n"
	if out != want {
		t.Errorf("output mismatch:\n got %q\nwant %q", out, want)
	}
}

// --- responses → chat translation ------------------------------------------

// Plan Task 8: TestResponsesSSEToChat — full fixture: response.created
// (dropped), reasoning delta, output_text.delta x2, response.completed with
// usage and a max_output_tokens cutoff (finish_reason: length); standard
// chat.completion.chunk frames; exactly one [DONE].
func TestResponsesSSEToChat(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"muse-spark-1","status":"in_progress"}}` + "\n\n" +
		"event: response.reasoning_text.delta\n" +
		`data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"thinking"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"Hel"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"lo"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":7,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"

	out, err := translate(t, in)
	if err != nil {
		t.Fatalf("TranslateResponsesStream: %v", err)
	}
	if strings.Contains(out, "response.") {
		t.Errorf("raw responses events leaked to the chat wire: %q", out)
	}

	ps := payloads(t, out)
	if len(ps) != 6 {
		t.Fatalf("payload count = %d, want 6 (reasoning, content x2, finish, usage, [DONE]): %q", len(ps), out)
	}

	if ps[5] != "[DONE]" {
		t.Errorf("last payload = %q, want [DONE]", ps[5])
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1", n)
	}

	reasoning := decodeChunk(t, ps[0])
	if len(reasoning.Choices) != 1 || reasoning.Choices[0].Delta.ReasoningContent == nil ||
		*reasoning.Choices[0].Delta.ReasoningContent != "thinking" {
		t.Errorf("reasoning frame = %s, want delta.reasoning_content \"thinking\"", ps[0])
	}

	for i, want := range []string{"Hel", "lo"} {
		c := decodeChunk(t, ps[i+1])
		if len(c.Choices) != 1 || c.Choices[0].Delta.Content == nil || *c.Choices[0].Delta.Content != want {
			t.Errorf("content frame %d = %s, want delta.content %q", i, ps[i+1], want)
		}
		if c.Choices[0].Index != 0 {
			t.Errorf("content frame %d index = %d, want 0", i, c.Choices[0].Index)
		}
	}

	finish := decodeChunk(t, ps[3])
	if len(finish.Choices) != 1 || finish.Choices[0].FinishReason == nil ||
		*finish.Choices[0].FinishReason != "length" {
		t.Errorf("finish frame = %s, want finish_reason \"length\" (max_output_tokens cutoff)", ps[3])
	}

	usage := decodeChunk(t, ps[4])
	if usage.Usage == nil {
		t.Fatalf("usage frame = %s, want usage object", ps[4])
	}
	if usage.Usage.PromptTokens != 7 || usage.Usage.CompletionTokens != 2 || usage.Usage.TotalTokens != 9 {
		t.Errorf("usage = %+v, want prompt 7 / completion 2 / total 9", usage.Usage)
	}
	if usage.Usage.PromptTokensDetails == nil || usage.Usage.PromptTokensDetails.CachedTokens != 3 {
		t.Errorf("usage.prompt_tokens_details = %+v, want cached_tokens 3", usage.Usage.PromptTokensDetails)
	}
	// OpenAI usage chunks always carry an empty choices array; clients doing
	// chunk.choices[0]?.delta throw when choices is undefined.
	if !strings.Contains(ps[4], `"choices":[]`) {
		t.Errorf("usage frame must carry \"choices\":[]: %s", ps[4])
	}
}

// Prompt minimum: TestTranslateResponsesDelta — output_text.delta becomes a
// chat content delta; without response.completed the stream still ends with a
// default finish_reason "stop" (plugin index.js:1212) and one [DONE]; input
// after an upstream [DONE] is ignored (index.js:879).
func TestTranslateResponsesDelta(t *testing.T) {
	in := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"Hi"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"!"}` + "\n\n" +
		"data: [DONE]\n\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"LATE"}` + "\n\n"

	out, err := translate(t, in)
	if err != nil {
		t.Fatalf("TranslateResponsesStream: %v", err)
	}
	if strings.Contains(out, "LATE") {
		t.Errorf("frame after upstream [DONE] was translated: %q", out)
	}

	ps := payloads(t, out)
	if len(ps) != 4 {
		t.Fatalf("payload count = %d, want 4 (content x2, finish, [DONE]): %q", len(ps), out)
	}

	c := decodeChunk(t, ps[0])
	if len(c.Choices) != 1 || c.Choices[0].Delta.Content == nil || *c.Choices[0].Delta.Content != "Hi" {
		t.Errorf("frame 0 = %s, want delta.content \"Hi\"", ps[0])
	}
	c = decodeChunk(t, ps[1])
	if len(c.Choices) != 1 || c.Choices[0].Delta.Content == nil || *c.Choices[0].Delta.Content != "!" {
		t.Errorf("frame 1 = %s, want delta.content \"!\"", ps[1])
	}

	finish := decodeChunk(t, ps[2])
	if len(finish.Choices) != 1 || finish.Choices[0].FinishReason == nil ||
		*finish.Choices[0].FinishReason != "stop" {
		t.Errorf("finish frame = %s, want finish_reason \"stop\"", ps[2])
	}
	if ps[3] != "[DONE]" {
		t.Errorf("last payload = %q, want [DONE]", ps[3])
	}
}

// Prompt minimum: TestTranslateResponsesCompleted — completed without a
// cutoff yields finish_reason "stop", then the translated usage frame, then
// exactly one [DONE]; output is empty of responses vocabulary.
func TestTranslateResponsesCompleted(t *testing.T) {
	in := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_2","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"ok"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":2}}}` + "\n\n"

	out, err := translate(t, in)
	if err != nil {
		t.Fatalf("TranslateResponsesStream: %v", err)
	}

	ps := payloads(t, out)
	if len(ps) != 4 {
		t.Fatalf("payload count = %d, want 4 (content, finish, usage, [DONE]): %q", len(ps), out)
	}

	finish := decodeChunk(t, ps[1])
	if len(finish.Choices) != 1 || finish.Choices[0].FinishReason == nil ||
		*finish.Choices[0].FinishReason != "stop" {
		t.Errorf("finish frame = %s, want finish_reason \"stop\"", ps[1])
	}

	usage := decodeChunk(t, ps[2])
	if usage.Usage == nil {
		t.Fatalf("usage frame = %s, want usage object", ps[2])
	}
	if usage.Usage.PromptTokens != 5 || usage.Usage.CompletionTokens != 2 || usage.Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v, want prompt 5 / completion 2 / total 7", usage.Usage)
	}
	if usage.Usage.PromptTokensDetails != nil {
		t.Errorf("prompt_tokens_details must be omitted when the upstream sent none: %+v", usage.Usage)
	}

	if ps[3] != "[DONE]" {
		t.Errorf("last payload = %q, want [DONE]", ps[3])
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] count = %d, want 1", n)
	}
}

// Prompt minimum: TestTranslateResponsesReasoning — both plugin reasoning
// event names (index.js:1122) map to delta.reasoning_content; an empty delta
// is skipped (index.js:1124/1135).
func TestTranslateResponsesReasoning(t *testing.T) {
	in := "event: response.reasoning_text.delta\n" +
		`data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"A"}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"B"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":""}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"

	out, err := translate(t, in)
	if err != nil {
		t.Fatalf("TranslateResponsesStream: %v", err)
	}

	ps := payloads(t, out)
	// reasoning A + reasoning B + finish + [DONE]; the empty content delta
	// produced no frame and completed carried no usage.
	if len(ps) != 4 {
		t.Fatalf("payload count = %d, want 4 (reasoning x2, finish, [DONE]): %q", len(ps), out)
	}

	for i, want := range []string{"A", "B"} {
		c := decodeChunk(t, ps[i])
		if len(c.Choices) != 1 || c.Choices[0].Delta.ReasoningContent == nil ||
			*c.Choices[0].Delta.ReasoningContent != want {
			t.Errorf("frame %d = %s, want delta.reasoning_content %q", i, ps[i], want)
		}
		if c.Choices[0].Delta.Content != nil {
			t.Errorf("frame %d leaked content: %s", i, ps[i])
		}
	}

	finish := decodeChunk(t, ps[2])
	if len(finish.Choices) != 1 || finish.Choices[0].FinishReason == nil ||
		*finish.Choices[0].FinishReason != "stop" {
		t.Errorf("finish frame = %s, want finish_reason \"stop\"", ps[2])
	}
	if ps[3] != "[DONE]" {
		t.Errorf("last payload = %q, want [DONE]", ps[3])
	}
}

// Prompt minimum: TestSSEPartialFrames — the parser tolerates multi-line
// data (joined with "\n" per the SSE spec), CRLF line endings, and a stream
// that ends without a trailing blank line; reads arrive one byte at a time.
func TestSSEPartialFrames(t *testing.T) {
	t.Run("multi-line data", func(t *testing.T) {
		// A cost chunk split over two data lines must still be recognized
		// as a cost frame once the lines are joined.
		in := "data: {\"choices\":[],\n" +
			"data: \"cost\":{\"total\":1}}\n\n" +
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"kept\"}}]}\n\n"
		var buf bytes.Buffer
		if err := FilterChatStream(iotest.OneByteReader(strings.NewReader(in)), &buf); err != nil {
			t.Fatalf("FilterChatStream: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "cost") {
			t.Errorf("multi-line cost frame not recognized as cost: %q", out)
		}
		want := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"kept\"}}]}\n\n" + "data: [DONE]\n\n"
		if out != want {
			t.Errorf("output mismatch:\n got %q\nwant %q", out, want)
		}
	})

	t.Run("crlf", func(t *testing.T) {
		ping := "event: ping\r\ndata: {\"type\":\"ping\",\"cost\":{\"total\":1}}\r\n\r\n"
		frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"crlf\"}}]}\r\n\r\n"
		in := ping + frame
		var buf bytes.Buffer
		if err := FilterChatStream(iotest.OneByteReader(strings.NewReader(in)), &buf); err != nil {
			t.Fatalf("FilterChatStream: %v", err)
		}
		out := buf.String()
		if strings.Contains(out, "ping") || strings.Contains(out, "cost") {
			t.Errorf("CRLF ping frame not dropped: %q", out)
		}
		// Kept frames stay byte-for-byte, CRLF included.
		want := frame + "data: [DONE]\n\n"
		if out != want {
			t.Errorf("output mismatch:\n got %q\nwant %q", out, want)
		}
	})

	t.Run("crlf responses lane", func(t *testing.T) {
		in := "event: response.output_text.delta\r\n" +
			"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"x\"}\r\n\r\n"
		out, err := translate(t, in)
		if err != nil {
			t.Fatalf("TranslateResponsesStream: %v", err)
		}
		ps := payloads(t, out)
		if len(ps) != 3 {
			t.Fatalf("payload count = %d, want 3 (content, finish, [DONE]): %q", len(ps), out)
		}
		c := decodeChunk(t, ps[0])
		if len(c.Choices) != 1 || c.Choices[0].Delta.Content == nil || *c.Choices[0].Delta.Content != "x" {
			t.Errorf("frame 0 = %s, want delta.content \"x\"", ps[0])
		}
		finish := decodeChunk(t, ps[1])
		if len(finish.Choices) != 1 || finish.Choices[0].FinishReason == nil ||
			*finish.Choices[0].FinishReason != "stop" {
			t.Errorf("finish frame = %s, want finish_reason \"stop\"", ps[1])
		}
	})

	t.Run("no trailing blank line", func(t *testing.T) {
		frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tail\"}}]}\n"
		var buf bytes.Buffer
		if err := FilterChatStream(iotest.OneByteReader(strings.NewReader(frame)), &buf); err != nil {
			t.Fatalf("FilterChatStream: %v", err)
		}
		out := buf.String()
		want := frame + "\n" + "data: [DONE]\n\n"
		if out != want {
			t.Errorf("unterminated final frame:\n got %q\nwant %q", out, want)
		}
	})

	t.Run("no final newline at all", func(t *testing.T) {
		frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tail\"}}]}"
		var buf bytes.Buffer
		if err := FilterChatStream(iotest.OneByteReader(strings.NewReader(frame)), &buf); err != nil {
			t.Fatalf("FilterChatStream: %v", err)
		}
		out := buf.String()
		want := frame + "\n\n" + "data: [DONE]\n\n"
		if out != want {
			t.Errorf("flush without newline:\n got %q\nwant %q", out, want)
		}
	})

	t.Run("comment lines", func(t *testing.T) {
		// `:` comment lines (standalone frames and inside data frames) are
		// skipped by the parser but preserved byte-for-byte in passthrough.
		in := ": stream-start\n\n" +
			": keep-alive\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"c\"}}]}\n\n"
		var buf bytes.Buffer
		if err := FilterChatStream(iotest.OneByteReader(strings.NewReader(in)), &buf); err != nil {
			t.Fatalf("FilterChatStream: %v", err)
		}
		want := in + "data: [DONE]\n\n"
		if got := buf.String(); got != want {
			t.Errorf("comment lines not preserved:\n got %q\nwant %q", got, want)
		}
	})

	t.Run("multibyte runes split across reads", func(t *testing.T) {
		// UTF-8 runes arrive split across reader boundaries; both lanes must
		// reassemble them untouched.
		frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"héllo ✓ мир\"}}]}\n\n"
		var buf bytes.Buffer
		if err := FilterChatStream(iotest.OneByteReader(strings.NewReader(frame)), &buf); err != nil {
			t.Fatalf("FilterChatStream: %v", err)
		}
		if got, want := buf.String(), frame+"data: [DONE]\n\n"; got != want {
			t.Errorf("passthrough corrupted multibyte runes:\n got %q\nwant %q", got, want)
		}

		in := "event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"héllo ✓ мир"}` + "\n\n"
		var out bytes.Buffer
		if err := TranslateResponsesStream(iotest.OneByteReader(strings.NewReader(in)), &out); err != nil {
			t.Fatalf("TranslateResponsesStream: %v", err)
		}
		c := decodeChunk(t, payloads(t, out.String())[0])
		if len(c.Choices) != 1 || c.Choices[0].Delta.Content == nil ||
			*c.Choices[0].Delta.Content != "héllo ✓ мир" {
			t.Errorf("translated content = %s, want \"héllo ✓ мир\"", payloads(t, out.String())[0])
		}
	})
}

// A frame beyond maxSSEFrameBytes must fail fast instead of buffering
// without bound — including a newline-free stream, which previously grew
// memory until EOF.
func TestSSEFrameSizeCap(t *testing.T) {
	in := "data: " + strings.Repeat("x", maxSSEFrameBytes) + "\n\n"
	var buf bytes.Buffer
	err := FilterChatStream(strings.NewReader(in), &buf)
	if err == nil {
		t.Fatalf("expected a size-cap error for a %d-byte frame, got nil", len(in))
	}
	if !strings.Contains(err.Error(), "SSE frame exceeds") {
		t.Errorf("error = %q, want the frame size cap message", err)
	}
	if strings.Contains(buf.String(), "[DONE]") {
		t.Errorf("no terminator may follow a cap failure: %q", buf.String())
	}
}

// Plugin parity (index.js:1179-1182): response.failed aborts the stream with
// an error carrying the upstream detail — no synthesized [DONE] after a
// failure.
func TestTranslateResponsesFailed(t *testing.T) {
	in := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n" +
		"event: response.failed\n" +
		`data: {"type":"response.failed","response":{"error":{"message":"boom"}}}` + "\n\n"

	out, err := translate(t, in)
	if err == nil {
		t.Fatalf("expected an error, got nil (out=%q)", out)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q must carry the upstream detail \"boom\"", err)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("no [DONE] may follow a failed stream: %q", out)
	}
	// Frames written before the failure stay written (streaming contract).
	if !strings.Contains(out, "partial") {
		t.Errorf("pre-failure frames must remain: %q", out)
	}
}

// Plugin parity (index.js:1187): a stream that opened zero output
// blocks is an error (EMPTY_RESPONSE), never a silent success — even when it
// ended with response.completed and usage.
func TestTranslateResponsesEmpty(t *testing.T) {
	t.Run("no events", func(t *testing.T) {
		out, err := translate(t, "")
		if err == nil {
			t.Fatalf("expected an error for an empty stream, got nil")
		}
		if !strings.Contains(err.Error(), "empty response") {
			t.Errorf("error %q must mention the empty response", err)
		}
		if out != "" {
			t.Errorf("nothing may be emitted for an empty stream: %q", out)
		}
	})

	t.Run("completed without output", func(t *testing.T) {
		in := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_3"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n"
		out, err := translate(t, in)
		if err == nil {
			t.Fatalf("expected an error for a zero-output stream, got nil")
		}
		if strings.Contains(out, "[DONE]") {
			t.Errorf("no [DONE] may follow an empty response: %q", out)
		}
	})
}

// Plugin parity (index.js:1144-1169): function_call items become chat
// tool_calls deltas — metadata chunk first, then argument fragments; the
// full arguments from output_item.done are only sent when no delta carried
// them (index.js:1165).
func TestTranslateResponsesToolCall(t *testing.T) {
	in := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read","status":"in_progress"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\""}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":":\"/x\"}"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_abc","name":"read","arguments":"{\"path\":\"/x\"}","status":"completed"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"

	out, err := translate(t, in)
	if err != nil {
		t.Fatalf("TranslateResponsesStream: %v", err)
	}

	ps := payloads(t, out)
	// meta + args x2 + finish + [DONE]; output_item.done must not repeat the
	// arguments a delta already streamed.
	if len(ps) != 5 {
		t.Fatalf("payload count = %d, want 5 (tool meta, args x2, finish, [DONE]): %q", len(ps), out)
	}

	meta := decodeChunk(t, ps[0])
	if len(meta.Choices) != 1 || len(meta.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("meta frame = %s, want one tool_calls entry", ps[0])
	}
	tc := meta.Choices[0].Delta.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Type != "function" || tc.Function == nil || tc.Function.Name != "read" {
		t.Errorf("tool meta = %+v, want id call_abc / type function / name read", tc)
	}

	var args string
	for i := 1; i <= 2; i++ {
		c := decodeChunk(t, ps[i])
		if len(c.Choices) != 1 || len(c.Choices[0].Delta.ToolCalls) != 1 {
			t.Fatalf("args frame %d = %s, want one tool_calls entry", i, ps[i])
		}
		entry := c.Choices[0].Delta.ToolCalls[0]
		if entry.Function == nil {
			t.Fatalf("args frame %d has no function: %s", i, ps[i])
		}
		if entry.Index != tc.Index {
			t.Errorf("args frame %d tool index = %d, want %d", i, entry.Index, tc.Index)
		}
		args += entry.Function.Arguments
	}
	if args != `{"path":"/x"}` {
		t.Errorf("streamed arguments = %q, want {\"path\":\"/x\"}", args)
	}

	if ps[4] != "[DONE]" {
		t.Errorf("last payload = %q, want [DONE]", ps[4])
	}
}

// io.EOF is the clean end of input for both lanes; other reader errors
// propagate and suppress the terminator.
func TestStreamReaderErrors(t *testing.T) {
	wantErr := errors.New("upstream exploded")

	t.Run("chat filter", func(t *testing.T) {
		var buf bytes.Buffer
		err := FilterChatStream(iotest.ErrReader(wantErr), &buf)
		if !errors.Is(err, wantErr) {
			t.Errorf("FilterChatStream error = %v, want %v", err, wantErr)
		}
		if strings.Contains(buf.String(), "[DONE]") {
			t.Errorf("a broken stream must not end with [DONE]: %q", buf.String())
		}
	})

	t.Run("responses translator", func(t *testing.T) {
		var buf bytes.Buffer
		err := TranslateResponsesStream(iotest.ErrReader(wantErr), &buf)
		if !errors.Is(err, wantErr) {
			t.Errorf("TranslateResponsesStream error = %v, want %v", err, wantErr)
		}
		if strings.Contains(buf.String(), "[DONE]") {
			t.Errorf("a broken stream must not end with [DONE]: %q", buf.String())
		}
	})
}
