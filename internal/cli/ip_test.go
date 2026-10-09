package cli

// Task 2 (Phase C): egress IP observation — GET /_zenctl/status gains
// egress_ip, refreshed by a debounced echo seam: lazily on status reads and
// by an explicit Refresh (startup hook), at most ONE echo in flight, minimum
// egressIPMinInterval between attempts, failures keep the last value.
// Hermetic: fake IPEchoer implementations only; the production echoer is
// exercised against a loopback httptest server. NO live network, and the
// production path stays behind config.EgressIPEcho (default false) so no
// daemon-side echo can fire without explicit user opt-in.
//
// Provenance: egress-IP echo has NO spec section — it is a user-requested
// extra from this plan's own origin (D3); do not claim spec parity for it.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zen-router/internal/config"
	"zen-router/internal/router"
)

// --- fixtures ---------------------------------------------------------------

// fakeEchoer is an in-process IPEchoer: it counts every attempt and returns
// the scripted result. started (buffered) signals call entry, release blocks
// the call until the test lets it finish — the seam for the single-in-flight
// assertion. Never touches the network.
type fakeEchoer struct {
	mu      sync.Mutex
	calls   int
	ip      string
	err     error
	started chan struct{} // optional: signalled on call entry
	release chan struct{} // optional: call waits here before returning
}

func newFakeEchoer(ip string) *fakeEchoer {
	return &fakeEchoer{ip: ip}
}

func (f *fakeEchoer) EgressIP(ctx context.Context) (string, error) {
	f.mu.Lock()
	f.calls++
	started, release := f.started, f.release
	f.mu.Unlock()

	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ip, f.err
}

func (f *fakeEchoer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeEchoer) set(ip string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ip, f.err = ip, err
}

// newEchoControl assembles control + router + tracker the way main.go wires
// them: Control.IPTracker → the tracker. interval/now are the debounce seams
// (tests use the fake clock).
func newEchoControl(t *testing.T, echoer IPEchoer, interval time.Duration, now func() time.Time) (*router.Router, *EgressIPTracker, http.Handler) {
	t.Helper()
	rot := newTestRouter(t)
	tr := newEgressIPTracker(echoer, interval, now)
	ctrl := &Control{
		Router:    rot,
		Listen:    "127.0.0.1:8787",
		StartedAt: time.Now().Add(-5 * time.Second),
		IPTracker: tr,
	}
	h := ctrl.Handler(http.NewServeMux())
	return rot, tr, h
}

// waitFor polls cond until it holds or 5s elapse.
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

// newFakeClock returns a readable clock seam: advance to move time forward.
func newFakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	cur := start
	return func() time.Time {
			return cur
		}, func(d time.Duration) {
			cur = cur.Add(d)
		}
}

// echoRoundTripperFunc adapts a function to http.RoundTripper (test-local).
type echoRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f echoRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// --- TestStatusEgressIPField ------------------------------------------------

// TestStatusEgressIPField: GET /_zenctl/status gains the egress_ip field
// (plan Task 2 controller ruling). The fake echoer's value must surface in
// the decoded payload AND in the raw JSON under the exact "egress_ip" key.
func TestStatusEgressIPField(t *testing.T) {
	fake := newFakeEchoer("203.0.113.9")
	_, _, h := newEchoControl(t, fake, egressIPMinInterval, time.Now)

	waitFor(t, "status.egress_ip to surface the echoed IP", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.9"
	})
	raw, _ := getStatus(t, h)
	if !strings.Contains(string(raw), `"egress_ip"`) {
		t.Errorf("status payload missing the \"egress_ip\" key: %s", truncate(raw))
	}
	if fake.count() != 1 {
		t.Errorf("echo calls = %d, want 1 for a single status read", fake.count())
	}
}

// --- TestEgressIPDisabledByDefault -----------------------------------------

