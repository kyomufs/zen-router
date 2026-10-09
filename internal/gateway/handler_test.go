package gateway

// Task 12 end-to-end tests: the OpenAI gateway surface runs against a fake
// Zen-gateway upstream (httptest, loopback only) and the real direct-only
// router (quota state in t.TempDir()). Hermetic: no network beyond
// 127.0.0.1.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zen-router/internal/config"
	"zen-router/internal/keys"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/router"
	"zen-router/internal/zen"
)

// --- test fixtures ---------------------------------------------------------

// writePoolFile writes a keys pool-config.json holding ks and returns its
// path (copied from router's stage_test; the pool file is the only key
// source once the env override is neutralized).
func writePoolFile(t *testing.T, ks []string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"pools": map[string]any{"opencode": map[string]any{"keys": ks}},
	})
	if err != nil {
		t.Fatalf("marshal pool file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pool.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write pool file: %v", err)
	}
	return path
}

// newTestRotator builds the real direct-only router: 2-key pool, quota
// state in t.TempDir().
func newTestRotator(t *testing.T) *router.Router {
	t.Helper()
	// Neutralize environment key overrides: the pool file is the only source.
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	st, err := quota.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("quota.Open: %v", err)
	}
	rot, err := router.New(router.Options{
		Store: st,
		Pool:  keys.New(writePoolFile(t, []string{"k1", "k2"})),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return rot
}

// upRequest is one request the fake upstream observed.
type upRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// fakeUpstream is the Zen-gateway stand-in: every request is recorded, then
// answered by the per-test script with the call number (1-based).
type fakeUpstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []upRequest
	respond func(call int, w http.ResponseWriter, r *http.Request)
}

func newFakeUpstream(t *testing.T, respond func(call int, w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	u := &fakeUpstream{respond: respond}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, upRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		call := len(u.reqs)
		u.mu.Unlock()
		u.respond(call, w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) requests() []upRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upRequest(nil), u.reqs...)
}

// flushRecorder counts http.Flusher.Flush calls: the handler must flush
// after every SSE frame written to the client.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushRecorder) Flush() {
	f.flushes++
	f.ResponseRecorder.Flush()
}

