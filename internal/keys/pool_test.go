package keys_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"zen-router/internal/keys"
)

// clearEnv pins the environment variables the pool reads so ambient values
// from the host shell never leak into expectations. No test may touch the
// real pool file under ~/.dsh — every case uses t.TempDir() or testdata/.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
}

// draw pulls n keys in round-robin order and records the has-real-keys flag
// reported by Next alongside each key.
func draw(t *testing.T, p *keys.Pool, n int) (got []string, real []bool) {
	t.Helper()
	for i := 0; i < n; i++ {
		k, ok := p.Next()
		got = append(got, k)
		real = append(real, ok)
	}
	return got, real
}

func writePoolFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestPoolFromFile(t *testing.T) {
	clearEnv(t)
	tests := []struct {
		name     string
		file     string
		wantKeys []string
		wantReal bool
	}{
		{
			// "public" and empty entries filtered, duplicate dropped,
			// original order preserved.
			name:     "opencode sub-pool filters public and dedups keeping order",
			file:     filepath.Join("testdata", "pool_opencode.json"),
			wantKeys: []string{"key-a", "key-b", "key-c"},
			wantReal: true,
		},
		{
			name:     "opencode-zen sub-pool read when opencode absent",
			file:     filepath.Join("testdata", "pool_opencode_zen.json"),
			wantKeys: []string{"zen-1", "zen-2"},
			wantReal: true,
		},
		{
			// Mirrors the plugin's `pools.opencode || pools["opencode-zen"]`:
			// strict OR fallback, the second sub-pool is never merged in.
			name:     "opencode wins outright over opencode-zen",
			file:     filepath.Join("testdata", "pool_both.json"),
			wantKeys: []string{"primary-1", "primary-2"},
			wantReal: true,
		},
		{
			name:     "pools without keys falls back to public",
			file:     filepath.Join("testdata", "pool_empty.json"),
			wantKeys: []string{"public"},
			wantReal: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := keys.New(tc.file)
			if got := p.Len(); got != len(tc.wantKeys) {
				t.Fatalf("Len() = %d, want %d", got, len(tc.wantKeys))
			}
			got, real := draw(t, p, len(tc.wantKeys))
			if !reflect.DeepEqual(got, tc.wantKeys) {
				t.Errorf("drawn keys = %q, want %q", got, tc.wantKeys)
			}
			for i, r := range real {
				if r != tc.wantReal {
					t.Errorf("draw %d: Next() ok = %v, want %v", i, r, tc.wantReal)
				}
			}
		})
	}
}

func TestPoolEnvAppends(t *testing.T) {
	file := filepath.Join("testdata", "pool_opencode.json") // key-a, key-b, key-c

	t.Run("file keys first then OPENCODE_ZEN_API_KEY", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OPENCODE_ZEN_API_KEY", "env-zen")
		t.Setenv("OPENCODE_GO_API_KEY", "env-go") // must lose to ZEN
		p := keys.New(file)
		got, _ := draw(t, p, p.Len())
		want := []string{"key-a", "key-b", "key-c", "env-zen"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("drawn keys = %q, want %q", got, want)
		}
	})

	t.Run("OPENCODE_GO_API_KEY used only when ZEN unset", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OPENCODE_GO_API_KEY", "env-go")
		p := keys.New(file)
		got, _ := draw(t, p, p.Len())
		want := []string{"key-a", "key-b", "key-c", "env-go"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("drawn keys = %q, want %q", got, want)
		}
	})

	t.Run("env only when there is no file", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OPENCODE_ZEN_API_KEY", "env-zen")
		p := keys.New("")
		if p.Len() != 1 {
			t.Fatalf("Len() = %d, want 1", p.Len())
		}
		key, real := p.Next()
		if key != "env-zen" || !real {
			t.Errorf("Next() = (%q, %v), want (%q, true)", key, real, "env-zen")
		}
	})

	t.Run("env duplicating a file key is deduped", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("OPENCODE_ZEN_API_KEY", "key-a")
		p := keys.New(file)
		got, _ := draw(t, p, p.Len())
		want := []string{"key-a", "key-b", "key-c"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("drawn keys = %q, want %q", got, want)
		}
	})
}

