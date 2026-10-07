package main

// Task 4 (Phase C): the usage() registry must advertise `tui` (spec §11,
// chained registry 1 → 3 → 8 → 4); Task 9 gates on help listing it.

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestUsageListsTUI mirrors TestUsageListsInstallSystemd: capture usage()'s
// stderr and require the `tui [--listen ADDR]` line.
func TestUsageListsTUI(t *testing.T) {
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
	if !strings.Contains(string(data), "zen-router tui") {
		t.Errorf("usage() must list `zen-router tui`, got:\n%s", data)
	}
}