// TestEgressIPDisabledByDefault: the §12 gate — config.EgressIPEcho defaults
// to false, NewEgressIPEchoer(false, ...) yields NO echoer, and a tracker
// without an echoer reports an empty egress_ip forever (no live call can be
// built without the user's отмашка).
func TestEgressIPDisabledByDefault(t *testing.T) {
	if config.Default().EgressIPEcho {
		t.Fatal("config.Default().EgressIPEcho = true, want false (§12 gate: live echo needs explicit opt-in)")
	}
	if e := NewEgressIPEchoer(false, nil); e != nil {
		t.Fatalf("NewEgressIPEchoer(false, nil) = %#v, want nil (disabled echoer must not exist)", e)
	}

	_, _, h := newEchoControl(t, NewEgressIPEchoer(false, nil), egressIPMinInterval, time.Now)
	for i := 0; i < 20; i++ {
		_, st := getStatus(t, h)
		if st.EgressIP != "" {
			t.Fatalf("status.egress_ip = %q after %d reads, want empty while the echo gate is closed", st.EgressIP, i+1)
		}
	}
}

// --- TestEgressIPBoundedUnderStatusBurst -----------------------------------

// TestEgressIPBoundedUnderStatusBurst: the TUI polls /_zenctl/status every
// 1s — a burst of status reads must trigger AT MOST ONE echo attempt (no
// per-poll fan-out): the first read starts it, every later read is gated by
// the single-in-flight flag and the egressIPMinInterval debounce.
func TestEgressIPBoundedUnderStatusBurst(t *testing.T) {
	fake := newFakeEchoer("198.51.100.20")
	_, _, h := newEchoControl(t, fake, egressIPMinInterval, time.Now)

	waitFor(t, "the first status read to start the echo", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "198.51.100.20"
	})
	if got := fake.count(); got != 1 {
		t.Fatalf("echo calls after initial read = %d, want 1", got)
	}
	for i := 0; i < 100; i++ {
		_, st := getStatus(t, h)
		if st.EgressIP != "198.51.100.20" {
			t.Fatalf("status.egress_ip = %q mid-burst, want the observed value to stick", st.EgressIP)
		}
	}
	if got := fake.count(); got != 1 {
		t.Errorf("echo calls after a 100-read burst = %d, want 1 — status reads must not fan out echoes", got)
	}
}

// --- TestEgressIPOneInFlight ------------------------------------------------

// TestEgressIPOneInFlight: while an echo attempt is blocked, concurrent
// status reads must NOT start additional attempts — exactly ONE call reaches
// the echoer (at most one in-flight, per the debounce rule).
func TestEgressIPOneInFlight(t *testing.T) {
	fake := &fakeEchoer{
		ip:      "192.0.2.77",
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	_, _, h := newEchoControl(t, fake, egressIPMinInterval, time.Now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_zenctl/status", nil))
	}()
	<-fake.started // the first read's echo is now in flight and blocked

	for i := 0; i < 20; i++ {
		_, st := getStatus(t, h)
		if st.EgressIP != "" {
			t.Fatalf("status.egress_ip = %q while the echo is still in flight, want empty (no value yet)", st.EgressIP)
		}
	}
	if got := fake.count(); got != 1 {
		t.Fatalf("echo calls while one attempt is in flight = %d, want exactly 1", got)
	}

	close(fake.release)
	<-done
	waitFor(t, "the blocked echo to land", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "192.0.2.77"
	})
	if got := fake.count(); got != 1 {
		t.Errorf("echo calls after completion = %d, want 1", got)
	}
}

// --- TestEgressIPRefreshReEchoes ---------------------------------------------

// TestEgressIPRefreshReEchoes: Refresh (the startup hook main.go wires at
// boot, and the path any egress change takes) marks the observation stale
// and must re-echo through the active transport so status.egress_ip reflects
// the NEW IP. The fake clock steps over the debounce window first; without
// the hook the value would stay stale forever (status reads only refresh a
// dirty tracker).
func TestEgressIPRefreshReEchoes(t *testing.T) {
	fake := newFakeEchoer("203.0.113.1")
	now, advance := newFakeClock(time.Now())
	_, tr, h := newEchoControl(t, fake, egressIPMinInterval, now)

	waitFor(t, "the initial echo", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.1"
	})
	if fake.count() != 1 {
		t.Fatalf("echo calls after initial echo = %d, want 1", fake.count())
	}

	// Step over the debounce window, then mark the observation stale (the
	// startup refresh hook main.go wires once at boot).
	advance(egressIPMinInterval + time.Second)
	fake.set("203.0.113.2", nil)
	tr.Refresh()

	waitFor(t, "status.egress_ip to refresh after Refresh", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.2"
	})
	if fake.count() != 2 {
		t.Errorf("echo calls = %d, want 2 (initial + explicit refresh)", fake.count())
	}
}

