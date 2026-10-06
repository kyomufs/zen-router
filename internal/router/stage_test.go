package router

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zen-router/internal/keys"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/zen"
)

// writePoolFile writes a keys pool-config.json holding ks and returns its
// path. A pool file entry of "public" is filtered by keys.readKeys, so
// ["public"] exercises the single-key fallback pool.
func writePoolFile(t *testing.T, ks []string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"pools": map[string]any{"opencode": map[string]any{"keys": ks}},
	})
	if err != nil {
		t.Fatalf("marshal pool file: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pool.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write pool file: %v", err)
	}
	return path
}

// fakeSwitch records which identity NextAttempt activated and hands back a
// marker transport, so no WireGuard tunnel (root) is ever needed in tests.
type fakeSwitch struct {
	id *quota.WarpIdentity
	rt http.RoundTripper
}

// testRouter bundles a Router with its test seams.
type testRouter struct {
	*Router
	sw         *fakeSwitch
	registered chan struct{}
}

func newTestRouter(t *testing.T, opts Options) *testRouter {
	t.Helper()
	// Neutralize the environment key override so the pool file is the only
	// key source.
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	st, err := quota.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("quota.Open: %v", err)
	}
	opts.Store = st
	if opts.Pool == nil {
		opts.Pool = keys.New(writePoolFile(t, []string{"k1", "k2"}))
	}

	sw := &fakeSwitch{rt: &http.Transport{}}
	opts.IdentitySwitch = func(id *quota.WarpIdentity) (http.RoundTripper, error) {
		sw.id = id
		return sw.rt, nil
	}
	registered := make(chan struct{}, 8)
	opts.SpareRegistrar = func(context.Context) error {
		registered <- struct{}{}
		return nil
	}

	r, err := New(opts)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return &testRouter{Router: r, sw: sw, registered: registered}
}

// setWarpEgress puts the router on the warp path without a tunnel.
func (tr *testRouter) setWarpEgress(t *testing.T) {
	t.Helper()
	tr.mu.Lock()
	tr.egress = proxy.EgressWarp
	tr.warpRT = tr.sw.rt
	tr.mu.Unlock()
	tr.store.SetCurrent("warp")
}

func addIdentity(t *testing.T, st *quota.Manager, device string, spentUntil time.Time) {
	t.Helper()
	st.AddIdentity(&quota.WarpIdentity{
		DeviceID:   device,
		SpentUntil: spentUntil.UnixMilli(),
	})
}

// TestNextAttemptKeyStep: a daily-limit report on egress E with a pool of
// >= 2 keys re-issues on the SAME egress with the next key (stage 1).
func TestNextAttemptKeyStep(t *testing.T) {
	tr := newTestRouter(t, Options{})
	tr.setWarpEgress(t)

	rep := Report{
		Kind:       zen.KindDailyLimit,
		Egress:     proxy.EgressWarp,
		Key:        "k1",
		Step:       0,
		RetryAfter: time.Hour,
	}
	att, ok := tr.NextAttempt(rep)
	if !ok {
		t.Fatal("NextAttempt: expected a key re-issue, got false")
	}
	if att.Step != 1 {
		t.Errorf("Step = %d, want 1", att.Step)
	}
	if att.Egress != proxy.EgressWarp {
		t.Errorf("Egress = %s, want warp (same egress)", att.Egress)
	}
	if att.Key == "k1" || att.Key == "" {
		t.Errorf("Key = %q, want a different pool key", att.Key)
	}
	if att.Transport != tr.sw.rt {
		t.Errorf("Transport = %v, want the warp transport", att.Transport)
	}
	snap := tr.store.Snapshot()
	if got := snap.Egress["warp"]; got == nil || got.Daily429 != 1 {
		t.Errorf("egress warp Daily429 = %v, want 1", got)
	}
	if got := snap.Keys["k1"]; got == nil || got.Daily429 != 1 {
		t.Errorf("key k1 Daily429 = %v, want 1", got)
	}
	if len(snap.Rotations) != 0 {
		t.Errorf("Rotations = %d, want 0 (key step is not a rotation)", len(snap.Rotations))
	}
}

