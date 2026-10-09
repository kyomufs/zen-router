package tui

// Panel grid contract (approved redesign step 2): the dashboard renders as
// a bordered panel grid — the two quota panels side by side at >=100 cols,
// stacked quotas at 80-99 cols, single column below 80 (status -> quotas)
// — with the LOG panel full-width below the grid and a fixed footer (keys
// line, then the transient action line as the very last line).
//
// Two invariants are pinned here beyond shape:
//
//   - content hug: quota panels size to their rows, never to the
//     terminal (the old view split leftover height evenly across all
//     tables — the "blank void" bug);
//   - frame fit + line width at every breakpoint.
//
// Panel titles keep the exact literals the hardening matrix asserts
// ("quota (per egress)", "quota (per key)", ...) — they now
// live embedded in the rounded top border of their panel. The identity
// pool / rotation history panels no longer exist (WARP excision).

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// smallJSON: minimal populated dashboard — 1 egress row, 2 key rows, 1
// log line — to measure hug against known row counts instead of guessing
// from the full-fat fixture.
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
    "updatedAt": 1759700000000,
    "egress": {
      "direct": {"ok": 3, "daily429": 0}
    },
    "keys": {
      "deadbeef": {"ok": 2, "daily429": 0},
      "cafebabe": {"ok": 1, "daily429": 1}
    }
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

// TestWideBreakpointPairsQuotas: >=100 cols puts the two quota panels side
// by side (titles share one output line) with the log panel below.
func TestWideBreakpointPairsQuotas(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	content := nm.View().Content

	eg, keys := lineOf(content, "quota (per egress)"), lineOf(content, "quota (per key)")
	logIdx := lineOf(content, "log tail")
	if eg < 0 || keys < 0 || logIdx < 0 {
		t.Fatalf("missing panel titles (eg=%d keys=%d log=%d):\n%s", eg, keys, logIdx, content)
	}
	if eg != keys {
		t.Errorf("quotas not side-by-side: egress line %d != keys line %d", eg, keys)
	}
	if logIdx <= eg {
		t.Errorf("log panel (line %d) must sit below the quota row (line %d)", logIdx, eg)
	}
}

// TestMidBreakpointStacksQuotas: 80-99 cols stacks the quota panels
// vertically (full width each); the log panel stays below both.
func TestMidBreakpointStacksQuotas(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	content := nm.View().Content

	eg, keys := lineOf(content, "quota (per egress)"), lineOf(content, "quota (per key)")
	logIdx := lineOf(content, "log tail")
	if eg < 0 || keys < 0 || logIdx < 0 {
		t.Fatalf("missing panel titles (eg=%d keys=%d log=%d):\n%s", eg, keys, logIdx, content)
	}
	if eg == keys {
		t.Errorf("quotas must stack at 90 cols, both titles on line %d", eg)
	}
	if !(eg < keys && keys < logIdx) {
		t.Errorf("stacked order broken: egress %d, keys %d, log %d", eg, keys, logIdx)
	}
}

// TestNarrowBreakpointIsSingleColumn: <80 cols unrolls everything into one
// column in the approved order — status, quotas, log.
func TestNarrowBreakpointIsSingleColumn(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 70, Height: 40})
	content := nm.View().Content

	ordered := []string{
		"daemon: up",
		"quota (per egress)",
		"quota (per key)",
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

// TestQuotaPanelsHugContent: the blank-void regression guard. The distance
// from the egress panel title to the next landmark (keys title when
// stacked, log title when paired) is bounded by the panel content itself
// (title + table header + data rows + borders + slack) — NOT a fraction of
// the terminal height. The old layout split leftover height evenly,
// blowing this bound at 120x40.
func TestQuotaPanelsHugContent(t *testing.T) {
	const maxGap = 8 // title + header rows + rows + borders + slack

	// Wide: the quota row hugs (1 egress row / 2 key rows side by side),
	// so the log panel follows within maxGap lines.
	m := smallModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	content := nm.View().Content
	eg, logIdx := lineOf(content, "quota (per egress)"), lineOf(content, "log tail")
	if eg < 0 || logIdx < 0 {
		t.Fatalf("missing titles (eg=%d log=%d):\n%s", eg, logIdx, content)
	}
	if gap := logIdx - eg; gap > maxGap || gap < 1 {
		t.Errorf("quota row spans %d lines, want 1..%d (content hug violated):\n%s",
			gap, maxGap, content)
	}

	// Mid: stacked quotas hug the same way — egress panel (1 row) ends at
	// the keys title, keys panel (2 rows) ends at the log title.
	nm, _ = update(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	content = nm.View().Content
	eg, keys, logIdx := lineOf(content, "quota (per egress)"),
		lineOf(content, "quota (per key)"), lineOf(content, "log tail")
	if eg < 0 || keys < 0 || logIdx < 0 {
		t.Fatalf("missing titles (eg=%d keys=%d log=%d):\n%s", eg, keys, logIdx, content)
	}
	if gap := keys - eg; gap > maxGap || gap < 1 {
		t.Errorf("egress panel spans %d lines, want 1..%d (content hug violated)", gap, maxGap)
	}
	if gap := logIdx - keys; gap > maxGap || gap < 1 {
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

		logIdx, keys := lineOf(content, "log tail"), lineOf(content, "quota (per key)")
		quitIdx := lineOf(content, "quit")
		if logIdx < 0 || keys < 0 || quitIdx < 0 {
			t.Fatalf("%dx%d: missing log/quota/help (log=%d keys=%d quit=%d):\n%s",
				tc.w, tc.h, logIdx, keys, quitIdx, content)
		}
		if logIdx <= keys {
			t.Errorf("%dx%d: log panel (line %d) not below quota row (line %d)",
				tc.w, tc.h, logIdx, keys)
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
	m.actionLabel = "stop daemon"
	last := lastLine(m.View().Content)
	if !strings.Contains(last, "in flight") {
		t.Errorf("last line %q must be the transient action line while pending", last)
	}
}

// TestViewFitsBreakpointSizes: frame fit (<= height) and explicit
// truncation (<= width, ANSI-stripped, counted in runes) at every
// breakpoint, including the tight 100x24 where the grid must shrink to
// make room.
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
		if !strings.Contains(content, "deadbeef") {
			t.Errorf("%dx%d: key row missing:\n%s", tc.w, tc.h, content)
		}
		if !strings.Contains(content, "quit") {
			t.Errorf("%dx%d: help line missing:\n%s", tc.w, tc.h, content)
		}
	}
}

// lastLine returns the final non-... trimmed line of the content.
func lastLine(content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	return lines[len(lines)-1]
}
