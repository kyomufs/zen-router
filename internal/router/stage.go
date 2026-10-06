// Staged rotation state machine (spec §6): a 429 moves one stage per
// attempt — (0) first attempt, (1) next API key on the same egress,
// (2) fresh WARP identity from the pre-registered pool, (3) direct — and
// false means the request is exhausted (the caller surfaces 429).
//
// Budget (D1): max 3 upstream attempts per request, counted by the CALLER
// (stage skips mean Attempt.Step is a stage id, not an attempt ordinal —
// e.g. a single-key pool runs stages 0→2→3 in three attempts). NextAttempt
// returns false once the report's Step reaches 3.
package router

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/warp"
	"zen-router/internal/zen"
)

// Gateway error.type values that identify a workspace-scoped account limit.
// These appear inside zen.KindDailyLimit, so Kind alone cannot tell them
// apart from the IP/key-lane FreeUsageLimitError.
const (
	typeGoUsageLimit    = "GoUsageLimitError"
	typeBlackUsageLimit = "BlackUsageLimitError"
	typeFreeUsageLimit  = "FreeUsageLimitError"
)

// Report is one classified upstream failure, handed back to NextAttempt by
// the caller after an attempt failed. Egress/Key/Step describe the attempt
// that failed; RetryAfter carries the upstream hint (0 = unknown).
//
// Step/Type/Workspace are the caller-carried bookkeeping extensions: Step is
// the failed attempt's stage (the decision table is keyed by it), Type is the
// raw gateway error.type (distinguishes the account-limit classes inside
// KindDailyLimit), Workspace is the limit's workspace metadata (carried for
// observability; FreeUsageLimitError never carries one).
type Report struct {
	Kind       zen.Kind
	Egress     proxy.Egress
	Key        string
	RetryAfter time.Duration
	Step       int
	Type       string
	Workspace  string
}

// Attempt is the next upstream attempt decided by NextAttempt: Key/Egress/
// Transport to use, Step = the stage this attempt represents (0 = first
// attempt, 1 = key, 2 = fresh identity, 3 = direct). Stages may be skipped
// (single-key pool → 0,2,3), so callers enforce D1 by counting EXECUTED
// attempts (≤ 3 per request), not by reading Step alone.
type Attempt struct {
	Key       string
	Egress    proxy.Egress
	Transport http.RoundTripper
	Step      int
}

// Attempt returns the first attempt (stage 0) for a new request: the next
// key from the round-robin pool on the router's current egress.
func (r *Router) Attempt() Attempt {
	key := "public"
	if r.pool != nil {
		if k, _ := r.pool.Next(); k != "" {
			key = k
		}
	}
	eg, rt := r.Egress()
	return Attempt{Key: key, Egress: eg, Transport: rt, Step: 0}
}

