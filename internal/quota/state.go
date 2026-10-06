package quota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State file layout (~/.dsh/state/zen-router/state.json). This is deliberately
// separate from the plugin's per-family quota.json: the router tracks which
// egress (direct/warp) is spent, plus the WARP device identity used for
// rotation. It reads the plugin's quota.json only as an advisory signal.
type State struct {
	Version   int    `json:"version"`
	Mode      string `json:"mode"` // auto | direct | warp
	Current   string `json:"current"`
	UpdatedAt int64  `json:"updatedAt"`

	Egress map[string]*EgressStats `json:"egress"`

	// Warp device identity, persisted so a restart reuses the same tunnel
	// instead of minting a new device on every boot.
	Warp *WarpIdentity `json:"warp,omitempty"`

	Rotations []Rotation `json:"rotations"`
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
	DeviceID  string `json:"deviceId"`
	Token     string `json:"token"`
	License   string `json:"license,omitempty"`
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
	AddressV4 string `json:"addressV4,omitempty"`
	AddressV6 string `json:"addressV6,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	ServerPub string `json:"serverPub,omitempty"`
	RegisteredAt int64 `json:"registeredAt"`
}

// Rotation is one recorded IP-rotation event.
type Rotation struct {
	At     int64  `json:"at"`
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// Manager owns the persisted state with a mutex; every mutation saves.
type Manager struct {
	mu   sync.Mutex
	path string
	s    *State
}

// DefaultPath is where the router keeps its state.
func DefaultPath() string {
	if v := os.Getenv("ZEN_ROUTER_STATE"); v != "" {
		return v
	}
	home := os.Getenv("DSH_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".dsh")
		}
	}
	return filepath.Join(home, "state", "zen-router", "state.json")
}

func emptyState() *State {
	return &State{
		Version: 1,
		Mode:    "auto",
		Current: "direct",
		Egress: map[string]*EgressStats{
			"direct": {},
			"warp":   {},
		},
	}
}

// Open loads (or initialises) the state file at path.
func Open(path string) (*Manager, error) {
	if path == "" {
		path = DefaultPath()
	}
	m := &Manager{path: path, s: emptyState()}
	data, err := os.ReadFile(path)
	if err == nil {
		var parsed State
		if jsonErr := json.Unmarshal(data, &parsed); jsonErr == nil && parsed.Version != 0 {
			if parsed.Egress == nil {
				parsed.Egress = map[string]*EgressStats{}
			}
			for _, k := range []string{"direct", "warp"} {
				if parsed.Egress[k] == nil {
					parsed.Egress[k] = &EgressStats{}
				}
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
	if m.s.Warp != nil {
		w := *m.s.Warp
		cp.Warp = &w
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

// SetWarp stores the WARP device identity.
func (m *Manager) SetWarp(id WarpIdentity) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Warp = &id
	m.saveLocked()
}

// GetWarp returns the stored WARP identity, or nil.
func (m *Manager) GetWarp() *WarpIdentity {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.s.Warp == nil {
		return nil
	}
	cp := *m.s.Warp
	return &cp
}

// ClearWarp forgets the stored WARP identity.
func (m *Manager) ClearWarp() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.s.Warp = nil
	m.saveLocked()
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
