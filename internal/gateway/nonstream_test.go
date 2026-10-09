package gateway

// Non-streaming client tests (whole-branch review finding 2): spec §5:179
// promises standard OpenAI chat, whose `stream` field defaults to FALSE.
// The upstream ALWAYS receives stream:true — relay framing, watchdogs,
// rotation and translation are unchanged — but when the client did not ask
// for SSE the handler must accumulate the translated frames in memory and
// write ONE buffered chat.completion JSON body instead. Loopback-only
// (httptest).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-router/internal/config"
	"zen-router/internal/proxy"
	"zen-router/internal/router"
)

// chatSSERespond is the shared upstream script: a reasoning delta, two
// content deltas, a finish frame, a real usage frame, and [DONE]. Frames
// carry only "id" (like real gateway deltas the stamp of model/created is
// upstream-optional) so the buffered path's request-context fallbacks are
// exercised too.
func chatSSERespond(_ int, w http.ResponseWriter, _ *http.Request) {
	writeSSE(wrap(w),
		`data: {"id":"chatcmpl-up","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`+"\n\n",
		`data: {"id":"chatcmpl-up","choices":[{"index":0,"delta":{"content":"Hel"}}]}`+"\n\n",
		`data: {"id":"chatcmpl-up","choices":[{"index":0,"delta":{"content":"lo"}}]}`+"\n\n",
		`data: {"id":"chatcmpl-up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		`data: {"id":"chatcmpl-up","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`+"\n\n",
		"data: [DONE]"+"\n\n",
	)
}

// requireJSONCompletion asserts the non-streaming client contract: 200,
// Content-Type application/json, NO SSE markers anywhere in the body, and
// a body that decodes as one JSON object. Returns the decoded completion.
func requireJSONCompletion(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (non-streaming client)", ct)
	}
	raw := rec.Body.String()
	for _, marker := range []string{"data: ", "[DONE]"} {
		if strings.Contains(raw, marker) {
			t.Errorf("SSE marker %q leaked into the buffered JSON body: %s", marker,
				truncate(rec.Body.Bytes()))
		}
	}
	return decodeJSONMap(t, rec.Body.Bytes())
}

// requireChoice decodes choices[0] of a chat.completion body and returns
// its message plus finish_reason.
func requireChoice(t *testing.T, env map[string]any) (message map[string]any, finish string) {
	t.Helper()
	choices, _ := env["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices length = %d, want exactly 1: %s", len(choices), truncate(mustMarshalForLog(t, env)))
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		t.Fatalf("choices[0] is not an object: %s", truncate(mustMarshalForLog(t, env)))
	}
	if idx, _ := c["index"].(float64); int(idx) != 0 {
		t.Errorf("choices[0].index = %v, want 0", c["index"])
	}
	msg, _ := c["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("choices[0].message missing: %s", truncate(mustMarshalForLog(t, env)))
	}
	if role, _ := msg["role"].(string); role != "assistant" {
		t.Errorf("message.role = %v, want assistant", msg["role"])
	}
	finish, _ = c["finish_reason"].(string)
	return msg, finish
}

// mustMarshalForLog renders a decoded map for failure messages.
func mustMarshalForLog(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal for log: %v", err)
	}
	return b
}

// nonStreamBody renders a chat request with the given stream-key fragment
// ("" = key omitted).
func nonStreamBody(streamKey string) string {
	return `{"model":"mimo-v2.6-flash-free",` + streamKey +
		`"messages":[{"role":"user","content":"hello"}]}`
}