// NextAttempt decides the next attempt after rep failed, or false when the
// request is exhausted (D1 budget / nothing left to rotate) and the caller
// must surface the error. Decision table (spec §6), by rep.Kind and the
// report's Step:
//
//	KindDailyLimit (workspace-scoped account limits — Go/Black — only ever
//	                rotate keys, identity rotation cannot help):
//	  Step 0 → Step 1: next key, SAME egress — only when the pool holds an
//	                   alternative beyond rep.Key; otherwise skip the key
//	                   stage and go straight to the rotation decision.
//	  Step 1 → Step 2: switch to WARP with a fresh (unspent) pre-registered
//	                   identity, synchronously — no registration. Inside
//	                   RotationCooldown of the last rotation only the FIRST
//	                   switch of the window is persisted for later requests
//	                   (this request still gets no re-issue, and subsequent
//	                   in-window reports return false without touching the
//	                   tunnel — no A→B→A churn).
//	  Step 2 → Step 3: direct, respecting the configured address family.
//	                   No fresh identity left → fall through to Step 3.
//	  Step ≥ 3        → false.
//	KindKeyRateLimit  → Step 0: next key only, same egress, NEVER identity or
//	                   egress rotation; Step ≥ 1 → false (host backoff).
//	Other kinds       → false: non-quota failures are not fixed by rotating
//	                   credentials; zen.IsRetryable() stays the host's
//	                   backoff concern (conservative: never burn the attempt
//	                   budget on server/model/auth errors).
//
// Every KindDailyLimit report is recorded against BOTH the egress and the
// key before the decision (spec §6.1), even when it ends in false.
func (r *Router) NextAttempt(rep Report) (Attempt, bool) {
	// Default the failed egress BEFORE recording so an Egress-less report
	// still increments the egress counter (the decision treats Current() as
	// the failed path either way).
	if rep.Egress == "" {
		rep.Egress = r.Current()
	}
	now := time.Now()
	if rep.Kind == zen.KindDailyLimit {
		r.recordReport(rep, now)
	}

	switch rep.Kind {
	case zen.KindDailyLimit:
		if rep.Step >= 3 {
			return Attempt{}, false
		}
		if accountScoped(rep) {
			// Workspace/account limits: keys only, at any step.
			if rep.Step != 0 {
				return Attempt{}, false
			}
			return r.keyStep(rep)
		}
		switch rep.Step {
		case 0:
			if att, ok := r.keyStep(rep); ok {
				return att, true
			}
			return r.identityStep(rep, now)
		case 1:
			return r.identityStep(rep, now)
		case 2:
			return r.directStep(rep)
		default:
			return Attempt{}, false
		}
	case zen.KindKeyRateLimit:
		if rep.Step != 0 {
			return Attempt{}, false
		}
		return r.keyStep(rep)
	default:
		return Attempt{}, false
	}
}

// accountScoped reports whether the daily-limit report is a workspace-scoped
// account limit (Go/Black usage windows): only a key change can help, never
// an identity rotation. Workspace metadata without a parsed type counts too
// — FreeUsageLimitError never carries a workspace.
func accountScoped(rep Report) bool {
	switch rep.Type {
	case typeGoUsageLimit, typeBlackUsageLimit:
		return true
	case typeFreeUsageLimit:
		return false
	}
	return rep.Workspace != ""
}

// recordReport persists one daily-limit report against the egress it failed
// on and the key it failed with (spec §6.1), with the spent window taken
// from RetryAfter or synthesized to the next UTC midnight. A warp-egress
// report additionally stamps the ACTIVE identity's SpentUntil/Last429At —
// the limited IP belongs to it, and without this the spent filter in
// freshIdentityIndex could never trigger (spec §6 step 3, "warp fully spent
// → direct", would be dead code). Workspace/account limits are exempt: they
// are not IP-scoped (spec §4), so their identities stay usable.
func (r *Router) recordReport(rep Report, now time.Time) {
	until := quota.NextReset(now)
	if rep.RetryAfter > 0 {
		until = now.Add(rep.RetryAfter)
	}
	if rep.Egress != "" {
		r.store.RecordDaily429(string(rep.Egress), until)
	}
	if rep.Key != "" {
		r.store.RecordKeyDaily429(rep.Key)
		r.store.SetKeySpent(rep.Key, until)
	}
	if rep.Egress == proxy.EgressWarp && !accountScoped(rep) {
		if id := r.store.ActiveIdentity(); id != nil {
			id.SpentUntil = until.UnixMilli()
			id.Last429At = now.UnixMilli()
			r.store.SetWarp(*id) // replaces the active pool entry, persists
		}
	}
}

// keyStep (stage 1): re-issue on the SAME egress with the next pool key.
// False when the pool holds no alternative — the caller then falls through
// to the next stage decision (NextAttempt) or surfaces the error.
func (r *Router) keyStep(rep Report) (Attempt, bool) {
	if r.pool == nil || !r.pool.HasAlternatives(rep.Key) {
		return Attempt{}, false
	}
	key, ok := r.otherKey(rep.Key)
	if !ok {
		return Attempt{}, false
	}
	eg, rt := r.attemptPath(rep.Egress)
	return Attempt{Key: key, Egress: eg, Transport: rt, Step: 1}, true
}

// otherKey draws pool keys (round-robin) until one differs from current,
// bounded by the pool length.
func (r *Router) otherKey(current string) (string, bool) {
	for i := 0; i < r.pool.Len(); i++ {
		if k, _ := r.pool.Next(); k != current {
			return k, true
		}
	}
	return "", false
}

