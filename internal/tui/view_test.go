package tui

// Task 5 (Phase C): dashboard sections (spec §7 screen) — status header
// (mode/egress/IP/latency), quota tables per egress AND per key with reset
// countdowns, identity pool table with credential fields never rendered,
// rotation history table (state.Rotations, last 50), log-tail viewport and
// the help line, with layout adapting to tea.WindowSizeMsg.
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
// latency windows, spent egress + spent key (deterministic countdowns),
// two identities — one carrying credential fields the view must never
// render — and 60 rotation rows (the view must render only the last 50).
func dashboardJSON(t *testing.T) string {
	t.Helper()
	spentEgress := time.Now().Add(2 * time.Hour).UnixMilli()
	spentKey := time.Now().Add(90 * time.Minute).UnixMilli()
	var rots strings.Builder
	for i := 1; i <= 60; i++ {
		if i > 1 {
			rots.WriteByte(',')
		}
		fmt.Fprintf(&rots,
			`{"at":%d,"from":"direct","to":"warp","reason":"rotation-%02d"}`,
			1759700000000+int64(i)*1000, i)
	}
	return fmt.Sprintf(`{
  "mode": "auto",
  "current": "warp",
  "up": true,
  "listen": "127.0.0.1:8787",
  "pid": 4242,
  "uptime_seconds": 7,
  "last_rotate": "2026-10-06T12:00:00Z",
  "rotating": false,
  "registering": false,
  "lastSpareError": "",
  "egress_ip": "198.51.100.9",
  "latency_ttfb_ms": {"direct": {"last_ms": 12, "avg_ms": 14, "count": 3}, "warp": {"last_ms": 30, "avg_ms": 31, "count": 4}},
  "latency_stream_ms": {"direct": {"last_ms": 50, "avg_ms": 55, "count": 2}, "warp": {"last_ms": 60, "avg_ms": 61, "count": 5}},
  "state": {
    "version": 2,
    "mode": "auto",
    "current": "warp",
    "updatedAt": 1759700000000,
    "egress": {
      "direct": {"ok": 11, "daily429": 2, "spentUntil": %d},
      "warp": {"ok": 7, "daily429": 0}
    },
    "keys": {
      "deadbeef": {"ok": 5, "daily429": 1, "spentUntil": %d}
    },
    "identities": [
      {"deviceId": "dev-aaa", "publicKey": "pk-aaa", "addressV4": "10.0.0.2", "registeredAt": 1763193600000, "token": "SECRETTOKEN123", "privateKey": "SECRETPRIVATE456"},
      {"deviceId": "dev-bbb", "publicKey": "pk-bbb", "addressV4": "10.0.0.3", "registeredAt": 1763193600000}
    ],
    "active": 1,
    "rotations": [%s]
  }
}`, spentEgress, spentKey, rots.String())
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

// TestViewStatusHeaderFields: the status header carries mode, current
// egress, the observed egress IP and both latency windows (per egress,
// last/avg/count — never a merged latency_ms number).
func TestViewStatusHeaderFields(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	for _, want := range []string{
		"zen-router control dashboard", // Task 4 header still present
		"daemon: up",
		"mode: auto",
		"egress: warp",
		"198.51.100.9",
		"latency ttfb",
		"direct 12/14ms",
		"warp 30/31ms",
		"latency stream",
		"direct 50/55ms",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("view lacks %q:\n%s", want, content)
		}
	}
}

// TestViewSurfacesSpareRegistrationError: spec §14 — a failed spare
// registration surfaces in the TUI (status.lastSpareError).
func TestViewSurfacesSpareRegistrationError(t *testing.T) {
	st := statusFromJSON(t, `{"up":true,"current":"direct","mode":"auto","lastSpareError":"register spare: HTTP 429"}`)
	m := New(newFake(fakeResult{status: st}))
	nm, _ := update(t, m, runCmd(t, m.Init()))
	content := nm.View().Content
	if !strings.Contains(content, "register spare: HTTP 429") {
		t.Errorf("view does not surface lastSpareError:\n%s", content)
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
		"warp",
		"deadbeef", // fingerprinted key (display-only)
		"1h59m",    // egress spent 2h from fixture time, minus Update's now
		"1h29m",    // key spent 90m from fixture time, minus Update's now
	} {
		if !strings.Contains(content, want) {
			t.Errorf("quota view lacks %q:\n%s", want, content)
		}
	}
}

// TestViewIdentityPoolRedactsCredentials: the identity pool table renders
// display-only fields; credential material (token/privateKey) must never
// reach the screen even when the fixture carries it.
func TestViewIdentityPoolRedactsCredentials(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	for _, want := range []string{
		"identity pool",
		"dev-aaa",
		"dev-bbb",
		"10.0.0.2",
		time.UnixMilli(1763193600000).Format("2006-01-02"), // registered stamp
	} {
		if !strings.Contains(content, want) {
			t.Errorf("identity pool view lacks %q:\n%s", want, content)
		}
	}
	for _, secret := range []string{
		"SECRETTOKEN123",
		"SECRETPRIVATE456",
		"privateKey",
		"token",
	} {
		if strings.Contains(content, secret) {
			t.Errorf("identity pool view leaks credential field %q:\n%s", secret, content)
		}
	}
}

// TestViewRotationHistoryLast50: rotation history table renders
// state.Rotations capped at the last 50 rows (fixture carries 60).
func TestViewRotationHistoryLast50(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	if !strings.Contains(content, "rotation history") {
		t.Fatalf("view lacks the rotation history section:\n%s", content)
	}
	if !strings.Contains(content, "rotation-60") {
		t.Errorf("view lacks the newest rotation row rotation-60")
	}
	if strings.Contains(content, "rotation-10") {
		t.Errorf("view renders a rotation older than the last 50 (rotation-10)")
	}
	if n := strings.Count(content, "rotation-"); n != 50 {
		t.Errorf("rendered rotation rows = %d, want exactly 50", n)
	}
}

// TestViewHelpLine: the spec §7 key bindings render through bubbles help.
func TestViewHelpLine(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	for _, want := range []string{"quit", "rotate", "direct", "warp", "start/stop"} {
		if !strings.Contains(content, want) {
			t.Errorf("help line lacks %q:\n%s", want, content)
		}
	}
}

// TestViewWindowSizeAdaptsLayout: tea.WindowSizeMsg drives SetWidth on the
// tables/viewport — after a 60-cell-wide window the rendered rotation row
// is clipped to 60 cells (it is 70 wide at the default width).
func TestViewWindowSizeAdaptsLayout(t *testing.T) {
	m := dashboardModel(t)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 30})
	content := stripANSI(m.View().Content)

	if !strings.Contains(content, "rotation-60") {
		t.Fatalf("rotation row lost after resize:\n%s", content)
	}
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "rotation-") && len(line) > 60 {
			t.Errorf("rotation line width %d > window width 60: %q", len(line), line)
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