func newChatRequest(body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func chatClientBody() string {
	return `{"model":"mimo-v2.6-flash-free","stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
}

// sseFrames splits a byte stream on SSE frame boundaries ("\n\n"), dropping
// empty trailing pieces.
func sseFrames(b []byte) []string {
	var out []string
	for _, part := range bytes.Split(b, []byte("\n\n")) {
		if len(bytes.TrimSpace(part)) > 0 {
			out = append(out, string(part))
		}
	}
	return out
}

func decodeJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode json %q: %v", truncate(raw), err)
	}
	return m
}

func truncate(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "…"
	}
	return string(b)
}

// requireOpenAIError asserts the CLIENT-facing error envelope is the
// OpenAI shape of spec §5:184-187 — {"error":{"message","type","code"}} —
// with NO top-level "type" wrapper (that wrapper is the UPSTREAM Zen
// envelope the daemon parses, never the one it emits). code mirrors type:
// one stable machine-readable class string per error, so Plan 3 mapping by
// error.type keeps working and code is checkable verbatim. Returns the
// decoded envelope so callers can assert 429-only metadata.
func requireOpenAIError(t *testing.T, rec *httptest.ResponseRecorder, wantType string) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	env := decodeJSONMap(t, rec.Body.Bytes())
	if _, ok := env["type"]; ok {
		t.Errorf("top-level type wrapper present — client envelope must be OpenAI shape {\"error\":{...}}: %s",
			truncate(rec.Body.Bytes()))
	}
	inner, _ := env["error"].(map[string]any)
	if inner == nil {
		t.Fatalf("envelope has no error object: %s", truncate(rec.Body.Bytes()))
	}
	if typ, _ := inner["type"].(string); typ != wantType {
		t.Errorf("error.type = %v, want %s", inner["type"], wantType)
	}
	if code, _ := inner["code"].(string); code != wantType {
		t.Errorf("error.code = %v, want %s (code mirrors type: one stable class string)", inner["code"], wantType)
	}
	if msg, _ := inner["message"].(string); msg == "" {
		t.Error("error.message is empty")
	}
	return env
}

// toolNames extracts display names from a tools array in either wire shape:
// OpenAI chat ({"function":{"name":…}}) or Responses flat ({"name":…}).
func toolNames(t *testing.T, toolsRaw any) []string {
	t.Helper()
	list, ok := toolsRaw.([]any)
	if !ok {
		t.Fatalf("tools is not an array: %#v", toolsRaw)
	}
	var names []string
	for _, entry := range list {
		tool, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("tool entry is not an object: %#v", entry)
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			names = append(names, name)
			continue
		}
		name, _ := tool["name"].(string)
		if name == "" {
			t.Fatalf("tool without a name: %#v", tool)
		}
		names = append(names, name)
	}
	return names
}

func requireNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// writeSSE streams a scripted SSE body with a flushing writer.
func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		_, _ = io.WriteString(w, f)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// dailyLimit429Body is the spec §4 typed envelope for an anonymous daily
// quota rejection (metadata present: limitName).
const dailyLimit429Body = `{"type":"error","error":{"type":"FreeUsageLimitError",` +
	`"message":"Free usage limit reached"},"metadata":{"limitName":"free"}}`

func writeDaily429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, dailyLimit429Body)
}

var sessionIDRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// --- TestChatHappyPath -----------------------------------------------------

// TestChatHappyPath: client POST → handler shapes the body (caller tools +
// injected gate tools visible upstream), sets all 8 disguise headers
// upstream, streams the SSE body through the filter (ping/cost frames
// dropped), terminates with a single [DONE], and flushes per frame.
func TestChatHappyPath(t *testing.T) {
	t.Run("streams filtered sse with gate tools and eight disguise headers", func(t *testing.T) {
		rot := newTestRotator(t)
		up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			writeSSE(wrap(w),
				`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hi"}}]}`+"\n\n",
				"event: ping"+"\n"+"data: {}"+"\n\n",
				`data: {"cost":0.001}`+"\n\n",
				`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n",
				"data: [DONE]"+"\n\n",
			)
		})
		h := New(rot, config.Default())
		h.Upstream = up.srv.URL

		clientBody := `{"model":"mimo-v2.6-flash-free","stream":true,` +
			`"messages":[{"role":"user","content":"hello"}],` +
			`"tools":[{"type":"function","function":{"name":"get_weather","description":"d","parameters":{"type":"object"}}}],` +
			`"tool_choice":"auto"}`
		rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(rec, newChatRequest(clientBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
		}

		reqs := up.requests()
		if len(reqs) != 1 {
			t.Fatalf("upstream requests = %d, want 1", len(reqs))
		}
		u := reqs[0]
		if u.Method != http.MethodPost || u.Path != "/zen/v1/chat/completions" {
			t.Errorf("upstream request = %s %s, want POST /zen/v1/chat/completions", u.Method, u.Path)
		}
		if ct := u.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("upstream Content-Type = %q, want application/json", ct)
		}
		if auth := u.Header.Get("Authorization"); auth != "Bearer k1" {
			t.Errorf("upstream Authorization = %q, want %q", auth, "Bearer k1")
		}

		// All 8 BuildHeaders disguise headers (IncludeSessionAffinity: true).
		if ua := u.Header.Get("User-Agent"); ua == "" {
			t.Error("upstream User-Agent is empty")
		}
		if client := u.Header.Get("x-opencode-client"); client == "" {
			t.Error("upstream x-opencode-client is empty")
		}
		session := u.Header.Get("x-opencode-session")
		if !sessionIDRe.MatchString(session) {
			t.Errorf("x-opencode-session = %q, want canonical ses_ id", session)
		}
		for _, name := range []string{"x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
			if got := u.Header.Get(name); got != session {
				t.Errorf("%s = %q, want session %q", name, got, session)
			}
		}
		if req := u.Header.Get("x-opencode-request"); !strings.HasPrefix(req, "req_") {
			t.Errorf("x-opencode-request = %q, want req_ prefix", req)
		}
		if proj := u.Header.Get("x-opencode-project"); !strings.HasPrefix(proj, "prj_") {
			t.Errorf("x-opencode-project = %q, want prj_ prefix", proj)
		}

		// Shaped body: caller tool first, gate tools appended, tool_choice
		// preserved because the caller had tools.
		b := decodeJSONMap(t, u.Body)
		if m, _ := b["model"].(string); m != "mimo-v2.6-flash-free" {
			t.Errorf("upstream model = %v, want mimo-v2.6-flash-free", b["model"])
		}
		if s, _ := b["stream"].(bool); !s {
			t.Errorf("upstream stream = %v, want true", b["stream"])
		}
		opts, _ := b["stream_options"].(map[string]any)
		if inc, _ := opts["include_usage"].(bool); !inc {
			t.Errorf("upstream stream_options.include_usage = %v, want true", opts)
		}
		requireNames(t, toolNames(t, b["tools"]), "get_weather", "bash", "read")
		if tc, _ := b["tool_choice"].(string); tc != "auto" {
			t.Errorf("upstream tool_choice = %q, want auto (caller tools present)", tc)
		}

		// Stream: ping/cost frames filtered, single [DONE], one flush per frame.
		frames := sseFrames(rec.Body.Bytes())
		if len(frames) != 3 {
			t.Fatalf("client frames = %d (%s), want 3", len(frames), truncate(rec.Body.Bytes()))
		}
		if rec.flushes != len(frames) {
			t.Errorf("flushes = %d, want %d (flush per frame)", rec.flushes, len(frames))
		}
		if !strings.Contains(frames[0], `"content":"Hi"`) {
			t.Errorf("frame 0 = %s, want content delta", frames[0])
		}
		body := rec.Body.String()
		if strings.Contains(body, "cost") || strings.Contains(body, "ping") {
			t.Errorf("client stream leaked cost/ping frame: %s", truncate(rec.Body.Bytes()))
		}
		if n := strings.Count(body, "data: [DONE]"); n != 1 {
			t.Errorf("[DONE] count = %d, want exactly 1", n)
		}
	})

	t.Run("zero caller tools overwrite a pre-existing tool_choice to none", func(t *testing.T) {
		rot := newTestRotator(t)
		up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			writeSSE(wrap(w),
				`data: {"id":"c","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n",
				"data: [DONE]"+"\n\n",
			)
		})
		h := New(rot, config.Default())
		h.Upstream = up.srv.URL

		// No tools array, but tool_choice already set: the gate rule must
		// overwrite it with "none" after injecting the two gate tools.
		clientBody := `{"model":"mimo-v2.6-flash-free","stream":true,` +
			`"messages":[{"role":"user","content":"hello"}],"tool_choice":"auto"}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newChatRequest(clientBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		reqs := up.requests()
		if len(reqs) != 1 {
			t.Fatalf("upstream requests = %d, want 1", len(reqs))
		}
		b := decodeJSONMap(t, reqs[0].Body)
		requireNames(t, toolNames(t, b["tools"]), "bash", "read")
		if tc, _ := b["tool_choice"].(string); tc != "none" {
			t.Errorf("upstream tool_choice = %q, want none", tc)
		}
	})
}

// wrap adapts an http.ResponseWriter to the plain signature writeSSE wants
// while keeping Flusher visibility.
func wrap(w http.ResponseWriter) http.ResponseWriter { return w }

// --- TestModelsEndpoint ----------------------------------------------------

// TestModelsEndpoint: GET /v1/models serves the OpenAI list from the
// daemon's zen.MODELS table through gateway.Mux; Mux mounts ONLY /v1/*.
func TestModelsEndpoint(t *testing.T) {
	rot := newTestRotator(t)
	cfg := config.Default()
	m := Mux(rot, cfg)

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) != len(zen.MODELS) {
		t.Fatalf("data length = %d, want %d", len(list.Data), len(zen.MODELS))
	}
	for i, m := range zen.MODELS {
		if list.Data[i].ID != m.ID {
			t.Errorf("data[%d].id = %q, want %q", i, list.Data[i].ID, m.ID)
		}
		if list.Data[i].Object != "model" {
			t.Errorf("data[%d].object = %q, want model", i, list.Data[i].Object)
		}
	}

	// Mux mounts ONLY /v1/*: control and legacy paths belong to main.go
	// (Task 13) and must not be served by this mux.
	for _, path := range []string{"/_zenctl/status", "/zen/v1/models"} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("Mux %s status = %d, want 404 (mounted outside this mux)", path, rec.Code)
		}
	}
}

// --- TestGatewayMounts -----------------------------------------------------

// TestGatewayMounts pins the mount contract cmdUp relies on (plan Task 13):
// the daemon listener stacks THREE prefixes — /_zenctl/* (cli.Control),
// /v1/* (gateway.Mux) and everything else, including the legacy /zen/v1/*
// (reverse proxy) — so gateway.Mux must serve the OpenAI surface under
// /v1/* and MISS the other two prefixes; swallowing either would
// 404-envelope the plugin's legacy traffic or shadow the control API.
func TestGatewayMounts(t *testing.T) {
	rot := newTestRotator(t)
	m := Mux(rot, config.Default())

	// Served: the OpenAI surface answers with the list envelope.
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/models status = %d, want 200", rec.Code)
	}
	var list struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode model list: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q, want list", list.Object)
	}
	if len(list.Data) == 0 {
		t.Error("data is empty, want the daemon's model table")
	}

	// Not served: both prefixes are mounted OUTSIDE this mux in cmdUp, so a
	// request reaching this mux for either must 404, not be answered.
	for _, path := range []string{"/_zenctl/status", "/zen/v1/models"} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("Mux %s status = %d, want 404 (mounted outside this mux)", path, rec.Code)
		}
	}
}

// --- TestDailyLimitKeyStepSequence -----------------------------------------

// TestDailyLimitKeyStepSequence: the upstream 429s the first attempt with
// FreeUsageLimitError; D1 re-issues on the next pool key (stage 1, same
// direct lane) and succeeds on the second call with the other key. The
// direct egress and the spent key are recorded; the client stream completes
// normally. ≤3 upstream calls (D1 budget), here 2 — one key stage only.
func TestDailyLimitKeyStepSequence(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(call int, w http.ResponseWriter, _ *http.Request) {
		if call == 1 {
			writeDaily429(w)
			return
		}
		writeSSE(wrap(w),
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n",
			"data: [DONE]"+"\n\n",
		)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(chatClientBody()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	reqs := up.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %d, want exactly 2", len(reqs))
	}

	// Attempt 1: key k1 on direct; attempt 2: key k2 (stage 1, same lane).
	wantKeys := []string{"Bearer k1", "Bearer k2"}
	for i := range reqs {
		if auth := reqs[i].Header.Get("Authorization"); auth != wantKeys[i] {
			t.Errorf("attempt %d Authorization = %q, want %q", i+1, auth, wantKeys[i])
		}
		if p := reqs[i].Path; p != "/zen/v1/chat/completions" {
			t.Errorf("attempt %d path = %s, want /zen/v1/chat/completions", i+1, p)
		}
	}

	// Counters: egress direct saw the one rejection, k1 saw it too.
	snap := rot.Store().Snapshot()
	if eg := snap.Egress["direct"]; eg == nil || eg.Daily429 != 1 {
		t.Errorf("egress direct = %+v, want Daily429=1", eg)
	}
	if ks := snap.Keys["k1"]; ks == nil || ks.Daily429 != 1 {
		t.Errorf("key k1 = %+v, want Daily429=1", ks)
	}

	// The client still got a normal stream.
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"ok"`) || strings.Count(body, "data: [DONE]") != 1 {
		t.Errorf("client stream = %s, want content frame + single [DONE]", truncate(rec.Body.Bytes()))
	}
}

