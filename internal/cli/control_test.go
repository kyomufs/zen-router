package cli

// Task 1 (Phase C): control-API dashboard data — success counters fed by the
// gateway's 2xx path, per-egress latency, rotation/spare-registration status,
// listen/uptime header fields, and a credential-redacted identity view.
// Hermetic: quota state in t.TempDir(), fake Zen upstream on httptest
// (loopback only), privileged router seams (identity switch, spare
// registration) faked — no live network, no tunnel, no daemon.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zen-router/internal/config"
	"zen-router/internal/gateway"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/router"
	"zen-router/internal/zen"
)

// --- fixtures ---------------------------------------------------------------

// newTestRouter builds the real staged router with its privileged operations
// faked: quota state in t.TempDir(), no-op identity switch (no tunnel) and an
// injected spare registrar (never the real Cloudflare registration).
func newTestRouter(t *testing.T, reg func(context.Context) error, cooldown time.Duration) *router.Router {
	t.Helper()
	// Neutralize environment key overrides so the fallback key is exactly
	// "public" and the assertions below are deterministic.
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	st, err := quota.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("quota.Open: %v", err)
	}
	if reg == nil {
		reg = func(context.Context) error { return nil }
	}
	rot, err := router.New(router.Options{
		Store:            st,
		Logger:           log.New(io.Discard, "", 0),
		RotationCooldown: cooldown,
		IdentitySwitch: func(*quota.WarpIdentity) (http.RoundTripper, error) {
			return http.DefaultTransport, nil
		},
		SpareRegistrar: reg,
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
// and per API key (quota.RecordKeySuccess, previously zero production
// callers) — and they must be visible in GET /_zenctl/status.
func TestGateway2xxRecordsSuccessCounters(t *testing.T) {
	rot := newTestRouter(t, nil, 0)
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
	if k := st.State.Keys["public"]; k != nil {
		keyOK = k.OK
	}
	if keyOK != 1 {
		t.Errorf("state.keys[\"public\"].ok = %d, want 1 — gateway 2xx must call quota.RecordKeySuccess", keyOK)
	}

	// Latency of the 2xx attempt recorded per egress (upstream slept 30ms).
	lat, ok := st.Latency["direct"]
	if !ok {
		t.Fatalf("status.latency has no \"direct\" entry: %#v", st.Latency)
	}
	if lat.Count != 1 {
		t.Errorf("status.latency.direct.count = %d, want 1", lat.Count)
	}
	if lat.LastMS < 10 {
		t.Errorf("status.latency.direct.last_ms = %d, want >= 10 (upstream slept 30ms)", lat.LastMS)
	}
	if lat.AvgMS != lat.LastMS {
		t.Errorf("status.latency.direct.avg_ms = %d, want last_ms %d for a single sample", lat.AvgMS, lat.LastMS)
	}
}

// --- TestProxyOnResultRecordsLatency ---------------------------------------

// TestProxyOnResultRecordsLatency: the legacy reverse-proxy path observes
// Result.LatencyMS (previously discarded at router.OnResult) — a 2xx result
// must land it in the per-egress latency view AND increment the egress
// success counter, both visible in /_zenctl/status.
func TestProxyOnResultRecordsLatency(t *testing.T) {
	rot := newTestRouter(t, nil, 0)
	rot.OnResult(proxy.Result{
		Egress:    proxy.EgressDirect,
		Status:    http.StatusOK,
		LatencyMS: 42,
		Peek:      `{"ok":true}`,
	})
	h := newTestControl(rot).Handler(http.NewServeMux())

	_, st := getStatus(t, h)
	lat, ok := st.Latency["direct"]
	if !ok {
		t.Fatalf("status.latency has no \"direct\" entry: %#v", st.Latency)
	}
	if lat.LastMS != 42 || lat.AvgMS != 42 || lat.Count != 1 {
		t.Errorf("status.latency.direct = %+v, want {last_ms:42 avg_ms:42 count:1} (Result.LatencyMS must not be discarded)", lat)
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
// address, daemon uptime, rotation/spare-registration activity — are served
// by GET /_zenctl/status with sane fresh-daemon values.
func TestStatusHeaderFields(t *testing.T) {
	rot := newTestRouter(t, nil, 0)
	h := newTestControl(rot).Handler(http.NewServeMux())

	_, st := getStatus(t, h)
	if st.Listen != "127.0.0.1:8787" {
		t.Errorf("status.listen = %q, want %q", st.Listen, "127.0.0.1:8787")
	}
	if st.UptimeSeconds < 4 || st.UptimeSeconds > 60 {
		t.Errorf("status.uptime_seconds = %d, want in [4,60] (StartedAt is 5s ago)", st.UptimeSeconds)
	}
	if st.Rotating {
		t.Error("status.rotating = true, want false (no rotation in flight)")
	}
	if st.Registering {
		t.Error("status.registering = true, want false (no spare registration in flight)")
	}
	if st.LastSpareError != "" {
		t.Errorf("status.lastSpareError = %q, want empty on a fresh daemon", st.LastSpareError)
	}
	if st.LastRotate != "" {
		t.Errorf("status.last_rotate = %q, want empty (no rotation this run)", st.LastRotate)
	}
	if !st.Up {
		t.Error("status.up = false, want true")
	}
	// Fresh daemon: both egresses report a zeroed latency view.
	for _, eg := range []string{"direct", "warp"} {
		lat, ok := st.Latency[eg]
		if !ok {
			t.Errorf("status.latency missing %q entry: %#v", eg, st.Latency)
			continue
		}
		if lat.Count != 0 || lat.LastMS != 0 || lat.AvgMS != 0 {
			t.Errorf("status.latency[%q] = %+v, want zeros before any request", eg, lat)
		}
	}
}

// --- TestStatusSurfacesSpareRegistration -----------------------------------

// TestStatusSurfacesSpareRegistration: a stage-2 identity switch schedules
// the background spare registration (spec §6/§14). While it runs, status.
// registering is true; its failure surfaces as status.lastSpareError and is
// cleared by the next successful registration. The same switch stamps
// status.last_rotate (RFC3339, parseable) without flipping rotating.
func TestStatusSurfacesSpareRegistration(t *testing.T) {
	regStarted := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	reg := func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(regStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return errors.New("registration exploded")
		}
		return nil
	}
	// 1ns cooldown: every identity step may schedule a fresh registration.
	rot := newTestRouter(t, reg, time.Nanosecond)
	// Two identities: the first switch consumes the active slot, the second
	// drive (from warp) needs a distinct fresh spare to switch onto.
	for _, dev := range []string{"dev1", "dev2"} {
		rot.Store().AddIdentity(&quota.WarpIdentity{
			DeviceID:     dev,
			RegisteredAt: time.Now().UnixMilli(),
		})
	}
	h := newTestControl(rot).Handler(http.NewServeMux())

	// Stage 2 from direct: switch identity → schedule spare registration.
	if _, ok := rot.NextAttempt(router.Report{
		Kind: zen.KindDailyLimit, Egress: proxy.EgressDirect, Key: "k1", Step: 1,
	}); !ok {
		t.Fatal("NextAttempt(step 1): want an identity-switch attempt")
	}
	<-regStarted
	_, st := getStatus(t, h)
	if !st.Registering {
		t.Error("status.registering = false, want true while spare registration is in flight")
	}

	close(release)
	waitFor(t, "lastSpareError to surface", func() bool {
		_, s := getStatus(t, h)
		return s.LastSpareError == "registration exploded" && !s.Registering
	})

	_, st = getStatus(t, h)
	if st.LastRotate == "" {
		t.Fatal("status.last_rotate = empty, want a stamp from the identity switch")
	}
	if _, err := time.Parse(time.RFC3339, st.LastRotate); err != nil {
		t.Errorf("status.last_rotate = %q, want RFC3339: %v", st.LastRotate, err)
	}
	if st.Rotating {
		t.Error("status.rotating = true, want false (no rotation in flight)")
	}

	// Second switch (warp side, cooldown neutralized): a successful
	// registration clears the stale error.
	if _, ok := rot.NextAttempt(router.Report{
		Kind: zen.KindDailyLimit, Egress: proxy.EgressWarp, Key: "k1", Step: 1,
	}); !ok {
		t.Fatal("NextAttempt(step 1, warp): want an identity-switch attempt")
	}
	waitFor(t, "lastSpareError to clear after a successful registration", func() bool {
		_, s := getStatus(t, h)
		return s.LastSpareError == "" && !s.Registering
	})
}

// waitFor polls cond until it holds or 5s elapse (the spare registration
// runs on a background goroutine).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// --- TestStatusHidesIdentityCredentials ------------------------------------

// TestStatusHidesIdentityCredentials: the status payload is the dashboard
// view (spec §14: loopback control API) — it must carry NO WARP credentials.
// Asserted through a JSON round-trip of the serialized payload: neither the
// "token"/"privateKey" field names nor their values may appear; the identity
// itself stays visible (deviceId) so the pool table still renders.
func TestStatusHidesIdentityCredentials(t *testing.T) {
	rot := newTestRouter(t, nil, 0)
	rot.Store().AddIdentity(&quota.WarpIdentity{
		DeviceID:     "dev-redact-1",
		Token:        "SECRET-TOKEN-ABC123",
		PrivateKey:   "SECRET-PRIVATE-KEY-XYZ789",
		PublicKey:    "pub-1",
		RegisteredAt: time.Now().UnixMilli(),
	})
	h := newTestControl(rot).Handler(http.NewServeMux())

	raw, st := getStatus(t, h)

	// Round-trip: decode the payload and re-walk the identity views.
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("re-decode status payload: %v", err)
	}
	for _, id := range append(identityViews(t, decoded, "state", "identities"),
		identityViews(t, decoded, "state", "warp")...) {
		if _, ok := id["token"]; ok {
			t.Errorf("identity in status payload has field \"token\": %#v", id)
		}
		if _, ok := id["privateKey"]; ok {
			t.Errorf("identity in status payload has field \"privateKey\": %#v", id)
		}
	}
	body := string(raw)
	for _, secret := range []string{"SECRET-TOKEN-ABC123", "SECRET-PRIVATE-KEY-XYZ789"} {
		if strings.Contains(body, secret) {
			t.Errorf("status payload leaks credential value %q: %s", secret, truncate(raw))
		}
	}
	if !strings.Contains(body, "dev-redact-1") {
		t.Errorf("status payload must still carry the identity itself: %s", truncate(raw))
	}
	if len(st.State.Identities) == 0 || st.State.Identities[0].DeviceID != "dev-redact-1" {
		t.Errorf("decoded identities = %#v, want dev-redact-1 present", st.State.Identities)
	}
}

// identityViews extracts the identity objects under the given JSON path:
// "state.identities" is an array, "state.warp" a single object.
func identityViews(t *testing.T, root map[string]any, path ...string) []map[string]any {
	t.Helper()
	var cur any = root
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	switch v := cur.(type) {
	case map[string]any:
		return []map[string]any{v}
	case []any:
		var out []map[string]any
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}