// TestNextAttemptSkipsKeyStepWhenSingleKey: with pool ["public"] there is no
// key re-issue — the decision skips straight to the rotation stage (Step 2).
func TestNextAttemptSkipsKeyStepWhenSingleKey(t *testing.T) {
	tr := newTestRouter(t, Options{Pool: keys.New(writePoolFile(t, []string{"public"}))})
	addIdentity(t, tr.store, "id0", time.Time{})
	addIdentity(t, tr.store, "id1", time.Time{})

	rep := Report{
		Kind:       zen.KindDailyLimit,
		Egress:     proxy.EgressDirect,
		Key:        "public",
		Step:       0,
		RetryAfter: time.Hour,
	}
	att, ok := tr.NextAttempt(rep)
	if !ok {
		t.Fatal("NextAttempt: expected rotation to proceed, got false")
	}
	if att.Step != 2 {
		t.Errorf("Step = %d, want 2 (key stage skipped)", att.Step)
	}
	if att.Egress != proxy.EgressWarp {
		t.Errorf("Egress = %s, want warp", att.Egress)
	}
	if att.Key != "public" {
		t.Errorf("Key = %q, want %q (key unchanged across identity stage)", att.Key, "public")
	}
	if att.Transport != tr.sw.rt {
		t.Errorf("Transport = %v, want the identity-switch transport", att.Transport)
	}
	if tr.sw.id == nil || tr.sw.id.DeviceID != "id1" {
		t.Errorf("switched identity = %v, want fresh non-active id1", tr.sw.id)
	}
	if id := tr.store.ActiveIdentity(); id == nil || id.DeviceID != "id1" {
		t.Errorf("active identity = %v, want id1", id)
	}
	if got := tr.store.Current(); got != "warp" {
		t.Errorf("Current = %s, want warp", got)
	}
}