// --- TestBudgetExhaustedSurfaces429 ----------------------------------------

// TestBudgetExhaustedSurfaces429: the upstream is out of daily quota for
// every attempt. The stage machine executes the first attempt plus ONE key
// re-issue (2 requests, within the D1 cap of 3), then the client receives
// exactly one 429 carrying the classified envelope, a Retry-After
// synthesized to the next UTC midnight, and metadata (429-only).
func TestBudgetExhaustedSurfaces429(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		// No Retry-After header: classification synthesizes it to midnight.
		writeDaily429(w)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	wantRetry := int(midnight.Sub(now).Seconds())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(chatClientBody()))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	if n := len(up.requests()); n != 2 {
		t.Errorf("upstream requests = %d, want exactly 2 (first attempt + one key re-issue, no infinite loop)", n)
	}

	env := requireOpenAIError(t, rec, "FreeUsageLimitError")
	md, _ := env["metadata"].(map[string]any)
	if md == nil {
		t.Fatalf("429 envelope metadata is not an object: %s", truncate(rec.Body.Bytes()))
	}
	// Value round-trip, not mere presence: the upstream 429 body carries
	// metadata.limitName and it must survive probeEnvelope → writeError.
	if ln, _ := md["limitName"].(string); ln != "free" {
		t.Errorf("metadata.limitName = %v, want free (round-tripped from the upstream 429)", md["limitName"])
	}

	gotRetry := rec.Header().Get("Retry-After")
	if gotRetry == "" {
		t.Fatal("Retry-After header missing on 429")
	}
	n, err := strconv.Atoi(gotRetry)
	if err != nil {
		t.Fatalf("Retry-After = %q, want whole seconds: %v", gotRetry, err)
	}
	if n < wantRetry-30 || n > wantRetry+5 {
		t.Errorf("Retry-After = %d, want ≈ %d (seconds to next UTC midnight)", n, wantRetry)
	}
}

