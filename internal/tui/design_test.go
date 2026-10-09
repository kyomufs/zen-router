package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// --- redesign: header band + status pill -----------------------------------

// TestHeaderBandStatusPill pins the redesigned header: line one is a
// full-width band carrying the dashboard title and a right-aligned state
// pill — "● UP" while the poll answers, "● DOWN" when the daemon is
// unreachable, "● WAIT" before the first poll. The pill is rendered with
// the existing theme status styles (ok/err/warn foreground) so the state
// reads at a glance. Chrome markers stay untouched: the title and poll
// lines remain contiguous raw text, and the header stays two lines so the
// line budget (coreLineCount) does not move.
func TestHeaderBandStatusPill(t *testing.T) {
	cases := []struct {
		name   string
		model  func(t *testing.T) Model
		pill   string
		absent []string
	}{
		{
			name: "up",
			model: func(t *testing.T) Model {
				m := New(newFake(fakeResult{status: upStatus()}))
				return runUpdateInit(t, m)
			},
			pill:   "● UP",
			absent: []string{"● DOWN", "● WAIT"},
		},
		{
			name: "down",
			model: func(t *testing.T) Model {
				m := New(newFake(fakeResult{err: errors.New("connection refused")}))
				return runUpdateInit(t, m)
			},
			pill:   "● DOWN",
			absent: []string{"● UP", "● WAIT"},
		},
		{
			name: "wait",
			model: func(t *testing.T) Model {
				return New(newFake(fakeResult{})) // never polled
			},
			pill:   "● WAIT",
			absent: []string{"● UP", "● DOWN"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.model(t)
			content := m.View().Content
			lines := strings.Split(content, "\n")

			// Chrome: title + poll line, in that order, still raw-contiguous.
			if len(lines) < 2 {
				t.Fatalf("frame has %d lines, want the two header lines", len(lines))
			}
			if !strings.Contains(lines[0], headerTitle) {
				t.Errorf("band line lacks %q:\n%q", headerTitle, lines[0])
			}
			if !strings.Contains(lines[1], headerPoll) {
				t.Errorf("poll line lacks %q:\n%q", headerPoll, lines[1])
			}

			// The band spans the full view width (measured display cells).
			if got, want := cells(lines[0]), m.viewWidth(); got != want {
				t.Errorf("band display width = %d, want full width %d:\n%q", got, want, lines[0])
			}

			// The band actually paints a background.
			if !strings.Contains(lines[0], "\x1b[48;2;") {
				t.Errorf("band line has no background SGR:\n%q", lines[0])
			}

			// Pill present, rendered with the theme's status style.
			if !strings.Contains(content, tc.pill) {
				t.Errorf("frame lacks pill %q:\n%s", tc.pill, content)
			}
			var pillStyle string
			switch tc.pill {
			case "● UP":
				pillStyle = m.th.statusOK.Render(tc.pill)
			case "● DOWN":
				pillStyle = m.th.statusErr.Render(tc.pill)
			case "● WAIT":
				pillStyle = m.th.statusWarn.Render(tc.pill)
			}
			if !strings.Contains(content, pillStyle) {
				t.Errorf("pill %q must render through its status style %q", tc.pill, pillStyle)
			}
			for _, absent := range tc.absent {
				if strings.Contains(content, absent) {
					t.Errorf("pill %q must be absent while %s", absent, tc.name)
				}
			}
		})
	}
}

// runUpdateInit drives Init's fetch through the scripted fake so the
// model reaches its first real state (up or down) before rendering.
func runUpdateInit(t *testing.T, m Model) Model {
	t.Helper()
	msg := runCmd(t, m.Init())
	nm, _ := update(t, m, msg)
	return nm
}

// --- redesign: status stats block -----------------------------------------

// TestStatusStatsRowStyling pins the up-state status block: labels render
// as dim runs, values as accent-bold runs (the header weight), and both
// latency windows share one stats line. The pinned display text must
// survive verbatim as text — styling only wraps the runs, so every marker
// still matches on stripANSI (the hardening matrix matches on display
// text for exactly this reason). Raw runs across a label/value boundary
// must NOT be contiguous, which proves the split actually happened.
func TestStatusStatsRowStyling(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content
	stripped := stripANSI(content)

	// Display text unchanged (the historical markers, as text).
	for _, want := range []string{
		"daemon: up | listen: 127.0.0.1:8787 | pid: 4242 | uptime: 7s",
		"ip: 198.51.100.9",
		"latency ttfb: direct 12/14ms (n=3)",
		"latency stream: direct 50/55ms (n=2)",
	} {
		if !strings.Contains(stripped, want) {
			t.Errorf("display text lost marker %q:\n%s", want, stripped)
		}
	}

	// Both latency windows sit on ONE stats line.
	merged := false
	for _, line := range strings.Split(stripped, "\n") {
		if strings.Contains(line, "latency ttfb") && strings.Contains(line, "latency stream") {
			merged = true
		}
	}
	if !merged {
		t.Errorf("latency windows must share one stats line:\n%s", stripped)
	}

	// Styling: state chunk colored, labels dim, values accent-bold — and
	// the runs must be split (no raw adjacency across the boundary).
	if !strings.Contains(content, m.th.statusOK.Render("daemon: up")) {
		t.Error("state chunk must render through statusOK")
	}
	if !strings.Contains(content, m.th.dimText.Render("ip: ")) {
		t.Error("label run must render through dimText")
	}
	if !strings.Contains(content, m.th.header.Render("198.51.100.9")) {
		t.Error("value run must render accent-bold (header weight)")
	}
	if !strings.Contains(content, m.th.dimText.Render("latency ttfb: ")) {
		t.Error("latency label run must render through dimText")
	}
	if strings.Contains(content, "ip: 198.51.100.9") {
		t.Error("label/value boundary must be a style-run boundary (raw contiguous)")
	}
	if strings.Contains(content, "daemon: up | listen:") {
		t.Error("state/label boundary must be a style-run boundary (raw contiguous)")
	}
}