// TestNextAttemptIdentityStep: after the key re-issue is also daily-limited
// (Step 1), switch to WARP with a fresh identity (Step 2). Inside the 30s
// cooldown: no re-issue for this request, but the rotation is persisted for
// later requests.
func TestNextAttemptIdentityStep(t *testing.T) {
	t.Run("fresh identity outside cooldown", func(t *testing.T) {
		tr := newTestRouter(t, Options{RotationCooldown: 30 * time.Second})
		addIdentity(t, tr.store, "id0", time.Time{}) // active
		addIdentity(t, tr.store, "id1", time.Time{})
		addIdentity(t, tr.store, "id2", time.Now().Add(time.Hour)) // spent spare

		rep := Report{
			Kind:       zen.KindDailyLimit,
			Egress:     proxy.EgressDirect,
			Key:        "k2",
			Step:       1,
			RetryAfter: time.Hour,
		}
		att, ok := tr.NextAttempt(rep)
		if !ok {
			t.Fatal("NextAttempt: expected identity re-issue, got false")
		}
		if att.Step != 2 {
			t.Errorf("Step = %d, want 2", att.Step)
		}
		if att.Egress != proxy.EgressWarp {
			t.Errorf("Egress = %s, want warp", att.Egress)
		}
		if att.Key != "k2" {
			t.Errorf("Key = %q, want k2 (identity stage keeps the key)", att.Key)
		}
		if att.Transport != tr.sw.rt {
			t.Errorf("Transport = %v, want the identity-switch transport", att.Transport)
		}
		if tr.sw.id == nil || tr.sw.id.DeviceID != "id1" {
			t.Errorf("switched identity = %v, want unspent non-active id1", tr.sw.id)
		}
		if id := tr.store.ActiveIdentity(); id == nil || id.DeviceID != "id1" {
			t.Errorf("active identity = %v, want id1", id)
		}
		if got := tr.store.Current(); got != "warp" {
			t.Errorf("Current = %s, want warp", got)
		}
		snap := tr.store.Snapshot()
		if len(snap.Rotations) != 1 {
			t.Errorf("Rotations = %d, want 1 persisted rotation", len(snap.Rotations))
		}
		tr.mu.Lock()
		last := tr.lastRotate
		tr.mu.Unlock()
		if time.Since(last) > time.Minute {
			t.Errorf("lastRotate not stamped, got %v", last)
		}
		// Background spare registration must be scheduled.
		select {
		case <-tr.registered:
		case <-time.After(2 * time.Second):
			t.Error("background spare registration was not scheduled")
		}
	})

	t.Run("inside cooldown no reissue but rotation persisted", func(t *testing.T) {
		tr := newTestRouter(t, Options{RotationCooldown: 30 * time.Second})
		addIdentity(t, tr.store, "id0", time.Time{}) // active
		addIdentity(t, tr.store, "id1", time.Time{})

		tr.mu.Lock()
		tr.lastRotate = time.Now() // inside RotationCooldown
		tr.mu.Unlock()

		rep := Report{
			Kind:       zen.KindDailyLimit,
			Egress:     proxy.EgressDirect,
			Key:        "k2",
			Step:       1,
			RetryAfter: time.Hour,
		}
		if _, ok := tr.NextAttempt(rep); ok {
			t.Error("NextAttempt inside cooldown = true, want false (no re-issue)")
		}
		// The rotation itself is still persisted for later requests.
		if tr.sw.id == nil || tr.sw.id.DeviceID != "id1" {
			t.Errorf("switched identity = %v, want id1 persisted despite cooldown", tr.sw.id)
		}
		if id := tr.store.ActiveIdentity(); id == nil || id.DeviceID != "id1" {
			t.Errorf("active identity = %v, want id1", id)
		}
		if got := tr.store.Current(); got != "warp" {
			t.Errorf("Current = %s, want warp", got)
		}
		if snap := tr.store.Snapshot(); len(snap.Rotations) != 1 {
			t.Errorf("Rotations = %d, want 1 persisted rotation", len(snap.Rotations))
		}
		// No Cloudflare-facing spare minting inside the cooldown.
		select {
		case <-tr.registered:
			t.Error("spare registration scheduled inside cooldown")
		case <-time.After(100 * time.Millisecond):
		}
	})
}

// TestNextAttemptDirectStep: every identity is spent (spentUntil in the
// future) — the step-2 decision falls through to direct (Step 3), respecting
// the configured address family.
func TestNextAttemptDirectStep(t *testing.T) {
	t.Run("switches to direct", func(t *testing.T) {
		tr := newTestRouter(t, Options{})
		future := time.Now().Add(time.Hour)
		addIdentity(t, tr.store, "id0", future) // active
		addIdentity(t, tr.store, "id1", future)
		addIdentity(t, tr.store, "id2", future)
		tr.setWarpEgress(t)

		rep := Report{
			Kind:       zen.KindDailyLimit,
			Egress:     proxy.EgressWarp,
			Key:        "k1",
			Step:       2,
			RetryAfter: time.Hour,
		}
		att, ok := tr.NextAttempt(rep)
		if !ok {
			t.Fatal("NextAttempt: expected direct attempt, got false")
		}
		if att.Step != 3 {
			t.Errorf("Step = %d, want 3", att.Step)
		}
		if att.Egress != proxy.EgressDirect {
			t.Errorf("Egress = %s, want direct", att.Egress)
		}
		if att.Key != "k1" {
			t.Errorf("Key = %q, want k1", att.Key)
		}
		if att.Transport == tr.sw.rt {
			t.Error("Transport is still the warp transport, want the direct transport")
		}
		if tr.sw.id != nil {
			t.Errorf("identity switch invoked (%v), want none (all spent)", tr.sw.id)
		}
		if got := tr.store.Current(); got != "direct" {
			t.Errorf("Current = %s, want direct", got)
		}
		snap := tr.store.Snapshot()
		if len(snap.Rotations) != 1 || snap.Rotations[0].To != "direct" {
			t.Errorf("Rotations = %v, want one warp->direct rotation", snap.Rotations)
		}
	})

	t.Run("direct transport respects family v6", func(t *testing.T) {
		tr := newTestRouter(t, Options{Family: "v6"})
		addIdentity(t, tr.store, "id0", time.Now().Add(time.Hour))
		tr.setWarpEgress(t)

		att, ok := tr.NextAttempt(Report{
			Kind:   zen.KindDailyLimit,
			Egress: proxy.EgressWarp,
			Key:    "k1",
			Step:   2,
		})
		if !ok {
			t.Fatal("NextAttempt: expected direct attempt, got false")
		}
		if att.Egress != proxy.EgressDirect {
			t.Fatalf("Egress = %s, want direct", att.Egress)
		}
		rt, ok := att.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport type = %T, want *http.Transport", att.Transport)
		}
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := rt.DialContext(ctx, "tcp", ln.Addr().String()); err == nil {
			t.Error("family=v6 direct transport dialed an IPv4 address; family not enforced")
		}
	})
}

