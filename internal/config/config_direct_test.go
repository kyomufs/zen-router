package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"zen-router/internal/config"
)

// TestDropsWarpEraTuningFields pins the removal of the WARP-era tuning
// knobs: neither the defaults nor a config file carrying them materializes
// poolSize / poolSpare / rotationCooldown anywhere in the effective config.
func TestDropsWarpEraTuningFields(t *testing.T) {
	banned := []string{"poolSize", "poolSpare", "rotationCooldown"}

	marshalAssert := func(t *testing.T, cfg *config.Config) {
		t.Helper()
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		for _, key := range banned {
			if strings.Contains(string(b), `"`+key+`"`) {
				t.Errorf("config JSON still carries removed field %q: %s", key, b)
			}
		}
	}

	t.Run("defaults", func(t *testing.T) {
		marshalAssert(t, config.Default())
	})

	t.Run("file values are ignored", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		writeConfig(t, `{
			"poolSize": 7,
			"poolSpare": 3,
			"rotationCooldown": "5s"
		}`)
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error: %v", err)
		}
		marshalAssert(t, cfg)
	})
}
