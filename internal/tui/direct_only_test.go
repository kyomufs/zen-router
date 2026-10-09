package tui

// Direct-only contract (the WARP/identity/rotation/spare excision):
// keyboard, help surfaces and the status block may expose only what the
// direct-only dashboard still offers. Added in Phase B, BEFORE the removal
// of the r/d/w bindings and their help entries — while the keys are still
// bound, TestRotateDirectWarpKeysAreInert, TestRemovedKeysDoNotTouchStopConfirmation
// and TestHelpSurfacesListOnlyRemainingKeys must FAIL (RED), then go green
// in Phase C. TestStatusBlockHasNoRemovedRows pins the status-block half of
// the contract (the removed rows are deleted in Phase A, where they block
// compilation; the test would have failed on the pre-Phase-A tree).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestRotateDirectWarpKeysAreInert: with an ActionSource wired (seams
// present, so the old bindings had every opportunity to fire), pressing
// r, d or w must do nothing at all — no command, no pending action, no
// frame change. The direct-only view offers no rotate/use action to start.
func TestRotateDirectWarpKeysAreInert(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))
	before := m.View().Content

	for _, k := range []string{"r", "d", "w"} {
		next, cmd := actionKey(t, m, k)
		if cmd != nil {
			t.Errorf("key %q must be inert in the direct-only view, got a command", k)
		}
		if next.pending {
			t.Errorf("key %q must not start an action (pending is set)", k)
		}
		if got := next.View().Content; got != before {
			t.Errorf("key %q must not change the frame:\nwant:\n%s\ngot:\n%s",
				k, before, got)
		}
	}
}

// TestRemovedKeysDoNotTouchStopConfirmation: while the two-step stop
// confirmation is armed, the removed keys must neither fire a command nor
// disarm the prompt — they are simply not bound anymore (esc/tab/? keep
// disarming, covered by the stop-confirmation tests).
func TestRemovedKeysDoNotTouchStopConfirmation(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	armed, cmd := actionKey(t, m, "s")
	if cmd != nil {
		t.Fatal("first `s` must arm the confirmation, not fire the stop")
	}
	armedFrame := armed.View().Content
	if !strings.Contains(armedFrame, "s again to confirm") {
		t.Fatalf("stop confirmation not armed:\n%s", armedFrame)
	}

	for _, k := range []string{"r", "d", "w"} {
		next, cmd := actionKey(t, armed, k)
		if cmd != nil {
			t.Errorf("key %q must not fire a command while stop is armed, got a command", k)
		}
		if !next.confirmStop {
			t.Errorf("key %q must not disarm the stop confirmation", k)
		}
		if got := next.View().Content; got != armedFrame {
			t.Errorf("key %q must not change the armed frame:\nwant:\n%s\ngot:\n%s",
				k, armedFrame, got)
		}
	}
}

// TestHelpSurfacesListOnlyRemainingKeys: neither the footer help line
// (keyMap.ShortHelp) nor the `?` overlay (helpOverlayRows) may list the
// rotate/direct/warp bindings — only quit, start/stop and panel navigation
// remain.
func TestHelpSurfacesListOnlyRemainingKeys(t *testing.T) {
	m := smallModel(t)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})

	footer := m.View().Content
	for _, gone := range []string{"rotate now", "direct egress", "warp egress"} {
		if strings.Contains(footer, gone) {
			t.Errorf("footer help must not list %q:\n%s", gone, footer)
		}
	}
	for _, keep := range []string{"quit", "start/stop daemon"} {
		if !strings.Contains(footer, keep) {
			t.Errorf("footer help must still list %q:\n%s", keep, footer)
		}
	}

	m, cmd := update(t, m, runeKey('?'))
	if cmd != nil {
		t.Fatal("? must not emit a command")
	}
	open := m.View().Content
	for _, gone := range []string{"rotate now", "direct egress", "warp egress"} {
		if strings.Contains(open, gone) {
			t.Errorf("help overlay must not list %q:\n%s", gone, open)
		}
	}
	for _, keep := range []string{
		"quit", "start/stop daemon", "next panel", "previous panel", "close help",
	} {
		if !strings.Contains(open, keep) {
			t.Errorf("help overlay must still list %q:\n%s", keep, open)
		}
	}
}

// TestStatusBlockHasNoRemovedRows: the up-daemon status block reports only
// what survives the excision — no mode/current row, no last-rotate /
// rotating / registering indicators, no spare-registration error row — and
// keeps the direct-only rows (listen/pid/uptime, egress IP, latencies).
func TestStatusBlockHasNoRemovedRows(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))
	content := m.View().Content

	for _, gone := range []string{
		"mode:", "egress: ", "last rotate", "rotating:", "registering:",
		"spare registration",
	} {
		if strings.Contains(content, gone) {
			t.Errorf("status block must not render %q:\n%s", gone, content)
		}
	}
	for _, keep := range []string{
		"daemon: up", "listen:", "uptime:", "ip:", "latency ttfb:", "latency stream:",
	} {
		if !strings.Contains(content, keep) {
			t.Errorf("status block must still render %q:\n%s", keep, content)
		}
	}
}