// TestMetadataNullUpstreamBecomesEmptyObject (DM-13): an upstream 429
// whose body carries "metadata":null must NOT reach the client as
// metadata:null — JSON null is treated as absent, so the envelope falls
// back to writeError's documented {} else-branch (429 metadata is always
// an object, spec §4:140).
func TestMetadataNullUpstreamBecomesEmptyObject(t *testing.T) {
	rot := newTestRotator(t)
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeUsageLimitError",`+
			`"message":"Free usage limit reached"},"metadata":null}`)
	})
	h := New(rot, config.Default())
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(chatClientBody()))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	env := requireOpenAIError(t, rec, "FreeUsageLimitError")
	md, ok := env["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("429 envelope metadata = %#v, want {} (upstream null is absent, never null): %s",
			env["metadata"], truncate(rec.Body.Bytes()))
	}
	if len(md) != 0 {
		t.Errorf("429 envelope metadata = %#v, want empty {}", md)
	}
	if body := rec.Body.String(); strings.Contains(body, `"metadata":null`) {
		t.Errorf("client body carries metadata:null: %s", truncate(rec.Body.Bytes()))
	}
}

// recordingRot is a rotator that always offers a next attempt and records
// every Report it receives — it proves the handler consults the rotator for
// bookkeeping even when the first-byte guard forbids the re-issue.
type recordingRot struct {
	reports []router.Report
}

