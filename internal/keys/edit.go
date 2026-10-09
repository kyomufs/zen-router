// Pool-file editing: the write half the pool itself refuses to have
// (pool.go stays read-only) — used by the control API's key-management
// routes. The document round-trips through a generic JSON map, so every
// unknown field and sub-pool survives an edit untouched; only the
// selected sub-pool's keys array changes. Writes are atomic (temp file +
// rename) and 0600.

package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ErrKeyNotFound reports a RemoveFileKey target that is not in the pool
// file (stale fingerprint — the TUI refetches on the next error).
var ErrKeyNotFound = errors.New("key not found in pool file")

// Fingerprint derives the stable display form of one raw API key: 8 hex
// chars of sha256 — enough to tell keys apart without disclosing any
// part of the secret. Mirrored by the control layer's redaction so the
// keys tab and the status quota table use the SAME identifier.
func Fingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:4])
}

// selectedPool returns the sub-pool name readKeys draws from: "opencode"
// if present, else "opencode-zen" if present, else "opencode" (add
// creates it). doc is the decoded pool-config document.
func selectedPool(doc map[string]any) string {
	pools, ok := doc["pools"].(map[string]any)
	if !ok {
		return "opencode"
	}
	if _, ok := pools["opencode"]; ok {
		return "opencode"
	}
	if _, ok := pools["opencode-zen"]; ok {
		return "opencode-zen"
	}
	return "opencode"
}

// loadPoolFile reads and decodes path into the generic document. Numbers
// decode as json.Number so the re-marshal never reformats them. A
// missing file yields an empty document (add then creates the file).
func loadPoolFile(path string) (map[string]any, error) {
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return nil, fmt.Errorf("read pool file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse pool file: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// poolKeys extracts the selected sub-pool's []string keys from the
// document; nil when the pools/sub-pool structure is absent.
func poolKeys(doc map[string]any, name string) []string {
	pools, ok := doc["pools"].(map[string]any)
	if !ok {
		return nil
	}
	entry, ok := pools[name].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := entry["keys"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// savePoolFile atomically replaces path with the re-encoded document,
// mode 0600. The temp file lives in the same directory (rename stays on
// one filesystem) and is removed on any failure.
func savePoolFile(path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pool file: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pool dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".pool-*.json")
	if err != nil {
		return fmt.Errorf("temp pool file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp pool file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp pool file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp pool file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace pool file: %w", err)
	}
	return nil
}

// FileKeys lists the selected sub-pool's raw keys exactly as readKeys
// filters them (drop "" and "public"), so the control layer fingerprints
// precisely the keys the pool draws. Missing file → empty list.
func FileKeys(path string) ([]string, error) {
	doc, err := loadPoolFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range poolKeys(doc, selectedPool(doc)) {
		if k != "" && k != "public" {
			out = append(out, k)
		}
	}
	return out, nil
}

// AddFileKey appends one raw key to the selected sub-pool (creating the
// file/pools structure when absent) and saves atomically. Duplicates —
// including a second add of the same secret under its fingerprint — are
// skipped: the call is idempotent and still returns the fingerprint.
func AddFileKey(path, raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty key")
	}
	if raw == "public" {
		return "", errors.New(`"public" is the anonymous fallback, not an addable key`)
	}
	doc, err := loadPoolFile(path)
	if err != nil {
		return "", err
	}
	name := selectedPool(doc)
	keys := poolKeys(doc, name)
	for _, k := range keys {
		if k == raw {
			return Fingerprint(raw), nil
		}
	}
	keys = append(keys, raw)
	if err := setPoolKeys(doc, name, keys); err != nil {
		return "", err
	}
	if err := savePoolFile(path, doc); err != nil {
		return "", err
	}
	return Fingerprint(raw), nil
}

// RemoveFileKey deletes the key whose Fingerprint matches fp from the
// selected sub-pool and saves atomically. ErrKeyNotFound when the pool
// file holds no such key.
func RemoveFileKey(path, fp string) error {
	doc, err := loadPoolFile(path)
	if err != nil {
		return err
	}
	name := selectedPool(doc)
	keys := poolKeys(doc, name)
	kept := make([]string, 0, len(keys))
	removed := false
	for _, k := range keys {
		if !removed && Fingerprint(k) == fp {
			removed = true
			continue
		}
		kept = append(kept, k)
	}
	if !removed {
		return ErrKeyNotFound
	}
	if err := setPoolKeys(doc, name, kept); err != nil {
		return err
	}
	return savePoolFile(path, doc)
}

// setPoolKeys writes the keys array into doc's pools/<name> entry,
// materializing the maps as needed (add to a file without a pools block).
func setPoolKeys(doc map[string]any, name string, keys []string) error {
	pools, ok := doc["pools"].(map[string]any)
	if !ok {
		pools = map[string]any{}
		doc["pools"] = pools
	}
	entry, ok := pools[name].(map[string]any)
	if !ok {
		entry = map[string]any{}
		pools[name] = entry
	}
	anyKeys := make([]any, len(keys))
	for i, k := range keys {
		anyKeys[i] = k
	}
	entry["keys"] = anyKeys
	return nil
}

// FingerprintSorted is FileKeys mapped to fingerprints and sorted — the
// stable listing order the control API serves (GET /_zenctl/keys).
func FingerprintSorted(path string) ([]string, error) {
	raw, err := FileKeys(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, k := range raw {
		fp := Fingerprint(k)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, fp)
	}
	sort.Strings(out)
	return out, nil
}
