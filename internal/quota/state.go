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
// router tracks which
// egress (direct/warp) is spent, plus the WARP device identity used for
// rotation. It reads the plugin's quota.json only as an advisory signal.
// Since schema v2 it also tracks per-API-key counters (Keys) and a pool of
// WARP identities with per-identity health (Identities/Active); v1 files are
// migrated to v2 on load.
type State struct {
	Version   int    `json:"version"`
	Mode      string `json:"mode"` // auto | direct | warp
	Current   string `json:"current"`
	UpdatedAt int64  `json:"updatedAt"`

	Egress map[string]*EgressStats `json:"egress"`

	// Keys counts observed outcomes per upstream API key (v2).
	Keys map[string]*KeyStats `json:"keys,omitempty"`

	// Warp is the legacy single-identity view, kept as a mirror of
	// Identities[Active] so pre-v2 readers (cmd/zen-router printEgress) and
	// downgrade paths keep working. Never edit it directly: mutate Identities.
	Warp *WarpIdentity `json:"warp,omitempty"`

	// Identities is the pool of registered WARP devices (v2); Active selects
	// the one currently serving the tunnel.
	Identities []*WarpIdentity `json:"identities,omitempty"`
	Active     int             `json:"active"`

	Rotations []Rotation `json:"rotations"`
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

// WarpIdentity is the registered WARP device.
type WarpIdentity struct {
	DeviceID string `json:"deviceId"`
	// Token and PrivateKey are credentials: omitempty keeps them out of
	// JSON when empty, so the control layer's Redacted() view omits the
	// fields entirely. Registered identities always carry real values, so
	// state.json on disk keeps full fidelity.
	Token        string `json:"token,omitempty"`
	License      string `json:"license,omitempty"`
	PrivateKey   string `json:"privateKey,omitempty"`
	PublicKey    string `json:"publicKey"`
	AddressV4    string `json:"addressV4,omitempty"`
	AddressV6    string `json:"addressV6,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	ServerPub    string `json:"serverPub,omitempty"`
	RegisteredAt int64  `json:"registeredAt"`

	// Per-identity health (v2), mirroring EgressStats so the rotation logic
	// can skip identities whose daily window has not rolled over.
	SpentUntil int64 `json:"spentUntil,omitempty"` // 0 = not spent
	Last429At  int64 `json:"last429At,omitempty"`
}

// Rotation is one recorded IP-rotation event.
type Rotation struct {
	At     int64  `json:"at"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Redacted returns the control-layer view of the identity with the
// credential fields (Token, PrivateKey) stripped. The control API's
// /_zenctl/status serves this copy ONLY — state.json on disk keeps the
// full-fidelity struct. With omitempty on those fields the redacted JSON
// payload contains neither the field names nor the values.
func (id WarpIdentity) Redacted() WarpIdentity {
	id.Token = ""
	id.PrivateKey = ""
	return id
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
		Mode:    "auto",
		Current: "direct",
		Egress: map[string]*EgressStats{
			"direct": {},
			"warp":   {},
		},
		Keys: map[string]*KeyStats{},
	}
}

// Open loads (or initialises) the state file at path. A v1 file is migrated
// to v2 (single `warp` identity moves into `identities[0]`) and the upgraded
// file is written back once.
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
			// Drop null identity slots from hand-edited/corrupt JSON first, so
			// Active always indexes a real entry. In-memory only: the file is
			// rewritten on the next save (or now, when migrating).
			if len(parsed.Identities) > 0 {
				// DM-5: did Active itself point at a slot being filtered out?
				// After filtering, the first valid identity sits at index 0,
				// so a null-targeted Active remaps there; an all-null pool
				// keeps the documented empty state (mirrorWarpLocked clamp:
				// Active 0, Warp nil). Active values that survive the filter
				// are untouched — out-of-range ones remain clamp-covered by
				// mirrorWarpLocked.
				activeWasNull := parsed.Active >= 0 && parsed.Active < len(parsed.Identities) &&
					parsed.Identities[parsed.Active] == nil
				kept := parsed.Identities[:0]
				for _, id := range parsed.Identities {
					if id != nil {
						kept = append(kept, id)
					}
				}
				parsed.Identities = kept
				if activeWasNull && len(kept) > 0 {
					parsed.Active = 0
				}
			}
			migrated := false
			if parsed.Version == 1 || (parsed.Warp != nil && len(parsed.Identities) == 0) {
				if parsed.Warp != nil && len(parsed.Identities) == 0 {
					w := *parsed.Warp
					parsed.Identities = []*WarpIdentity{&w}
				}
				parsed.Active = 0
				parsed.Version = 2
				migrated = true
			}
			if parsed.Egress == nil {
				parsed.Egress = map[string]*EgressStats{}
			}
			for _, k := range []string{"direct", "warp"} {
				if parsed.Egress[k] == nil {
					parsed.Egress[k] = &EgressStats{}
				}
			}
			if parsed.Keys == nil {
				parsed.Keys = map[string]*KeyStats{}
			}
			m.s = &parsed
			m.mirrorWarpLocked()
			if migrated {
				// Persist the upgrade immediately so the on-disk file is v2.
				m.saveLocked()
			}
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
	m.mirrorWarpLocked()
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

// mirrorWarpLocked keeps the legacy Warp field equal to the active identity.
// A missing, out-of-range, or nil active entry means "no active identity":
// Active is clamped in memory and Warp stays nil — the same nil-guard
// behavior as Identity/activeIdentityLocked.
func (m *Manager) mirrorWarpLocked() {
	if m.s.Active < 0 || m.s.Active >= len(m.s.Identities) {
		m.s.Active = 0
	}
	if len(m.s.Identities) == 0 || m.s.Identities[m.s.Active] == nil {
		m.s.Warp = nil
		return
	}
	w := *m.s.Identities[m.s.Active]
	m.s.Warp = &w
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
	if m.s.Warp != nil {
		w := *m.s.Warp
		cp.Warp = &w
	}
	// Preserve slots (including any nil) so indices stay aligned with Active
	// in the copy.
	cp.Identities = make([]*WarpIdentity, len(m.s.Identities))
	for i, id := range m.s.Identities {
		if id == nil {
			continue
		}
		vv := *id
		cp.Identities[i] = &vv
	}
	cp.Rotations = append([]Rotation(nil), m.s.Rotations...)
	return cp
}

// Mode returns the configured routing mode.
func (m *Manager) Mode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s.Mode
}

