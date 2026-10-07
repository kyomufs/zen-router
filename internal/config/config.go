// Package config resolves the daemon's filesystem locations (XDG base dirs),
// loads config.json with ZEN_ROUTER_* environment overrides, and migrates the
// legacy DSH state file into the XDG state directory once.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Paths holds every filesystem location the daemon uses. StateFile is the
// default state location; ZEN_ROUTER_STATE overrides it at runtime and is
// read through StateFile (consumed by internal/quota).
type Paths struct {
	ConfigDir string
	StateDir  string
	StateFile string
	LogFile   string
}

// DefaultPaths resolves the XDG base directories: $XDG_CONFIG_HOME/zen-router
// and $XDG_STATE_HOME/zen-router, falling back to ~/.config/zen-router and
// ~/.local/state/zen-router when the environment variables are empty, as the
// XDG base directory specification requires. An error means neither the XDG
// vars nor a home directory could be resolved — there is no silent fallback
// to relative paths.
func DefaultPaths() (Paths, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	stateHome := os.Getenv("XDG_STATE_HOME")
	if configHome == "" || stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve home directory (set HOME or the XDG_* vars): %w", err)
		}
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		if stateHome == "" {
			stateHome = filepath.Join(home, ".local", "state")
		}
	}
	stateDir := filepath.Join(stateHome, "zen-router")
	return Paths{
		ConfigDir: filepath.Join(configHome, "zen-router"),
		StateDir:  stateDir,
		StateFile: filepath.Join(stateDir, "state.json"),
		LogFile:   filepath.Join(stateDir, "zen.log"),
	}, nil
}

// StateFile returns the effective state file path: $ZEN_ROUTER_STATE when set,
// otherwise the XDG default from DefaultPaths. This is the single source of
// truth for the state location; internal/quota delegates here.
func StateFile() (string, error) {
	if v := os.Getenv("ZEN_ROUTER_STATE"); v != "" {
		return v, nil
	}
	paths, err := DefaultPaths()
	if err != nil {
		return "", err
	}
	return paths.StateFile, nil
}

// LegacyStatePath is the pre-XDG state location:
// $DSH_HOME/state/zen-router/state.json with DSH_HOME defaulting to ~/.dsh.
// It is only ever read by MigrateLegacyState. An error means neither DSH_HOME
// nor a home directory could be resolved.
func LegacyStatePath() (string, error) {
	home := os.Getenv("DSH_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory for legacy state (set DSH_HOME or HOME): %w", err)
		}
		home = filepath.Join(h, ".dsh")
	}
	return filepath.Join(home, "state", "zen-router", "state.json"), nil
}

// Config is the in-memory daemon configuration. config.json is parsed through
// rawConfig — duration fields on disk are Go duration strings ("30s") — so
// Config itself is NOT the on-disk shape: do not marshal Config back to disk;
// rawConfig governs the file format.
type Config struct {
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
	// Family is the address family for direct egress dialing. Accepted values
	// are exactly "auto", "v4" and "v6" (input is case-insensitive, stored
	// lowercase; no "4"/"6" aliases); anything else fails Load. Override:
	// ZEN_ROUTER_FAMILY.
	Family               string        `json:"family"`
	KeyPoolFile          string        `json:"keyPoolFile"` // "" = derive later
	PoolSize             int           `json:"poolSize"`
	PoolSpare            int           `json:"poolSpare"`
	RotationCooldown     time.Duration `json:"rotationCooldown"`
	FirstEventTimeout    time.Duration `json:"firstEventTimeout"`
	IdleTimeout          time.Duration `json:"idleTimeout"`
	ResponsesIdleTimeout time.Duration `json:"responsesIdleTimeout"`
	// EgressIPEcho enables the live egress-IP echo (plan Task 2): when true
	// the daemon GETs an IP-echo endpoint through the active transport to
	// populate status.egress_ip. Default false — §12 gate: the live call
	// exists only after explicit user opt-in, never by default.
	EgressIPEcho bool `json:"egressIPEcho"`
}

// Default returns the built-in configuration defaults.
func Default() *Config {
	return &Config{
		Listen:               "127.0.0.1:8787",
		Upstream:             "https://opencode.ai",
		Family:               "auto",
		KeyPoolFile:          "",
		PoolSize:             4,
		PoolSpare:            1,
		RotationCooldown:     30 * time.Second,
		FirstEventTimeout:    30 * time.Second,
		IdleTimeout:          120 * time.Second,
		ResponsesIdleTimeout: 300 * time.Second,
	}
}

// normalizeFamily validates a family value: exactly auto, v4 or v6,
// case-insensitive on input, stored lowercase. No 4/6 aliases — the surface
// stays small and explicit.
func normalizeFamily(v string) (string, error) {
	switch strings.ToLower(v) {
	case "auto":
		return "auto", nil
	case "v4":
		return "v4", nil
	case "v6":
		return "v6", nil
	default:
		return "", fmt.Errorf("invalid family %q (want auto|v4|v6)", v)
	}
}

