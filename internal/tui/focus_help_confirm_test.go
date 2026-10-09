package tui

// Focus / help-overlay / stop-confirmation contract (approved redesign —
// RED first):
//
//   - tab / shift+tab cycle the focused panel (egress quota -> key quota ->
//     log tail -> wrap). The focused
//     panel is highlighted, so every step changes the frame and a full
//     cycle of focusPanels steps restores it byte-identical;
//   - ? toggles a full-screen keyboard overlay: while open the dashboard
//     is replaced by every binding, ? closes it back to the exact previous
//     frame, and both frames fit the terminal;
//   - `s` on an UP daemon arms a confirmation on the first press (prompt
//     rendered, NO seam call, NO command) and fires the stop on the second;
//     esc cancels. Starting the daemon (down state) stays a single press —
//     it is not destructive.

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// focusPanels: number of focusable panels in the tab order (egress quota,
// key quota, log tail).
const focusPanels = 3

func tabMsg() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab}
}

func shiftTabMsg() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
}

func runeKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// --- focus cycling ----------------------------------------------------------

// TestTabCyclesPanelFocus: `tab` moves the focus forward — the frame must
// visibly change (accent border) — and a full cycle returns to the exact
// initial frame. Focus never emits a command.
func TestTabCyclesPanelFocus(t *testing.T) {
	m := smallModel(t)
	base := m.View().Content

	m, cmd := update(t, m, tabMsg())
	if cmd != nil {
		t.Error("tab must not emit a command")
	}
	if got := m.View().Content; got == base {
		t.Fatalf("first tab must move the focus (frame unchanged):\n%s", got)
	}
	for i := 2; i <= focusPanels; i++ {
		m, _ = update(t, m, tabMsg())
	}
	if got := m.View().Content; got != base {
		t.Errorf("%d tabs must restore the initial focus frame", focusPanels)
	}
}

// TestShiftTabCyclesFocusBackwards: shift+tab walks the order backwards —
// undoing a tab restores the previous frame exactly.
func TestShiftTabCyclesFocusBackwards(t *testing.T) {
	m := smallModel(t)
	base := m.View().Content

	m, _ = update(t, m, tabMsg())
	forward := m.View().Content
	if forward == base {
		t.Fatal("tab must move the focus first")
	}

	m, cmd := update(t, m, shiftTabMsg())
	if cmd != nil {
		t.Error("shift+tab must not emit a command")
	}
	if got := m.View().Content; got != base {
		t.Errorf("shift+tab after tab must restore the previous frame")
	}
	m, _ = update(t, m, shiftTabMsg())
	if got := m.View().Content; got == base {
		t.Errorf("another shift+tab must keep cycling (frame unchanged)")
	}
}

// --- help overlay -----------------------------------------------------------

// TestHelpOverlayToggles: `?` swaps the dashboard for a full-screen
// keyboard overlay listing every binding; a second `?` restores the exact
// previous frame. Purity: repeated renders while open are byte-identical.
func TestHelpOverlayToggles(t *testing.T) {
	m := smallModel(t)
	before := m.View().Content

	m, cmd := update(t, m, runeKey('?'))
	if cmd != nil {
		t.Error("? must not emit a command")
	}
	open := m.View().Content
	if open == before {
		t.Fatalf("? must open the help overlay (frame unchanged):\n%s", open)
	}
	// The overlay REPLACES the dashboard — no grid/log panels underneath.
	for _, absent := range []string{
		"quota (per egress)",
		"quota (per key)",
		"identity pool",
		"rotation history",
		"log tail",
	} {
		if strings.Contains(open, absent) {
			t.Errorf("overlay must replace the dashboard, %q still present:\n%s", absent, open)
		}
	}
	// Every binding is listed.
	for _, want := range []string{
		"quit",
		"start/stop daemon",
		"next panel",
		"previous panel",
		"close help",
	} {
		if !strings.Contains(open, want) {
			t.Errorf("overlay lacks binding %q:\n%s", want, open)
		}
	}
	if stable := m.View().Content; stable != open {
		t.Errorf("overlay frame not stable across renders:\nfirst:\n%s\nagain:\n%s", open, stable)
	}

	m, cmd = update(t, m, runeKey('?'))
	if cmd != nil {
		t.Error("? must not emit a command on close")
	}
	if got := m.View().Content; got != before {
		t.Errorf("second ? must restore the dashboard byte-identical:\nwant:\n%s\ngot:\n%s", before, got)
	}
}

