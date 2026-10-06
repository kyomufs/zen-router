package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"zen-router/internal/config"
	"zen-router/internal/quota"
)

// clearEnv pins every environment variable the config package reads so that
// ambient values (real XDG dirs, DSH_HOME, ZEN_ROUTER_* overrides) never leak
// into test expectations and no test can touch the real home directory.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"XDG_CONFIG_HOME",
		"XDG_STATE_HOME",
		"ZEN_ROUTER_LISTEN",
		"ZEN_ROUTER_UPSTREAM",
		"ZEN_ROUTER_KEY_POOL_FILE",
		"ZEN_ROUTER_STATE",
		"DSH_HOME",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", t.TempDir())
}

func TestDefaultPaths(t *testing.T) {
	t.Run("honors XDG_CONFIG_HOME and XDG_STATE_HOME", func(t *testing.T) {
		clearEnv(t)
		configHome := t.TempDir()
		stateHome := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", configHome)
		t.Setenv("XDG_STATE_HOME", stateHome)

		got := config.DefaultPaths()
		want := config.Paths{
			ConfigDir: filepath.Join(configHome, "zen-router"),
			StateDir:  filepath.Join(stateHome, "zen-router"),
			StateFile: filepath.Join(stateHome, "zen-router", "state.json"),
			LogFile:   filepath.Join(stateHome, "zen-router", "zen.log"),
		}
		if got != want {
			t.Errorf("DefaultPaths() = %+v, want %+v", got, want)
		}
	})

	t.Run("falls back to ~/.config and ~/.local/state when XDG vars are empty", func(t *testing.T) {
		clearEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)

		got := config.DefaultPaths()
		want := config.Paths{
			ConfigDir: filepath.Join(home, ".config", "zen-router"),
			StateDir:  filepath.Join(home, ".local", "state", "zen-router"),
			StateFile: filepath.Join(home, ".local", "state", "zen-router", "state.json"),
			LogFile:   filepath.Join(home, ".local", "state", "zen-router", "zen.log"),
		}
		if got != want {
			t.Errorf("DefaultPaths() = %+v, want %+v", got, want)
		}
	})
}

// writeConfig creates <XDG_CONFIG_HOME>/zen-router/config.json with content.
func writeConfig(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "zen-router")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

func TestConfigEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	writeConfig(t, `{
		"listen": "127.0.0.1:9999",
		"upstream": "https://file.example",
		"keyPoolFile": "/from/file/pool.json",
		"poolSize": 7,
		"idleTimeout": "60s"
	}`)

	t.Run("env overrides beat the file", func(t *testing.T) {
		t.Setenv("ZEN_ROUTER_LISTEN", "127.0.0.1:1234")
		t.Setenv("ZEN_ROUTER_KEY_POOL_FILE", "/from/env/pool.json")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Listen != "127.0.0.1:1234" {
			t.Errorf("Listen = %q, want ZEN_ROUTER_LISTEN value 127.0.0.1:1234", cfg.Listen)
		}
		if cfg.KeyPoolFile != "/from/env/pool.json" {
			t.Errorf("KeyPoolFile = %q, want ZEN_ROUTER_KEY_POOL_FILE value /from/env/pool.json", cfg.KeyPoolFile)
		}
		// Fields without env overrides still come from the file.
		if cfg.Upstream != "https://file.example" {
			t.Errorf("Upstream = %q, want file value https://file.example", cfg.Upstream)
		}
		if cfg.PoolSize != 7 {
			t.Errorf("PoolSize = %d, want file value 7", cfg.PoolSize)
		}
		if cfg.IdleTimeout != 60*time.Second {
			t.Errorf("IdleTimeout = %s, want file value 60s", cfg.IdleTimeout)
		}
	})

	t.Run("file wins when env overrides are empty", func(t *testing.T) {
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if cfg.Listen != "127.0.0.1:9999" {
			t.Errorf("Listen = %q, want file value 127.0.0.1:9999", cfg.Listen)
		}
		if cfg.KeyPoolFile != "/from/file/pool.json" {
			t.Errorf("KeyPoolFile = %q, want file value /from/file/pool.json", cfg.KeyPoolFile)
		}
	})
}

