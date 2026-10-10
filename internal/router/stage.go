// Staged rotation state machine: stage 0 is always the direct lane with a
// pool key; a classified 429 moves ONE stage further —
//
//   - quota evidence (KindDailyLimit: FreeUsage/Go/Black) → stage 1: the
//     next API key on the SAME (direct) lane. Quota is account state, not
//     an IP block: WARP is never offered (alztrk: "a 429 is not evidence
//     of an IP block");
//   - rate-limit evidence with no quota signal (KindKeyRateLimit,
//     KindIPLimit) → stage 1 on the WARP lane, when configured: rotate
//     the warp-cli identity, then re-issue the SAME key through the new
//     SOCKS5 egress. KindKeyRateLimit without a warp lane keeps the
//     legacy same-lane key step; KindIPLimit without one surfaces (key
//     rotation cannot move an egress IP).
//
// Any further quota report at stage ≥ 1 returns false: the caller surfaces
// the 429. There is no back-and-forth between lanes (the ping-pong that
// got the old WARP switcher disabled).
//
// Budget (D1): max 3 upstream attempts per request, counted by the CALLER
// (stage skips mean Attempt.Step is a stage id, not an attempt ordinal).
// NextAttempt returns false once the report's Step reaches 1.
package router

import (
	"net/http"
	"time"

	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/store"
	"zen-router/internal/zen"
)

// Report is one classified upstream failure, handed back to NextAttempt by
// the caller after an attempt failed. Egress/Key/Step describe the attempt
// that failed; RetryAfter carries the upstream hint (0 = unknown).
//
// Step is the caller-carried bookkeeping extension: the failed attempt's
// stage (the decision table is keyed by it).
type Report struct {
	Kind       zen.Kind
	Egress     proxy.Egress
	Key        string
	RetryAfter time.Duration
	Step       int
}

// Attempt is the next upstream attempt decided by NextAttempt: Key/Egress/
// Transport to use, Step = the stage this attempt represents (0 = first
// attempt, 1 = next key). Callers enforce D1 by counting EXECUTED attempts
// (≤ 3 per request), not by reading Step alone.
type Attempt struct {
	Key       string
	Egress    proxy.Egress
	Transport http.RoundTripper
	Step      int
}

// Attempt returns the first attempt (stage 0) for a new request: the next
// key from the round-robin pool on the direct lane.
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
// request is exhausted (nothing left to rotate) and the caller must surface
// the error. Decision table, by rep.Kind and the report's Step:
//
//	KindDailyLimit → Step 0: next key on the same (direct) lane, only when
//	                 the pool holds an alternative beyond rep.Key;
//	                 otherwise false. Step ≥ 1 → false (one key rotation
//	                 per request). NEVER stages WARP: quota is not an IP
//	                 block, spinning the egress identity cannot fix it.
//	KindKeyRateLimit → Step 0 + warp lane: rotate the WARP identity, then
//	                 re-issue the same key through the new SOCKS5 egress
//	                 (stage 1 on the warp lane). Step 0 without warp →
//	                 the legacy key step (same-lane key rotation, the only
//	                 rotation a direct-only deployment has). Step ≥ 1 →
//	                 false (host backoff).
//	KindIPLimit    → Step 0 + warp lane: warp identity rotation (same key)
//	                 — a plain 429 with no quota evidence is the IP-block
//	                 signal alztrk's rate-limit class exists for. Without
//	                 a warp lane → false: key rotation cannot change an
//	                 egress IP, so surfacing beats a futile retry.
//	                 Step ≥ 1 → false.
//	Other kinds    → false: non-quota failures are not fixed by rotating
//	                 credentials; zen.IsRetryable() stays the host's
//	                 backoff concern (conservative: never burn the attempt
//	                 budget on server/model/auth errors).
//
// Every KindDailyLimit report is recorded against BOTH the egress and the
// key before the decision (spec §6.1), even when it ends in false.
func (r *Router) NextAttempt(rep Report) (Attempt, bool) {
	// Default the failed egress BEFORE recording so an Egress-less report
	// still increments the egress counter.
	if rep.Egress == "" {
		rep.Egress = r.Current()
	}
	now := time.Now()
	switch rep.Kind {
	case zen.KindDailyLimit:
		r.recordReport(rep, now)
		r.recordHistory(store.Kind429, rep.Key)
		if rep.Step != 0 {
			return Attempt{}, false
		}
		return r.keyStep(rep)
	case zen.KindKeyRateLimit:
		// Per-key rate 429: quota counters deliberately unchanged (key
		// rotation does not apply), but the history store still counts it
		// as a 429. Stage 1 is the warp lane when configured; without one
		// the legacy key step stands (the direct lane's only rotation).
		r.recordHistory(store.Kind429, rep.Key)
		if rep.Step != 0 {
			return Attempt{}, false
		}
		if r.warp == nil {
			return r.keyStep(rep)
		}
		return r.warpStep(rep)
	case zen.KindIPLimit:
		// Plain 429 with no quota evidence: the IP-block signal the warp
		// lane exists for. The key is kept (only the egress identity
		// moves). No warp lane → surface: key rotation cannot change an
		// egress IP.
		r.recordHistory(store.Kind429, rep.Key)
		if rep.Step != 0 || r.warp == nil {
			return Attempt{}, false
		}
		return r.warpStep(rep)
	default:
		return Attempt{}, false
	}
}

// recordReport persists one daily-limit report against the egress it failed
// on and the key it failed with (spec §6.1), with the spent window taken
// from RetryAfter or synthesized to the next UTC midnight.
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
}

// keyStep (stage 1): re-issue on the SAME lane with the next pool key.
// False when the pool holds no alternative — the request is exhausted.
func (r *Router) keyStep(rep Report) (Attempt, bool) {
	if r.pool == nil || !r.pool.HasAlternatives(rep.Key) {
		return Attempt{}, false
	}
	key, ok := r.otherKey(rep.Key)
	if !ok {
		return Attempt{}, false
	}
	eg, rt := r.Egress()
	return Attempt{Key: key, Egress: eg, Transport: rt, Step: 1}, true
}

// warpStep (stage 1): rotate the WARP identity and re-issue the SAME key
// through the fresh SOCKS5 egress. A rate limit is bound to the key+lane
// pair, not to the key alone, so the key is kept and only the egress
// identity moves. A failed rotation degrades to the key step when the pool
// has an alternative, else false — rotation failure is logged by the warp
// manager itself.
func (r *Router) warpStep(rep Report) (Attempt, bool) {
	if err := r.warp.Rotate(); err != nil {
		r.log.Printf("warp: rotation failed, degrading to key step: %v", err)
		return r.keyStep(rep)
	}
	return Attempt{
		Key:       rep.Key,
		Egress:    proxy.EgressWarp,
		Transport: r.warp.Transport(),
		Step:      1,
	}, true
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