// TestStatusStatsRowDegenerate: the no-samples latency pair merges into
// the same single stats line (display text preserved).
func TestStatusStatsRowDegenerate(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))
	m = runUpdateInit(t, m)
	stripped := stripANSI(m.View().Content)

	merged := false
	for _, line := range strings.Split(stripped, "\n") {
		if strings.Contains(line, "latency ttfb: no samples") &&
			strings.Contains(line, "latency stream: no samples") {
			merged = true
		}
	}
	if !merged {
		t.Errorf("degenerate latency windows must share one stats line:\n%s", stripped)
	}
}

// TestDownWaitStatusColoring: the down line renders as one error-colored
// run (so the pinned "daemon: down (" marker stays raw-contiguous), the
// wait line as one warn-colored run.
func TestDownWaitStatusColoring(t *testing.T) {
	down := runUpdateInit(t, New(newFake(fakeResult{err: errors.New("connection refused")})))
	if !strings.Contains(down.View().Content, down.th.statusErr.Render("daemon: down (connection refused)")) {
		t.Errorf("down line must render as one statusErr run:\n%s", down.View().Content)
	}

	wait := New(newFake(fakeResult{}))
	if !strings.Contains(wait.View().Content, wait.th.statusWarn.Render("daemon: waiting for the first status poll...")) {
		t.Errorf("wait line must render as one statusWarn run:\n%s", wait.View().Content)
	}
}

// --- redesign: log level colors -------------------------------------------

// TestLogLevelColoring pins substring-driven log coloring: lines with
// "warn:" render in the warn color, lines with "error" in the error
// color, everything else stays raw text. Equality against the same-
// process theme render makes the assertion profile-proof; the pins run
// against the full View so the whole path (applyLog → viewport → panel)
// is covered.
func TestLogLevelColoring(t *testing.T) {
	m := dashboardModel(t)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.logLines = []string{
		"info: daemon ready",
		"2024-01-02T03:04:05Z warn: upstream slow",
		"level=error quota exhausted",
	}
	m.applyLog()

	content := m.View().Content
	warnLine := "2024-01-02T03:04:05Z warn: upstream slow"
	errLine := "level=error quota exhausted"
	if !strings.Contains(content, m.th.statusWarn.Render(warnLine)) {
		t.Errorf("warn line must render as one statusWarn run:\n%s", content)
	}
	if !strings.Contains(content, m.th.statusErr.Render(errLine)) {
		t.Errorf("error line must render as one statusErr run:\n%s", content)
	}
	if !strings.Contains(content, "info: daemon ready") {
		t.Errorf("plain line lost from the log tail:\n%s", content)
	}
	if strings.Contains(content, m.th.statusWarn.Render("info: daemon ready")) ||
		strings.Contains(content, m.th.statusErr.Render("info: daemon ready")) {
		t.Errorf("plain line must stay uncolored:\n%s", content)
	}
}

// TestLogTailErrorColored: the log-fetch error notice routes through the
// same substring rule ("error" → err color).
func TestLogTailErrorColored(t *testing.T) {
	m := dashboardModel(t)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m.logErr = errors.New("tail pipe closed")
	content := m.View().Content
	if !strings.Contains(content, m.th.statusErr.Render("log tail error: tail pipe closed")) {
		t.Errorf("log tail error notice must render in the err color:\n%s", content)
	}
}

// --- redesign: narrow-width column fit -------------------------------------

// TestColumnTitlesNeverEllipsized: shrinking a quota table at a narrow
// window must never eat a column title — the fit floors every column at
// its title's rune width, so "Resets in" and friends survive verbatim as
// display text at every width the layout claims to fit (>=30).
func TestColumnTitlesNeverEllipsized(t *testing.T) {
	for _, width := range []int{30, 34, 40} {
		m := dashboardModel(t)
		m, _ = update(t, m, tea.WindowSizeMsg{Width: width, Height: 24})
		display := stripANSI(m.View().Content)
		for _, want := range []string{
			"Resets in", "Egress", "Key", "OK", "429",
		} {
			if !strings.Contains(display, want) {
				t.Errorf("width %d: column title %q ellipsized or lost:\n%s", width, want, display)
			}
		}
	}
}

// --- redesign: help line styling -------------------------------------------

// TestHelpLineThemeStyles pins the footer help to the theme: keys render
// in the accent-bold weight, descriptions and the " • " separator in the
// dim color — never the bubbles defaults. Byte-equality against the
// same-process theme render keeps the assertion profile-proof.
func TestHelpLineThemeStyles(t *testing.T) {
	m := dashboardModel(t)
	content := m.View().Content

	if !strings.Contains(content, m.th.dimText.Render("quit")) {
		t.Errorf("help description must render in dimText:\n%s", content)
	}
	if !strings.Contains(content, m.th.header.Render("q")) {
		t.Errorf("help key must render in the accent-bold weight:\n%s", content)
	}
	if !strings.Contains(content, m.th.dimText.Render(" • ")) {
		t.Errorf("help separator must render in dimText:\n%s", content)
	}
}
