package quota

import (
	"bytes"
	"encoding/json"
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

// TestStateV2RoundTrip pins persistence of per-key counters and identity
// health fields across a save/reload cycle.
func TestStateV2RoundTrip(t *testing.T) {
	m, path := openTest(t)

	m.RecordKeySuccess("public")
	m.RecordKeySuccess("public")
	m.RecordKeyDaily429("public")
	spent := time.UnixMilli(1700000000000)
	m.SetKeySpent("public", spent)

	idx := m.AddIdentity(&WarpIdentity{
		DeviceID:     "dev-a",
		Token:        "tok-a",
		PrivateKey:   "priv-a",
		PublicKey:    "pub-a",
		RegisteredAt: 111,
		SpentUntil:   222,
		Last429At:    333,
	})
	if idx != 0 {
		t.Fatalf("AddIdentity first index = %d, want 0", idx)
	}
	idx = m.AddIdentity(&WarpIdentity{
		DeviceID:     "dev-b",
		Token:        "tok-b",
		PrivateKey:   "priv-b",
		PublicKey:    "pub-b",
		RegisteredAt: 444,
	})
	if idx != 1 {
		t.Fatalf("AddIdentity second index = %d, want 1", idx)
	}
	if err := m.SetActiveIdentity(1); err != nil {
		t.Fatalf("SetActiveIdentity(1): %v", err)
	}

	// Reload from disk.
	m2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if got := m2.Snapshot().Version; got != 2 {
		t.Errorf("Version after reload = %d, want 2", got)
	}

	ks := m2.KeyStats("public")
	if ks.OK != 2 || ks.Daily429 != 1 {
		t.Errorf("KeyStats(public) = %+v, want ok=2 daily429=1", ks)
	}
	if ks.Last429At == 0 {
		t.Errorf("KeyStats(public).Last429At = 0, want set")
	}
	if ks.SpentUntil != spent.UnixMilli() {
		t.Errorf("KeyStats(public).SpentUntil = %d, want %d", ks.SpentUntil, spent.UnixMilli())
	}

	ids := m2.Identities()
	if len(ids) != 2 {
		t.Fatalf("len(Identities) = %d, want 2", len(ids))
	}
	if ids[0].DeviceID != "dev-a" || ids[1].DeviceID != "dev-b" {
		t.Errorf("Identities DeviceIDs = %q,%q; want dev-a,dev-b", ids[0].DeviceID, ids[1].DeviceID)
	}
	if ids[0].RegisteredAt != 111 || ids[0].SpentUntil != 222 || ids[0].Last429At != 333 {
		t.Errorf("identity[0] health = %+v, want registeredAt=111 spentUntil=222 last429At=333", ids[0])
	}
	if ids[1].RegisteredAt != 444 {
		t.Errorf("identity[1].RegisteredAt = %d, want 444", ids[1].RegisteredAt)
	}
	if act := m2.ActiveIdentity(); act == nil || act.DeviceID != "dev-b" {
		t.Errorf("ActiveIdentity() = %+v, want dev-b", act)
	}
}

// TestStateMigratesV1 feeds a literal v1 state file and asserts the in-memory
// and on-disk upgrade to v2.
func TestStateMigratesV1(t *testing.T) {
	v1 := `{
  "version": 1,
  "mode": "warp",
  "current": "warp",
  "updatedAt": 1700000000000,
  "egress": {
    "direct": {"ok": 5, "daily429": 1, "spentUntil": 123},
    "warp": {"ok": 9}
  },
  "warp": {
    "deviceId": "dev-1",
    "token": "tok-1",
    "privateKey": "priv-1",
    "publicKey": "pub-1",
    "registeredAt": 42
  },
  "rotations": [
    {"at": 1700000000000, "from": "direct", "to": "warp", "reason": "daily limit"}
  ]
}`
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(v1), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	m, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1 fixture: %v", err)
	}

	s := m.Snapshot()
	if s.Version != 2 {
		t.Errorf("in-memory Version = %d, want 2", s.Version)
	}
	if len(s.Identities) != 1 {
		t.Fatalf("len(Identities) = %d, want 1", len(s.Identities))
	}
	if s.Identities[0].DeviceID != "dev-1" || s.Identities[0].Token != "tok-1" {
		t.Errorf("identities[0] = %+v, want dev-1/tok-1", s.Identities[0])
	}
	if s.Identities[0].RegisteredAt != 42 {
		t.Errorf("identities[0].RegisteredAt = %d, want 42", s.Identities[0].RegisteredAt)
	}
	if s.Active != 0 {
		t.Errorf("Active = %d, want 0", s.Active)
	}
	if s.Keys == nil {
		t.Error("Keys = nil, want non-nil map")
	}
	if s.Warp == nil || s.Warp.DeviceID != "dev-1" {
		t.Errorf("legacy Warp mirror = %+v, want dev-1", s.Warp)
	}
	if w := m.GetWarp(); w == nil || w.DeviceID != "dev-1" {
		t.Errorf("GetWarp() = %+v, want dev-1", w)
	}
	// Egress/rotations preserved.
	if s.Mode != "warp" || s.Current != "warp" {
		t.Errorf("Mode/Current = %q/%q, want warp/warp", s.Mode, s.Current)
	}
	if s.Egress["direct"] == nil || s.Egress["direct"].OK != 5 || s.Egress["direct"].SpentUntil != 123 {
		t.Errorf("egress direct = %+v, want ok=5 spentUntil=123", s.Egress["direct"])
	}
	if s.Egress["warp"] == nil || s.Egress["warp"].OK != 9 {
		t.Errorf("egress warp = %+v, want ok=9", s.Egress["warp"])
	}
	if len(s.Rotations) != 1 || s.Rotations[0].Reason != "daily limit" {
		t.Errorf("Rotations = %+v, want 1 entry reason=daily limit", s.Rotations)
	}

	// Migration must be persisted immediately: the file on disk is v2.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	var onDisk State
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal migrated file: %v", err)
	}
	if onDisk.Version != 2 {
		t.Errorf("on-disk Version = %d, want 2", onDisk.Version)
	}
	if len(onDisk.Identities) != 1 || onDisk.Identities[0].DeviceID != "dev-1" {
		t.Errorf("on-disk identities = %+v, want 1x dev-1", onDisk.Identities)
	}
	if onDisk.Egress["direct"] == nil || onDisk.Egress["direct"].OK != 5 {
		t.Errorf("on-disk egress direct not preserved: %+v", onDisk.Egress["direct"])
	}
	if len(onDisk.Rotations) != 1 {
		t.Errorf("on-disk rotations = %d, want 1", len(onDisk.Rotations))
	}
}

