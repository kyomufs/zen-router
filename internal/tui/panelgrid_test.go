package tui

// Panel grid contract (approved redesign step 2): the dashboard renders as
// a bordered panel grid — 2x2 at >=100 cols (quota·egress | quota·keys /
// identity pool | rotation history), stacked quotas at 80-99 cols, single
// column below 80 (status -> quotas -> identity -> rotation -> log) — with
// the LOG panel full-width below the grid and a fixed footer (keys line,
// then the transient action line as the very last line).
//
// Two invariants are pinned here beyond shape:
//
//   - content hug: quota/identity panels size to their rows, never to the
//     terminal (the old view split leftover height evenly across all four
//     tables — the "blank void" bug);
//   - frame fit + line width at every breakpoint.
//
// Panel titles keep the exact literals the hardening matrix asserts
// ("quota (per egress)", "identity pool (2, active 1)", ...) — they now
// live embedded in the rounded top border of their panel.

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// smallJSON: minimal populated dashboard — 2 egress rows, 1 key row, 1
// identity, 2 rotations, 1 log line — to measure hug against known row
// counts instead of guessing from the 60-rotation fixture.
func smallJSON(t *testing.T) string {
	t.Helper()
	return `{
  "up": true,
  "mode": "auto",
  "current": "direct",
  "listen": "127.0.0.1:8787",
  "pid": 7,
  "uptime_seconds": 7,
  "state": {
    "version": 2,
    "mode": "auto",
    "current": "direct",
    "updatedAt": 1759700000000,
    "egress": {
      "direct": {"ok": 3, "daily429": 0},
      "warp": {"ok": 1, "daily429": 1}
    },
    "keys": {"deadbeef": {"ok": 2, "daily429": 0}},
    "identities": [
      {"deviceId": "dev-one", "publicKey": "pk-one", "addressV4": "10.0.0.9", "registeredAt": 1763193600000}
    ],
    "active": 1,
    "rotations": [
      {"at": 1759700001000, "from": "direct", "to": "warp", "reason": "rotation-01"},
      {"at": 1759700002000, "from": "warp", "to": "direct", "reason": "rotation-02"}
    ]
  }
}`
}

// smallModel polls the minimal fixture once (with a one-line log tail).
func smallModel(t *testing.T) Model {
	t.Helper()
	st := statusFromJSON(t, smallJSON(t))
	m := New(newFake(fakeResult{status: st}), WithLogTail(&fakeLogSource{lines: []string{"boot"}}))
	nm, _ := update(t, m, runCmd(t, m.Init()))
	return nm
}

// lineOf returns the index of the first line containing substr, or -1.
func lineOf(content, substr string) int {
	for i, l := range strings.Split(content, "\n") {
		if strings.Contains(l, substr) {
			return i
		}
	}
	return -1
}

// TestPanelsRenderWithBorders: every panel is a rounded-border box whose
// top border embeds its title — the shape the redesign promises (the old
// view rendered bare section headings with no boxes).
func TestPanelsRenderWithBorders(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	content := nm.View().Content

	for _, title := range []string{
		"quota (per egress)",
		"quota (per key)",
		"identity pool",
		"rotation history",
		"log tail",
	} {
		idx := lineOf(content, title)
		if idx < 0 {
			t.Errorf("panel title %q missing:\n%s", title, content)
			continue
		}
		line := strings.Split(content, "\n")[idx]
		if !strings.Contains(line, "╭") || !strings.Contains(line, "╮") {
			t.Errorf("title %q is not embedded in a rounded top border: %q", title, line)
		}
	}
}