// identityStep (stage 2): switch to WARP with a fresh pre-registered
// identity from the pool. With no fresh identity left the decision falls
// through to direct (stage 3). Cooldown: inside RotationCooldown of the last
// rotation only the FIRST switch of the window is persisted (so later
// requests start on the fresh identity) and it yields no re-issue; further
// in-window reports return false without switching — a cooldown burst must
// not reconfigure the tunnel per request or ping-pong A→B→A.
func (r *Router) identityStep(rep Report, now time.Time) (Attempt, bool) {
	r.mu.Lock()
	inside := time.Since(r.lastRotate) < r.rotationCooldown
	already := r.cooldownSwapped
	from := r.egress
	r.mu.Unlock()
	if inside && already {
		return Attempt{}, false
	}

	idx := r.freshIdentityIndex(now)
	if idx < 0 {
		return r.directStep(rep)
	}
	id := r.store.Identity(idx)
	if id == nil {
		return r.directStep(rep)
	}

	rt, err := r.switchIdentity(idx, id)
	if err != nil {
		r.log.Printf("identity switch failed: %v", err)
		if inside {
			return Attempt{}, false
		}
		// Warp unusable → fall back to direct so requests keep flowing
		// (parity with applyRotation).
		return r.directStep(rep)
	}
	if from != proxy.EgressWarp {
		r.store.RecordRotation(string(from), string(proxy.EgressWarp), "fresh warp identity")
	}
	if inside {
		r.mu.Lock()
		r.cooldownSwapped = true
		r.mu.Unlock()
		return Attempt{}, false
	}
	r.mu.Lock()
	r.lastRotate = time.Now()
	r.cooldownSwapped = false
	r.mu.Unlock()
	r.scheduleSpareRegistration()
	return Attempt{Key: rep.Key, Egress: proxy.EgressWarp, Transport: rt, Step: 2}, true
}

// freshIdentityIndex returns the pool index of a fresh identity to switch to,
// or -1 when none is usable. Preference: an UNSPENT entry other than the
// active one (the hot spare); when leaving direct egress the active identity
// itself is acceptable if unspent (direct → warp activation needs no mint).
// While already on warp the active entry never counts: its IP is the one
// that just failed.
func (r *Router) freshIdentityIndex(now time.Time) int {
	snap := r.store.Snapshot()
	ids := r.store.Identities()
	active := snap.Active
	for i := range ids {
		if i == active {
			continue
		}
		if ids[i].SpentUntil > now.UnixMilli() {
			continue
		}
		return i
	}
	if r.Current() != proxy.EgressWarp && active >= 0 && active < len(ids) {
		if ids[active].SpentUntil <= now.UnixMilli() {
			return active
		}
	}
	return -1
}