func TestPoolFallbackPublic(t *testing.T) {
	clearEnv(t)
	tests := []struct {
		name string
		file func(t *testing.T) string
	}{
		{
			name: "missing file",
			file: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") },
		},
		{
			name: "empty path",
			file: func(t *testing.T) string { return "" },
		},
		{
			name: "malformed file",
			file: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "broken.json")
				writePoolFile(t, p, "not json {")
				return p
			},
		},
		{
			name: "file holds only public entries",
			file: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "public-only.json")
				writePoolFile(t, p, `{"pools":{"opencode":{"keys":["public","public"]}}}`)
				return p
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := keys.New(tc.file(t))
			if got := p.Len(); got != 1 {
				t.Fatalf("Len() = %d, want 1", got)
			}
			key, real := p.Next()
			if key != "public" || real {
				t.Errorf("Next() = (%q, %v), want (%q, false)", key, real, "public")
			}
		})
	}
}

func TestPoolRoundRobin(t *testing.T) {
	clearEnv(t)
	file := filepath.Join(t.TempDir(), "pool.json")
	writePoolFile(t, file, `{"pools":{"opencode":{"keys":["k1","k2","k3"]}}}`)

	p := keys.New(file)
	want := []string{"k1", "k2", "k3", "k1", "k2", "k3", "k1"}
	got, real := draw(t, p, len(want))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drawn keys = %q, want %q", got, want)
	}
	for i, r := range real {
		if !r {
			t.Errorf("draw %d: Next() ok = false, want true", i)
		}
	}
}

func TestPoolMtimeReload(t *testing.T) {
	clearEnv(t)
	file := filepath.Join(t.TempDir(), "pool.json")
	writePoolFile(t, file, `{"pools":{"opencode":{"keys":["old-1","old-2"]}}}`)

	p := keys.New(file)
	if key, _ := p.Next(); key != "old-1" {
		t.Fatalf("first draw = %q, want %q", key, "old-1")
	}

	// Rewrite with a single new key and bump mtime explicitly so the change
	// is observable even on filesystems with coarse timestamp granularity.
	writePoolFile(t, file, `{"pools":{"opencode":{"keys":["new-1"]}}}`)
	mtime := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := p.Len(); got != 1 {
		t.Fatalf("Len() after rewrite = %d, want 1 (reloaded)", got)
	}
	// The old index (1) must be reduced modulo the new length (1): no
	// out-of-range draw, and the fresh key is served without rebuilding
	// the Pool.
	for i := 0; i < 3; i++ {
		key, real := p.Next()
		if key != "new-1" || !real {
			t.Fatalf("draw %d after reload = (%q, %v), want (%q, true)", i, key, real, "new-1")
		}
	}

	// Unchanged mtime → cached list (mtime-gated reload, as in the plugin's
	// loadPoolKeys): a rewrite that keeps the same mtime is not observed.
	writePoolFile(t, file, `{"pools":{"opencode":{"keys":["stale-1"]}}}`)
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if key, _ := p.Next(); key != "new-1" {
		t.Errorf("draw with unchanged mtime = %q, want %q (cached)", key, "new-1")
	}
}

func TestHasAlternatives(t *testing.T) {
	clearEnv(t)
	tests := []struct {
		name    string
		keys    string // JSON keys array content
		current string
		want    bool
	}{
		{
			// The router's step-1 gate: a single-key pool of "public" must
			// not trigger a key re-issue.
			name:    "pool of only public with current public",
			keys:    `["public"]`,
			current: "public",
			want:    false,
		},
		{
			name:    "pool of only public with different current",
			keys:    `["public"]`,
			current: "real-key",
			want:    true,
		},
		{
			name:    "two keys current is one of them",
			keys:    `["a","b"]`,
			current: "a",
			want:    true,
		},
		{
			name:    "single real key equals current",
			keys:    `["a"]`,
			current: "a",
			want:    false,
		},
		{
			name:    "single real key differs from current",
			keys:    `["a"]`,
			current: "b",
			want:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "pool.json")
			writePoolFile(t, file, `{"pools":{"opencode":{"keys":`+tc.keys+`}}}`)
			p := keys.New(file)
			if got := p.HasAlternatives(tc.current); got != tc.want {
				t.Errorf("HasAlternatives(%q) = %v, want %v", tc.current, got, tc.want)
			}
		})
	}
}
