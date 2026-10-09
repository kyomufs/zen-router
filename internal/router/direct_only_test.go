package router

import (
	"testing"

	"zen-router/internal/keys"
	"zen-router/internal/proxy"
	"zen-router/internal/zen"
)

// TestNextAttemptStopsAfterKeyStep pins the direct-only decision table:
// after the key stage (Step 1) the request is exhausted — there is no WARP
// identity stage and no direct-fallback stage anymore.
func TestNextAttemptStopsAfterKeyStep(t *testing.T) {
	tr := newTestRouter(t, Options{}) // pool: k1, k2

	// Step 0 (daily limit, key k1 in a multi-key pool) → next key, step 1.
	att, ok := tr.NextAttempt(Report{Kind: zen.KindDailyLimit, Key: "k1", Step: 0})
	if !ok {
		t.Fatalf("step 0: want key rotation attempt")
	}
	if att.Step != 1 {
		t.Fatalf("step 0: attempt.Step = %d, want 1", att.Step)
	}
	if att.Egress != proxy.EgressDirect {
		t.Errorf("step 0: attempt egress = %q, want %q", att.Egress, proxy.EgressDirect)
	}
	if att.Key == "k1" {
		t.Errorf("step 0: attempt key = %q, want an alternative pool key", att.Key)
	}

	// Step 1 (daily limit again) → exhausted: no identity/direct stages.
	if att2, ok2 := tr.NextAttempt(Report{Kind: zen.KindDailyLimit, Key: att.Key, Step: 1}); ok2 {
		t.Errorf("step 1: want exhaustion, got attempt %+v", att2)
	}
}

// TestNextAttemptSingleKeyStopsImmediately: a single-key pool has no
// alternative, so a Step 0 daily limit is immediately exhausted.
func TestNextAttemptSingleKeyStopsImmediately(t *testing.T) {
	tr := newTestRouter(t, Options{Pool: keys.New(writePoolFile(t, []string{"public"}))})

	if att, ok := tr.NextAttempt(Report{Kind: zen.KindDailyLimit, Key: "public", Step: 0}); ok {
		t.Errorf("single-key pool: want exhaustion, got attempt %+v", att)
	}
}

// TestKeyRateLimitStopsAfterKeyStep: key-rate limits also never leave the
// direct lane.
func TestKeyRateLimitStopsAfterKeyStep(t *testing.T) {
	tr := newTestRouter(t, Options{})

	att, ok := tr.NextAttempt(Report{Kind: zen.KindKeyRateLimit, Key: "k1", Step: 0})
	if !ok || att.Step != 1 || att.Egress != proxy.EgressDirect {
		t.Fatalf("step 0: got ok=%v attempt=%+v, want direct step 1", ok, att)
	}
	if att2, ok2 := tr.NextAttempt(Report{Kind: zen.KindKeyRateLimit, Key: att.Key, Step: 1}); ok2 {
		t.Errorf("step 1: want exhaustion, got attempt %+v", att2)
	}
}