// TestOpenKeepsExistingV2: a v2 file must load without being rewritten.
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
		t.Error("Open rewrote an already-v2 state file; want no rewrite")
	}

	if ks := m.KeyStats("public"); ks.OK != 3 || ks.Daily429 != 1 {
		t.Errorf("KeyStats(public) = %+v, want ok=3 daily429=1", ks)
	}
	if act := m.ActiveIdentity(); act == nil || act.DeviceID != "dev-x" {
		t.Errorf("ActiveIdentity() = %+v, want dev-x", act)
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

func TestActiveIdentitySwap(t *testing.T) {
	m, path := openTest(t)
	m.AddIdentity(&WarpIdentity{DeviceID: "a", Token: "ta", PrivateKey: "pa", PublicKey: "qa"})
	m.AddIdentity(&WarpIdentity{DeviceID: "b", Token: "tb", PrivateKey: "pb", PublicKey: "qb"})

	if act := m.ActiveIdentity(); act == nil || act.DeviceID != "a" {
		t.Fatalf("initial ActiveIdentity() = %+v, want a", act)
	}
	if err := m.SetActiveIdentity(1); err != nil {
		t.Fatalf("SetActiveIdentity(1): %v", err)
	}
	if act := m.ActiveIdentity(); act == nil || act.DeviceID != "b" {
		t.Errorf("ActiveIdentity() after swap = %+v, want b", act)
	}
	if w := m.GetWarp(); w == nil || w.DeviceID != "b" {
		t.Errorf("GetWarp() after swap = %+v, want b", w)
	}
	if err := m.SetActiveIdentity(5); err == nil {
		t.Error("SetActiveIdentity(5) = nil error, want out-of-range error")
	}
	if id := m.Identity(0); id == nil || id.DeviceID != "a" {
		t.Errorf("Identity(0) = %+v, want a", id)
	}
	if id := m.Identity(9); id != nil {
		t.Errorf("Identity(9) = %+v, want nil", id)
	}

	// Swap persists.
	m2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if act := m2.ActiveIdentity(); act == nil || act.DeviceID != "b" {
		t.Errorf("reloaded ActiveIdentity() = %+v, want b", act)
	}
}

// TestExistingAccessorsCompileWork pins the pre-v2 accessor signatures so
// existing callers (internal/router, cmd/zen-router, internal/cli) keep working.
func TestExistingAccessorsCompileWork(t *testing.T) {
	m, _ := openTest(t)

	// Mode.
	if m.Mode() != "auto" {
		t.Errorf("initial Mode() = %q, want auto", m.Mode())
	}
	if err := m.SetMode("direct"); err != nil {
		t.Errorf("SetMode(direct): %v", err)
	}
	if m.Mode() != "direct" {
		t.Errorf("Mode() = %q, want direct", m.Mode())
	}
	if err := m.SetMode("bogus"); err == nil {
		t.Error("SetMode(bogus) = nil, want error")
	}

	// Current.
	if m.Current() != "direct" {
		t.Errorf("initial Current() = %q, want direct", m.Current())
	}
	m.SetCurrent("warp")
	if m.Current() != "warp" {
		t.Errorf("Current() = %q, want warp", m.Current())
	}

	// Daily429 / IsSpent / RecordSuccess.
	m.RecordDaily429("direct", time.Now().Add(-time.Hour)) // past reset: not spent
	if m.IsSpent("direct") {
		t.Error("IsSpent(direct) after past resetAt = true, want false")
	}
	m.RecordDaily429("direct", time.Now().Add(time.Hour))
	if !m.IsSpent("direct") {
		t.Error("IsSpent(direct) after future resetAt = false, want true")
	}
	m.RecordSuccess("direct")
	if m.IsSpent("direct") {
		t.Error("IsSpent(direct) after RecordSuccess = true, want false (spent cleared)")
	}

	// Legacy warp accessors atop the identity pool.
	if m.GetWarp() != nil {
		t.Error("GetWarp() on empty pool != nil, want nil")
	}
	m.SetWarp(WarpIdentity{DeviceID: "legacy", Token: "t", PrivateKey: "p", PublicKey: "q"})
	if w := m.GetWarp(); w == nil || w.DeviceID != "legacy" {
		t.Errorf("GetWarp() = %+v, want legacy", w)
	}
	m.SetWarp(WarpIdentity{DeviceID: "legacy2", Token: "t", PrivateKey: "p", PublicKey: "q"})
	if w := m.GetWarp(); w == nil || w.DeviceID != "legacy2" {
		t.Errorf("GetWarp() after replace = %+v, want legacy2 (SetWarp replaces active)", w)
	}
	if n := len(m.Identities()); n != 1 {
		t.Errorf("len(Identities()) after two SetWarp = %d, want 1", n)
	}
	m.ClearWarp()
	if m.GetWarp() != nil {
		t.Error("GetWarp() after ClearWarp != nil, want nil")
	}

	// Snapshot carries the legacy fields cmd/zen-router reads.
	snap := m.Snapshot()
	if snap.UpdatedAt == 0 {
		t.Error("Snapshot().UpdatedAt = 0, want set")
	}
	if snap.Egress["direct"] == nil || snap.Egress["warp"] == nil {
		t.Errorf("Snapshot().Egress missing defaults: %+v", snap.Egress)
	}

	// Rotation cap at 50.
	for i := 0; i < 55; i++ {
		m.RecordRotation("direct", "warp", "r")
	}
	if n := len(m.Snapshot().Rotations); n != 50 {
		t.Errorf("len(Rotations) = %d, want 50", n)
	}

	// NextReset: next midnight UTC.
	got := NextReset(time.Date(2026, 10, 6, 12, 30, 0, 0, time.UTC))
	want := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("NextReset = %v, want %v", got, want)
	}
}
