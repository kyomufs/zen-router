package cli

import (
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
	// State is the quota snapshot with identity credentials stripped by
	// Redacted() — state.json on disk keeps the full fidelity.
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
	// Latency is the per-egress 2xx response latency view (last/avg/count).
	Latency map[string]router.EgressLatency `json:"latency"`
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
		Latency:        c.Router.Latency(),
	})
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
