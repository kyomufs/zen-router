// Egress IP observation (plan Task 2).
//
// Provenance: egress-IP echo has NO spec section — it is a user-requested
// extra from this plan's own origin (D3); do not claim spec parity for it.
//
// The seam is injectable: IPEchoer is the interface tests fake, and the
// production httpEchoer only exists when config.EgressIPEcho == true (§12
// gate — default false; live calls happen only after explicit user opt-in,
// never in tests). EgressIPTracker applies the debounce so the TUI's 1s
// status polling can never fan out echo requests.
package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// egressIPMinInterval is the debounce: the minimum time between two
	// echo attempts, no matter which trigger fires (status read or
	// rotation) and no matter whether the previous attempt succeeded or
	// failed. 30s against a 1s TUI poll bounds the echo rate to ≤ 2
	// requests/minute with zero per-poll fan-out.
	egressIPMinInterval = 30 * time.Second
	// egressIPEchoTimeout bounds one echo attempt; the spawned refresh
	// never outlives it even if the endpoint hangs.
	egressIPEchoTimeout = 5 * time.Second
	// egressIPEchoURL is the plain-text IP-echo endpoint the production
	// echoer GETs through the ACTIVE transport.
	egressIPEchoURL = "https://api.ipify.org/"
	// egressIPEchoMaxBody caps how much of the echo response is read.
	egressIPEchoMaxBody = 512
)

// IPEchoer observes the public IP of the active egress path. Production
// implementation: httpEchoer (gated by config.EgressIPEcho). Tests inject a
// fake — no live network ever runs from the test suite.
type IPEchoer interface {
	EgressIP(ctx context.Context) (string, error)
}

// NewEgressIPEchoer builds the production echoer, gated by the §12 switch:
// enabled == false (the default — config.EgressIPEcho) returns nil, and a
// nil echoer makes every tracker refresh a silent no-op, so no live call can
// exist without the user's opt-in. transport must yield the ACTIVE egress
// transport (router.Egress) and is re-read on every call so observations
// follow rotation.
func NewEgressIPEchoer(enabled bool, transport func() http.RoundTripper) IPEchoer {
	if !enabled {
		return nil
	}
	return &httpEchoer{url: egressIPEchoURL, transport: transport}
}

// httpEchoer is the production IPEchoer: a plain GET of the IP-echo endpoint
// through the injected active transport, validated to a bare IP address.
type httpEchoer struct {
	url       string
	transport func() http.RoundTripper // nil → http.DefaultTransport
}

// EgressIP performs one echo attempt. Redirects are refused (the observation
// must come from the active egress itself), non-2xx responses and non-IP
// bodies are errors — a failed observation is never reported as a value.
func (e *httpEchoer) EgressIP(ctx context.Context) (string, error) {
	rt := http.RoundTripper(http.DefaultTransport)
	if e.transport != nil {
		if t := e.transport(); t != nil {
			rt = t
		}
	}
	client := &http.Client{
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 3xx stays as a non-2xx error
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.url, nil)
	if err != nil {
		return "", fmt.Errorf("egress IP echo request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("egress IP echo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("egress IP echo: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, egressIPEchoMaxBody))
	if err != nil {
		return "", fmt.Errorf("egress IP echo read: %w", err)
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("egress IP echo: unexpected response %q", ip)
	}
	return ip, nil
}

// EgressIPTracker holds the last observed egress IP and schedules debounced
// refreshes. It starts STALE (never observed) and becomes stale again after
// every successful rotation (Router.OnRotated → Refresh) or failed attempt —
// while a clean value only changes on rotation, because in this daemon the
// egress IP only ever changes via rotation.
//
// Debounce rule (both triggers funnel into maybeStart):
//   - at most ONE echo in flight (inFlight gate);
//   - at least egressIPMinInterval between attempt starts (lastTry gate),
//     which doubles as the retry backoff for failures;
//   - a failed echo keeps the last value (run only commits on success).
type EgressIPTracker struct {
	echoer   IPEchoer // nil = echo disabled (§12 gate) → permanent no-op
	interval time.Duration
	now      func() time.Time // clock seam for tests

	mu       sync.Mutex
	ip       string    // last SUCCESSFUL observation ("" = unknown)
	lastTry  time.Time // start of the last attempt (zero = never)
	inFlight bool      // at most one spawned attempt
	obsStale bool      // value needs a refresh (starts true; set by Refresh)
}

// NewEgressIPTracker builds a tracker with the production debounce
// (egressIPMinInterval) and the real clock. A nil echoer yields a tracker
// that always reports "" — the disabled configuration.
func NewEgressIPTracker(echoer IPEchoer) *EgressIPTracker {
	return newEgressIPTracker(echoer, egressIPMinInterval, time.Now)
}

// newEgressIPTracker is the test seam: custom interval and clock.
func newEgressIPTracker(echoer IPEchoer, interval time.Duration, now func() time.Time) *EgressIPTracker {
	return &EgressIPTracker{
		echoer:   echoer,
		interval: interval,
		now:      now,
		obsStale: true,
	}
}

// IP returns the last observed egress IP ("" = unknown / echo disabled) and
// lazily starts a refresh when the value is stale and the debounce allows
// one (the status-read trigger; non-blocking — the echo runs in a goroutine).
func (t *EgressIPTracker) IP() string {
	if t == nil {
		return ""
	}
	t.maybeStart()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ip
}

// Refresh is the rotation trigger (Router.OnRotated): it marks the value
// stale and starts an echo immediately when the debounce window allows —
// otherwise the next status read picks it up once the window elapses.
func (t *EgressIPTracker) Refresh() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.obsStale = true
	t.mu.Unlock()
	t.maybeStart()
}

// maybeStart spawns an attempt iff a refresh is needed and the debounce
// (single in-flight + minimum interval) allows it. Safe for concurrent
// status readers and rotation callbacks.
func (t *EgressIPTracker) maybeStart() {
	if t == nil || t.echoer == nil {
		return
	}
	t.mu.Lock()
	due := t.obsStale && !t.inFlight &&
		(t.lastTry.IsZero() || t.now().Sub(t.lastTry) >= t.interval)
	if !due {
		t.mu.Unlock()
		return
	}
	t.inFlight = true
	t.lastTry = t.now()
	t.mu.Unlock()
	go t.run()
}

// run performs one attempt: only a successful, non-empty observation commits
// the new value; failures keep the previous one (and the still-stale flag
// schedules the next attempt after the debounce window).
func (t *EgressIPTracker) run() {
	defer func() {
		t.mu.Lock()
		t.inFlight = false
		t.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), egressIPEchoTimeout)
	defer cancel()
	ip, err := t.echoer.EgressIP(ctx)
	if err != nil || strings.TrimSpace(ip) == "" {
		return // keep the last value; stale stays set for a later retry
	}
	t.mu.Lock()
	t.ip = ip
	t.obsStale = false
	t.mu.Unlock()
}