func (r *recordingRot) Attempt() router.Attempt {
	return router.Attempt{Key: "k1", Egress: proxy.EgressDirect, Transport: http.DefaultTransport, Step: 0}
}

func (r *recordingRot) NextAttempt(rep router.Report) (router.Attempt, bool) {
	r.reports = append(r.reports, rep)
	return router.Attempt{
		Key:       "k2",
		Egress:    rep.Egress,
		Transport: http.DefaultTransport,
		Step:      rep.Step + 1,
	}, true
}

// countingRot wraps a Rotator and counts every upstream RoundTrip: the
// executed-attempt counter, observable even for a refused dial — a dead
// server never receives an HTTP request, so the upstream request recorder
// alone could not prove "never re-issue".
type countingRot struct {
	inner Rotator
	mu    sync.Mutex
	dials int
}

func (c *countingRot) Attempt() router.Attempt {
	return c.wrap(c.inner.Attempt())
}

func (c *countingRot) NextAttempt(rep router.Report) (router.Attempt, bool) {
	att, ok := c.inner.NextAttempt(rep)
	if ok {
		att = c.wrap(att)
	}
	return att, ok
}

func (c *countingRot) wrap(att router.Attempt) router.Attempt {
	att.Transport = &countingRT{base: att.Transport, rot: c}
	return att
}

func (c *countingRot) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dials
}

// countingRT counts RoundTrip calls — one per executed upstream attempt,
// whether the dial succeeds or is refused.
type countingRT struct {
	base http.RoundTripper
	rot  *countingRot
}