// TestWideBreakpointIsTwoByTwo: >=100 cols puts the two quota panels side
// by side on row 1 and identity/rotation side by side on row 2 — titles of
// a row share one output line.
func TestWideBreakpointIsTwoByTwo(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	content := nm.View().Content

	eg, keys := lineOf(content, "quota (per egress)"), lineOf(content, "quota (per key)")
	id, rot := lineOf(content, "identity pool"), lineOf(content, "rotation history")
	if eg < 0 || keys < 0 || id < 0 || rot < 0 {
		t.Fatalf("missing panel titles (eg=%d keys=%d id=%d rot=%d):\n%s", eg, keys, id, rot, content)
	}
	if eg != keys {
		t.Errorf("row 1 not side-by-side: egress line %d != keys line %d", eg, keys)
	}
	if id != rot {
		t.Errorf("row 2 not side-by-side: identity line %d != rotation line %d", id, rot)
	}
	if !(eg < id) {
		t.Errorf("row 1 (line %d) must be above row 2 (line %d)", eg, id)
	}
}

// TestMidBreakpointStacksQuotas: 80-99 cols stacks the quota panels
// vertically (full width each) but keeps identity/rotation paired.
func TestMidBreakpointStacksQuotas(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	content := nm.View().Content

	eg, keys := lineOf(content, "quota (per egress)"), lineOf(content, "quota (per key)")
	id, rot := lineOf(content, "identity pool"), lineOf(content, "rotation history")
	if eg < 0 || keys < 0 || id < 0 || rot < 0 {
		t.Fatalf("missing panel titles (eg=%d keys=%d id=%d rot=%d):\n%s", eg, keys, id, rot, content)
	}
	if eg == keys {
		t.Errorf("quotas must stack at 90 cols, both titles on line %d", eg)
	}
	if !(eg < keys) {
		t.Errorf("egress panel (line %d) must be above keys panel (line %d)", eg, keys)
	}
	if id != rot {
		t.Errorf("identity/rotation must stay side-by-side at 90 cols: %d != %d", id, rot)
	}
	if keys > id {
		t.Errorf("quotas (line %d) must be above identity row (line %d)", keys, id)
	}
}

// TestNarrowBreakpointIsSingleColumn: <80 cols unrolls everything into one
// column in the approved order — status, quotas, identity, rotation, log.
func TestNarrowBreakpointIsSingleColumn(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 70, Height: 40})
	content := nm.View().Content

	ordered := []string{
		"daemon: up",
		"quota (per egress)",
		"quota (per key)",
		"identity pool",
		"rotation history",
		"log tail",
	}
	prev, prevName := -1, "(start)"
	for _, needle := range ordered {
		idx := lineOf(content, needle)
		if idx < 0 {
			t.Fatalf("missing %q:\n%s", needle, content)
		}
		if idx <= prev {
			t.Errorf("single-column order broken: %q (line %d) not below %q (line %d)",
				needle, idx, prevName, prev)
		}
		prev, prevName = idx, needle
	}
}

// TestQuotaPanelsHugContent: the blank-void regression guard. With 2
// egress rows the distance from the egress panel title to the identity row
// title is the egress panel itself (title + table header + 2 rows + bottom
// border + slack) — NOT a fraction of the terminal height. The old layout
// split leftover height evenly, blowing this bound at 120x40.
func TestQuotaPanelsHugContent(t *testing.T) {
	m := smallModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	content := nm.View().Content
	lines := strings.Split(content, "\n")

	const maxGap = 8 // title + header row + 2 data rows + border + slack
	eg, id := lineOf(content, "quota (per egress)"), lineOf(content, "identity pool")
	if eg < 0 || id < 0 {
		t.Fatalf("missing titles (eg=%d id=%d):\n%s", eg, id, content)
	}
	if gap := id - eg; gap > maxGap || gap < 1 {
		t.Errorf("egress panel spans %d lines, want 1..%d (content hug violated):\n%s",
			gap, maxGap, strings.Join(lines[eg:min(id+1, len(lines))], "\n"))
	}

	keys, rot := lineOf(content, "quota (per key)"), lineOf(content, "rotation history")
	if keys < 0 || rot < 0 {
		t.Fatalf("missing titles (keys=%d rot=%d):\n%s", keys, rot, content)
	}
	if gap := rot - keys; gap > maxGap || gap < 1 {
		t.Errorf("keys panel spans %d lines, want 1..%d (content hug violated)", gap, maxGap)
	}
}

