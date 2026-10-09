package cli

// Control-API dashboard data: success counters fed by the gateway's 2xx
// path, per-egress latency, listen/uptime header fields, and API-key
// fingerprinting. Hermetic: quota state in t.TempDir(), fake Zen upstream on
// httptest (loopback only), direct-only router — no live network, no tunnel,
// no daemon.

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zen-router/internal/config"
	"zen-router/internal/gateway"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/router"
	"zen-router/internal/store"
)

// --- fixtures ---------------------------------------------------------------

// newTestRouter builds the direct-only router: quota state in t.TempDir(),
// key pool defaulting to the fallback key "public".
func newTestRouter(t *testing.T) *router.Router {
	t.Helper()
	// Neutralize environment key overrides so the fallback key is exactly
	// "public" and the assertions below are deterministic.
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	st, err := quota.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("quota.Open: %v", err)
	}
	rot, err := router.New(router.Options{
		Store:  st,
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return rot
}

// newTestControl builds the control API the way main.go wires it: resolved
// listen address and daemon start stamp five seconds in the past.
func newTestControl(rot *router.Router) *Control {
	return &Control{
		Router:    rot,
		Listen:    "127.0.0.1:8787",
		StartedAt: time.Now().Add(-5 * time.Second),
	}
}

// newTestRouterWithHistory is newTestRouter with a history store attached
// (stats endpoint fixture).
func newTestRouterWithHistory(t *testing.T, hist *store.Store) *router.Router {
	t.Helper()
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	st, err := quota.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("quota.Open: %v", err)
	}
	rot, err := router.New(router.Options{
		Store:   st,
		Logger:  log.New(io.Discard, "", 0),
		History: hist,
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return rot
}

// newChatUpstream serves a scripted 2xx chat SSE stream after an optional
// delay, so the observed gateway latency is non-trivial.
func newChatUpstream(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"c","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]"+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// gatewayStack assembles the production request path: control API + /v1/*
// gateway on ONE handler (same listener as the daemon).
func gatewayStack(t *testing.T, rot *router.Router, up *httptest.Server) http.Handler {
	t.Helper()
	gw := gateway.New(rot, config.Default())
	gw.Upstream = up.URL
	// Production wiring (main.go does the same): the optional Recorder seam
	// feeds dashboard counters + latency on 2xx.
	gw.Recorder = rot
	root := http.NewServeMux()
	root.Handle("/v1/", gw)
	return newTestControl(rot).Handler(root)
}

func newChatReq() *http.Request {
	body := `{"model":"mimo-v2.6-flash-free","stream":true,` +
		`"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// getStatus issues GET /_zenctl/status against the daemon handler and
// returns both the raw payload (for round-trip greps) and the decoded view.
func getStatus(t *testing.T, h http.Handler) ([]byte, Status) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_zenctl/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_zenctl/status: HTTP %d, want 200: %s", rec.Code, truncate(rec.Body.Bytes()))
	}
	raw := append([]byte(nil), rec.Body.Bytes()...)
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode status payload: %v\n%s", err, truncate(raw))
	}
	return raw, st
}

func truncate(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "…"
	}
	return string(b)
}

// --- TestGateway2xxRecordsSuccessCounters ----------------------------------

// TestGateway2xxRecordsSuccessCounters: a 2xx upstream attempt on the OpenAI
// surface must feed BOTH success counters — per egress (quota.RecordSuccess)
// and per API key (quota.RecordKeySuccess) — and they must be visible in
// GET /_zenctl/status.
func TestGateway2xxRecordsSuccessCounters(t *testing.T) {
	rot := newTestRouter(t)
	up := newChatUpstream(t, 30*time.Millisecond)
	h := gatewayStack(t, rot, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newChatReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions: HTTP %d, want 200: %s", rec.Code, truncate(rec.Body.Bytes()))
	}

	_, st := getStatus(t, h)
	var egressOK int64
	if e := st.State.Egress["direct"]; e != nil {
		egressOK = e.OK
	}
	if egressOK != 1 {
		t.Errorf("state.egress.direct.ok = %d, want 1 — gateway 2xx must call quota.RecordSuccess", egressOK)
	}
	var keyOK int64
	if k := st.State.Keys[fingerprintKey("public")]; k != nil {
		keyOK = k.OK
	}
	if keyOK != 1 {
		t.Errorf("state.keys[%q].ok = %d, want 1 — gateway 2xx must feed the per-key counter", fingerprintKey("public"), keyOK)
	}

	// TTFB latency of the 2xx attempt recorded per egress (upstream slept
	// 30ms). The STREAM bucket must stay untouched — the two windows are
	// separate (review F1).
	lat := st.LatencyTTFB["direct"]
	if lat.Count != 1 {
		t.Errorf("status.latency_ttfb_ms.direct.count = %d, want 1", lat.Count)
	}
	if lat.LastMS < 10 {
		t.Errorf("status.latency_ttfb_ms.direct.last_ms = %d, want >= 10 (upstream slept 30ms)", lat.LastMS)
	}
	if lat.AvgMS != lat.LastMS {
		t.Errorf("status.latency_ttfb_ms.direct.avg_ms = %d, want last_ms %d for a single sample", lat.AvgMS, lat.LastMS)
	}
	if stream := st.LatencyStream["direct"]; stream.Count != 0 {
		t.Errorf("status.latency_stream_ms.direct.count = %d, want 0 (gateway path must not feed the stream bucket)", stream.Count)
	}
}

// --- TestProxyOnResultRecordsLatency ---------------------------------------

// TestProxyOnResultRecordsLatency: the legacy reverse-proxy path observes
// Result.LatencyMS (previously discarded at router.OnResult) — a 2xx result
// must land it in the STREAM bucket of the per-egress latency view AND
// increment the egress success counter, both visible in /_zenctl/status.
// The TTFB bucket must stay untouched: different measurement window
// (review F1).
func TestProxyOnResultRecordsLatency(t *testing.T) {
	rot := newTestRouter(t)
	rot.OnResult(proxy.Result{
		Egress:    proxy.EgressDirect,
		Status:    http.StatusOK,
		LatencyMS: 42,
		Peek:      `{"ok":true}`,
	})
	h := newTestControl(rot).Handler(http.NewServeMux())

	_, st := getStatus(t, h)
	lat := st.LatencyStream["direct"]
	if lat.LastMS != 42 || lat.AvgMS != 42 || lat.Count != 1 {
		t.Errorf("status.latency_stream_ms.direct = %+v, want {last_ms:42 avg_ms:42 count:1} (Result.LatencyMS must not be discarded)", lat)
	}
	if ttfb := st.LatencyTTFB["direct"]; ttfb.Count != 0 {
		t.Errorf("status.latency_ttfb_ms.direct.count = %d, want 0 (proxy path must not feed the ttfb bucket)", ttfb.Count)
	}
	var egressOK int64
	if e := st.State.Egress["direct"]; e != nil {
		egressOK = e.OK
	}
	if egressOK != 1 {
		t.Errorf("state.egress.direct.ok = %d, want 1 — 2xx proxy result must call quota.RecordSuccess", egressOK)
	}
}

// --- TestStatusHeaderFields ------------------------------------------------

// TestStatusHeaderFields: the dashboard header fields — resolved listen
// address and daemon uptime — are served by GET /_zenctl/status with sane
// fresh-daemon values.
func TestStatusHeaderFields(t *testing.T) {
	rot := newTestRouter(t)
	h := newTestControl(rot).Handler(http.NewServeMux())

	_, st := getStatus(t, h)
	if st.Listen != "127.0.0.1:8787" {
		t.Errorf("status.listen = %q, want %q", st.Listen, "127.0.0.1:8787")
	}
	if st.UptimeSeconds < 4 || st.UptimeSeconds > 60 {
		t.Errorf("status.uptime_seconds = %d, want in [4,60] (StartedAt is 5s ago)", st.UptimeSeconds)
	}
	if !st.Up {
		t.Error("status.up = false, want true")
	}
	// Fresh daemon: both latency windows report a zeroed direct view.
	for _, m := range []struct {
		name string
		byEg map[string]router.EgressLatency
	}{
		{"latency_ttfb_ms", st.LatencyTTFB},
		{"latency_stream_ms", st.LatencyStream},
	} {
		lat, ok := m.byEg["direct"]
		if !ok {
			t.Errorf("status.%s missing \"direct\" entry: %#v", m.name, m.byEg)
			continue
		}
		if lat.Count != 0 || lat.LastMS != 0 || lat.AvgMS != 0 {
			t.Errorf("status.%s[\"direct\"] = %+v, want zeros before any request", m.name, lat)
		}
	}
}

// --- TestStatusReportsPID --------------------------------------------------

// TestStatusReportsPID (fix F1): status.pid is the pid of the process
// serving GET /_zenctl/status — `up --detach` binds its readiness check on
// it, so a pre-existing daemon can never satisfy the poll. The control layer
// reports its own process (here: the test binary itself), and `pid` is an
// additive JSON field.
func TestStatusReportsPID(t *testing.T) {
	rot := newTestRouter(t)
	h := newTestControl(rot).Handler(http.NewServeMux())

	raw, st := getStatus(t, h)
	if !strings.Contains(string(raw), `"pid"`) {
		t.Fatalf("raw status payload lacks the additive pid field:\n%s", raw)
	}
	if st.Pid == 0 {
		t.Fatalf("status.pid = 0, want the serving process's pid %d", os.Getpid())
	}
	if st.Pid != os.Getpid() {
		t.Errorf("status.pid = %d, want %d (the process running this handler)", st.Pid, os.Getpid())
	}
}

// --- TestGateway3xxNotRecorded ---------------------------------------------

// TestGateway3xxNotRecorded: a pass-through 3xx (304 — http.Client does not
// follow it, so the handler sees the status) must NOT feed the dashboard
// counters nor the TTFB latency view: only 200..299 counts as success,
// parity with router.OnResult (review F3).
func TestGateway3xxNotRecorded(t *testing.T) {
	rot := newTestRouter(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(up.Close)
	h := gatewayStack(t, rot, up)

	// Whatever envelope the relay ends up writing, nothing may be recorded.
	h.ServeHTTP(httptest.NewRecorder(), newChatReq())

	_, st := getStatus(t, h)
	if e := st.State.Egress["direct"]; e != nil && e.OK != 0 {
		t.Errorf("state.egress.direct.ok = %d, want 0 (3xx is not a success)", e.OK)
	}
	for fp, k := range st.State.Keys {
		if k != nil && k.OK != 0 {
			t.Errorf("state.keys[%q].ok = %d, want 0 (3xx must not reach the key counter)", fp, k.OK)
		}
	}
	if lat := st.LatencyTTFB["direct"]; lat.Count != 0 {
		t.Errorf("status.latency_ttfb_ms.direct.count = %d, want 0 (3xx must not be recorded)", lat.Count)
	}
}

// --- TestStatsEndpoint -------------------------------------------------------

// TestStatsEndpoint: GET /_zenctl/stats serves OK/429 rollups from the
// history store (per day, per IP), and ControlClient.Stats decodes the same
// payload through a live loopback server. A daemon without history storage
// reports empty arrays (200, not 404/500) so the TUI never special-cases it.
func TestStatsEndpoint(t *testing.T) {
	// Router WITH history: one gateway 2xx (RecordSuccess), one daily 429
	// (OnResult), IP stamp from a fixed getter.
	hist, err := store.Open(filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer hist.Close()
	hist.SetIPGetter(func() string { return "198.51.100.9" })
	rot := newTestRouterWithHistory(t, hist)

	rot.RecordSuccess("direct", "secret-key", 5)
	rot.OnResult(proxy.Result{Egress: proxy.EgressDirect, DailyLimit: true, Status: 429})

	srv := httptest.NewServer(newTestControl(rot).Handler(http.NewServeMux()))
	defer srv.Close()
	client := NewControlClient(srv.Listener.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := client.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats(): %v", err)
	}
	if len(st.Days) != 1 || st.Days[0].OK != 1 || st.Days[0].N429 != 1 {
		t.Fatalf("stats.days = %+v, want one row ok:1 429:1", st.Days)
	}
	if len(st.IPs) != 1 || st.IPs[0].IP != "198.51.100.9" || st.IPs[0].OK != 1 || st.IPs[0].N429 != 1 {
		t.Fatalf("stats.ips = %+v, want 198.51.100.9 ok:1 429:1", st.IPs)
	}

	// Router WITHOUT history: empty arrays, HTTP 200.
	plain := newTestRouter(t)
	rec := httptest.NewRecorder()
	newTestControl(plain).Handler(http.NewServeMux()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_zenctl/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_zenctl/stats (no history): HTTP %d, want 200", rec.Code)
	}
	var payload Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode empty stats: %v", err)
	}
	if payload.Days == nil || payload.IPs == nil || len(payload.Days) != 0 || len(payload.IPs) != 0 {
		t.Errorf("empty stats payload = %s, want non-null empty arrays", truncate(rec.Body.Bytes()))
	}
}

// --- TestStatusFingerprintsAPIKeys -----------------------------------------

// TestStatusFingerprintsAPIKeys: state.keys in the status payload must never
// carry a full raw API key value (review F5) — each key is replaced by its
// sha256[:8] display fingerprint while the per-key counters stay intact.
// state.json on disk keeps the raw key (control layer only).
func TestStatusFingerprintsAPIKeys(t *testing.T) {
	const rawKey = "sk-test-FULLVALUE-123"
	rot := newTestRouter(t)
	rot.Store().RecordRequestSuccess("direct", rawKey)
	h := newTestControl(rot).Handler(http.NewServeMux())

	raw, st := getStatus(t, h)
	body := string(raw)
	if strings.Contains(body, rawKey) {
		t.Errorf("status payload leaks raw API key %q: %s", rawKey, truncate(raw))
	}
	fp := fingerprintKey(rawKey)
	if len(fp) != 8 {
		t.Errorf("fingerprint %q length = %d, want 8 hex chars", fp, len(fp))
	}
	if len(st.State.Keys) != 1 {
		t.Fatalf("state.keys = %#v, want exactly the fingerprinted key", st.State.Keys)
	}
	ks, ok := st.State.Keys[fp]
	if !ok {
		t.Fatalf("state.keys has no fingerprint %q: %#v", fp, st.State.Keys)
	}
	if ks.OK != 1 {
		t.Errorf("state.keys[%q].ok = %d, want 1 — counters must survive redaction", fp, ks.OK)
	}
}