func (rt *countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.rot.mu.Lock()
	rt.rot.dials++
	rt.rot.mu.Unlock()
	return rt.base.RoundTrip(r)
}

// --- TestNoReissueAfterFirstByte -------------------------------------------

// TestNoReissueAfterFirstByte: attempt 1 streams one frame to the client and
// then dies. firstByteWritten latches, so no re-issue happens — even when
// the rotator says a next attempt exists (poisoned attempt 2 would 429).
// The failure Report is still handed to the rotator, so rotation bookkeeping
// is recorded for later requests.
func TestNoReissueAfterFirstByte(t *testing.T) {
	abortAfterFrame := func(call int, w http.ResponseWriter, _ *http.Request) {
		if call == 1 {
			writeSSE(wrap(w), `data: {"id":"c","choices":[{"index":0,"delta":{"content":"Hi"}}]}`+"\n\n")
			panic(http.ErrAbortHandler) // kill the upstream body mid-stream
		}
		writeDaily429(w)
	}

	t.Run("guard beats an eager rotator and the report is still recorded", func(t *testing.T) {
		rot := &recordingRot{}
		up := newFakeUpstream(t, abortAfterFrame)
		h := New(rot, config.Default())
		h.Upstream = up.srv.URL

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newChatRequest(chatClientBody()))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (headers already committed)", rec.Code)
		}
		reqs := up.requests()
		if len(reqs) != 1 {
			t.Errorf("upstream requests = %d, want 1 (no re-issue after first byte)", len(reqs))
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"content":"Hi"`) {
			t.Errorf("client stream missing the flushed frame: %s", truncate(rec.Body.Bytes()))
		}
		if strings.Contains(body, "[DONE]") {
			t.Errorf("client stream must not carry [DONE] after a mid-stream failure: %s",
				truncate(rec.Body.Bytes()))
		}
		if len(rot.reports) != 1 {
			t.Fatalf("rotator reports = %d, want 1 (guarded failure still reported)", len(rot.reports))
		}
		if rot.reports[0].Key != "k1" || rot.reports[0].Step != 0 {
			t.Errorf("report = %+v, want Key=k1 Step=0", rot.reports[0])
		}
	})

	t.Run("rotation still records for later requests", func(t *testing.T) {
		rot := newTestRotator(t)
		up := newFakeUpstream(t, func(call int, w http.ResponseWriter, _ *http.Request) {
			switch {
			case call == 1:
				writeSSE(wrap(w), `data: {"id":"c","choices":[{"index":0,"delta":{"content":"Hi"}}]}`+"\n\n")
				panic(http.ErrAbortHandler)
			case call == 2:
				writeDaily429(w)
			default:
				writeSSE(wrap(w),
					`data: {"id":"c","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n",
					"data: [DONE]"+"\n\n",
				)
			}
		})
		h := New(rot, config.Default())
		h.Upstream = up.srv.URL

		// Request 1: guarded mid-stream failure — exactly one upstream call.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newChatRequest(chatClientBody()))
		if n := len(up.requests()); n != 1 {
			t.Fatalf("request 1 upstream calls = %d, want 1", n)
		}

		// Request 2: the usual daily 429 → rotation still runs and records.
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, newChatRequest(chatClientBody()))
		if rec2.Code != http.StatusOK {
			t.Fatalf("request 2 status = %d, want 200", rec2.Code)
		}
		if n := len(up.requests()); n != 3 {
			t.Fatalf("total upstream calls = %d, want 3 (1 + 2-then-success)", n)
		}
		snap := rot.Store().Snapshot()
		if eg := snap.Egress["direct"]; eg == nil || eg.Daily429 != 1 {
			t.Errorf("egress direct = %+v, want Daily429=1 recorded by request 2", eg)
		}
		if ks := snap.Keys["k2"]; ks == nil || ks.Daily429 != 1 {
			t.Errorf("key k2 = %+v, want Daily429=1 recorded by request 2", ks)
		}
	})
}

// --- TestResponsesAutoRoute ------------------------------------------------

