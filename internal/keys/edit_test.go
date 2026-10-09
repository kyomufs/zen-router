package keys

// Pool-file editing (Phase 3): list/add/remove round-trips that preserve
// unknown document structure, skip duplicates, and match by fingerprint.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func poolPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "pool-config.json")
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("pool file is not JSON: %v", err)
	}
	return doc
}

func TestAddListRemoveRoundTrip(t *testing.T) {
	path := poolPath(t)

	fp, err := AddFileKey(path, "sk-alpha")
	if err != nil {
		t.Fatalf("AddFileKey: %v", err)
	}
	if fp != Fingerprint("sk-alpha") {
		t.Fatalf("fp = %q, want %q", fp, Fingerprint("sk-alpha"))
	}

	// Duplicate add is idempotent (same fp, still one entry).
	if _, err := AddFileKey(path, "sk-alpha"); err != nil {
		t.Fatalf("duplicate AddFileKey: %v", err)
	}
	fps, err := FingerprintSorted(path)
	if err != nil {
		t.Fatalf("FingerprintSorted: %v", err)
	}
	if len(fps) != 1 {
		t.Fatalf("fps = %v, want 1 entry", fps)
	}

	if err := RemoveFileKey(path, fp); err != nil {
		t.Fatalf("RemoveFileKey: %v", err)
	}
	if err := RemoveFileKey(path, fp); err != ErrKeyNotFound {
		t.Fatalf("second remove err = %v, want ErrKeyNotFound", err)
	}
}

func TestEditPreservesForeignStructure(t *testing.T) {
	path := poolPath(t)
	doc := `{"custom":"keep","pools":{"opencode":{"keys":["public","sk-a"],"extra":42},"other":{"keys":["sk-x"]}}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddFileKey(path, "sk-b"); err != nil {
		t.Fatalf("AddFileKey: %v", err)
	}

	got := readDoc(t, path)
	if got["custom"] != "keep" {
		t.Fatalf("custom field lost: %v", got["custom"])
	}
	other := got["pools"].(map[string]any)["other"]
	if other == nil {
		t.Fatal("foreign sub-pool lost")
	}
	oc := got["pools"].(map[string]any)["opencode"].(map[string]any)
	if n, ok := oc["extra"].(float64); !ok || n != 42 {
		t.Fatalf("opencode.extra lost: %v", oc["extra"])
	}
	keys := oc["keys"].([]any)
	if len(keys) != 3 || keys[2].(string) != "sk-b" {
		t.Fatalf("keys = %v, want [public sk-a sk-b]", keys)
	}

	// "public" never lists but also never gets removed by fingerprint.
	fps, err := FingerprintSorted(path)
	if err != nil || len(fps) != 2 {
		t.Fatalf("FingerprintSorted = %v, %v; want 2 (public filtered)", fps, err)
	}
}

func TestAddRejectsEmptyAndPublic(t *testing.T) {
	path := poolPath(t)
	if _, err := AddFileKey(path, ""); err == nil {
		t.Fatal("empty key must be rejected")
	}
	if _, err := AddFileKey(path, "public"); err == nil {
		t.Fatal(`"public" must be rejected`)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rejected adds must not create the file")
	}
}

func TestFilePermissions(t *testing.T) {
	path := poolPath(t)
	if _, err := AddFileKey(path, "sk-a"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pool file mode = %v, want 0600", fi.Mode().Perm())
	}
}
