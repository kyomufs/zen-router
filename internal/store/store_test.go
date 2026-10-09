package store

import (
	"path/filepath"
	"testing"
)

// TestRoundtrip: Record → ByDay/ByIP aggregation, fingerprint-only keys,
// "unknown" IP bucket, reopen persistence.
func TestRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.SetIPGetter(func() string { return "203.0.113.7" })

	recs := []struct{ kind, key string }{
		{KindOK, "secret-key-one"},
		{KindOK, "secret-key-one"},
		{Kind429, "secret-key-two"},
	}
	for _, r := range recs {
		if err := s.Record(r.kind, r.key); err != nil {
			t.Fatalf("Record(%s): %v", r.kind, err)
		}
	}
	// Empty getter → "unknown", not "".
	s.SetIPGetter(func() string { return "" })
	if err := s.Record(KindOK, ""); err != nil {
		t.Fatalf("Record (no ip): %v", err)
	}
	s.SetIPGetter(nil)

	days, err := s.ByDay(7)
	if err != nil {
		t.Fatalf("ByDay: %v", err)
	}
	if len(days) != 1 {
		t.Fatalf("ByDay: %d rows, want 1 (today)", len(days))
	}
	if days[0].OK != 3 || days[0].N429 != 1 {
		t.Errorf("today = ok:%d 429:%d, want ok:3 429:1", days[0].OK, days[0].N429)
	}

	ips, err := s.ByIP(7)
	if err != nil {
		t.Fatalf("ByIP: %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("ByIP: %d rows, want 2 (real ip + unknown)", len(ips))
	}
	// Most recently seen first: "unknown".
	if ips[0].IP != "unknown" || ips[0].OK != 1 {
		t.Errorf("ips[0] = %+v, want unknown/ok:1", ips[0])
	}
	if ips[1].IP != "203.0.113.7" || ips[1].OK != 2 || ips[1].N429 != 1 {
		t.Errorf("ips[1] = %+v, want 203.0.113.7 ok:2 429:1", ips[1])
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: data persisted.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	days2, err := s2.ByDay(7)
	if err != nil || len(days2) != 1 || days2[0].OK != 3 {
		t.Errorf("after reopen: ByDay = %+v (err %v), want today ok:3", days2, err)
	}

	// Window filter: 0-day window clamped to 1, still today.
	if d, err := s2.ByDay(0); err != nil || len(d) != 1 {
		t.Errorf("ByDay(0) = %+v (err %v), want today only", d, err)
	}
	// ByDay with an empty (fresh) DB returns an empty slice, not nil/err.
	s3, _ := Open(filepath.Join(t.TempDir(), "empty.db"))
	defer s3.Close()
	if d, err := s3.ByDay(7); err != nil || d == nil || len(d) != 0 {
		t.Errorf("empty ByDay = %#v (err %v), want empty non-nil slice", d, err)
	}
}

// TestFingerprint: the store's fingerprint matches the control layer's
// sha256[:8] hex derivation (efa1f375 for "public") and never leaks input.
func TestFingerprint(t *testing.T) {
	if got := Fingerprint("public"); got != "efa1f375" {
		t.Errorf(`Fingerprint("public") = %q, want "efa1f375"`, got)
	}
	if got := Fingerprint("secret-key-one"); got == "" || got == "secret-key-one" {
		t.Errorf("Fingerprint must be a non-empty digest, got %q", got)
	}
}