// TestResponsesAutoRoute: a Responses-capable model routes the (chat-wire)
// client request to POST /zen/v1/responses with a translated body —
// stream:true forced, no EnsureFreeLaneShape applied, effort clamped
// handler-side — and the translated Responses stream comes back as stamped
// chat frames (id/model/created on every frame, usage frame with
// "choices":[]), ending in a single [DONE].
func TestResponsesAutoRoute(t *testing.T) {
	const responsesModel = "muse-spark-1.3-contributor-free"

	t.Run("auto routes and stamps translated frames", func(t *testing.T) {
		rot := newTestRotator(t)
		up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			writeSSE(wrap(w),
				`data: {"type":"response.output_text.delta","delta":"Hello","output_index":0}`+"\n\n",
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}`+"\n\n",
			)
		})
		h := New(rot, config.Default())
		h.Upstream = up.srv.URL

		// Client requests SSE explicitly while over-asking effort: the
		// handler must still force stream:true upstream (and clamp "max" →
		// "xhigh" before ChatToResponses) — the client flag only chooses
		// the LOCAL rendering (buffered JSON when false/omitted).
		// Whole-branch-2 nit (N-1): this is no longer the force-proof —
		// the client body below already sends "stream":true, so the
		// upstream value would be true either way. The force itself is
		// pinned by TestNonStreamingResponsesLane (client stream:false →
		// upstream still true).
		clientBody := `{"model":"` + responsesModel + `","stream":true,` +
			`"messages":[{"role":"user","content":"hello"}],"reasoning_effort":"max"}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newChatRequest(clientBody))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
		}
		reqs := up.requests()
		if len(reqs) != 1 {
			t.Fatalf("upstream requests = %d, want 1", len(reqs))
		}
		u := reqs[0]
		if u.Path != "/zen/v1/responses" {
			t.Fatalf("upstream path = %s, want /zen/v1/responses", u.Path)
		}

		b := decodeJSONMap(t, u.Body)
		if s, _ := b["stream"].(bool); !s {
			t.Errorf("upstream stream = %v, want true (forced upstream regardless of the client flag)", b["stream"])
		}
		if _, ok := b["messages"]; ok {
			t.Errorf("responses body carries chat-only key messages: %s", truncate(u.Body))
		}
		if tc, _ := b["tool_choice"].(string); tc != "auto" {
			t.Errorf("upstream tool_choice = %q, want auto (no EnsureFreeLaneShape)", tc)
		}
		requireNames(t, toolNames(t, b["tools"]), "bash", "read")
		reasoning, _ := b["reasoning"].(map[string]any)
		if effort, _ := reasoning["effort"].(string); effort != "xhigh" {
			t.Errorf("reasoning.effort = %v, want xhigh (clamped from max)", reasoning["effort"])
		}

		// Translated chat frames out.
		frames := sseFrames(rec.Body.Bytes())
		if len(frames) != 4 {
			t.Fatalf("client frames = %d (%s), want delta+finish+usage+[DONE]",
				len(frames), truncate(rec.Body.Bytes()))
		}
		var (
			firstID  string
			firstMdl string
			firstCrt float64
		)
		for i, f := range frames[:3] {
			if !strings.HasPrefix(f, "data: ") {
				t.Fatalf("frame %d = %q, want data: payload", i, f)
			}
			var chunk map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(f, "data: ")), &chunk); err != nil {
				t.Fatalf("frame %d not JSON: %v", i, err)
			}
			id, _ := chunk["id"].(string)
			model, _ := chunk["model"].(string)
			created, hasCreated := chunk["created"].(float64)
			if id == "" || model == "" || !hasCreated || created <= 0 {
				t.Errorf("frame %d missing stamps (created is a unix timestamp): %s", i, f)
			}
			if model != responsesModel {
				t.Errorf("frame %d model = %q, want %q", i, model, responsesModel)
			}
			if i == 0 {
				firstID, firstMdl, firstCrt = id, model, created
			} else if id != firstID || model != firstMdl || created != firstCrt {
				t.Errorf("frame %d stamps = (%s,%s,%v), want stable (%s,%s,%v)",
					i, id, model, created, firstID, firstMdl, firstCrt)
			}
		}
		if !strings.Contains(frames[0], `"content":"Hello"`) {
			t.Errorf("delta frame = %s, want content Hello", frames[0])
		}
		if !strings.Contains(frames[1], `"finish_reason":"stop"`) {
			t.Errorf("finish frame = %s, want finish_reason stop", frames[1])
		}
		usage := decodeJSONMap(t, []byte(strings.TrimPrefix(frames[2], "data: ")))
		if choices, ok := usage["choices"].([]any); !ok || len(choices) != 0 {
			t.Errorf("usage frame choices = %#v, want empty array", usage["choices"])
		}
		uu, _ := usage["usage"].(map[string]any)
		if tt, _ := uu["total_tokens"].(float64); int(tt) != 7 {
			t.Errorf("usage total_tokens = %v, want 7", uu["total_tokens"])
		}
		if strings.Count(rec.Body.String(), "data: [DONE]") != 1 {
			t.Errorf("[DONE] count = %d, want 1", strings.Count(rec.Body.String(), "data: [DONE]"))
		}
	})

	t.Run("ChatToResponses input error surfaces 400 before any upstream attempt", func(t *testing.T) {
		rot := newTestRotator(t)
		up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
			t.Error("upstream must not be reached for an input error")
			w.WriteHeader(http.StatusOK)
		})
		h := New(rot, config.Default())
		h.Upstream = up.srv.URL

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newChatRequest(`{"model":"`+responsesModel+`","messages":[]}`))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
		}
		if n := len(up.requests()); n != 0 {
			t.Errorf("upstream requests = %d, want 0 (rejected pre-upstream)", n)
		}
		requireOpenAIError(t, rec, "InvalidRequestError")
	})
}

