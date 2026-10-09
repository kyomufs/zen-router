package router

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"zen-router/internal/keys"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/store"
)

// Router owns the egress decision. The gateway is DIRECT-ONLY: the single
// rotation mechanism is the API key pool (stage-1 key rotation decided per
// request by NextAttempt). The router itself never switches transports.
type Router struct {
	log   *log.Logger
	store *quota.Manager

	// history is the SQLite request-event store feeding the TUI stats
	// views (per-day / per-IP OK+429). Optional: nil disables history
	// recording without touching the quota counters; recording failures
	// are logged, never surfaced to the request path.
	history *store.Store

	mu       sync.Mutex
	directRT http.RoundTripper

	pool *keys.Pool
	// latency accumulates observed 2xx response latency per (kind, egress)
	// (dashboard data, plan Task 1) — volatile, deliberately NOT persisted.
	// The two kinds measure DIFFERENT windows (review F1) and must never
	// average together.
	latency map[latencyKey]*latencyEntry
}

// Options configures a Router.
type Options struct {
	Store  *quota.Manager
	Logger *log.Logger
	// Pool is the API key pool behind stage-1 key rotation. Nil means the
	// single-key fallback (keys.New("")).
	Pool *keys.Pool
	// Family pins the direct transport's dialing to "auto", "v4" or "v6".
	Family string
	// History is the SQLite request-event store (nil = no history).
	History *store.Store
}

// New builds a Router on the direct lane.
func New(opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Pool == nil {
		opts.Pool = keys.New("")
	}
	if opts.Store == nil {
		st, err := quota.Open("")
		if err != nil {
			return nil, err
		}
		opts.Store = st
	}
	return &Router{
		log:      opts.Logger,
		store:    opts.Store,
		history:  opts.History,
		directRT: directTransportFor(opts.Family),
		pool:     opts.Pool,
		latency:  seedLatency(),
	}, nil
}

// History exposes the SQLite request-event store for the control API
// (GET /_zenctl/stats). May be nil when history recording is disabled.
func (r *Router) History() *store.Store { return r.history }

// recordHistory appends one outcome event to the SQLite history store.
// Nil-safe and failure-tolerant: history is dashboard data and must never
// delay or fail a request.
func (r *Router) recordHistory(kind, key string) {
	if r.history == nil {
		return
	}
	if err := r.history.Record(kind, key); err != nil {
		r.log.Printf("warn: history record: %v", err)
	}
}

// directTransportFor builds the direct transport, pinning its dialer to the
// configured address family ("v4"/"v6") so direct attempts honor
// config.Family; "auto" (or anything else) leaves the dialer untouched.
func directTransportFor(family string) http.RoundTripper {
	rt := proxy.DirectTransport()
	t, ok := rt.(*http.Transport)
	if !ok {
		return rt
	}
	var netw string
	switch family {
	case "v4":
		netw = "tcp4"
	case "v6":
		netw = "tcp6"
	default:
		return rt
	}
	base := t.DialContext
	if base == nil {
		return rt
	}
	t.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return base(ctx, netw, addr)
	}
	return t
}

// Egress returns the current path name and its transport. Direct-only: the
// value is constant, kept as the seam the gateway calls per request.
func (r *Router) Egress() (proxy.Egress, http.RoundTripper) {
	return proxy.EgressDirect, r.directRT
}

// Current returns the active egress name (always direct).
func (r *Router) Current() proxy.Egress { return proxy.EgressDirect }

// Store exposes the state manager for read-only commands.
func (r *Router) Store() *quota.Manager { return r.store }

