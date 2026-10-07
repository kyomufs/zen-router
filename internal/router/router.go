package router

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"zen-router/internal/keys"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/warp"
)

// Device is the WireGuard interface name used for WARP egress.
const Device = "zenwarp"

// Router owns the egress decision and performs IP rotation on quota exhaustion.
type Router struct {
	log    *log.Logger
	store  *quota.Manager
	client *warp.Client

	mu         sync.Mutex
	egress     proxy.Egress
	directRT   http.RoundTripper
	warpRT     http.RoundTripper
	tunnel     *warp.Tunnel
	rotating   bool
	lastRotate time.Time
	minRotate  time.Duration

	// rotationCooldown throttles how often we mint a new WARP identity, so a
	// burst of 429s does not spam the Cloudflare API.
	rotationCooldown time.Duration

	// Staged rotation state (stage.go): key pool, identity-pool sizing and
	// the test seams for the two privileged operations (WARP identity swap,
	// Cloudflare spare registration).
	pool           *keys.Pool
	poolSize       int
	poolSpare      int
	identitySwitch func(*quota.WarpIdentity) (http.RoundTripper, error)
	spareRegistrar func(context.Context) error
	registering    bool
	// lastSpareError is the most recent background spare-registration
	// failure ("" since the last success), surfaced by the control API.
	lastSpareError string
	// latency accumulates observed 2xx response latency per (kind, egress)
	// (dashboard data, plan Task 1) — volatile, deliberately NOT persisted.
	// The two kinds measure DIFFERENT windows (review F1) and must never
	// average together.
	latency map[latencyKey]*latencyEntry
	// cooldownSwapped records that the first identity switch inside the
	// current rotation-cooldown window was already persisted, so further
	// reports in the window return false without churning the tunnel.
	cooldownSwapped bool

	// OnRotated is an optional callback fired whenever the ACTIVE EGRESS
	// actually changes — rotation branches (direct→warp via setEgress, the
	// warp fresh-identity mint), the ensureWarp-failure direct fallback
	// (even though the rotation failed: no success-only history row, but the
	// transport changed), the stage-2 identity switch (setEgress, plus a
	// direct fire for warp→warp where the egress name is unchanged), and the
	// manual /_zenctl/use mode switch (setEgress). Nil (the default) fires
	// nothing. It is set once at wiring time (before the HTTP server starts)
	// and runs on the rotating/request goroutine outside router locks: it
	// must not block (plan Task 2: the egress-IP refresh trigger).
	OnRotated func()
}

// Options configures a Router.
type Options struct {
	Store  *quota.Manager
	Logger *log.Logger
	// Device overrides the WireGuard interface name (tests).
	Device string
	// Cooldown between automatic rotations.
	RotationCooldown time.Duration
	// Pool is the API key pool behind stage-1 key rotation. Nil means the
	// single-key fallback (keys.New("")).
	Pool *keys.Pool
	// Family pins the direct transport's dialing to "auto", "v4" or "v6"
	// (stage 3 "direct" attempts must respect the configured family).
	Family string
	// PoolSize and PoolSpare size the WARP identity pool target
	// (spec §6: 4 live + 1 spare); background spare registration stops at
	// PoolSize+PoolSpare identities.
	PoolSize  int
	PoolSpare int
	// IdentitySwitch overrides the synchronous WARP identity swap (tests):
	// it must reconfigure the transport for the given PRE-REGISTERED identity
	// without performing any registration. Nil uses the real tunnel swap.
	IdentitySwitch func(*quota.WarpIdentity) (http.RoundTripper, error)
	// SpareRegistrar overrides background spare registration (tests). Nil
	// registers a fresh spare against Cloudflare in the background.
	SpareRegistrar func(context.Context) error
}

