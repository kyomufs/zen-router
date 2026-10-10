// Package warp manages the Cloudflare WARP egress lane: a local SOCKS5
// proxy (warp-cli in proxy mode on 127.0.0.1:40000) whose identity is
// rotated on demand via disconnect → registration new → connect. The
// mechanic is ported from alztrk/opencode-ip-rotator (rotator.py): bounded
// rotation attempts, a post-rotation settle sleep, and a "new IP differs
// from the old one" success check; like there, a 429 is never assumed to
// be an IP block without that corroboration (classification lives in the
// router stage machine — WARP is only offered to per-key RATE limits on
// the anonymous lane, never to account-quota limits).
//
// A nil *Manager is the disabled lane: every method is nil-safe and no-ops,
// so the rest of the daemon never branches on configuration.
package warp

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultSocks is warp-cli's local SOCKS5 endpoint (proxy mode default
	// port; the 2026 CLI takes no --port flag).
	DefaultSocks = "127.0.0.1:40000"
	// DefaultCLI is the warp-cli binary looked up on PATH.
	DefaultCLI = "warp-cli"

	// cliTimeout bounds ONE warp-cli invocation (parity with the
	// reference repo's timeout=10).
	cliTimeout = 10 * time.Second
	// rotateAttempts is the identity-rotation retry budget
	// (WARP_ROTATION_ATTEMPTS=4 upstream).
	rotateAttempts = 4
	// postRotationSleep is the settle delay after connect before the new
	// IP is probed (WARP_POST_ROTATION_SLEEP=3 upstream).
	postRotationSleep = 3 * time.Second
	// flowWait bounds how long Rotate waits for in-flight warp flows to
	// drain before rotating anyway (the reference repo's flow lock; we
	// degrade to proceeding rather than deadlocking a stuck stream).
	flowWait = 10 * time.Second
	// rotCooldown suppresses re-rotation inside a hot 429 storm: the lane
	// identity was already swapped within this window, so the failing
	// requests share the fresh IP instead of each spinning warp-cli (anti-
	// churn: the failure mode that got the old WARP ping-pong disabled).
	rotCooldown = 60 * time.Second
	// ipEchoURL is the public-IP probe used to corroborate a rotation.
	ipEchoURL = "https://api.ipify.org"
	// statusCacheTTL debounces warp-cli status spawns across control-API
	// polls (the TUI polls every second; warp-cli is a D-Bus round-trip).
	statusCacheTTL = 5 * time.Second
)

// Options configures a Manager.
type Options struct {
	// Socks is the local SOCKS5 endpoint ("" = DefaultSocks).
	Socks string
	// CLI is the warp-cli binary ("" = DefaultCLI).
	CLI string
	// Logger defaults to log.Default().
	Logger *log.Logger
	// OnRotated, when set, is called after every successful identity
	// rotation with the new egress IP — the wiring point for persistent
	// rotation stats (SQLite), kept out of this package on purpose.
	OnRotated func(ip string)
}

// Manager is the WARP lane. Zero value unusable — use New (which returns
// nil when disabled).
type Manager struct {
	socks string
	cli   string
	log   *log.Logger

	onRotated func(ip string)

	// mu serializes rotations: concurrent 429s must not interleave
	// disconnect/registration/connect sequences.
	mu sync.Mutex
	// lastRot is the time of the last completed rotation (cooldown gate),
	// guarded by mu.
	lastRot time.Time
	// inflight counts RoundTrips currently flowing through the lane;
	// Rotate waits for it to drain (flow lock).
	inflight atomic.Int64

	rotations atomic.Int64
	lastErr   atomic.Pointer[string]
	lastIP    atomic.Pointer[string]

	// status cache (connected flag + expiry), guarded by stMu.
	stMu        sync.Mutex
	stConnected bool
	stAt        time.Time

	// transport is the memoized lane RoundTripper (see Transport).
	transportOnce sync.Once
	transport     http.RoundTripper
}

// New builds a Manager. Never nil — disabled lanes are represented by a
// nil *Manager, and every method below is nil-safe.
func New(opts Options) *Manager {
	if opts.Socks == "" {
		opts.Socks = DefaultSocks
	}
	if opts.CLI == "" {
		opts.CLI = DefaultCLI
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	return &Manager{
		socks:     opts.Socks,
		cli:       opts.CLI,
		log:       opts.Logger,
		onRotated: opts.OnRotated,
	}
}

// Socks exposes the configured SOCKS5 endpoint (status payload).
func (m *Manager) Socks() string {
	if m == nil {
		return ""
	}
	return m.socks
}

// ---------------------------------------------------------------- rotating

// Rotate re-issues the WARP identity: disconnect → registration new →
// connect, retrying up to rotateAttempts while the public IP stays the
// same. In-flight flows are given flowWait to drain first (flow lock);
// after that the rotation proceeds and torn streams are the caller's
// normal transport-failure path. Serialized by mu — concurrent callers
// queue, they do not interleave. A rotation completed within rotCooldown
// is skipped (returns nil): a 429 storm shares the fresh identity instead
// of every caller spinning its own warp-cli cycle.
func (m *Manager) Rotate() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.lastRot.IsZero() && time.Since(m.lastRot) < rotCooldown {
		m.log.Printf("warp: rotation skipped (cooldown, last %s ago)", time.Since(m.lastRot).Round(time.Second))
		return nil
	}
	m.lastRot = time.Now()

	old := m.probeIP()
	m.waitDrain()

	var lastErr error
	for attempt := 1; attempt <= rotateAttempts; attempt++ {
		if err := m.cycle(); err != nil {
			lastErr = err
			m.log.Printf("warp: rotation attempt %d/%d failed: %v", attempt, rotateAttempts, err)
			time.Sleep(time.Duration(attempt) * postRotationSleep)
			continue
		}
		time.Sleep(postRotationSleep)
		if ip := m.probeIP(); ip != "" && ip != old {
			m.rotations.Add(1)
			m.lastErr.Store(nil)
			m.lastIP.Store(&ip)
			m.log.Printf("warp: rotated (ip %s → %s, rotation #%d)", old, ip, m.rotations.Load())
			if m.onRotated != nil {
				m.onRotated(ip)
			}
			return nil
		}
		// Same IP (or probe failed): the identity did not visibly move —
		// retry the whole cycle like the reference rotator does.
		lastErr = fmt.Errorf("ip unchanged after rotation (still %q)", old)
		m.log.Printf("warp: rotation attempt %d/%d: %v", attempt, rotateAttempts, lastErr)
	}
	m.setErr(lastErr)
	return lastErr
}

