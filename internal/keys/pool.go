// Package keys implements the API key pool behind staged key rotation
// (spec §6): keys come from the dsh-api-key-pool pool-config.json file plus a
// single environment override, are drawn round-robin, and the file is
// re-read whenever its mtime changes so runtime pool edits take effect
// without restarting the daemon. The pool never writes to the file.
package keys

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Pool is a round-robin API key pool backed by an optional pool-config.json
// file. It is safe for concurrent use.
type Pool struct {
	mu       sync.Mutex
	poolFile string
	keys     []string
	real     bool // at least one key other than "public"
	idx      int

	loaded   bool
	mtime    time.Time
	hasMtime bool
}

// New returns a pool that reads poolFile on first use. poolFile may be empty;
// a missing or malformed file is not an error — the pool then falls back to
// the environment and ultimately to the single key "public".
func New(poolFile string) *Pool {
	return &Pool{poolFile: poolFile}
}

// Next returns the next key in round-robin order. The bool reports whether
// the pool holds at least one key other than the literal "public", i.e.
// whether real keys are configured beyond the anonymous fallback.
func (p *Pool) Next() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reload()
	key := p.keys[p.idx%len(p.keys)]
	p.idx = (p.idx + 1) % len(p.keys)
	return key, p.real
}

// Len returns the number of keys currently in the pool.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reload()
	return len(p.keys)
}

// HasAlternatives reports whether the pool contains at least one key
// different from current — the router's step-1 question "is there another
// key to try?". With the fallback pool ["public"] and current "public" it
// reports false, so a single-key pool never triggers a key re-issue.
func (p *Pool) HasAlternatives(current string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reload()
	for _, k := range p.keys {
		if k != current {
			return true
		}
	}
	return false
}

// reload re-reads the pool file when its mtime changed since the last load
// (mirrors the plugin's loadPoolKeys). The round-robin index is reduced
// modulo the new length so it can never point past the end of the reloaded
// list.
func (p *Pool) reload() {
	mtime, ok := p.fileMtime()
	if p.loaded && ok == p.hasMtime && (!ok || mtime.Equal(p.mtime)) {
		return
	}
	p.mtime, p.hasMtime, p.loaded = mtime, ok, true
	p.keys, p.real = readKeys(p.poolFile)
	p.idx %= len(p.keys)
}

// fileMtime reports the pool file's modification time; ok is false when the
// file is absent (or poolFile is empty), matching the plugin's null mtime.
func (p *Pool) fileMtime() (mtime time.Time, ok bool) {
	if p.poolFile == "" {
		return time.Time{}, false
	}
	fi, err := os.Stat(p.poolFile)
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// readKeys assembles the key list exactly like the plugin's readPoolKeys:
// file entries first, then the single environment override, deduplicated
// preserving first occurrence, falling back to ["public"] when empty.
func readKeys(poolFile string) (keys []string, real bool) {
	var sources []string

	// File keys: a missing or unparseable file contributes nothing.
	if poolFile != "" {
		if data, err := os.ReadFile(poolFile); err == nil {
			var doc struct {
				Pools map[string]*struct {
					Keys []string `json:"keys"`
				} `json:"pools"`
			}
			if json.Unmarshal(data, &doc) == nil {
				// The plugin selects pools.opencode || pools["opencode-zen"]:
				// a strict OR fallback where the first present sub-pool wins
				// and the other is never merged. Mirrored verbatim so the
				// daemon draws exactly the keys the plugin would draw.
				entry := doc.Pools["opencode"]
				if entry == nil {
					entry = doc.Pools["opencode-zen"]
				}
				if entry != nil {
					for _, k := range entry.Keys {
						// Plugin: oc.keys.filter((k) => k && k !== "public")
						// — drop empty entries and the literal "public".
						if k != "" && k != "public" {
							sources = append(sources, k)
						}
					}
				}
			}
		}
	}

	// Environment override: a single value, OPENCODE_ZEN_API_KEY preferred
	// over OPENCODE_GO_API_KEY (plugin: A || B). Appended after the file
	// keys; unlike file entries it is not filtered for "public".
	env := os.Getenv("OPENCODE_ZEN_API_KEY")
	if env == "" {
		env = os.Getenv("OPENCODE_GO_API_KEY")
	}
	if env != "" {
		sources = append(sources, env)
	}

	// Dedup preserving the first occurrence (plugin: new Set(sources)).
	seen := make(map[string]struct{}, len(sources))
	keys = make([]string, 0, len(sources))
	for _, k := range sources {
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}

	if len(keys) == 0 {
		return []string{"public"}, false
	}
	for _, k := range keys {
		if k != "public" {
			real = true
			break
		}
	}
	return keys, real
}