// New builds a Router and restores its egress from persisted state.
func New(opts Options) (*Router, error) {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Device == "" {
		opts.Device = Device
	}
	if opts.RotationCooldown == 0 {
		opts.RotationCooldown = 30 * time.Second
	}
	if opts.Pool == nil {
		opts.Pool = keys.New("")
	}
	if opts.PoolSize <= 0 {
		opts.PoolSize = 4
	}
	if opts.PoolSpare <= 0 {
		opts.PoolSpare = 1
	}
	if opts.Store == nil {
		st, err := quota.Open("")
		if err != nil {
			return nil, err
		}
		opts.Store = st
	}
	r := &Router{
		log:              opts.Logger,
		store:            opts.Store,
		client:           warp.NewClient(),
		directRT:         directTransportFor(opts.Family),
		rotationCooldown: opts.RotationCooldown,
		pool:             opts.Pool,
		poolSize:         opts.PoolSize,
		poolSpare:        opts.PoolSpare,
		identitySwitch:   opts.IdentitySwitch,
		spareRegistrar:   opts.SpareRegistrar,
		egress:           proxy.EgressDirect,
		latency:          seedLatency(),
	}
	// Restore the active egress from state; default to direct.
	switch r.store.Current() {
	case "warp":
		r.egress = proxy.EgressWarp
	default:
		r.egress = proxy.EgressDirect
	}
	return r, nil
}

// directTransportFor builds the direct transport, pinning its dialer to the
// configured address family ("v4"/"v6") so stage-3 direct attempts honor
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

// Egress returns the current path name and its transport. It is called per
// request so rotation can swap the path live.
func (r *Router) Egress() (proxy.Egress, http.RoundTripper) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.egress == proxy.EgressWarp && r.warpRT != nil {
		return proxy.EgressWarp, r.warpRT
	}
	return proxy.EgressDirect, r.directRT
}

// Current returns the active egress name.
func (r *Router) Current() proxy.Egress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.egress
}

// Store exposes the state manager for read-only commands.
func (r *Router) Store() *quota.Manager { return r.store }

// OnResult is the proxy.ResultHook: it records outcomes and triggers rotation
// when the daily quota gate trips.
func (r *Router) OnResult(res proxy.Result) {
	switch {
	case res.DailyLimit:
		r.store.RecordDaily429(string(res.Egress), quota.NextReset(time.Now()))
		r.log.Printf("[%s] daily quota exhausted (status %d, retry-after %q) -> rotating",
			res.Egress, res.Status, res.RetryAfter)
		go r.rotate(fmt.Sprintf("daily limit on %s (HTTP %d)", res.Egress, res.Status))
	case res.Status >= 200 && res.Status < 300:
		// Counter bookkeeping shared with the gateway path — one lock, one
		// state.json save (review F4). The legacy proxy carries no API key,
		// hence "". The latency observation goes into the STREAM bucket:
		// Result.LatencyMS spans RoundTrip → body close (the full stream
		// duration), a different window than the gateway's TTFB (review F1).
		r.store.RecordRequestSuccess(string(res.Egress), "")
		r.recordLatency(string(res.Egress), LatencyStream, res.LatencyMS)
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

// seedLatency pre-creates every (kind, egress) slot so the status payload
// always presents both kinds for both egresses, zeroed until observed.
func seedLatency() map[latencyKey]*latencyEntry {
	m := make(map[latencyKey]*latencyEntry, 4)
	for _, kind := range []LatencyKind{LatencyTTFB, LatencyStream} {
		for _, eg := range []string{string(proxy.EgressDirect), string(proxy.EgressWarp)} {
			m[latencyKey{kind: kind, egress: eg}] = &latencyEntry{}
		}
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
// status payload. Both known egresses are always present (zeroed until
// observed); the kinds live in separate maps and never mix (review F1).
func (r *Router) Latency(kind LatencyKind) map[string]EgressLatency {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]EgressLatency, 2)
	for _, eg := range []string{string(proxy.EgressDirect), string(proxy.EgressWarp)} {
		out[eg] = EgressLatency{}
	}
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

// LastRotate reports when this process last completed a rotation (zero =
// none this run); surfaced as status.last_rotate.
func (r *Router) LastRotate() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastRotate
}

// Rotating reports whether a rotation is currently in flight.
func (r *Router) Rotating() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rotating
}

// Registering reports whether a background spare registration is in flight
// (spec §14: surfaced by the TUI while the pool refills).
func (r *Router) Registering() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.registering
}

// LastSpareError returns the most recent background spare-registration
// failure ("" = none since the last success), surfaced by the control API.
func (r *Router) LastSpareError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSpareError
}