// TestKeyRateLimitNoIdentityRotate: KindKeyRateLimit rotates to the next key
// only; never touches identity or egress. With no alternatives the caller
// surfaces the 429 (host backoff).
func TestKeyRateLimitNoIdentityRotate(t *testing.T) {
	tr := newTestRouter(t, Options{})
	addIdentity(t, tr.store, "id0", time.Time{})
	addIdentity(t, tr.store, "id1", time.Time{})
	tr.setWarpEgress(t)

	rep := Report{
		Kind:   zen.KindKeyRateLimit,
		Egress: proxy.EgressWarp,
		Key:    "k1",
		Step:   0,
	}
	att, ok := tr.NextAttempt(rep)
	if !ok {
		t.Fatal("NextAttempt: expected next-key re-issue, got false")
	}
	if att.Step != 1 {
		t.Errorf("Step = %d, want 1", att.Step)
	}
	if att.Egress != proxy.EgressWarp {
		t.Errorf("Egress = %s, want warp (no egress switch)", att.Egress)
	}
	if att.Key == "k1" || att.Key == "" {
		t.Errorf("Key = %q, want a different key", att.Key)
	}
	if tr.sw.id != nil {
		t.Errorf("identity switch invoked (%v), want none", tr.sw.id)
	}
	if id := tr.store.ActiveIdentity(); id == nil || id.DeviceID != "id0" {
		t.Errorf("active identity = %v, want id0 untouched", id)
	}
	if snap := tr.store.Snapshot(); len(snap.Rotations) != 0 {
		t.Errorf("Rotations = %d, want 0", len(snap.Rotations))
	}
	if got := tr.store.Current(); got != "warp" {
		t.Errorf("Current = %s, want warp", got)
	}

	// Budget exhausted: no further stages for a key rate limit.
	if _, ok := tr.NextAttempt(Report{
		Kind:   zen.KindKeyRateLimit,
		Egress: proxy.EgressWarp,
		Key:    att.Key,
		Step:   1,
	}); ok {
		t.Error("NextAttempt at Step 1 = true, want false (host backoff)")
	}
	if tr.sw.id != nil {
		t.Errorf("identity switch after key exhaustion (%v), want none", tr.sw.id)
	}
}

