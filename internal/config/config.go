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
// XDG base directory specification requires.
func DefaultPaths() Paths {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(userHomeDir(), ".config")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(userHomeDir(), ".local", "state")
	}
	stateDir := filepath.Join(stateHome, "zen-router")
	return Paths{
		ConfigDir: filepath.Join(configHome, "zen-router"),
		StateDir:  stateDir,
		StateFile: filepath.Join(stateDir, "state.json"),
		LogFile:   filepath.Join(stateDir, "zen.log"),
	}
}

// StateFile returns the effective state file path: $ZEN_ROUTER_STATE when set,
// otherwise the XDG default from DefaultPaths. This is the single source of
// truth for the state location; internal/quota delegates here.
func StateFile() string {
	if v := os.Getenv("ZEN_ROUTER_STATE"); v != "" {
		return v
	}
	return DefaultPaths().StateFile
}

// LegacyStatePath is the pre-XDG state location:
// $DSH_HOME/state/zen-router/state.json with DSH_HOME defaulting to ~/.dsh.
// It is only ever read by MigrateLegacyState.
func LegacyStatePath() string {
	home := os.Getenv("DSH_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".dsh")
		}
	}
	return filepath.Join(home, "state", "zen-router", "state.json")
}

// Config is the daemon configuration loaded from config.json.
type Config struct {
	Listen               string        `json:"listen"`
	Upstream             string        `json:"upstream"`
	KeyPoolFile          string        `json:"keyPoolFile"` // "" = derive later
	PoolSize             int           `json:"poolSize"`
	PoolSpare            int           `json:"poolSpare"`
	RotationCooldown     time.Duration `json:"rotationCooldown"`
	FirstEventTimeout    time.Duration `json:"firstEventTimeout"`
	IdleTimeout          time.Duration `json:"idleTimeout"`
	ResponsesIdleTimeout time.Duration `json:"responsesIdleTimeout"`
}

// Default returns the built-in configuration defaults.
func Default() *Config {
	return &Config{
		Listen:               "127.0.0.1:8787",
		Upstream:             "https://opencode.ai",
		KeyPoolFile:          "",
		PoolSize:             4,
		PoolSpare:            1,
		RotationCooldown:     30 * time.Second,
		FirstEventTimeout:    30 * time.Second,
		IdleTimeout:          120 * time.Second,
		ResponsesIdleTimeout: 300 * time.Second,
	}
}

// rawConfig mirrors config.json; a nil field keeps the built-in default.
// Durations are Go duration strings ("30s").
type rawConfig struct {
	Listen               *string `json:"listen"`
	Upstream             *string `json:"upstream"`
	KeyPoolFile          *string `json:"keyPoolFile"`
	PoolSize             *int    `json:"poolSize"`
	PoolSpare            *int    `json:"poolSpare"`
	RotationCooldown     *string `json:"rotationCooldown"`
	FirstEventTimeout    *string `json:"firstEventTimeout"`
	IdleTimeout          *string `json:"idleTimeout"`
	ResponsesIdleTimeout *string `json:"responsesIdleTimeout"`
}

func (r *rawConfig) apply(cfg *Config) error {
	if r.Listen != nil {
		cfg.Listen = *r.Listen
	}
	if r.Upstream != nil {
		cfg.Upstream = *r.Upstream
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
// yields an error. Environment overrides are applied last and win over the
// file: ZEN_ROUTER_LISTEN → Listen, ZEN_ROUTER_UPSTREAM → Upstream,
// ZEN_ROUTER_KEY_POOL_FILE → KeyPoolFile. ZEN_ROUTER_STATE does not appear in
// Config; it is consumed through StateFile.
func Load() (*Config, error) {
	cfg := Default()
	path := filepath.Join(DefaultPaths().ConfigDir, "config.json")
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
	return cfg, nil
}

// MigrateLegacyState performs the one-shot copy of the pre-XDG state file
// (LegacyStatePath) into paths.StateFile, creating the parent directory. It
// reports whether a copy happened: false with a nil error when the legacy file
// is absent or the destination already exists — the destination is never
// overwritten. The legacy original is left in place.
func MigrateLegacyState(paths Paths) (bool, error) {
	src := LegacyStatePath()
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
	if err := os.MkdirAll(filepath.Dir(paths.StateFile), 0o755); err != nil {
		return false, fmt.Errorf("create state dir: %w", err)
	}
	if err := os.WriteFile(paths.StateFile, data, 0o644); err != nil {
		return false, fmt.Errorf("write state file %s: %w", paths.StateFile, err)
	}
	return true, nil
}

func userHomeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}