// Env overrides must apply even when config.json does not exist: a fresh
// install runs on defaults + environment alone.
func TestEnvOverridesWithoutConfigFile(t *testing.T) {
	clearEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no config.json
	t.Setenv("ZEN_ROUTER_LISTEN", "127.0.0.1:5555")
	t.Setenv("ZEN_ROUTER_UPSTREAM", "https://env.example")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:5555" {
		t.Errorf("Listen = %q, want ZEN_ROUTER_LISTEN value 127.0.0.1:5555", cfg.Listen)
	}
	if cfg.Upstream != "https://env.example" {
		t.Errorf("Upstream = %q, want ZEN_ROUTER_UPSTREAM value https://env.example", cfg.Upstream)
	}
	if cfg.PoolSize != 4 || cfg.IdleTimeout != 120*time.Second {
		t.Errorf("non-overridden fields = pool %d / idle %s, want defaults 4 / 120s", cfg.PoolSize, cfg.IdleTimeout)
	}
}

func TestLoadDefaults(t *testing.T) {
	want := *config.Default()

	t.Run("missing file yields all defaults", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no zen-router/config.json inside

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if *cfg != want {
			t.Errorf("Load() = %+v, want defaults %+v", *cfg, want)
		}
	})

	t.Run("empty file yields all defaults", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		writeConfig(t, "")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		if *cfg != want {
			t.Errorf("Load() = %+v, want defaults %+v", *cfg, want)
		}
	})

	t.Run("defaults match the documented values", func(t *testing.T) {
		d := config.Default()
		if d.Listen != "127.0.0.1:8787" {
			t.Errorf("default Listen = %q, want 127.0.0.1:8787", d.Listen)
		}
		if d.Upstream != "https://opencode.ai" {
			t.Errorf("default Upstream = %q, want https://opencode.ai", d.Upstream)
		}
		if d.KeyPoolFile != "" {
			t.Errorf("default KeyPoolFile = %q, want empty", d.KeyPoolFile)
		}
		if d.PoolSize != 4 {
			t.Errorf("default PoolSize = %d, want 4", d.PoolSize)
		}
		if d.PoolSpare != 1 {
			t.Errorf("default PoolSpare = %d, want 1", d.PoolSpare)
		}
		if d.RotationCooldown != 30*time.Second {
			t.Errorf("default RotationCooldown = %s, want 30s", d.RotationCooldown)
		}
		if d.FirstEventTimeout != 30*time.Second {
			t.Errorf("default FirstEventTimeout = %s, want 30s", d.FirstEventTimeout)
		}
		if d.IdleTimeout != 120*time.Second {
			t.Errorf("default IdleTimeout = %s, want 120s", d.IdleTimeout)
		}
		if d.ResponsesIdleTimeout != 300*time.Second {
			t.Errorf("default ResponsesIdleTimeout = %s, want 300s", d.ResponsesIdleTimeout)
		}
	})
}