// TestHelpOverlayFitsTerminal: with the overlay open the whole frame still
// fits common terminal sizes — every line within the width, the frame
// within the height, the quit binding reachable.
func TestHelpOverlayFitsTerminal(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{80, 24}, {60, 30}} {
		m := smallModel(t)
		m, _ = update(t, m, tea.WindowSizeMsg{Width: tc.w, Height: tc.h})
		m, _ = update(t, m, runeKey('?'))
		content := m.View().Content

		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		if len(lines) > tc.h {
			t.Errorf("%dx%d overlay renders %d lines, want <= %d:\n%s",
				tc.w, tc.h, len(lines), tc.h, content)
		}
		for _, line := range lines {
			if n := utf8.RuneCountInString(stripANSI(line)); n > tc.w {
				t.Errorf("%dx%d overlay line has %d cells, want <= %d: %q",
					tc.w, tc.h, n, tc.w, line)
			}
		}
		if !strings.Contains(content, "quit") {
			t.Errorf("%dx%d overlay lacks the quit binding:\n%s", tc.w, tc.h, content)
		}
	}
}

// --- stop confirmation ------------------------------------------------------

// TestStopDaemonNeedsSecondPress: the first `s` on an up daemon only arms
// the confirmation (prompt on screen, no command, no seam call); the
// second `s` fires the stop exactly once and clears the prompt.
func TestStopDaemonNeedsSecondPress(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))
	before := m.View().Content

	m, cmd := actionKey(t, m, "s")
	if cmd != nil {
		t.Fatal("first `s` must arm the confirmation, not fire the stop")
	}
	if af.stopCalls != 0 {
		t.Fatalf("stop fired on the first press: calls = %d", af.stopCalls)
	}
	armed := m.View().Content
	for _, want := range []string{"stop the daemon?", "s again to confirm"} {
		if !strings.Contains(armed, want) {
			t.Fatalf("confirmation prompt lacks %q:\n%s", want, armed)
		}
	}
	if armed == before {
		t.Fatal("confirmation prompt must change the frame")
	}
	if stable := m.View().Content; stable != armed {
		t.Error("armed frame not stable across renders")
	}

	m, cmd = actionKey(t, m, "s")
	if cmd == nil {
		t.Fatal("second `s` must fire the stop")
	}
	m, _ = update(t, m, runActionBatch(t, cmd))
	if af.stopCalls != 1 {
		t.Errorf("Stop calls = %d, want exactly 1", af.stopCalls)
	}
	if strings.Contains(m.View().Content, "s again to confirm") {
		t.Errorf("confirmation prompt must clear after the stop:\n%s", m.View().Content)
	}
}

// TestStopConfirmationCancelledByEsc: esc disarms the prompt without any
// seam call and restores the exact previous frame.
func TestStopConfirmationCancelledByEsc(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))
	before := m.View().Content

	m, cmd := actionKey(t, m, "s")
	if cmd != nil {
		t.Fatal("first `s` must arm the confirmation, not fire the stop")
	}
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd != nil {
		t.Error("esc must not emit a command")
	}
	after := m.View().Content
	if strings.Contains(after, "s again to confirm") {
		t.Errorf("esc must cancel the confirmation prompt:\n%s", after)
	}
	if after != before {
		t.Errorf("esc must restore the pre-confirmation frame:\nwant:\n%s\ngot:\n%s", before, after)
	}
	if af.stopCalls != 0 {
		t.Errorf("stop fired without confirmation: calls = %d", af.stopCalls)
	}
}

// TestStartDaemonNeedsNoConfirmation: the down-state half of `s` spawns the
// daemon on the FIRST press — starting is not destructive, no prompt.
func TestStartDaemonNeedsNoConfirmation(t *testing.T) {
	af := newActionFake(fakeResult{err: errDaemonDown})
	sp := &recordingSpawner{}
	m := New(af, WithSpawner(sp.spawn))
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "s")
	if cmd == nil {
		t.Fatal("`s` must return the spawn command on the first press (no confirmation)")
	}
	if strings.Contains(m.View().Content, "s again to confirm") {
		t.Errorf("start must not prompt for confirmation:\n%s", m.View().Content)
	}
	if sp.calls != 0 {
		t.Errorf("spawner ran before its command executed: calls = %d", sp.calls)
	}
}