// TestLogPanelSitsBelowGridAndFooterLast: the LOG panel is the bottom-most
// panel at every breakpoint; the keys line sits below it; with an action in
// flight the transient action line is the very last line of the frame.
func TestLogPanelSitsBelowGridAndFooterLast(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{120, 40}, {90, 30}, {70, 40}} {
		m := smallModel(t)
		nm, _ := update(t, m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
		content := nm.View().Content
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")

		logIdx, rotIdx := lineOf(content, "log tail"), lineOf(content, "rotation history")
		quitIdx := lineOf(content, "quit")
		if logIdx < 0 || rotIdx < 0 || quitIdx < 0 {
			t.Fatalf("%dx%d: missing log/rotation/help (log=%d rot=%d quit=%d):\n%s",
				tc.w, tc.h, logIdx, rotIdx, quitIdx, content)
		}
		if logIdx < rotIdx {
			t.Errorf("%dx%d: log panel (line %d) above rotation (line %d)", tc.w, tc.h, logIdx, rotIdx)
		}
		if quitIdx < logIdx {
			t.Errorf("%dx%d: keys line (line %d) above log panel (line %d)", tc.w, tc.h, quitIdx, logIdx)
		}
		if quitIdx < len(lines)-3 { // keys line is in the fixed 2-line footer zone
			t.Errorf("%dx%d: keys line at %d, want within last 3 lines (%d total)",
				tc.w, tc.h, quitIdx, len(lines))
		}
	}

	// Pending action: footer = keys line + action line, action last.
	m := dashboardModel(t)
	m.pending = true
	m.actionLabel = "rotate"
	last := lastLine(m.View().Content)
	if !strings.Contains(last, "in flight") {
		t.Errorf("last line %q must be the transient action line while pending", last)
	}
}

// TestViewFitsBreakpointSizes: frame fit (<= height) and explicit
// truncation (<= width, ANSI-stripped, counted in runes) at every
// breakpoint, including the tight 100x24 where rotation/history must shrink
// first to make room.
func TestViewFitsBreakpointSizes(t *testing.T) {
	for _, tc := range []struct{ w, h int }{
		{120, 40},
		{100, 24},
		{90, 30},
		{70, 40},
	} {
		m := dashboardModel(t)
		nm, _ := update(t, m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
		content := nm.View().Content
		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		if len(lines) > tc.h {
			t.Errorf("%dx%d: %d lines > height %d:\n%s", tc.w, tc.h, len(lines), tc.h, content)
		}
		for _, line := range lines {
			if n := utf8.RuneCountInString(stripANSI(line)); n > tc.w {
				t.Errorf("%dx%d: line of %d cells > width %d: %q", tc.w, tc.h, n, tc.w, line)
			}
		}
		if !strings.Contains(content, "rotation-60") {
			t.Errorf("%dx%d: newest rotation row missing:\n%s", tc.w, tc.h, content)
		}
		if !strings.Contains(content, "quit") {
			t.Errorf("%dx%d: help line missing:\n%s", tc.w, tc.h, content)
		}
	}
}

// TestIdentityPanelExplainsEmptyPool: in production the pool is empty (no
// reachable Cloudflare API); the panel must say so instead of showing a
// bare column header (spec §14 data reality).
func TestIdentityPanelExplainsEmptyPool(t *testing.T) {
	st := statusFromJSON(t, `{"up":true,"mode":"auto","current":"direct","state":{"version":2,"mode":"auto","current":"direct","updatedAt":1759700000000,"egress":{},"keys":{},"identities":[],"active":0,"rotations":[]}}`)
	m := New(newFake(fakeResult{status: st}), WithLogTail(&fakeLogSource{lines: []string{"boot"}}))
	nm, _ := update(t, m, runCmd(t, m.Init()))
	content := nm.View().Content

	if !strings.Contains(content, "identity pool (0, active 0)") {
		t.Errorf("empty pool title missing:\n%s", content)
	}
	if !strings.Contains(content, "no identities") {
		t.Errorf("empty pool must explain itself (want \"no identities\"):\n%s", content)
	}
}

// lastLine returns the final non-... trimmed line of the content.
func lastLine(content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	return lines[len(lines)-1]
}