// --- TestEgressIPFailureKeepsLastValue -------------------------------------

// TestEgressIPFailureKeepsLastValue: a failed echo keeps the last observed
// IP and the interval acts as backoff — a status burst between attempts must
// not fan out retries, and the next attempt is allowed only once the clock
// steps over egressIPMinInterval.
func TestEgressIPFailureKeepsLastValue(t *testing.T) {
	fake := newFakeEchoer("203.0.113.1")
	now, advance := newFakeClock(time.Now())
	_, tr, h := newEchoControl(t, fake, egressIPMinInterval, now)

	waitFor(t, "the initial echo", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.1"
	})

	// Refresh marks the value stale; the follow-up echo FAILS.
	advance(egressIPMinInterval + time.Second)
	fake.set("", errors.New("echo endpoint down"))
	tr.Refresh()
	waitFor(t, "the failed post-refresh attempt", func() bool {
		return fake.count() >= 2
	})

	// The last value must survive the failure...
	_, st := getStatus(t, h)
	if st.EgressIP != "203.0.113.1" {
		t.Errorf("status.egress_ip = %q after a failed echo, want the last observed value 203.0.113.1", st.EgressIP)
	}
	// ...and the interval must suppress retries during a status burst.
	for i := 0; i < 50; i++ {
		getStatus(t, h)
	}
	if got := fake.count(); got != 2 {
		t.Errorf("echo calls after 50 reads inside the debounce window = %d, want 2 — failures must not fan out", got)
	}

	// Once the window elapses, the stale value is retried (and again keeps
	// the last good value on failure).
	advance(egressIPMinInterval + time.Second)
	getStatus(t, h)
	waitFor(t, "the retry after the debounce window", func() bool {
		return fake.count() >= 3
	})
	_, st = getStatus(t, h)
	if st.EgressIP != "203.0.113.1" {
		t.Errorf("status.egress_ip = %q after a failed retry, want the last observed value 203.0.113.1", st.EgressIP)
	}
}

// --- TestHTTPEchoerProduction -------------------------------------------------

// TestHTTPEchoerProduction: the production echoer performs a plain GET of the
// IP-echo endpoint THROUGH the injected active transport (loopback httptest
// server here — no live network) and returns the trimmed IP. Non-2xx
// responses and non-IP bodies are errors, never values.
func TestHTTPEchoerProduction(t *testing.T) {
	t.Run("returns the echoed IP through the active transport", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("198.51.100.7\n"))
		}))
		t.Cleanup(srv.Close)

		var used atomic.Int32
		e := &httpEchoer{url: srv.URL, transport: func() http.RoundTripper {
			return echoRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				used.Add(1)
				return http.DefaultTransport.RoundTrip(req)
			})
		}}
		ip, err := e.EgressIP(context.Background())
		if err != nil {
			t.Fatalf("EgressIP(): %v", err)
		}
		if ip != "198.51.100.7" {
			t.Errorf("EgressIP() = %q, want 198.51.100.7 (trimmed)", ip)
		}
		if used.Load() == 0 {
			t.Error("the injected active transport was never used — the echo must go through router.Egress's transport")
		}
	})

	t.Run("non-2xx response is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusBadGateway)
		}))
		t.Cleanup(srv.Close)

		e := &httpEchoer{url: srv.URL, transport: func() http.RoundTripper { return http.DefaultTransport }}
		if _, err := e.EgressIP(context.Background()); err == nil {
			t.Fatal("EgressIP() on HTTP 502: want an error, got nil")
		}
	})

	t.Run("non-IP body is an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>not an ip</html>"))
		}))
		t.Cleanup(srv.Close)

		e := &httpEchoer{url: srv.URL, transport: func() http.RoundTripper { return http.DefaultTransport }}
		if _, err := e.EgressIP(context.Background()); err == nil {
			t.Fatal("EgressIP() on a non-IP body: want an error, got nil")
		}
	})
}

