package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/router"
)

// ControlPrefix reserves a path namespace on the proxy listener for the CLI.
// The plugin only ever targets /zen/..., so this never collides with upstream
// traffic, and it keeps control on the same localhost-only port.
const ControlPrefix = "/_zenctl/"

// Status is the JSON payload served by GET /_zenctl/status: the dashboard
// header fields plus the redacted quota state (plan Task 1, spec §7).
type Status struct {
	Mode    string `json:"mode"`
	Current string `json:"current"`
	Up      bool   `json:"up"`
	// State is the quota snapshot with identity credentials and raw API
	// keys stripped by the control layer — state.json on disk keeps the
	// full fidelity.
	State quota.State `json:"state"`

	// Listen is the resolved listen address ("host:port").
	Listen string `json:"listen"`
	// UptimeSeconds is the daemon uptime (whole seconds, clamped at 0).
	UptimeSeconds int64 `json:"uptime_seconds"`
	// LastRotate is the RFC3339 stamp of the last rotation completed by
	// THIS process ("" = none this run).
	LastRotate string `json:"last_rotate"`
	// Rotating reports an in-flight rotation.
	Rotating bool `json:"rotating"`
	// Registering reports an in-flight background spare registration.
	Registering bool `json:"registering"`
	// LastSpareError is the most recent spare-registration failure
	// ("" = none since the last success).
	LastSpareError string `json:"lastSpareError"`
	// LatencyTTFB is the per-egress GATEWAY window (request start → 2xx
	// response headers) — the dashboard number Task 5 renders.
	LatencyTTFB map[string]router.EgressLatency `json:"latency_ttfb_ms"`
	// LatencyStream is the per-egress REVERSE-PROXY window (RoundTrip →
	// body close = full stream duration). Never averaged with TTFB
	// (review F1).
	LatencyStream map[string]router.EgressLatency `json:"latency_stream_ms"`
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
	case route == "rotate" && r.Method == http.MethodPost:
		c.handleRotate(w)
	case route == "use" && r.Method == http.MethodPost:
		c.handleUse(w, r)
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
	for i, id := range snap.Identities {
		if id != nil {
			redacted := id.Redacted()
			snap.Identities[i] = &redacted
		}
	}
	if snap.Warp != nil {
		redacted := snap.Warp.Redacted()
		snap.Warp = &redacted
	}
	snap.Keys = fingerprintKeys(snap.Keys)

	var uptime int64
	if !c.StartedAt.IsZero() {
		if u := int64(time.Since(c.StartedAt).Seconds()); u > 0 {
			uptime = u
		}
	}
	lastRotate := ""
	if t := c.Router.LastRotate(); !t.IsZero() {
		lastRotate = t.Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, Status{
		Mode:           c.Router.Store().Mode(),
		Current:        string(c.Router.Current()),
		Up:             true,
		State:          snap,
		Listen:         c.Listen,
		UptimeSeconds:  uptime,
		LastRotate:     lastRotate,
		Rotating:       c.Router.Rotating(),
		Registering:    c.Router.Registering(),
		LastSpareError: c.Router.LastSpareError(),
		LatencyTTFB:    c.Router.Latency(router.LatencyTTFB),
		LatencyStream:  c.Router.Latency(router.LatencyStream),
	})
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

func (c *Control) handleRotate(w http.ResponseWriter) {
	to, err := c.Router.RotateNow("manual rotate via CLI")
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   err.Error(),
			"current": string(c.Router.Current()),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"rotated_to": string(to),
		"mode":       c.Router.Store().Mode(),
	})
}

func (c *Control) handleUse(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	var e proxy.Egress
	switch mode {
	case "direct":
		e = proxy.EgressDirect
	case "warp":
		e = proxy.EgressWarp
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "mode must be direct or warp",
		})
		return
	}
	if err := c.Router.Use(r.Context(), e); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"current": string(c.Router.Current()),
		"mode":    c.Router.Store().Mode(),
	})
}

func (c *Control) handleStop(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
	if c.Shutdown != nil {
		c.Shutdown()
	}
}

// ErrNotRunning is returned by client commands when the daemon is unreachable.
var ErrNotRunning = fmt.Errorf("zen-router daemon is not running (start it with `zen-router up`)")