// rawConfig mirrors config.json; a nil field keeps the built-in default.
// Durations are Go duration strings ("30s"). This — not Config — is the
// on-disk shape.
type rawConfig struct {
	Listen               *string `json:"listen"`
	Upstream             *string `json:"upstream"`
	Family               *string `json:"family"`
	KeyPoolFile          *string `json:"keyPoolFile"`
	PoolSize             *int    `json:"poolSize"`
	PoolSpare            *int    `json:"poolSpare"`
	RotationCooldown     *string `json:"rotationCooldown"`
	FirstEventTimeout    *string `json:"firstEventTimeout"`
	IdleTimeout          *string `json:"idleTimeout"`
	ResponsesIdleTimeout *string `json:"responsesIdleTimeout"`
	EgressIPEcho         *bool   `json:"egressIPEcho"`
}

func (r *rawConfig) apply(cfg *Config) error {
	if r.Listen != nil {
		cfg.Listen = *r.Listen
	}
	if r.Upstream != nil {
		cfg.Upstream = *r.Upstream
	}
	if r.Family != nil {
		family, err := normalizeFamily(*r.Family)
		if err != nil {
			return err
		}
		cfg.Family = family
	}
	if r.KeyPoolFile != nil {
		cfg.KeyPoolFile = *r.KeyPoolFile
	}
	if r.PoolSize != nil {
		cfg.PoolSize = *r.PoolSize
	}
	if r.PoolSpare != nil {
		cfg.PoolSpare = *r.PoolSpare
	}
	if r.EgressIPEcho != nil {
		cfg.EgressIPEcho = *r.EgressIPEcho
	}
	durations := []struct {
		name string
		src  *string
		dst  *time.Duration
	}{
		{"rotationCooldown", r.RotationCooldown, &cfg.RotationCooldown},
		{"firstEventTimeout", r.FirstEventTimeout, &cfg.FirstEventTimeout},
		{"idleTimeout", r.IdleTimeout, &cfg.IdleTimeout},
		{"responsesIdleTimeout", r.ResponsesIdleTimeout, &cfg.ResponsesIdleTimeout},
	}
	for _, d := range durations {
		if d.src == nil {
			continue
		}
		v, err := time.ParseDuration(*d.src)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", d.name, *d.src, err)
		}
		*d.dst = v
	}
	return nil
}

// Load reads <ConfigDir>/config.json on top of the defaults from Default.
// A missing or empty file yields the defaults without error; malformed JSON
// or an invalid value yields an error. Environment overrides are applied last
// and win over the file: ZEN_ROUTER_LISTEN → Listen, ZEN_ROUTER_UPSTREAM →
// Upstream, ZEN_ROUTER_KEY_POOL_FILE → KeyPoolFile, ZEN_ROUTER_FAMILY →
// Family (validated like the file value). ZEN_ROUTER_STATE does not appear in
// Config; it is consumed through StateFile. Env overrides apply even when
// config.json does not exist.
func Load() (*Config, error) {
	cfg := Default()
	paths, err := DefaultPaths()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(paths.ConfigDir, "config.json")
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			var raw rawConfig
			if unerr := json.Unmarshal(data, &raw); unerr != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, unerr)
			}
			if unerr := raw.apply(cfg); unerr != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, unerr)
			}
		}
	case os.IsNotExist(err):
		// Missing file: keep the defaults, env overrides still apply below.
	default:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if v := os.Getenv("ZEN_ROUTER_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("ZEN_ROUTER_UPSTREAM"); v != "" {
		cfg.Upstream = v
	}
	if v := os.Getenv("ZEN_ROUTER_KEY_POOL_FILE"); v != "" {
		cfg.KeyPoolFile = v
	}
	if v := os.Getenv("ZEN_ROUTER_FAMILY"); v != "" {
		family, ferr := normalizeFamily(v)
		if ferr != nil {
			return nil, fmt.Errorf("ZEN_ROUTER_FAMILY: %w", ferr)
		}
		cfg.Family = family
	}
	return cfg, nil
}

// MigrateLegacyState performs the one-shot copy of the pre-XDG state file
// (LegacyStatePath) into paths.StateFile, creating the parent directory. The
// destination is written atomically (temp file in the same directory, then
// rename). It reports whether a copy happened: false with a nil error when the
// legacy file is absent or the destination already exists — the destination is
// never overwritten (existence is checked before the rename; a race between
// check and rename is accepted). The legacy original is left in place.
func MigrateLegacyState(paths Paths) (bool, error) {
	src, err := LegacyStatePath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat legacy state %s: %w", src, err)
	}
	if _, err := os.Stat(paths.StateFile); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat state file %s: %w", paths.StateFile, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false, fmt.Errorf("read legacy state %s: %w", src, err)
	}
	dir := filepath.Dir(paths.StateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName) // no-op once the rename succeeded
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write temp state file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close temp state file %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return false, fmt.Errorf("chmod temp state file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, paths.StateFile); err != nil {
		return false, fmt.Errorf("rename state file %s: %w", paths.StateFile, err)
	}
	tmpName = "" // committed; skip the deferred cleanup
	return true, nil
}