// OnResult is the proxy.ResultHook: it records outcomes. A daily-limit
// observation stamps the spent window; it does NOT trigger any rotation —
// quota-aware key selection happens per request in NextAttempt.
func (r *Router) OnResult(res proxy.Result) {
	switch {
	case res.DailyLimit:
		r.store.RecordDaily429(string(res.Egress), quota.NextReset(time.Now()))
		r.recordHistory(store.Kind429, "")
		r.log.Printf("[%s] daily quota exhausted (status %d, retry-after %q)",
			res.Egress, res.Status, res.RetryAfter)
	case res.Status >= 200 && res.Status < 300:
		// Counter bookkeeping shared with the gateway path — one lock, one
		// state.json save (review F4). The legacy proxy path passes "" for
		// the key, hence "". The latency observation goes into the STREAM
		// bucket: Result.LatencyMS spans RoundTrip → body close (the full
		// stream duration), a different window than the gateway's TTFB
		// (review F1).
		r.store.RecordRequestSuccess(string(res.Egress), "")
		r.recordLatency(string(res.Egress), LatencyStream, res.LatencyMS)
		r.recordHistory(store.KindOK, "")
	}
}

// --- dashboard data (plan Task 1, spec §7) ---------------------------------

// LatencyKind selects which measurement window an observation belongs to.
// The two kinds are NEVER averaged together (review Task 1, finding F1).
type LatencyKind string

const (
	// LatencyTTFB is the gateway path window: request start (around Do) →
	// 2xx response headers. The dashboard-meaningful number (Task 5).
	LatencyTTFB LatencyKind = "ttfb"
	// LatencyStream is the legacy reverse-proxy window: RoundTrip → body
	// close — the full stream duration of an OpenAI-style request.
	LatencyStream LatencyKind = "stream"
)

// EgressLatency is the status-payload view of one egress's observed 2xx
// response latency: the last sample, the running average and how many
// samples back them up (count 0 = no request observed yet).
type EgressLatency struct {
	LastMS int64 `json:"last_ms"`
	AvgMS  int64 `json:"avg_ms"`
	Count  int64 `json:"count"`
}

// latencyKey addresses one accumulator: measurement window × egress.
type latencyKey struct {
	kind   LatencyKind
	egress string
}

// latencyEntry is the mutable accumulator behind EgressLatency, guarded by
// Router.mu.
type latencyEntry struct {
	last  int64
	sum   int64
	count int64
}

// seedLatency pre-creates the direct (kind) slots so the status payload
// always presents both kinds, zeroed until observed.
func seedLatency() map[latencyKey]*latencyEntry {
	m := make(map[latencyKey]*latencyEntry, 2)
	for _, kind := range []LatencyKind{LatencyTTFB, LatencyStream} {
		m[latencyKey{kind: kind, egress: string(proxy.EgressDirect)}] = &latencyEntry{}
	}
	return m
}

// RecordSuccess implements the optional gateway.Recorder seam: a 2xx attempt
// on the OpenAI surface feeds BOTH success counters — per egress and per API
// key — in ONE quota lock/save (quota.RecordRequestSuccess, review F4), plus
// the TTFB latency bucket (gateway window: Do → response headers, review F1).
// quota.RecordKeySuccess had zero production callers before plan Task 1.
func (r *Router) RecordSuccess(egress proxy.Egress, key string, latencyMS int64) {
	r.store.RecordRequestSuccess(string(egress), key)
	r.recordLatency(string(egress), LatencyTTFB, latencyMS)
	r.recordHistory(store.KindOK, key)
}

// recordLatency folds one 2xx observation into its (kind, egress)
// accumulator.
func (r *Router) recordLatency(egress string, kind LatencyKind, latencyMS int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := latencyKey{kind: kind, egress: egress}
	e := r.latency[k]
	if e == nil {
		e = &latencyEntry{}
		r.latency[k] = e
	}
	e.last = latencyMS
	e.sum += latencyMS
	e.count++
}

// Latency returns a snapshot of one kind's per-egress latency view for the
// status payload. The direct slot is always present (zeroed until
// observed); the kinds live in separate maps and never mix (review F1).
func (r *Router) Latency(kind LatencyKind) map[string]EgressLatency {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]EgressLatency{string(proxy.EgressDirect): {}}
	for k, e := range r.latency {
		if k.kind != kind {
			continue
		}
		avg := int64(0)
		if e.count > 0 {
			avg = e.sum / e.count
		}
		out[k.egress] = EgressLatency{LastMS: e.last, AvgMS: avg, Count: e.count}
	}
	return out
}