// --- Fix round 1 ------------------------------------------------------------
//
// Findings F1–F5 of review round 1: staleness generation, refresh-hook
// coverage, interval-gate isolation, value trimming. All hermetic.

// TestEgressIPRefreshDuringInFlight (F1): a Refresh landing WHILE an echo
// attempt is in flight must force a follow-up attempt — the in-flight result
// carries the pre-change IP, so committing it may not clear staleness
// (staleness generation counter: maybeStart snapshots gen, run clears the
// stale flag only when gen is unchanged). Without the generation the test
// times out waiting for the post-refresh value.
func TestEgressIPRefreshDuringInFlight(t *testing.T) {
	fake := &fakeEchoer{
		ip:      "203.0.113.1",
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	now, advance := newFakeClock(time.Now())
	_, tr, h := newEchoControl(t, fake, egressIPMinInterval, now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_zenctl/status", nil))
	}()
	<-fake.started // attempt 1 in flight, blocked, will succeed with the PRE-refresh IP

	// Refresh lands mid-attempt: marks stale + bumps the generation, but
	// cannot start a second echo (single-in-flight gate).
	tr.Refresh()

	close(fake.release)
	<-done
	waitFor(t, "the pre-refresh attempt to land", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.1"
	})

	// The in-flight attempt must NOT have cleared staleness (its gen is
	// older than the refresh's) → once the window elapses a SECOND attempt
	// fires with the post-refresh IP.
	fake.set("203.0.113.2", nil)
	advance(egressIPMinInterval + time.Second)
	getStatus(t, h)
	waitFor(t, "a follow-up attempt after the refresh", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.2"
	})
	if got := fake.count(); got < 2 {
		t.Errorf("echo calls = %d, want >= 2 (the refresh during flight forced a second attempt)", got)
	}
}

// TestEgressIPBurstIntervalGateWithFailingEcho (F4): unlike the success
// burst test (where obsStale=false already bounds the reads), every attempt
// here FAILS so obsStale stays true throughout — the 100-read burst is
// bounded ONLY by the lastTry interval gate. Expect exactly 2 calls at the
// burst, and a third once the window elapses (proving the gate, not
// staleness, was the bound).
func TestEgressIPBurstIntervalGateWithFailingEcho(t *testing.T) {
	fake := &fakeEchoer{err: errors.New("echo endpoint down")}
	now, advance := newFakeClock(time.Now())
	_, _, h := newEchoControl(t, fake, egressIPMinInterval, now)

	waitFor(t, "the first (failing) attempt", func() bool {
		getStatus(t, h)
		return fake.count() >= 1
	})
	advance(egressIPMinInterval + time.Second)
	getStatus(t, h)
	waitFor(t, "the second (failing) attempt after the window", func() bool {
		getStatus(t, h)
		return fake.count() >= 2
	})

	// obsStale is still true (both attempts failed) — only the interval
	// gate may bound this burst.
	for i := 0; i < 100; i++ {
		getStatus(t, h)
	}
	if got := fake.count(); got != 2 {
		t.Errorf("echo calls after a 100-read burst with a failing echoer = %d, want 2 — the interval gate must be load-bearing", got)
	}

	advance(egressIPMinInterval + time.Second)
	getStatus(t, h)
	waitFor(t, "a third attempt once the window elapses", func() bool {
		getStatus(t, h)
		return fake.count() >= 3
	})
}

// TestEgressIPTrimsCommittedValue (F5): run() validates TrimSpace(ip) but
// must commit the TRIMMED value — status.egress_ip is a bare IP.
func TestEgressIPTrimsCommittedValue(t *testing.T) {
	fake := newFakeEchoer("  203.0.113.55 \n")
	_, _, h := newEchoControl(t, fake, egressIPMinInterval, time.Now)

	waitFor(t, "status.egress_ip to show the trimmed IP", func() bool {
		_, st := getStatus(t, h)
		return st.EgressIP == "203.0.113.55"
	})
}
