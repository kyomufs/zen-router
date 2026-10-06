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
	// cooldownSwapped records that the first identity switch inside the
	// current rotation-cooldown window was already persisted, so further
	// reports in the window return false without churning the tunnel.
	cooldownSwapped bool
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
		r.store.RecordSuccess(string(res.Egress))
	}
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
		// Fall back to direct so requests keep flowing.
		r.setEgress(proxy.EgressDirect)
		return proxy.EgressDirect, err
	}
	return proxy.EgressWarp, nil
}

// setEgress swaps the active path and persists it.
func (r *Router) setEgress(e proxy.Egress) {
	r.mu.Lock()
	r.egress = e
	if e == proxy.EgressWarp && r.warpRT != nil {
		// keep existing warpRT
	} else if e == proxy.EgressDirect {
		r.warpRT = nil
	}
	r.mu.Unlock()
	r.store.SetCurrent(string(e))
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
