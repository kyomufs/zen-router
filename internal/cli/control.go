package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"zen-router/internal/quota"
	"zen-router/internal/router"
	"zen-router/internal/store"
)

// ControlPrefix reserves a path namespace on the proxy listener for the CLI.
// The plugin only ever targets /zen/..., so this never collides with upstream
// traffic, and it keeps control on the same localhost-only port.
const ControlPrefix = "/_zenctl/"

// StatsWindowDays is the aggregation window of GET /_zenctl/stats: how many
// trailing calendar days (including today) the day and IP rollups cover.
const StatsWindowDays = 30

// Stats is the JSON payload served by GET /_zenctl/stats: OK/429 rollups
// per local calendar day and per observed egress IP, computed by the daemon
// from the SQLite request-history store (the TUI stays a plain HTTP client).
type Stats struct {
	Days []store.DayStat `json:"days"`
	IPs  []store.IPStat  `json:"ips"`
}

// Status is the JSON payload served by GET /_zenctl/status: the dashboard
// header fields plus the redacted quota state (plan Task 1, spec §7).
type Status struct {
	Up bool `json:"up"`
	// State is the quota snapshot with raw API keys stripped by the
	// control layer — state.json on disk keeps the full fidelity.
	State quota.State `json:"state"`

	// Listen is the resolved listen address ("host:port").
	Listen string `json:"listen"`
	// Pid is the pid of the daemon process serving this endpoint — `up
	// --detach` binds its readiness poll on it, so a pre-existing daemon
	// on the same address can never turn the poll green (fix F1; additive
	// JSON field, Task 4/5 TUI renders it).
	Pid int `json:"pid"`
	// UptimeSeconds is the daemon uptime (whole seconds, clamped at 0).
	UptimeSeconds int64 `json:"uptime_seconds"`
	// LatencyTTFB is the per-egress GATEWAY window (request start → 2xx
	// response headers) — the dashboard number Task 5 renders.
	LatencyTTFB map[string]router.EgressLatency `json:"latency_ttfb_ms"`
	// LatencyStream is the per-egress REVERSE-PROXY window (RoundTrip →
	// body close = full stream duration). Never averaged with TTFB
	// (review F1).
	LatencyStream map[string]router.EgressLatency `json:"latency_stream_ms"`
	// EgressIP is the last observed public IP of the active egress
	// ("" = unknown: echo disabled via config.EgressIPEcho, not yet
	// performed, or the last attempt failed). Refreshed on startup and
	// lazily on status reads, debounced by egressIPMinInterval — plan
	// Task 2 (plan-local extra, no spec section; provenance D3).
	EgressIP string `json:"egress_ip"`
}

// Control exposes CLI control endpoints over an existing router + proxy.
type Control struct {
	Router *router.Router
	// Shutdown triggers a graceful daemon stop (wired to context cancel).
	Shutdown func()

	// Listen is the resolved listen address (main.go from config), surfaced
	// to the dashboard as status.listen.
	Listen string
	// StartedAt is when the daemon started, surfaced as
	// status.uptime_seconds. Zero means unknown (reported as 0).
	StartedAt time.Time
	// IPTracker observes the egress public IP for status.egress_ip
	// (plan Task 2). Nil reports "" — no echo, no fan-out.
	IPTracker *EgressIPTracker
}

// Handler returns an http.Handler that routes ControlPrefix to the control
// API and everything else to the proxy.
func (c *Control) Handler(proxyHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, ControlPrefix) {
			c.serve(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})
}

func (c *Control) serve(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, ControlPrefix)
	switch {
	case route == "status" && r.Method == http.MethodGet:
		c.handleStatus(w)
	case route == "stats" && r.Method == http.MethodGet:
		c.handleStats(w)
	case route == "stop" && r.Method == http.MethodPost:
		c.handleStop(w)
	default:
		http.Error(w, `{"error":"unknown control route"}`, http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (c *Control) handleStatus(w http.ResponseWriter) {
	// Control-layer redaction ONLY: state.json on disk keeps credentials,
	// the loopback dashboard payload never sees them (plan Task 1).
	snap := c.Router.Store().Snapshot()
	snap.Keys = fingerprintKeys(snap.Keys)

	var uptime int64
	if !c.StartedAt.IsZero() {
		if u := int64(time.Since(c.StartedAt).Seconds()); u > 0 {
			uptime = u
		}
	}

	writeJSON(w, http.StatusOK, Status{
		Up:            true,
		State:         snap,
		Listen:        c.Listen,
		Pid:           os.Getpid(), // this very process serves the endpoint
		UptimeSeconds: uptime,
		LatencyTTFB:   c.Router.Latency(router.LatencyTTFB),
		LatencyStream: c.Router.Latency(router.LatencyStream),
		// IP() also triggers the debounced lazy refresh when the value is
		// stale; the echo itself runs asynchronously, so this read never
		// blocks and a status-poll burst cannot fan out echo requests.
		EgressIP: c.IPTracker.IP(),
	})
}

// handleStats serves the request-history rollups (GET /_zenctl/stats).
// The router owns the SQLite handle; a daemon started without history
// storage reports empty rollups rather than an error.
func (c *Control) handleStats(w http.ResponseWriter) {
	h := c.Router.History()
	days, err := h.ByDay(StatsWindowDays)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	ips, err := h.ByIP(StatsWindowDays)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if days == nil {
		days = []store.DayStat{}
	}
	if ips == nil {
		ips = []store.IPStat{}
	}
	writeJSON(w, http.StatusOK, Stats{Days: days, IPs: ips})
}

// fingerprintKeys replaces every raw API key in state.keys with a display
// fingerprint (sha256[:8] hex); the per-key COUNTERS stay intact and so does
// state.json on disk (review Task 1, finding F5). Control layer only — the
// payload must never carry a full raw key value.
func fingerprintKeys(in map[string]*quota.KeyStats) map[string]*quota.KeyStats {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]*quota.KeyStats, len(in))
	for k, v := range in {
		out[fingerprintKey(k)] = v
	}
	return out
}

// fingerprintKey derives the stable display name for one raw API key.
// 8 hex chars of sha256: enough to tell keys apart in the TUI without
// disclosing any part of the secret.
func fingerprintKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:4])
}

func (c *Control) handleStop(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
	if c.Shutdown != nil {
		c.Shutdown()
	}
}

// ErrNotRunning is returned by client commands when the daemon is unreachable.
var ErrNotRunning = fmt.Errorf("zen-router daemon is not running (start it with `zen-router up`)")