// IP returns the last observed lane egress IP ("" before the first
// successful rotation/probe). Nil-safe.
func (m *Manager) IP() string {
	if m == nil {
		return ""
	}
	if p := m.lastIP.Load(); p != nil {
		return *p
	}
	return ""
}

// cycle runs ONE disconnect → registration new → connect sequence.
func (m *Manager) cycle() error {
	for _, args := range [][]string{
		{"disconnect"},
		{"registration", "new"},
		{"connect"},
	} {
		if out, err := m.run(args...); err != nil {
			return fmt.Errorf("warp-cli %s: %w (output: %s)",
				strings.Join(args, " "), err, strings.TrimSpace(out))
		}
	}
	return nil
}

// waitDrain blocks up to flowWait for in-flight flows to finish, then
// proceeds regardless: a wedged stream must not wedge rotation forever.
func (m *Manager) waitDrain() {
	deadline := time.Now().Add(flowWait)
	for m.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := m.inflight.Load(); n > 0 {
		m.log.Printf("warp: rotating with %d flow(s) still in flight", n)
	}
}

// probeIP best-effort reads the lane's current public IP; "" = unknown
// (probe failure is not a rotation error by itself — the next probe
// comparison decides).
func (m *Manager) probeIP() string {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipEchoURL, nil)
	if err != nil {
		return ""
	}
	client := &http.Client{Transport: m.Transport(), Timeout: cliTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// run executes one warp-cli invocation (TOS already accepted at install
// time; --accept-tos keeps unattended rotations from stalling on the
// prompt) and returns its combined output.
func (m *Manager) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliTimeout)
	defer cancel()
	full := append([]string{"--accept-tos"}, args...)
	out, err := exec.CommandContext(ctx, m.cli, full...).CombinedOutput()
	return string(out), err
}

// ----------------------------------------------------------------- status

// Snapshot is the lane's reportable state (control API /_zenctl/warp).
type Snapshot struct {
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Socks     string `json:"socks"`
	IP        string `json:"ip,omitempty"`
	Rotations int64  `json:"rotations"`
	Inflight  int64  `json:"inflight"`
	LastError string `json:"last_error,omitempty"`
}

// Snapshot reports the lane state; Connected is debounced by
// statusCacheTTL so a fast status poll does not spawn warp-cli per beat.
func (m *Manager) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{Enabled: false}
	}
	s := Snapshot{
		Enabled:   true,
		Socks:     m.socks,
		Rotations: m.rotations.Load(),
		Inflight:  m.inflight.Load(),
	}
	if ip := m.lastIP.Load(); ip != nil {
		s.IP = *ip
	}
	if e := m.lastErr.Load(); e != nil {
		s.LastError = *e
	}
	m.stMu.Lock()
	if time.Since(m.stAt) < statusCacheTTL {
		s.Connected = m.stConnected
		m.stMu.Unlock()
		return s
	}
	m.stMu.Unlock()

	connected := m.statusConnected()
	m.stMu.Lock()
	m.stConnected, m.stAt = connected, time.Now()
	m.stMu.Unlock()
	s.Connected = connected
	return s
}

// statusConnected parses `warp-cli status` ("Status update: Connected").
func (m *Manager) statusConnected() bool {
	out, err := m.run("status")
	if err != nil {
		return false
	}
	line := strings.ToLower(out)
	return strings.Contains(line, "connected") && !strings.Contains(line, "disconnected")
}

func (m *Manager) setErr(err error) {
	if m == nil || err == nil {
		return
	}
	msg := err.Error()
	m.lastErr.Store(&msg)
}

// ----------------------------------------------------------------- flows

// BeginFlow registers one in-flight request on the lane (flow lock).
func (m *Manager) BeginFlow() {
	if m != nil {
		m.inflight.Add(1)
	}
}

// EndFlow unregisters one in-flight request.
func (m *Manager) EndFlow() {
	if m != nil {
		m.inflight.Add(-1)
	}
}