// SetMode records the routing mode and persists it.
func (m *Manager) SetMode(mode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch mode {
	case "auto", "direct", "warp":
	default:
		return fmt.Errorf("unknown mode %q (want auto|direct|warp)", mode)
	}
	m.s.Mode = mode
	m.saveLocked()
	return nil
}

// Current returns the active egress name.
func (m *Manager) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s.Current
}

// SetCurrent switches the active egress and persists it.
func (m *Manager) SetCurrent(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Current = name
	m.saveLocked()
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

// SetWarp stores the WARP device identity as the active pool entry: it
// replaces the active identity, or seeds an empty pool (legacy single-identity
// semantics on top of Identities).
func (m *Manager) SetWarp(id WarpIdentity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := id
	if len(m.s.Identities) == 0 {
		m.s.Identities = []*WarpIdentity{&cp}
		m.s.Active = 0
	} else {
		m.s.Identities[m.activeLocked()] = &cp
	}
	m.saveLocked()
}

// GetWarp returns the active WARP identity, or nil when the pool is empty.
func (m *Manager) GetWarp() *WarpIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeIdentityLocked()
}

// ClearWarp forgets the active WARP identity (removes it from the pool).
func (m *Manager) ClearWarp() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.s.Identities) == 0 {
		return
	}
	i := m.activeLocked()
	m.s.Identities = append(m.s.Identities[:i], m.s.Identities[i+1:]...)
	if m.s.Active >= len(m.s.Identities) {
		m.s.Active = len(m.s.Identities) - 1
	}
	if m.s.Active < 0 {
		m.s.Active = 0
	}
	m.saveLocked()
}

// AddIdentity appends a WARP identity to the pool and returns its index, or
// -1 when id is nil (nothing is stored or saved). The first identity becomes
// active automatically (Active defaults to 0).
func (m *Manager) AddIdentity(id *WarpIdentity) int {
	if id == nil {
		return -1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *id
	m.s.Identities = append(m.s.Identities, &cp)
	m.saveLocked()
	return len(m.s.Identities) - 1
}

// Identity returns a copy of the pool entry at index i, or nil when out of
// range.
func (m *Manager) Identity(i int) *WarpIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.s.Identities) || m.s.Identities[i] == nil {
		return nil
	}
	cp := *m.s.Identities[i]
	return &cp
}

// ActiveIdentity returns a copy of the currently active identity, or nil when
// the pool is empty.
func (m *Manager) ActiveIdentity() *WarpIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeIdentityLocked()
}

// SetActiveIdentity switches the active pool entry and persists it.
func (m *Manager) SetActiveIdentity(i int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.s.Identities) {
		return fmt.Errorf("identity index %d out of range (have %d)", i, len(m.s.Identities))
	}
	m.s.Active = i
	m.saveLocked()
	return nil
}

// Identities returns value copies of every pool entry, preserving indices so
// out[i] corresponds to Identity(i) and SetActiveIdentity(i); a nil slot (not
// reachable after Open's load-time filtering) appears as the zero value.
func (m *Manager) Identities() []WarpIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WarpIdentity, len(m.s.Identities))
	for i, id := range m.s.Identities {
		if id == nil {
			continue
		}
		out[i] = *id
	}
	return out
}

func (m *Manager) activeLocked() int {
	if m.s.Active < 0 || m.s.Active >= len(m.s.Identities) {
		return 0
	}
	return m.s.Active
}

func (m *Manager) activeIdentityLocked() *WarpIdentity {
	if len(m.s.Identities) == 0 {
		return nil
	}
	id := m.s.Identities[m.activeLocked()]
	if id == nil {
		return nil
	}
	cp := *id
	return &cp
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

// RecordRotation appends a rotation event, keeping the last 50.
func (m *Manager) RecordRotation(from, to, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Rotations = append(m.s.Rotations, Rotation{
		At:     time.Now().UnixMilli(),
		From:   from,
		To:     to,
		Reason: reason,
	})
	if len(m.s.Rotations) > 50 {
		m.s.Rotations = m.s.Rotations[len(m.s.Rotations)-50:]
	}
	m.saveLocked()
}

// NextReset returns the next midnight-UTC bucket rollover (matches the plugin).
func NextReset(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}