// finishRotation is the shared body of the rotate()/RotateNow deferred
// critical sections: it releases the rotation guard, stamps a new cooldown
// window and CLEARS the in-window switch latch — every lastRotate stamp from
// the rotation paths goes through here, so a stale latch can never suppress
// the first identity switch of the next window. It never switches identities
// itself (also the test seam for these defers: tests must not run
// applyRotation, which registers a device against Cloudflare).
func (r *Router) finishRotation() {
	r.mu.Lock()
	r.rotating = false
	r.lastRotate = time.Now()
	r.cooldownSwapped = false
	r.mu.Unlock()
}

// fireRotated invokes the optional OnRotated callback after an EFFECTIVE
// change of the active egress — a value change through setEgress (rotation,
// its direct fallback, the manual Use switch) or a fresh-identity rotation
// whose egress name is unchanged. Nil-safe and non-blocking by contract of
// OnRotated — used as the egress-IP refresh trigger (Task 2). RecordRotation
// stays success-only: the history semantics are unchanged by this hook.
func (r *Router) fireRotated() {
	if r.OnRotated != nil {
		r.OnRotated()
	}
}

// rotate switches egress in response to a spent bucket. Direct -> warp the
// first time; warp -> fresh WARP identity (new IP) on subsequent hits.
func (r *Router) rotate(reason string) {
	r.mu.Lock()
	if r.rotating {
		r.mu.Unlock()
		return
	}
	if time.Since(r.lastRotate) < r.rotationCooldown {
		r.mu.Unlock()
		r.log.Printf("rotation suppressed (cooldown %s)", r.rotationCooldown)
		return
	}
	r.rotating = true
	from := r.egress
	r.mu.Unlock()

	defer r.finishRotation()

	to, err := r.applyRotation(from, reason)
	if err != nil {
		r.log.Printf("rotation failed (%s): %v", reason, err)
		return
	}
	// Observation hook already fired inside applyRotation (setEgress on the
	// direct branch, the fresh-identity fire on the warp branch).
	r.store.RecordRotation(string(from), string(to), reason)
	r.log.Printf("rotated %s -> %s (%s)", from, to, reason)
}

// RotateNow forces an immediate rotation regardless of cooldown, used by the
// CLI `rotate` command. It returns the new egress.
func (r *Router) RotateNow(reason string) (proxy.Egress, error) {
	r.mu.Lock()
	if r.rotating {
		r.mu.Unlock()
		return r.egress, fmt.Errorf("rotation already in progress")
	}
	r.rotating = true
	from := r.egress
	r.mu.Unlock()

	defer r.finishRotation()

	to, err := r.applyRotation(from, reason)
	if err != nil {
		r.log.Printf("manual rotation failed (%s): %v", reason, err)
		return from, err
	}
	// Observation hook already fired inside applyRotation (see rotate).
	r.store.RecordRotation(string(from), string(to), reason)
	r.log.Printf("rotated %s -> %s (%s)", from, to, reason)
	return to, nil
}

// applyRotation performs the actual switch and returns the new egress.
func (r *Router) applyRotation(from proxy.Egress, reason string) (proxy.Egress, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// First trip off direct: bring up WARP and route through it.
	if from == proxy.EgressDirect {
		if err := r.ensureWarp(ctx, true); err != nil {
			return proxy.EgressDirect, err
		}
		r.setEgress(proxy.EgressWarp)
		return proxy.EgressWarp, nil
	}

	// Already on warp and it is spent: mint a fresh WARP identity for a new IP.
	if err := r.ensureWarp(ctx, true); err != nil {
		// Fall back to direct so requests keep flowing. The active egress
		// changes here even though the rotation FAILED (no success-only
		// RecordRotation row) — the observed IP is stale either way, and
		// setEgress fires the hook.
		r.setEgress(proxy.EgressDirect)
		return proxy.EgressDirect, err
	}
	// Fresh identity registered: the egress NAME is unchanged (no setEgress
	// call, no success-only history row needed beyond the caller's), but the
	// public IP is new → fire the observation hook directly.
	r.fireRotated()
	return proxy.EgressWarp, nil
}

