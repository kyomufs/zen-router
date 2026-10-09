package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"zen-router/internal/config"
)

// State file layout ($XDG_STATE_HOME/zen-router/state.json; the legacy
// ~/.dsh/state location is migrated once by config.MigrateLegacyState).
// This is deliberately separate from the plugin's per-family quota.json: the
// router tracks which egress (direct-only) is spent and per-API-key counters
// (Keys). It reads the plugin's quota.json only as an advisory signal.
// Pre-excision files carrying mode/current/warp/identities/rotations load
// normally — those fields are unknown now and are dropped on the next save.
type State struct {
	Version   int   `json:"version"`
	UpdatedAt int64 `json:"updatedAt"`

	Egress map[string]*EgressStats `json:"egress"`

	// Keys counts observed outcomes per upstream API key.
	Keys map[string]*KeyStats `json:"keys,omitempty"`
}

// KeyStats counts observed outcomes for one upstream API key.
type KeyStats struct {
	OK         int64 `json:"ok"`
	Daily429   int64 `json:"daily429"`
	Last429At  int64 `json:"last429At,omitempty"`
	SpentUntil int64 `json:"spentUntil,omitempty"` // 0 = not spent
}

// EgressStats counts observed outcomes for one egress path.
type EgressStats struct {
	OK         int64 `json:"ok"`
	Daily429   int64 `json:"daily429"`
	LastOKAt   int64 `json:"lastOkAt,omitempty"`
	Last429At  int64 `json:"last429At,omitempty"`
	SpentUntil int64 `json:"spentUntil,omitempty"` // 0 = not spent
}

// Manager owns the persisted state with a mutex; every mutation saves.
type Manager struct {
	mu   sync.Mutex
	path string
	s    *State
}

// DefaultPath is where the router keeps its state: $ZEN_ROUTER_STATE when set,
// otherwise the XDG default owned by internal/config (single source of truth;
// config never imports quota, so there is no cycle). An error means no home
// directory could be resolved for the XDG fallback.
func DefaultPath() (string, error) {
	return config.StateFile()
}

func emptyState() *State {
	return &State{
		Version: 2,
		Egress: map[string]*EgressStats{
			"direct": {},
		},
		Keys: map[string]*KeyStats{},
	}
}

// Open loads (or initialises) the state file at path. Pre-excision files
// (mode/current/warp/identities/rotations) load as-is — the unknown fields
// are ignored and disappear on the next save.
func Open(path string) (*Manager, error) {
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, fmt.Errorf("resolve state path: %w", err)
		}
		path = p
	}
	m := &Manager{path: path, s: emptyState()}
	data, err := os.ReadFile(path)
	if err == nil {
		var parsed State
		if jsonErr := json.Unmarshal(data, &parsed); jsonErr == nil && parsed.Version != 0 {
			// Direct-only lane set: drop every pre-excision lane (e.g. the
			// old warp lane) so the saved file stays direct-only.
			direct := &EgressStats{}
			if parsed.Egress != nil && parsed.Egress["direct"] != nil {
				direct = parsed.Egress["direct"]
			}
			parsed.Egress = map[string]*EgressStats{"direct": direct}
			if parsed.Keys == nil {
				parsed.Keys = map[string]*KeyStats{}
			}
			m.s = &parsed
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	return m, nil
}

// Path returns the backing file location.
func (m *Manager) Path() string { return m.path }

func (m *Manager) saveLocked() {
	m.s.Version = 2
	m.s.UpdatedAt = time.Now().UnixMilli()
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(m.s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(m.path, append(data, '\n'), 0o644)
}

// Snapshot returns a copy of the current state for read-only commands.
func (m *Manager) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *m.s
	cp.Egress = map[string]*EgressStats{}
	for k, v := range m.s.Egress {
		vv := *v
		cp.Egress[k] = &vv
	}
	cp.Keys = map[string]*KeyStats{}
	for k, v := range m.s.Keys {
		if v == nil {
			continue
		}
		vv := *v
		cp.Keys[k] = &vv
	}
	return cp
}

// RecordSuccess increments the success counter for an egress and clears spent.
func (m *Manager) RecordSuccess(egress string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.egressLocked(egress)
	b.OK++
	b.LastOKAt = time.Now().UnixMilli()
	b.SpentUntil = 0
	m.saveLocked()
}

// RecordDaily429 increments the daily-limit counter and, when resetAt is in
// the future, marks the egress as spent until then.
func (m *Manager) RecordDaily429(egress string, resetAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.egressLocked(egress)
	b.Daily429++
	b.Last429At = time.Now().UnixMilli()
	if resetAt.After(time.Now()) {
		b.SpentUntil = resetAt.UnixMilli()
	}
	m.saveLocked()
}

// IsSpent reports whether the egress is marked spent (its daily window has not
// rolled over yet).
func (m *Manager) IsSpent(egress string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.egressLocked(egress)
	return b.SpentUntil > time.Now().UnixMilli()
}

func (m *Manager) egressLocked(name string) *EgressStats {
	if m.s.Egress[name] == nil {
		m.s.Egress[name] = &EgressStats{}
	}
	return m.s.Egress[name]
}

// RecordKeyDaily429 increments the key's daily-limit counter and stamps
// Last429At with now.
func (m *Manager) RecordKeyDaily429(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.keyStatsLocked(key)
	k.Daily429++
	k.Last429At = time.Now().UnixMilli()
	m.saveLocked()
}

// RecordKeySuccess increments the key's success counter.
func (m *Manager) RecordKeySuccess(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyStatsLocked(key).OK++
	m.saveLocked()
}

// RecordRequestSuccess increments BOTH success counters for one 2xx request —
// per egress and (when key != "") per API key — under a single lock and a
// single state.json save. The gateway path used to walk RecordSuccess and
// RecordKeySuccess in sequence, writing the file twice per request
// (review Task 1, finding F4). Existing single-purpose methods stay for
// their other callers.
func (m *Manager) RecordRequestSuccess(egress, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.egressLocked(egress)
	b.OK++
	b.LastOKAt = time.Now().UnixMilli()
	b.SpentUntil = 0
	if key != "" {
		m.keyStatsLocked(key).OK++
	}
	m.saveLocked()
}

// KeyStats returns a value copy of the key's counters; unknown keys read as
// the zero value.
func (m *Manager) KeyStats(key string) KeyStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s.Keys == nil {
		return KeyStats{}
	}
	k := m.s.Keys[key]
	if k == nil {
		return KeyStats{}
	}
	return *k
}

// SetKeySpent marks the key as spent until the given time (a past time means
// not spent).
func (m *Manager) SetKeySpent(key string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keyStatsLocked(key).SpentUntil = until.UnixMilli()
	m.saveLocked()
}

func (m *Manager) keyStatsLocked(key string) *KeyStats {
	if m.s.Keys == nil {
		m.s.Keys = map[string]*KeyStats{}
	}
	if m.s.Keys[key] == nil {
		m.s.Keys[key] = &KeyStats{}
	}
	return m.s.Keys[key]
}

// NextReset returns the next midnight-UTC bucket rollover (matches the plugin).
func NextReset(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}
