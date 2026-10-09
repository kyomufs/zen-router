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

// testRouter bundles a Router with its test seams.
type testRouter struct {
	*Router
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

	r, err := New(opts)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return &testRouter{Router: r}
}

// TestNextAttemptKeyStep: a daily-limit report with a pool of >= 2 keys
// re-issues on the direct lane with the next key (stage 1); key-step reports
// record both counters.
func TestNextAttemptKeyStep(t *testing.T) {
	tr := newTestRouter(t, Options{})

	rep := Report{
		Kind:       zen.KindDailyLimit,
		Egress:     proxy.EgressDirect,
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
	if att.Egress != proxy.EgressDirect {
		t.Errorf("Egress = %s, want direct (same lane)", att.Egress)
	}
	if att.Key == "k1" || att.Key == "" {
		t.Errorf("Key = %q, want a different pool key", att.Key)
	}
	snap := tr.store.Snapshot()
	if got := snap.Egress["direct"]; got == nil || got.Daily429 != 1 {
		t.Errorf("egress direct Daily429 = %v, want 1", got)
	}
	if got := snap.Keys["k1"]; got == nil || got.Daily429 != 1 {
		t.Errorf("key k1 Daily429 = %v, want 1", got)
	}
}

// TestNextAttemptDirectTransportRespectsFamily: the key step hands back the
// direct transport, which honors the configured address family (v6 dials no
// IPv4).
func TestNextAttemptDirectTransportRespectsFamily(t *testing.T) {
	tr := newTestRouter(t, Options{Family: "v6"})

	att, ok := tr.NextAttempt(Report{
		Kind:   zen.KindDailyLimit,
		Egress: proxy.EgressDirect,
		Key:    "k1",
		Step:   0,
	})
	if !ok {
		t.Fatal("NextAttempt: expected a key re-issue, got false")
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
}

// TestKeyRateLimitRotatesKeyOnly: KindKeyRateLimit rotates to the next key
// only. With no alternatives the caller surfaces the 429 (host backoff).
func TestKeyRateLimitRotatesKeyOnly(t *testing.T) {
	tr := newTestRouter(t, Options{})

	rep := Report{
		Kind:   zen.KindKeyRateLimit,
		Egress: proxy.EgressDirect,
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
	if att.Egress != proxy.EgressDirect {
		t.Errorf("Egress = %s, want direct", att.Egress)
	}
	if att.Key == "k1" || att.Key == "" {
		t.Errorf("Key = %q, want a different key", att.Key)
	}

	// Budget exhausted: no further stages for a key rate limit.
	if _, ok := tr.NextAttempt(Report{
		Kind:   zen.KindKeyRateLimit,
		Egress: proxy.EgressDirect,
		Key:    att.Key,
		Step:   1,
	}); ok {
		t.Error("NextAttempt at Step 1 = true, want false (host backoff)")
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

	t.Run("empty egress defaults to current path", func(t *testing.T) {
		// An Egress-less report must record against the router's current
		// egress (the decision already treats it as the failed path) — not
		// silently drop the egress counter.
		tr := newTestRouter(t, Options{})
		tr.NextAttempt(Report{
			Kind:       zen.KindDailyLimit,
			Key:        "k1",
			Step:       0,
			RetryAfter: time.Hour,
		})
		snap := tr.store.Snapshot()
		if eg := snap.Egress["direct"]; eg == nil || eg.Daily429 != 1 {
			t.Errorf("egress direct = %v, want Daily429=1 for the defaulted egress", eg)
		}
		if ks := snap.Keys["k1"]; ks == nil || ks.Daily429 != 1 {
			t.Errorf("key k1 = %v, want Daily429=1", ks)
		}
	})

	t.Run("exhausted report still records both counters", func(t *testing.T) {
		tr := newTestRouter(t, Options{})
		if _, ok := tr.NextAttempt(Report{
			Kind:       zen.KindDailyLimit,
			Egress:     proxy.EgressDirect,
			Key:        "k1",
			Step:       1,
			RetryAfter: time.Hour,
		}); ok {
			t.Error("NextAttempt at Step 1 = true, want false")
		}
		snap := tr.store.Snapshot()
		if eg := snap.Egress["direct"]; eg == nil || eg.Daily429 != 1 {
			t.Errorf("egress direct = %v, want Daily429=1 recorded before the false decision", eg)
		}
		if ks := snap.Keys["k1"]; ks == nil || ks.Daily429 != 1 {
			t.Errorf("key k1 = %v, want Daily429=1 recorded before the false decision", ks)
		}
	})
}