func TestLoadMalformedJSON(t *testing.T) {
	clearEnv(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeConfig(t, `{"listen": `)

	cfg, err := config.Load()
	if err == nil {
		t.Fatal("Load() succeeded on malformed config.json, want error")
	}
	if cfg != nil {
		t.Errorf("Load() returned config %+v alongside error, want nil", cfg)
	}
}

func TestStateFileEnvOverride(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	xdgDefault := filepath.Join(home, ".local", "state", "zen-router", "state.json")
	if got := config.StateFile(); got != xdgDefault {
		t.Errorf("StateFile() = %q, want XDG default %q", got, xdgDefault)
	}
	// quota.DefaultPath delegates to config.StateFile: no env set → XDG path.
	if got := quota.DefaultPath(); got != xdgDefault {
		t.Errorf("quota.DefaultPath() = %q, want XDG default %q", got, xdgDefault)
	}

	custom := filepath.Join(t.TempDir(), "custom-state.json")
	t.Setenv("ZEN_ROUTER_STATE", custom)
	if got := config.StateFile(); got != custom {
		t.Errorf("StateFile() = %q, want ZEN_ROUTER_STATE value %q", got, custom)
	}
	if got := quota.DefaultPath(); got != custom {
		t.Errorf("quota.DefaultPath() = %q, want ZEN_ROUTER_STATE value %q", got, custom)
	}
}

func TestLegacyStatePath(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	wantFallback := filepath.Join(home, ".dsh", "state", "zen-router", "state.json")
	if got := config.LegacyStatePath(); got != wantFallback {
		t.Errorf("LegacyStatePath() = %q, want DSH_HOME default %q", got, wantFallback)
	}

	dshHome := t.TempDir()
	t.Setenv("DSH_HOME", dshHome)
	want := filepath.Join(dshHome, "state", "zen-router", "state.json")
	if got := config.LegacyStatePath(); got != want {
		t.Errorf("LegacyStatePath() = %q, want DSH_HOME value %q", got, want)
	}
}

func TestMigrateLegacyState(t *testing.T) {
	const legacyContent = `{"version":1,"mode":"auto","current":"warp"}` + "\n"

	// setup points DSH_HOME and XDG_STATE_HOME at fresh temp dirs and returns
	// the legacy file path (not yet created).
	setup := func(t *testing.T) (legacyFile string) {
		t.Helper()
		clearEnv(t)
		dshHome := t.TempDir()
		t.Setenv("DSH_HOME", dshHome)
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		return filepath.Join(dshHome, "state", "zen-router", "state.json")
	}
	writeLegacy := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir legacy dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write legacy state: %v", err)
		}
	}
	readFile := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}

	t.Run("copies legacy state once, original untouched, second run no-op", func(t *testing.T) {
		legacyFile := setup(t)
		writeLegacy(t, legacyFile, legacyContent)
		paths := config.DefaultPaths()

		migrated, err := config.MigrateLegacyState(paths)
		if err != nil {
			t.Fatalf("MigrateLegacyState() error: %v", err)
		}
		if !migrated {
			t.Fatal("MigrateLegacyState() = false, want true on first run")
		}
		if got := readFile(t, paths.StateFile); got != legacyContent {
			t.Errorf("migrated state = %q, want %q", got, legacyContent)
		}
		if got := readFile(t, legacyFile); got != legacyContent {
			t.Errorf("legacy state modified by migration: %q, want %q", got, legacyContent)
		}

		migrated, err = config.MigrateLegacyState(paths)
		if err != nil {
			t.Fatalf("second MigrateLegacyState() error: %v", err)
		}
		if migrated {
			t.Error("second MigrateLegacyState() = true, want false (destination exists)")
		}
		if got := readFile(t, paths.StateFile); got != legacyContent {
			t.Errorf("migrated state after second run = %q, want unchanged %q", got, legacyContent)
		}
	})

	t.Run("missing legacy file reports no migration", func(t *testing.T) {
		legacyFile := setup(t)
		paths := config.DefaultPaths()

		migrated, err := config.MigrateLegacyState(paths)
		if err != nil {
			t.Fatalf("MigrateLegacyState() error: %v", err)
		}
		if migrated {
			t.Error("MigrateLegacyState() = true, want false when legacy file is absent")
		}
		if _, err := os.Stat(paths.StateFile); !os.IsNotExist(err) {
			t.Errorf("destination %s exists, want absent", paths.StateFile)
		}
		if _, err := os.Stat(legacyFile); !os.IsNotExist(err) {
			t.Errorf("legacy file %s unexpectedly exists", legacyFile)
		}
	})

	t.Run("existing destination is never overwritten", func(t *testing.T) {
		legacyFile := setup(t)
		writeLegacy(t, legacyFile, legacyContent)
		paths := config.DefaultPaths()
		const sentinel = `{"version":1,"current":"sentinel"}` + "\n"
		if err := os.MkdirAll(filepath.Dir(paths.StateFile), 0o755); err != nil {
			t.Fatalf("mkdir state dir: %v", err)
		}
		if err := os.WriteFile(paths.StateFile, []byte(sentinel), 0o644); err != nil {
			t.Fatalf("write destination: %v", err)
		}

		migrated, err := config.MigrateLegacyState(paths)
		if err != nil {
			t.Fatalf("MigrateLegacyState() error: %v", err)
		}
		if migrated {
			t.Error("MigrateLegacyState() = true, want false when destination exists")
		}
		if got := readFile(t, paths.StateFile); got != sentinel {
			t.Errorf("destination = %q, want sentinel %q untouched", got, sentinel)
		}
		if got := readFile(t, legacyFile); got != legacyContent {
			t.Errorf("legacy state = %q, want %q untouched", got, legacyContent)
		}
	})
}
