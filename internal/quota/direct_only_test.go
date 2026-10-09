package quota

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyStateJSON returns a real pre-excision state file (schema v2) carrying
// the WARP identity pool, mode/current and rotation history. Direct-only
// zen-router must load it and drop every warp-related field on the next save.
// spentUntil is computed relative to now so the quota gate stays meaningful
// regardless of when the test runs.
func legacyStateJSON() string {
	spentUntil := time.Now().Add(time.Hour).UnixMilli()
	return fmt.Sprintf(`{
  "version": 2,
  "mode": "warp",
  "current": "warp",
  "updatedAt": 1791499322139,
  "egress": {
    "direct": {"ok": 862, "daily429": 22, "spentUntil": %[1]d},
    "warp": {"ok": 3, "daily429": 29, "spentUntil": %[1]d}
  },
  "keys": {
    "public": {"ok": 861, "daily429": 27, "spentUntil": %[1]d}
  },
  "warp": {"deviceId": "77b5f5d6-0000-0000-0000-000000000000", "publicKey": "abc"},
  "identities": [
    {"deviceId": "b9b61422-0000-0000-0000-000000000000", "publicKey": "def", "registeredAt": 1791498895255}
  ],
  "active": 0,
  "rotations": [
    {"at": 1791499279394, "from": "direct", "to": "warp", "reason": "daily limit"}
  ]
}
`, spentUntil)
}

func writeLegacyState(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// TestOpenLegacyWarpStateDropsWarpFieldsOnSave pins the migration contract:
// a pre-excision state file must load without error, and the very next save
// must rewrite it WITHOUT mode/current/warp/identities/rotations keys.
func TestOpenLegacyWarpStateDropsWarpFieldsOnSave(t *testing.T) {
	path := writeLegacyState(t, legacyStateJSON())

	m, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy state: %v", err)
	}
	// Trigger a save through a production path.
	m.RecordSuccess("direct")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state after save: %v", err)
	}
	saved := string(data)
	for _, banned := range []string{`"mode"`, `"current"`, `"warp"`, `"identities"`, `"rotations"`, `"active"`} {
		if strings.Contains(saved, banned) {
			t.Errorf("saved state still contains %s after excision save:\n%s", banned, saved)
		}
	}
	// The direct counters and the key pool survive the excision.
	if !strings.Contains(saved, `"direct"`) {
		t.Errorf("direct egress counters dropped:\n%s", saved)
	}
	if !strings.Contains(saved, `"public"`) {
		t.Errorf("key stats dropped:\n%s", saved)
	}
}

// TestOpenSeedsDirectEgressOnly pins that the egress seed lane set contains
// exactly "direct" — the warp lane must never reappear.
func TestOpenSeedsDirectEgressOnly(t *testing.T) {
	m, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("Open empty state: %v", err)
	}
	eg := m.Snapshot().Egress
	if eg["direct"] == nil {
		t.Fatalf("direct lane missing from egress seed: %#v", eg)
	}
	if _, ok := eg["warp"]; ok {
		t.Errorf("warp lane present in egress seed: %#v", eg)
	}
}

// TestLegacyStateKeepsKeysAndDirectStats ensures the excision loses nothing
// but the warp machinery: per-key and direct egress counters carry over.
func TestLegacyStateKeepsKeysAndDirectStats(t *testing.T) {
	path := writeLegacyState(t, legacyStateJSON())
	m, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy state: %v", err)
	}
	st := m.Snapshot()
	if got := st.Egress["direct"]; got == nil || got.OK != 862 || got.Daily429 != 22 {
		t.Errorf("direct stats lost: %#v", got)
	}
	if k := m.KeyStats("public"); k.OK != 861 || k.Daily429 != 27 {
		t.Errorf("key stats lost: %#v", k)
	}
	// spentUntil must survive — it drives the quota gate.
	if !m.IsSpent("direct") {
		t.Errorf("direct spentUntil lost")
	}
}