// setEgress swaps the active path and persists it — the SINGLE point where
// the active egress changes (rotation, its direct fallback, and the manual
// /_zenctl/use mode switch all route through here), so an egress change
// always fires the OnRotated observation hook exactly once (changed-value
// only: no fire when the mode is re-asserted unchanged).
func (r *Router) setEgress(e proxy.Egress) {
	r.mu.Lock()
	changed := r.egress != e
	r.egress = e
	if e == proxy.EgressWarp && r.warpRT != nil {
		// keep existing warpRT
	} else if e == proxy.EgressDirect {
		r.warpRT = nil
	}
	r.mu.Unlock()
	r.store.SetCurrent(string(e))
	if changed {
		r.fireRotated() // active transport changed → observed IP is stale
	}
}

// Use forces the active egress (direct or warp), bringing WARP up if needed.
func (r *Router) Use(ctx context.Context, e proxy.Egress) error {
	if e == proxy.EgressWarp {
		if err := r.ensureWarp(ctx, false); err != nil {
			return err
		}
	}
	r.setEgress(e)
	r.store.SetMode(string(e))
	return nil
}

// ensureWarp brings the WARP tunnel up and (re)builds its transport.
// forceNew registers a fresh WARP device, minting a new egress IP.
func (r *Router) ensureWarp(ctx context.Context, forceNew bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	identity := r.store.GetWarp()
	needRegister := forceNew || identity == nil

	var priv *warp.Key
	var prof *warp.WireGuardProfile

	if needRegister {
		kp, err := warp.NewKeyPair()
		if err != nil {
			return fmt.Errorf("generate warp key: %w", err)
		}
		dev, err := r.client.Register(ctx, kp.Public)
		if err != nil {
			return fmt.Errorf("register warp device: %w", err)
		}
		// Rebuild the client with this identity so config fetch is authenticated.
		r.client = warp.NewClient()
		r.client.Device = dev
		p, err := r.client.GetWireGuardProfile(ctx)
		if err != nil {
			return fmt.Errorf("fetch warp config: %w", err)
		}
		priv = kp
		prof = p
		r.store.SetWarp(quota.WarpIdentity{
			DeviceID:     dev.ID,
			Token:        dev.Token,
			License:      dev.Account.License,
			PrivateKey:   kp.Private,
			PublicKey:    kp.Public,
			AddressV4:    p.AddressV4,
			AddressV6:    p.AddressV6,
			Endpoint:     p.Endpoint,
			ServerPub:    p.ServerPub,
			RegisteredAt: time.Now().UnixMilli(),
		})
	} else {
		// Reuse the persisted identity; re-fetch config in case endpoints moved.
		r.client = warp.NewClient()
		r.client.Device = &warp.Device{ID: identity.DeviceID, Token: identity.Token}
		kp := &warp.Key{Private: identity.PrivateKey, Public: identity.PublicKey}
		p, err := r.client.GetWireGuardProfile(ctx)
		if err != nil {
			// Config fetch failed; force a fresh registration next time.
			r.store.ClearWarp()
			return fmt.Errorf("refetch warp config: %w", err)
		}
		priv = kp
		prof = p
		// Refresh persisted endpoint/addresses.
		id := *identity
		id.AddressV4 = p.AddressV4
		id.AddressV6 = p.AddressV6
		id.Endpoint = p.Endpoint
		id.ServerPub = p.ServerPub
		r.store.SetWarp(id)
	}

	t := warp.NewTunnel(Device, priv, prof)
	var err error
	if t.DeviceExists() {
		err = t.Reconfigure()
	} else {
		err = t.Up()
	}
	if err != nil {
		return fmt.Errorf("configure warp tunnel: %w", err)
	}
	r.tunnel = t

	resolver := warp.NewResolver(nil)
	rt, err := proxy.WarpTransport(Device, resolver)
	if err != nil {
		return fmt.Errorf("build warp transport: %w", err)
	}
	r.warpRT = rt
	return nil
}

// Tunnel returns the active tunnel, or nil when not on WARP.
func (r *Router) Tunnel() *warp.Tunnel {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunnel
}

// Stop tears the tunnel down (used on shutdown / mode switch to direct).
func (r *Router) Stop() {
	if t := r.Tunnel(); t != nil {
		_ = t.Down()
	}
}