// TestAccountLimitNoRotation: workspace-scoped account limits
// (GoUsageLimitError / BlackUsageLimitError, different workspace) may only
// rotate keys — identity/egress rotation never helps.
func TestAccountLimitNoRotation(t *testing.T) {
	for _, typ := range []string{"GoUsageLimitError", "BlackUsageLimitError"} {
		t.Run(typ, func(t *testing.T) {
			tr := newTestRouter(t, Options{})
			addIdentity(t, tr.store, "id0", time.Time{})
			addIdentity(t, tr.store, "id1", time.Time{})
			tr.setWarpEgress(t)

			att, ok := tr.NextAttempt(Report{
				Kind:      zen.KindDailyLimit,
				Type:      typ,
				Workspace: "ws-other",
				Egress:    proxy.EgressWarp,
				Key:       "k1",
				Step:      0,
			})
			if !ok {
				t.Fatal("NextAttempt: expected key rotation, got false")
			}
			if att.Step != 1 {
				t.Errorf("Step = %d, want 1", att.Step)
			}
			if att.Egress != proxy.EgressWarp {
				t.Errorf("Egress = %s, want warp (key rotation only)", att.Egress)
			}
			if att.Key == "k1" || att.Key == "" {
				t.Errorf("Key = %q, want a different key", att.Key)
			}

			// Step 1 exhausted: never escalate to identity rotation.
			if _, ok := tr.NextAttempt(Report{
				Kind:      zen.KindDailyLimit,
				Type:      typ,
				Workspace: "ws-other",
				Egress:    proxy.EgressWarp,
				Key:       att.Key,
				Step:      1,
			}); ok {
				t.Error("NextAttempt at Step 1 = true, want false (no identity rotation)")
			}
			if tr.sw.id != nil {
				t.Errorf("identity switch invoked (%v), want none", tr.sw.id)
			}
			if id := tr.store.ActiveIdentity(); id == nil || id.DeviceID != "id0" {
				t.Errorf("active identity = %v, want id0 untouched", id)
			}
			if snap := tr.store.Snapshot(); len(snap.Rotations) != 0 {
				t.Errorf("Rotations = %d, want 0", len(snap.Rotations))
			}
			if got := tr.store.Current(); got != "warp" {
				t.Errorf("Current = %s, want warp", got)
			}
		})
	}
}

// TestReportRecordsBothCounters: one daily-limit report increments the egress
// stats AND the key stats, with spentUntil taken from RetryAfter (or
// synthesized to UTC midnight when absent).
func TestReportRecordsBothCounters(t *testing.T) {
	t.Run("retry-after sets spentUntil", func(t *testing.T) {
		tr := newTestRouter(t, Options{})
		before := time.Now()
		tr.NextAttempt(Report{
			Kind:       zen.KindDailyLimit,
			Egress:     proxy.EgressDirect,
			Key:        "k1",
			Step:       0,
			RetryAfter: 45 * time.Minute,
		})
		after := time.Now()

		snap := tr.store.Snapshot()
		eg := snap.Egress["direct"]
		if eg == nil || eg.Daily429 != 1 {
			t.Fatalf("egress direct = %v, want Daily429=1", eg)
		}
		ks := snap.Keys["k1"]
		if ks == nil || ks.Daily429 != 1 {
			t.Fatalf("key k1 = %v, want Daily429=1", ks)
		}
		if eg.SpentUntil < before.Add(45*time.Minute).UnixMilli() ||
			eg.SpentUntil > after.Add(45*time.Minute).UnixMilli() {
			t.Errorf("egress spentUntil = %d, want ~now+45m", eg.SpentUntil)
		}
		if ks.SpentUntil != eg.SpentUntil {
			t.Errorf("key spentUntil = %d, egress spentUntil = %d, want equal", ks.SpentUntil, eg.SpentUntil)
		}
	})

	t.Run("zero retry-after synthesizes UTC midnight", func(t *testing.T) {
		tr := newTestRouter(t, Options{})
		tr.NextAttempt(Report{
			Kind:   zen.KindDailyLimit,
			Egress: proxy.EgressDirect,
			Key:    "k1",
			Step:   0,
		})
		snap := tr.store.Snapshot()
		eg := snap.Egress["direct"]
		ks := snap.Keys["k1"]
		if eg == nil || ks == nil || eg.Daily429 != 1 || ks.Daily429 != 1 {
			t.Fatalf("counters = egress %v / key %v, want both at 1", eg, ks)
		}
		now := time.Now()
		if eg.SpentUntil <= now.UnixMilli() || eg.SpentUntil > now.Add(24*time.Hour).UnixMilli() {
			t.Errorf("egress spentUntil = %d, want next UTC midnight", eg.SpentUntil)
		}
		if ks.SpentUntil != eg.SpentUntil {
			t.Errorf("key spentUntil = %d, want %d (same window)", ks.SpentUntil, eg.SpentUntil)
		}
	})
}