// TestNonStreamingClientJSON: clients sending stream:false (a) or omitting
// the flag entirely (b) get one JSON completion — assembled content,
// reasoning and usage — while the upstream still sees stream:true.
func TestNonStreamingClientJSON(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streamKey string
	}{
		{"stream false returns one JSON completion", `"stream":false,`},
		{"omitted stream defaults to non-streaming", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rot := newTestRotator(t)
			up := newFakeUpstream(t, chatSSERespond)
			h := New(rot, config.Default())
			h.Upstream = up.srv.URL

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newChatRequest(nonStreamBody(tc.streamKey)))
			env := requireJSONCompletion(t, rec)

			// Upstream unchanged: the relay always sees an SSE stream.
			reqs := up.requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(reqs))
			}
			ub := decodeJSONMap(t, reqs[0].Body)
			if s, _ := ub["stream"].(bool); !s {
				t.Errorf("upstream stream = %v, want true (only the client rendering is buffered)",
					ub["stream"])
			}
			if so, _ := ub["stream_options"].(map[string]any); so == nil {
				t.Errorf("upstream stream_options missing: %s", truncate(reqs[0].Body))
			}

			// Completion envelope fields.
			if o, _ := env["object"].(string); o != "chat.completion" {
				t.Errorf("object = %v, want chat.completion", env["object"])
			}
			if id, _ := env["id"].(string); !strings.HasPrefix(id, "chatcmpl") {
				t.Errorf("id = %v, want chatcmpl* prefix", env["id"])
			}
			if mdl, _ := env["model"].(string); mdl != "mimo-v2.6-flash-free" {
				t.Errorf("model = %v, want the request model (frames carried no model stamp)",
					env["model"])
			}
			if cr, _ := env["created"].(float64); cr <= 0 {
				t.Errorf("created = %v, want > 0 fallback stamp", env["created"])
			}

			msg, finish := requireChoice(t, env)
			if c, _ := msg["content"].(string); c != "Hello" {
				t.Errorf("message.content = %q, want Hello (assembled from deltas)", c)
			}
			if rc, _ := msg["reasoning_content"].(string); rc != "think" {
				t.Errorf("message.reasoning_content = %q, want think", rc)
			}
			if finish != "stop" {
				t.Errorf("finish_reason = %q, want stop (from the translated finish frame)", finish)
			}
			usage, _ := env["usage"].(map[string]any)
			if usage == nil {
				t.Fatalf("usage missing from completion: %s", truncate(mustMarshalForLog(t, env)))
			}
			if tt, _ := usage["total_tokens"].(float64); int(tt) != 7 {
				t.Errorf("usage.total_tokens = %v, want 7 (from the usage frame)", usage["total_tokens"])
			}
		})
	}
}

