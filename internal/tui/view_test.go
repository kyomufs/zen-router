package tui

// Task 5 (Phase C): dashboard sections (spec §7 screen) — status header
// (daemon/listen/pid/uptime, egress IP, direct latency), quota tables per
// egress AND per key with reset countdowns, log-tail viewport and the help
// line, with layout adapting to tea.WindowSizeMsg. Direct-only: no mode
// row, no identity pool, no rotation history (those sections were excised
// with WARP; quota.State carries only egress + keys).
//
// Hermeticity contract (same as tui_test.go): canned Status fixtures decoded
// from JSON through the real cli.Status decode path, messages fed straight
// into Update(), View().Content asserted — no tea.Program, no live daemon,
// no real filesystem.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"zen-router/internal/cli"
)

// statusFromJSON decodes a canned status payload through the exact JSON
// contract the control API serves (mirrors *cli.ControlClient.Status), so
// fixtures need no daemon-side package imports (TestImportBoundaries).
func statusFromJSON(t *testing.T, raw string) *cli.Status {
	t.Helper()
	var st cli.Status
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatalf("decode fixture status: %v", err)
	}
	return &st
}

// dashboardJSON builds the full-fat canned status: header fields, both
// latency windows, spent egress + spent key (deterministic countdowns).
// Identity/rotation blocks are deliberately absent (features excised);
// legacy top-level warp-era fields stay as a decode-drop belt — the
// direct-only cli.Status must ignore them.
func dashboardJSON(t *testing.T) string {
	t.Helper()
	spentEgress := time.Now().Add(2 * time.Hour).UnixMilli()
	spentKey := time.Now().Add(90 * time.Minute).UnixMilli()
	return fmt.Sprintf(`{
  "mode": "auto",
  "current": "direct",
  "up": true,
  "listen": "127.0.0.1:8787",
  "pid": 4242,
  "uptime_seconds": 7,
  "last_rotate": "2026-10-06T12:00:00Z",
  "rotating": false,
  "registering": false,
  "lastSpareError": "",
  "egress_ip": "198.51.100.9",
  "latency_ttfb_ms": {"direct": {"last_ms": 12, "avg_ms": 14, "count": 3}},
  "latency_stream_ms": {"direct": {"last_ms": 50, "avg_ms": 55, "count": 2}},
  "state": {
    "version": 2,
    "mode": "auto",
    "current": "direct",
    "updatedAt": 1759700000000,
    "egress": {
      "direct": {"ok": 11, "daily429": 2, "spentUntil": %d}
    },
    "keys": {
      "deadbeef": {"ok": 5, "daily429": 1, "spentUntil": %d}
    }
  }
}`, spentEgress, spentKey)
}

// dashboardModel polls once through the fake source and returns the model
// with the canned dashboard status applied.
func dashboardModel(t *testing.T) Model {
	t.Helper()
	st := statusFromJSON(t, dashboardJSON(t))
	// A log tail is part of the dashboard (spec §7): the fixture feeds one
	// so every render exercises the viewport budget path, not the notice.
	m := New(newFake(fakeResult{status: st}), WithLogTail(&fakeLogSource{lines: []string{
		"daemon starting",
		"control api listening on 127.0.0.1:8787",
		"tail line 3",
	}}))
	msg := runCmd(t, m.Init())
	nm, _ := update(t, m, msg)
	return nm
}

// stripANSI removes escape sequences so display widths can be measured.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			i++                            // ESC
			if i < len(s) && s[i] == '[' { // CSI
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestViewStatusHeaderFields: the status header carries daemon state and
// the observed egress IP plus the direct latency windows (last/avg/count —
// never a merged latency_ms number). Direct-only: no mode/current rows.
func TestViewStatusHeaderFields(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	for _, want := range []string{
		"zen-router control dashboard", // Task 4 header still present
		"daemon: up",
		"198.51.100.9",
		"latency ttfb",
		"direct 12/14ms",
		"latency stream",
		"direct 50/55ms",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("view lacks %q:\n%s", want, content)
		}
	}
}

// TestViewQuotaTablesWithResetCountdowns: quota is rendered as two tables —
// per egress and per key — each with a countdown to the reset (spentUntil
// when spent, otherwise the next daily window).
func TestViewQuotaTablesWithResetCountdowns(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	for _, want := range []string{
		"quota (per egress)",
		"quota (per key)",
		"Resets in", // countdown column
		"direct",
		"deadbeef", // fingerprinted key (display-only)
		"1h59m",    // egress spent 2h from fixture time, minus Update's now
		"1h29m",    // key spent 90m from fixture time, minus Update's now
	} {
		if !strings.Contains(content, want) {
			t.Errorf("quota view lacks %q:\n%s", want, content)
		}
	}
}

// (The identity-pool and rotation-history tests that lived here were
// deleted with the WARP excision — those panels no longer exist.)
// TestViewHelpLine: the spec §7 key bindings render through bubbles help —
// direct-only bindings (no rotate/direct/warp egress keys).
func TestViewHelpLine(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	for _, want := range []string{"quit", "start/stop"} {
		if !strings.Contains(content, want) {
			t.Errorf("help line lacks %q:\n%s", want, content)
		}
	}
}

// TestViewWindowSizeAdaptsLayout: tea.WindowSizeMsg drives SetWidth on the
// tables/viewport — after a 60-cell-wide window every rendered line stays
// within the window width and the quota rows survive the resize.
func TestViewWindowSizeAdaptsLayout(t *testing.T) {
	m := dashboardModel(t)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 30})
	content := stripANSI(m.View().Content)

	if !strings.Contains(content, "deadbeef") {
		t.Fatalf("key row lost after resize:\n%s", content)
	}
	for _, line := range strings.Split(content, "\n") {
		// Display cells, not bytes: the panel redesign wraps rows in
		// 3-byte box-drawing borders (utf8 — same metric as panelgrid).
		if n := utf8.RuneCountInString(line); n > 60 {
			t.Errorf("line width %d cells > window width 60: %q", n, line)
		}
	}
}

// --- fix round 1: terminal fit (F1) -----------------------------------------

// TestViewFitsTerminalSize: bubbletea v2 altscreen clips the BOTTOM of the
// frame, so the whole dashboard — including the brief-mandated help line —
// must fit the terminal height at common sizes.
func TestViewFitsTerminalSize(t *testing.T) {
	cases := []struct{ w, h int }{
		{80, 24},
		{60, 30},
	}
	for _, tc := range cases {
		m := dashboardModel(t)
		nm, _ := update(t, m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
		content := nm.View().Content
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		if len(lines) > tc.h {
			t.Errorf("%dx%d: view renders %d lines, want <= %d:\n%s",
				tc.w, tc.h, len(lines), tc.h, content)
		}
		if !strings.Contains(content, "quit") {
			t.Errorf("%dx%d: help line missing from content:\n%s", tc.w, tc.h, content)
		}
	}
}

// TestViewDegenerateSizesNoPanic: 0x0 / 1x1 window messages must not crash
// (fit is only claimed for typical sizes; degenerate ones just render).
func TestViewDegenerateSizesNoPanic(t *testing.T) {
	m := dashboardModel(t)
	for _, tc := range []struct{ w, h int }{{0, 0}, {1, 1}} {
		nm, _ := update(t, m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
		if content := nm.View().Content; content == "" {
			t.Errorf("%dx%d: empty content", tc.w, tc.h)
		}
	}
}