// switchIdentity activates the pre-registered identity at idx: it swaps the
// tunnel/transport SYNCHRONOUSLY without any Cloudflare registration (spec §6
// hot spare), then persists the activation and the warp egress.
func (r *Router) switchIdentity(idx int, id *quota.WarpIdentity) (http.RoundTripper, error) {
	apply := r.identitySwitch
	if apply == nil {
		apply = r.applyIdentity
	}
	rt, err := apply(id)
	if err != nil {
		return nil, err
	}
	if err := r.store.SetActiveIdentity(idx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.warpRT = rt
	r.mu.Unlock()
	r.setEgress(proxy.EgressWarp)
	return rt, nil
}

// applyIdentity is the real identity swap: it reconfigures the WireGuard
// tunnel from the identity's PERSISTED fields and rebuilds the warp
// transport — no registration, no network call to Cloudflare (caveat: the
// persisted endpoint can go stale; spare re-registration refreshes the pool).
// Test seam: Options.IdentitySwitch.
func (r *Router) applyIdentity(id *quota.WarpIdentity) (http.RoundTripper, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	kp := &warp.Key{Private: id.PrivateKey, Public: id.PublicKey}
	prof := &warp.WireGuardProfile{
		AddressV4: id.AddressV4,
		AddressV6: id.AddressV6,
		ServerPub: id.ServerPub,
		Endpoint:  id.Endpoint,
	}
	t := warp.NewTunnel(Device, kp, prof)
	if t.DeviceExists() {
		if err := t.Reconfigure(); err != nil {
			return nil, fmt.Errorf("reconfigure warp tunnel: %w", err)
		}
	} else if err := t.Up(); err != nil {
		return nil, fmt.Errorf("bring up warp tunnel: %w", err)
	}
	r.tunnel = t

	rt, err := proxy.WarpTransport(Device, warp.NewResolver(nil))
	if err != nil {
		return nil, fmt.Errorf("build warp transport: %w", err)
	}
	return rt, nil
}

// directStep (stage 3): all identities spent (or warp unusable) → switch to
// direct, respecting the configured address family (the direct transport is
// family-pinned at construction). Already on direct → false.
func (r *Router) directStep(rep Report) (Attempt, bool) {
	if rep.Egress == proxy.EgressDirect {
		return Attempt{}, false
	}
	from := r.Current()
	r.setEgress(proxy.EgressDirect)
	if from != proxy.EgressDirect {
		r.store.RecordRotation(string(from), string(proxy.EgressDirect), "all identities spent")
	}
	return Attempt{Key: rep.Key, Egress: proxy.EgressDirect, Transport: r.directRT, Step: 3}, true
}

// attemptPath resolves the transport for re-issuing on e: the warp transport
// when that path is still up, otherwise the router's current path (mirrors
// Egress() when warp has been torn down).
func (r *Router) attemptPath(e proxy.Egress) (proxy.Egress, http.RoundTripper) {
	if e == proxy.EgressWarp {
		r.mu.Lock()
		warpRT := r.warpRT
		r.mu.Unlock()
		if warpRT != nil {
			return proxy.EgressWarp, warpRT
		}
	}
	return r.Egress()
}

// scheduleSpareRegistration kicks off background registration of the next
// pool spare so the hot spare just consumed is replaced (spec §6: spare
// registration is background and throttled). Single-flight, bounded by a
// 60s context (mirrors rotate()'s lifecycle); registration itself jitters
// and stops at the PoolSize+PoolSpare target. Called only outside the
// rotation cooldown — minting inside the cooldown would spam the Cloudflare
// API, which is exactly what rotationCooldown protects.
func (r *Router) scheduleSpareRegistration() {
	if len(r.store.Identities()) >= r.poolSize+r.poolSpare {
		return
	}
	r.mu.Lock()
	if r.registering {
		r.mu.Unlock()
		return
	}
	r.registering = true
	r.mu.Unlock()

	reg := r.spareRegistrar
	if reg == nil {
		reg = r.registerSpare
	}
	go func() {
		defer func() {
			r.mu.Lock()
			r.registering = false
			r.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := reg(ctx); err != nil {
			r.log.Printf("spare registration failed: %v", err)
		}
	}()
}

// registerSpare is the real background spare registration: jittered to
// respect Cloudflare rate limits (spec §6), it registers a fresh device via
// a LOCAL client (the active identity's client stays untouched) and appends
// it to the pool without activating it — the spare stays cold until a stage-2
// switch consumes it.
func (r *Router) registerSpare(ctx context.Context) error {
	if len(r.store.Identities()) >= r.poolSize+r.poolSpare {
		return nil
	}
	// Jitter (spec §6): never mint on a predictable cadence.
	jitter := time.Duration(rand.Int64N(int64(2 * time.Second)))
	select {
	case <-time.After(jitter):
	case <-ctx.Done():
		return ctx.Err()
	}
	if len(r.store.Identities()) >= r.poolSize+r.poolSpare {
		return nil
	}

	kp, err := warp.NewKeyPair()
	if err != nil {
		return fmt.Errorf("generate warp key: %w", err)
	}
	c := warp.NewClient()
	dev, err := c.Register(ctx, kp.Public)
	if err != nil {
		return fmt.Errorf("register warp device: %w", err)
	}
	c.Device = dev
	p, err := c.GetWireGuardProfile(ctx)
	if err != nil {
		return fmt.Errorf("fetch warp config: %w", err)
	}
	r.store.AddIdentity(&quota.WarpIdentity{
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
	return nil
}