// --- TestUpstreamDownSurfaces502 -------------------------------------------

// TestUpstreamDownSurfaces502: a dial error classifies as KindTransport —
// retryable, but NextAttempt has no stage for it, so the client gets one
// KindTransport-class JSON envelope with status 502 instead of a hang.
// attempts==1 (counted at the transport) asserts transport failures are
// never re-issued beyond the loop's rules.
func TestUpstreamDownSurfaces502(t *testing.T) {
	rot := &countingRot{inner: newTestRotator(t)}
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // loopback port with nothing listening: dial is refused

	h := New(rot, config.Default())
	h.Upstream = dead.URL

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatRequest(chatClientBody()))

	if n := rot.attempts(); n != 1 {
		t.Errorf("upstream attempts = %d, want 1 (transport failures are never re-issued)", n)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	env := requireOpenAIError(t, rec, "TransportError")
	if _, ok := env["metadata"]; ok {
		t.Errorf("metadata must appear only on 429 envelopes: %s", truncate(rec.Body.Bytes()))
	}
}

// --- TestFirstEventBudgetCoversHeaders -------------------------------------

// TestFirstEventBudgetCoversHeaders: an upstream that completes the
// connection but writes NOTHING (no status line, no headers) must be cut
// off by cfg.FirstEventTimeout — the budget covers connect + response
// headers (the port arms FIRST_EVENT before fetch), not only the body's
// first byte. Without a header-phase timer the handler goroutine would be
// pinned inside http.Client.Do until the client disconnects.
func TestFirstEventBudgetCoversHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.FirstEventTimeout = 100 * time.Millisecond

	rot := newTestRotator(t)
	release := make(chan struct{})
	up := newFakeUpstream(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		// Accept the request, then write NOTHING: no status, no headers.
		<-release
	})
	// Cleanups run LIFO: release the stalled handler BEFORE srv.Close waits
	// on it, so the suite can never deadlock even in the failing state.
	t.Cleanup(func() { close(release) })

	h := New(rot, cfg)
	h.Upstream = up.srv.URL

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, newChatRequest(chatClientBody()))
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler still blocked after 2s: first-event budget (%s) did not cover the connect+headers phase",
			cfg.FirstEventTimeout)
	}

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %q)", rec.Code, truncate(rec.Body.Bytes()))
	}
	requireOpenAIError(t, rec, "TransportError")
	if n := len(up.requests()); n != 1 {
		t.Errorf("upstream attempts = %d, want 1 (stalled-headers attempt is not re-issued)", n)
	}
}