// TestNonStreamingToolCallAssembly: tool-call deltas arriving across
// several frames assemble into ONE message.tool_calls entry with id, name
// and concatenated arguments; the delta index is preserved; a stream that
// ends without a usage frame falls back to zero usage (documented choice:
// better a zeroed usage object than a missing one — clients can always add).
func TestNonStreamingToolCallAssembly(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeSSE(wrap(w),
			`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[`+
				`{"index":0,"id":"call_1","type":"function",`+
				`"function":{"name":"get_weather","arguments":""}}]}}]}`+"\n\n",
			`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[`+
				`{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`+"\n\n",
			`data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[`+
				`{"index":0,"function":{"arguments":"\"MSK\"}"}}]}}]}`+"\n\n",
			`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n",
			"data: [DONE]"+"\n\n",
		)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(nonStreamBody(`"stream":false,`)))
	env := requireJSONCompletion(t, rec)

	msg, finish := requireChoice(t, env)
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls length = %d, want 1 assembled call: %s", len(tcs),
			truncate(mustMarshalForLog(t, env)))
	}
	tc, _ := tcs[0].(map[string]any)
	if tc == nil {
		t.Fatalf("tool_calls[0] is not an object: %s", truncate(mustMarshalForLog(t, env)))
	}
	if idx, _ := tc["index"].(float64); int(idx) != 0 {
		t.Errorf("tool_calls[0].index = %v, want 0 (delta index preserved)", tc["index"])
	}
	if id, _ := tc["id"].(string); id != "call_1" {
		t.Errorf("tool_calls[0].id = %v, want call_1", tc["id"])
	}
	if typ, _ := tc["type"].(string); typ != "function" {
		t.Errorf("tool_calls[0].type = %v, want function", tc["type"])
	}
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		t.Fatalf("tool_calls[0].function missing: %s", truncate(mustMarshalForLog(t, env)))
	}
	if n, _ := fn["name"].(string); n != "get_weather" {
		t.Errorf("function.name = %v, want get_weather", fn["name"])
	}
	if args, _ := fn["arguments"].(string); args != `{"city":"MSK"}` {
		t.Errorf("function.arguments = %q, want {\"city\":\"MSK\"} (concatenated deltas)", args)
	}
	// No usage frame upstream → documented zero-ish default, still present.
	usage, _ := env["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("usage missing (must default to zeros when absent): %s",
			truncate(mustMarshalForLog(t, env)))
	}
	if tt, _ := usage["total_tokens"].(float64); int(tt) != 0 {
		t.Errorf("usage.total_tokens = %v, want 0 default", usage["total_tokens"])
	}
}

// TestNonStreamingBudgetExhausted429: a non-streaming client whose upstream
// 429s through the whole rotation budget gets the OpenAI error envelope
// (nothing was written before the flush, so the pre-flush rotation gate
// still applied) with the 429 status, metadata and Retry-After — and the
// rotation really ran: both attempts executed on k1→k2 (the single key
// stage; the stage machine has no further stages).
func TestNonStreamingBudgetExhausted429(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeDaily429(w)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(nonStreamBody(`"stream":false,`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	reqs := up.requests()
	if len(reqs) != 2 {
		t.Errorf("upstream requests = %d, want 2 (key stage executed pre-flush, then exhausted)", len(reqs))
	}
	var keys []string
	for _, ur := range reqs {
		auth := ur.Header.Get("Authorization")
		keys = append(keys, strings.TrimPrefix(auth, "Bearer "))
	}
	if len(keys) == 2 && !(keys[0] == "k1" && keys[1] == "k2") {
		t.Errorf("attempt keys = %v, want [k1 k2] (one key re-issue)", keys)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("Retry-After missing on 429 envelope")
	}

	env := requireOpenAIError(t, rec, "FreeUsageLimitError")
	md, _ := env["metadata"].(map[string]any)
	if md == nil {
		t.Fatalf("429 metadata must survive as a top-level sibling: %s", truncate(rec.Body.Bytes()))
	}
	if ln, _ := md["limitName"].(string); ln != "free" {
		t.Errorf("metadata.limitName = %v, want free (quota info round-tripped)", md["limitName"])
	}
	if _, ok := env["choices"]; ok {
		t.Errorf("completion choices leaked into the error envelope: %s", truncate(rec.Body.Bytes()))
	}
}

// TestNonStreamingUpstreamDiesMidBuffer: the upstream dies AFTER emitting
// frames but before the terminator. Nothing was written to the client, so
// the attempt is reported (KindTransport is terminal — one attempt), and
// the client gets ONE OpenAI error envelope — never a partial JSON
// completion and never half-written SSE.
func TestNonStreamingUpstreamDiesMidBuffer(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeSSE(wrap(w),
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"partial"}}]}`+"\n\n",
		)
		panic(http.ErrAbortHandler)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(nonStreamBody(`"stream":false,`)))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	env := requireOpenAIError(t, rec, "TransportError")
	if _, ok := env["choices"]; ok {
		t.Errorf("partial completion leaked into the error body: %s", truncate(rec.Body.Bytes()))
	}
	if body := rec.Body.String(); strings.Contains(body, "partial") {
		t.Errorf("mid-buffer content leaked into the error body: %s", truncate(rec.Body.Bytes()))
	}
	if n := len(up.requests()); n != 1 {
		t.Errorf("upstream requests = %d, want 1 (KindTransport is terminal)", n)
	}
}

