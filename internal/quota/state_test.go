package quota

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	m, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	return m, path
}

// TestStateV2RoundTrip pins persistence of per-key counters and egress
// health fields across a save/reload cycle.
func TestStateV2RoundTrip(t *testing.T) {
	m, path := openTest(t)

	m.RecordKeySuccess("public")
	m.RecordKeySuccess("public")
	m.RecordKeyDaily429("public")
	spent := time.UnixMilli(1700000000000)
	m.SetKeySpent("public", spent)
	m.RecordRequestSuccess("direct", "public")

	// Reload from disk.
	m2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if got := m2.Snapshot().Version; got != 2 {
		t.Errorf("Version after reload = %d, want 2", got)
	}

	ks := m2.KeyStats("public")
	if ks.OK != 3 || ks.Daily429 != 1 {
		t.Errorf("KeyStats(public) = %+v, want ok=3 daily429=1", ks)
	}
	if ks.Last429At == 0 {
		t.Errorf("KeyStats(public).Last429At = 0, want set")
	}
	if ks.SpentUntil != spent.UnixMilli() {
		t.Errorf("KeyStats(public).SpentUntil = %d, want %d", ks.SpentUntil, spent.UnixMilli())
	}
	if e := m2.Snapshot().Egress["direct"]; e == nil || e.OK != 1 {
		t.Errorf("egress[direct] = %#v, want ok=1", e)
	}
}

// TestOpenKeepsExistingV2 pins that opening an on-disk state (including a
// pre-excision file carrying unknown warp-era keys) does NOT rewrite the
// file, and the known counters survive the load.
func TestOpenKeepsExistingV2(t *testing.T) {
	v2 := `{
  "version": 2,
  "mode": "auto",
  "current": "direct",
  "updatedAt": 1700000000000,
  "egress": {"direct": {"ok": 1}, "warp": {}},
  "keys": {"public": {"ok": 3, "daily429": 1}},
  "identities": [{"deviceId": "dev-x", "token": "t", "privateKey": "p", "publicKey": "q", "registeredAt": 7}],
  "active": 0,
  "rotations": []
}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(v2), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	m, err := Open(path)
	if err != nil {
		t.Fatalf("Open v2 fixture: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read fixture: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Open rewrote an already-loaded state file; want no rewrite")
	}

	if ks := m.KeyStats("public"); ks.OK != 3 || ks.Daily429 != 1 {
		t.Errorf("KeyStats(public) = %+v, want ok=3 daily429=1", ks)
	}
	if e := m.Snapshot().Egress["direct"]; e == nil || e.OK != 1 {
		t.Errorf("egress[direct] = %#v, want ok=1", e)
	}
}

func TestRecordKey429(t *testing.T) {
	m, _ := openTest(t)
	before := time.Now().UnixMilli()

	m.RecordKeyDaily429("alpha")
	ks := m.KeyStats("alpha")
	if ks.Daily429 != 1 {
		t.Errorf("Daily429 = %d, want 1", ks.Daily429)
	}
	if ks.Last429At < before {
		t.Errorf("Last429At = %d, want >= %d", ks.Last429At, before)
	}
	m.RecordKeyDaily429("alpha")
	if got := m.KeyStats("alpha").Daily429; got != 2 {
		t.Errorf("Daily429 after 2nd call = %d, want 2", got)
	}
	if other := m.KeyStats("beta"); other.Daily429 != 0 || other.OK != 0 {
		t.Errorf("KeyStats(beta) = %+v, want zero", other)
	}
}

func TestRecordKeySuccess(t *testing.T) {
	m, _ := openTest(t)
	m.RecordKeySuccess("alpha")
	m.RecordKeySuccess("alpha")
	ks := m.KeyStats("alpha")
	if ks.OK != 2 {
		t.Errorf("OK = %d, want 2", ks.OK)
	}
	if ks.Daily429 != 0 {
		t.Errorf("Daily429 = %d, want 0", ks.Daily429)
	}
}

// TestRecordRequestSuccess pins the batched gateway entry point (review
// Task 1, F4): ONE call moves BOTH the per-egress and per-key success
// counters, persists them for a fresh reader, and an empty key (legacy
// proxy path) only moves the egress counter.
func TestRecordRequestSuccess(t *testing.T) {
	m, path := openTest(t)
	m.RecordRequestSuccess("direct", "alpha")
	snap := m.Snapshot()
	if e := snap.Egress["direct"]; e == nil || e.OK != 1 {
		t.Errorf("egress[direct].OK = %#v, want 1 after one batched call", snap.Egress["direct"])
	}
	if ks := m.KeyStats("alpha"); ks.OK != 1 {
		t.Errorf("keys[alpha].OK = %d, want 1 after one batched call", ks.OK)
	}
	// A fresh reader sees BOTH increments: one lock, one save.
	m2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open %q: %v", path, err)
	}
	if e := m2.Snapshot().Egress["direct"]; e == nil || e.OK != 1 {
		t.Errorf("persisted egress[direct].OK = %#v, want 1", e)
	}
	if ks := m2.KeyStats("alpha"); ks.OK != 1 {
		t.Errorf("persisted keys[alpha].OK = %d, want 1", ks.OK)
	}
	// Proxy path: empty key touches only the egress counter.
	m.RecordRequestSuccess("direct", "")
	if ks := m.KeyStats("alpha"); ks.OK != 1 {
		t.Errorf("keys[alpha].OK = %d after keyless call, want 1 (unchanged)", ks.OK)
	}
	if e := m.Snapshot().Egress["direct"]; e == nil || e.OK != 2 {
		t.Errorf("egress[direct].OK = %#v, want 2", e)
	}
}

func TestSetKeySpent(t *testing.T) {
	m, _ := openTest(t)
	until := time.UnixMilli(1700000000000)
	m.SetKeySpent("alpha", until)
	if got := m.KeyStats("alpha").SpentUntil; got != until.UnixMilli() {
		t.Errorf("SpentUntil = %d, want %d", got, until.UnixMilli())
	}
	// Unknown key reads are nil-safe zeros.
	if zero := m.KeyStats("nope"); zero.OK != 0 || zero.SpentUntil != 0 {
		t.Errorf("KeyStats(nope) = %+v, want zero", zero)
	}
}
