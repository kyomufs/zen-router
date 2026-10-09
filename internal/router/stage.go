// Staged rotation state machine (direct-only): a 429 moves ONE stage —
// (0) first attempt, (1) next API key from the pool on the same (direct)
// lane — and any further quota report returns false: the caller surfaces
// the 429. There is no WARP identity stage and no egress switching; the
// gateway is direct-only and IP rotation happens at the network layer.
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
//	KindDailyLimit → Step 0: next key on the same lane, only when the pool
//	                 holds an alternative beyond rep.Key; otherwise false.
//	                 Step ≥ 1 → false (one key rotation per request).
//	KindKeyRateLimit → Step 0: next key only; Step ≥ 1 → false (host
//	                 backoff).
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
		if rep.Step != 0 {
			return Attempt{}, false
		}
		return r.keyStep(rep)
	case zen.KindKeyRateLimit:
		if rep.Step != 0 {
			return Attempt{}, false
		}
		return r.keyStep(rep)
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