// TestNonStreamingResponsesLane: the Responses lane honors the client flag
// too — upstream still streams (translated + stamped), the client gets one
// JSON completion carrying translated reasoning, content, finish and usage.
// Both loopable stream-key shapes are pinned (N-3): "stream":false and the
// omitted flag, which takes the same body["stream"].(bool) → false path.
func TestNonStreamingResponsesLane(t *testing.T) {
	const responsesModel = "muse-spark-1.3-contributor-free"

	for _, tc := range []struct {
		name      string
		streamKey string
	}{
		{"stream false returns one JSON completion", `"stream":false,`},
		{"omitted stream defaults to non-streaming", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rot := newTestRotator(t)
			up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeSSE(wrap(w),
					`data: {"type":"response.reasoning_text.delta","delta":"think","output_index":0}`+"\n\n",
					`data: {"type":"response.output_text.delta","delta":"Hello","output_index":0}`+"\n\n",
					`data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}`+"\n\n",
				)
			})
			h := New(rot, config.Default())
			h.Upstream = up.srv.URL

			clientBody := `{"model":"` + responsesModel + `",` + tc.streamKey +
				`"messages":[{"role":"user","content":"hello"}]}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newChatRequest(clientBody))
			env := requireJSONCompletion(t, rec)

			reqs := up.requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(reqs))
			}
			if p := reqs[0].Path; p != "/zen/v1/responses" {
				t.Errorf("upstream path = %s, want /zen/v1/responses", p)
			}
			ub := decodeJSONMap(t, reqs[0].Body)
			if s, _ := ub["stream"].(bool); !s {
				t.Errorf("upstream stream = %v, want true (responses lane streams upstream regardless)",
					ub["stream"])
			}

			if o, _ := env["object"].(string); o != "chat.completion" {
				t.Errorf("object = %v, want chat.completion", env["object"])
			}
			if mdl, _ := env["model"].(string); mdl != responsesModel {
				t.Errorf("model = %v, want %s (stamped through translation)", env["model"], responsesModel)
			}
			if id, _ := env["id"].(string); !strings.HasPrefix(id, "chatcmpl") {
				t.Errorf("id = %v, want chatcmpl* prefix (stamped)", env["id"])
			}

			msg, finish := requireChoice(t, env)
			if c, _ := msg["content"].(string); c != "Hello" {
				t.Errorf("message.content = %q, want Hello", c)
			}
			if rc, _ := msg["reasoning_content"].(string); rc != "think" {
				t.Errorf("message.reasoning_content = %q, want think", rc)
			}
			if finish != "stop" {
				t.Errorf("finish_reason = %q, want stop", finish)
			}
			usage, _ := env["usage"].(map[string]any)
			if usage == nil {
				t.Fatalf("usage missing: %s", truncate(mustMarshalForLog(t, env)))
			}
			if pt, _ := usage["prompt_tokens"].(float64); int(pt) != 5 {
				t.Errorf("usage.prompt_tokens = %v, want 5", usage["prompt_tokens"])
			}
			if tt, _ := usage["total_tokens"].(float64); int(tt) != 7 {
				t.Errorf("usage.total_tokens = %v, want 7", usage["total_tokens"])
			}
		})
	}
}

// oneShotRot is the DM-11 seam: Attempt offers a plain direct attempt and
// NextAttempt grants exactly ONE re-issue, then exhausts. The live router
// can never produce this shape — a post-buffer failure is KindTransport and
// the router declines it (ledger: "KindTransport→false") — so the buffer's
// reset() guarantee is pinned here by construction.
type oneShotRot struct {
	offered bool
}

func (r *oneShotRot) Attempt() router.Attempt {
	return router.Attempt{Key: "k1", Egress: proxy.EgressDirect, Transport: http.DefaultTransport, Step: 0}
}

func (r *oneShotRot) NextAttempt(_ router.Report) (router.Attempt, bool) {
	if r.offered {
		return router.Attempt{}, false
	}
	r.offered = true
	return router.Attempt{
		Key:       "k2",
		Egress:    proxy.EgressDirect,
		Transport: http.DefaultTransport,
		Step:      1,
	}, true
}

// TestBufferResetAfterFailedAttempt (DM-11): defense-in-depth for
// completionBuffer.reset(). Attempt 1 emits one complete content frame and
// then dies mid-stream; the buffered client wrote nothing, so the handler
// re-issues (oneShotRot grants it — the live rotator would not). The ONE
// flushed chat.completion must carry ONLY attempt-2 content: a partially
// buffered failed attempt never leaks into the re-issued success.
func TestBufferResetAfterFailedAttempt(t *testing.T) {
	rot := &oneShotRot{}
	up := newFakeUpstream(t, func(call int, w http.ResponseWriter, _ *http.Request) {
		if call == 1 {
			writeSSE(wrap(w),
				`data: {"id":"c","choices":[{"index":0,"delta":{"content":"partial-first"}}]}`+"\n\n",
			)
			panic(http.ErrAbortHandler) // attempt 1 dies after buffering a frame
		}
		writeSSE(wrap(w),
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"second"}}]}`+"\n\n",
			`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
			"data: [DONE]"+"\n\n",
		)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(nonStreamBody(`"stream":false,`)))

	env := requireJSONCompletion(t, rec)
	if n := len(up.requests()); n != 2 {
		t.Fatalf("upstream requests = %d, want 2 (one re-issue granted)", n)
	}
	msg, finish := requireChoice(t, env)
	if c, _ := msg["content"].(string); c != "second" {
		t.Errorf("message.content = %q, want %q (attempt-1 frames must be reset)", c, "second")
	}
	if body := rec.Body.String(); strings.Contains(body, "partial-first") {
		t.Errorf("attempt-1 content leaked into the flushed body: %s", truncate(rec.Body.Bytes()))
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop (from the attempt-2 finish frame)", finish)
	}
}

// TestNonStreamingZeroFrameFlush (DM-12): a clean 2xx upstream stream that
// carries ONLY the terminator — no content, no finish, no usage frame —
// still flushes ONE valid chat.completion: 200, application/json, empty
// content, finish_reason "stop", the zero usage object, the request model
// and a chatcmpl* id (documented buffer.go flush fallbacks).
func TestNonStreamingZeroFrameFlush(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeSSE(wrap(w), "data: [DONE]"+"\n\n")
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(nonStreamBody(`"stream":false,`)))
	env := requireJSONCompletion(t, rec)

	if n := len(up.requests()); n != 1 {
		t.Errorf("upstream requests = %d, want 1", n)
	}
	if o, _ := env["object"].(string); o != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", env["object"])
	}
	if id, _ := env["id"].(string); !strings.HasPrefix(id, "chatcmpl") {
		t.Errorf("id = %v, want chatcmpl* prefix (random fallback stamp)", env["id"])
	}
	if mdl, _ := env["model"].(string); mdl != "mimo-v2.6-flash-free" {
		t.Errorf("model = %v, want the request model (no frame stamped it)", env["model"])
	}
	if cr, _ := env["created"].(float64); cr <= 0 {
		t.Errorf("created = %v, want > 0 fallback stamp", env["created"])
	}

	msg, finish := requireChoice(t, env)
	if c, _ := msg["content"].(string); c != "" {
		t.Errorf("message.content = %q, want empty (stream carried no content)", c)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop (flush fallback)", finish)
	}
	usage, _ := env["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("usage missing (must default to the zero object): %s",
			truncate(mustMarshalForLog(t, env)))
	}
	if tt, _ := usage["total_tokens"].(float64); int(tt) != 0 {
		t.Errorf("usage.total_tokens = %v, want 0 default", usage["total_tokens"])
	}
}

// TestBufferFirstChoiceWins (DM-8): a buffered stream whose frames carry
// MORE than one choice (indexes 0 and 1, different contents) flushes only
// the index-0 message. OpenAI non-streaming semantics are a single choice,
// so completionBuffer.merge skips choices with Index > 0 instead of
// concatenating every choice's deltas into the one message. The upstream
// is Zen/Anthropic-backed (no n>1) — this defends the merge anyway. The
// SSE path is untouched: the skip lives in the buffer only.
func TestBufferFirstChoiceWins(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeSSE(wrap(w),
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"alpha"}},`+
				`{"index":1,"delta":{"content":"beta"}}]}`+"\n\n",
			`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
			"data: [DONE]"+"\n\n",
		)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(nonStreamBody(`"stream":false,`)))
	env := requireJSONCompletion(t, rec)

	msg, finish := requireChoice(t, env)
	if c, _ := msg["content"].(string); c != "alpha" {
		t.Errorf("message.content = %q, want %q (choices with index > 0 must not concatenate)",
			c, "alpha")
	}
	if body := rec.Body.String(); strings.Contains(body, "beta") {
		t.Errorf("index-1 content leaked into the flushed body: %s", truncate(rec.Body.Bytes()))
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
}
