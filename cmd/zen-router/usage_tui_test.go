package main

// Task 4 (Phase C): the usage() registry must advertise `tui` (spec §11,
// chained registry 1 → 3 → 8 → 4); Task 9 gates on help listing it.
// Fix round 1 (F3): the `tui` entry must sit in the same flag column as its
// sibling control entries.

import (
	"io"
	"os"
	"strings"
	"testing"
)

// capturedUsage runs usage() with stderr swapped to a pipe and returns its
// output (same pattern as TestUsageListsInstallSystemd).
func capturedUsage(t *testing.T) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	usage()
	os.Stderr = old
	w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read usage output: %v", err)
	}
	return string(data)
}

// TestUsageListsTUI: usage() lists the `tui [--listen ADDR]` line (Task 9
// gate).
func TestUsageListsTUI(t *testing.T) {
	out := capturedUsage(t)
	if !strings.Contains(out, "zen-router tui") {
		t.Errorf("usage() must list `zen-router tui`, got:\n%s", out)
	}
}

// TestUsageTUIFlagColumn (review F3): the `[` of the tui entry starts in the
// same column as the status entry — a one-space drift must not regress.
func TestUsageTUIFlagColumn(t *testing.T) {
	out := capturedUsage(t)
	flagCol := func(prefix string) int {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.Index(line, "[")
			}
		}
		t.Fatalf("no usage line prefixed %q in:\n%s", prefix, out)
		return -1
	}
	want := flagCol("  zen-router status")
	got := flagCol("  zen-router tui")
	if got != want {
		t.Errorf("tui `[--listen ADDR]` column = %d, want %d (aligned with status)\nusage:\n%s",
			got, want, out)
	}
}
