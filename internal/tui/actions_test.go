package tui

// Task 6 (Phase C): spec §7 r/d/w/s actions — rotate / force direct|warp /
// start-stop — through the injected ActionSource (asserted on the status
// source) and the Spawner seam, with the in-flight guard, the spinner while
// a request is pending, and action errors surfaced in the status area.
//
// Hermeticity contract (task brief):
//   - the action fake records Rotate/Use/Stop calls, the recording spawner
//     records spawn calls — no live daemon (127.0.0.1:8787), no network,
//     no systemctl, and no process is ever launched (the recording fake is
//     what runs instead of exec);
//   - messages are fed straight into Update() — no tea.Program;
//   - commands executed in tests: the action batch (spinner kick returns a
//     message immediately; the request runs the recording fake) and fetch
//     commands through fakeSource. tea.Tick commands are never executed.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// --- fake ActionSource ------------------------------------------------------

// actionFake records every control action. It satisfies StatusSource via
// the embedded fakeSource AND the ActionSource seam (Rotate/Use/Stop), so
// New discovers the actions by assertion — no network is ever touched.
type actionFake struct {
	*fakeSource
	rotateCalls int
	useCalls    int
	useModes    []string
	stopCalls   int
	rotateErr   error
	stopErr     error
}

func (a *actionFake) Rotate(context.Context) (string, error) {
	a.rotateCalls++
	if a.rotateErr != nil {
		return "", a.rotateErr
	}
	return "warp", nil
}

func (a *actionFake) Use(_ context.Context, mode string) (string, error) {
	a.useCalls++
	a.useModes = append(a.useModes, mode)
	return mode, nil
}

func (a *actionFake) Stop(context.Context) error {
	a.stopCalls++
	return a.stopErr
}

// newActionFake wires a recording ActionSource with scripted status
// results (last one repeats, like newFake).
func newActionFake(results ...fakeResult) *actionFake {
	return &actionFake{fakeSource: newFake(results...)}
}

// --- helpers ----------------------------------------------------------------

// actionKey feeds one spec §7 key into Update.
func actionKey(t *testing.T, m Model, k string) (Model, tea.Cmd) {
	t.Helper()
	return update(t, m, tea.KeyPressMsg{Code: rune(k[0]), Text: k})
}

// runActionBatch executes the command an action key press returned and
// returns the completion message to feed back into Update. It understands
// a tea.Batch (spinner kick + request): the kick returns a message without
// sleeping and is skipped here (tests feed spinner.TickMsg explicitly),
// the request runs against the recording fake. Nothing networked, no
// process, no tea.Tick.
func runActionBatch(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg := runCmd(t, cmd)
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return msg // a direct (non-batched) completion is fine too
	}
	var done tea.Msg
	for _, child := range batch {
		out := child()
		if _, isSpinner := out.(spinner.TickMsg); isSpinner {
			continue
		}
		if done != nil {
			t.Fatal("action batch produced more than one completion message")
		}
		done = out
	}
	if done == nil {
		t.Fatal("action batch produced no completion message")
	}
	return done
}

// inflightLine returns the rendered status line that reports the pending
// action (fatal when absent).
func inflightLine(t *testing.T, m Model) string {
	t.Helper()
	for _, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(line, "in flight") {
			return line
		}
	}
	t.Fatalf("view has no in-flight action line:\n%s", m.View().Content)
	return ""
}

// --- r / d / w / s(up) ------------------------------------------------------

// TestRotateKeyFiresRotateRequest: `r` (spec §7 "rotate now") fires exactly
// one Rotate request through the seam. The command carries the I/O — until
// it runs the fake records nothing — and while it is in flight the view
// shows the spinner + label. Feeding the completion back clears the line,
// lands the single call, and emits no further command (the 1s poll chain
// owns its own ticks).
func TestRotateKeyFiresRotateRequest(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}
	if af.rotateCalls != 0 {
		t.Fatalf("Rotate calls = %d before the command ran, want 0 (I/O lives in the command)",
			af.rotateCalls)
	}
	if line := inflightLine(t, m); !strings.Contains(line, "rotate") {
		t.Errorf("in-flight line %q lacks the action label \"rotate\"", line)
	}

	done := runActionBatch(t, cmd)
	m, fin := update(t, m, done)
	if fin != nil {
		t.Errorf("an action completion must not emit a command (poll chain owns ticks), got %v", fin)
	}
	if af.rotateCalls != 1 {
		t.Errorf("Rotate calls = %d, want exactly 1", af.rotateCalls)
	}
	if strings.Contains(m.View().Content, "in flight") {
		t.Errorf("in-flight line must disappear after completion:\n%s", m.View().Content)
	}
}

// TestUseKeysForceDirectAndWarp: `d` forces direct, `w` forces warp
// (spec §7), each through Use with the right mode argument.
func TestUseKeysForceDirectAndWarp(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	for _, k := range []string{"d", "w"} {
		m, cmd := actionKey(t, m, k)
		if cmd == nil {
			t.Fatalf("`%s` must return the use request command", k)
		}
		m, _ = update(t, m, runActionBatch(t, cmd))
	}
	if af.useCalls != 2 {
		t.Errorf("Use calls = %d, want 2", af.useCalls)
	}
	if got := strings.Join(af.useModes, ","); got != "direct,warp" {
		t.Errorf("Use modes = %q, want \"direct,warp\"", got)
	}
}

// TestStopWhenDaemonUp: `s` with the daemon up stops it through the seam
// (the spawn half of `s` is covered by TestSpawnWhenDaemonDownViaSpawner).
func TestStopWhenDaemonUp(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "s")
	if cmd == nil {
		t.Fatal("`s` must return the stop command when the daemon is up")
	}
	m, _ = update(t, m, runActionBatch(t, cmd))
	if af.stopCalls != 1 {
		t.Errorf("Stop calls = %d, want exactly 1", af.stopCalls)
	}
}

// --- in-flight guard --------------------------------------------------------

// TestActionInFlightGuardIgnoresKeys pins the chosen semantics: while a
// request is in flight every action key is IGNORED — no command, no state
// change, no seam call (they are neither queued nor fired late).
func TestActionInFlightGuardIgnoresKeys(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}
	before := m.View().Content
	for _, k := range []string{"r", "d", "w", "s"} {
		m, second := actionKey(t, m, k)
		if second != nil {
			t.Errorf("key %q fired while a request was in flight (want ignored)", k)
		}
		if after := m.View().Content; after != before {
			t.Errorf("key %q changed the view while in flight:\nbefore:\n%s\nafter:\n%s",
				k, before, after)
		}
	}
	if af.rotateCalls != 0 || af.useCalls != 0 || af.stopCalls != 0 {
		t.Fatalf("seam calls while in flight: rotate=%d use=%d stop=%d, want all 0",
			af.rotateCalls, af.useCalls, af.stopCalls)
	}

	_ = runActionBatch(t, cmd)
	if af.rotateCalls != 1 {
		t.Errorf("Rotate calls = %d after completion, want exactly 1", af.rotateCalls)
	}
}

// --- errors -----------------------------------------------------------------

// TestActionErrorSurfacesInView: a failed request (e.g. rotate hitting a
// 409) renders in the status/error area — "rotate failed: … HTTP 409 …" —
// and stays visible across a status poll result (the 1s cadence must not
// swallow it before the user can read it).
func TestActionErrorSurfacesInView(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	af.rotateErr = errors.New("control POST rotate: HTTP 409: rotation already in progress")
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}
	m, _ = update(t, m, runActionBatch(t, cmd))

	for _, want := range []string{"rotate failed", "HTTP 409"} {
		if !strings.Contains(m.View().Content, want) {
			t.Errorf("view lacks %q after a failed rotate:\n%s", want, m.View().Content)
		}
	}

	// A poll result (statusMsg) must not clear the message.
	m, _ = update(t, m, statusMsg{status: upStatus()})
	if !strings.Contains(m.View().Content, "HTTP 409") {
		t.Errorf("action error must survive a status poll:\n%s", m.View().Content)
	}
}

// --- spinner ----------------------------------------------------------------

// TestSpinnerRunsWhileActionPending: the in-flight line carries a live
// spinner (bubbles/v2) — feeding a spinner.TickMsg advances the frame, the
// completion stops the animation, and a late tick after completion is
// swallowed (no orphan tick loop).
func TestSpinnerRunsWhileActionPending(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}

	before := inflightLine(t, m)
	m, rearm := update(t, m, spinner.TickMsg{})
	if rearm == nil {
		t.Error("a spinner tick while pending must re-arm the animation")
	}
	if after := inflightLine(t, m); after == before {
		t.Errorf("spinner frame did not advance (line stayed %q)", before)
	}

	m, _ = update(t, m, runActionBatch(t, cmd))
	if strings.Contains(m.View().Content, "in flight") {
		t.Errorf("in-flight line must disappear after completion:\n%s", m.View().Content)
	}
	// Late tick: the animation loop must be stopped while idle.
	m, orphan := update(t, m, spinner.TickMsg{})
	if orphan != nil {
		t.Error("a spinner tick after completion must not re-arm the loop (idle model)")
	}
}

// --- quit / poll chain / layout ---------------------------------------------

// TestQuitWhileActionInFlight: `q` stays a quit key even with a request in
// flight (quit semantics unchanged by Task 6).
func TestQuitWhileActionInFlight(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}
	m, quit := actionKey(t, m, "q")
	if quit == nil {
		t.Fatal("`q` must still quit while a request is in flight")
	}
	if _, ok := runCmd(t, quit).(tea.QuitMsg); !ok {
		t.Fatalf("`q` produced %T, want tea.QuitMsg", runCmd(t, quit))
	}
}

// TestPollChainKeepsRunningWhileActionPending pins the documented choice:
// the spec §7 1s poll KEEPS RUNNING during an in-flight request (it is
// neither paused nor allowed to clear the action state), and the guard
// stays armed until the completion lands.
func TestPollChainKeepsRunningWhileActionPending(t *testing.T) {
	af := newActionFake(fakeResult{status: upStatus()})
	m := New(af)
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}

	m, fetch := update(t, m, pollMsg(time.Now()))
	if fetch == nil {
		t.Fatal("pollMsg must still trigger a fetch while a request is in flight")
	}
	m, tick := update(t, m, runCmd(t, fetch))
	if tick == nil {
		t.Fatal("a poll result must still schedule the next tick while a request is in flight")
	}
	if !strings.Contains(m.View().Content, "in flight") {
		t.Errorf("the status poll must not clear the in-flight line:\n%s", m.View().Content)
	}

	// Still guarded after the poll cycle.
	m, second := actionKey(t, m, "r")
	if second != nil {
		t.Error("the guard must stay armed across a poll cycle")
	}

	m, _ = update(t, m, runActionBatch(t, cmd))
	if af.rotateCalls != 1 {
		t.Errorf("Rotate calls = %d, want exactly 1", af.rotateCalls)
	}
	if strings.Contains(m.View().Content, "in flight") {
		t.Errorf("in-flight line must disappear after completion:\n%s", m.View().Content)
	}
}

// TestViewFitsTerminalSizeWhileActionPending: the status block gains the
// action line while a request is pending — the layout budget must absorb
// it so the help line stays on-screen (spec §7 fit invariant).
func TestViewFitsTerminalSizeWhileActionPending(t *testing.T) {
	st := statusFromJSON(t, dashboardJSON(t))
	m := New(
		&actionFake{fakeSource: newFake(fakeResult{status: st})},
		WithLogTail(&fakeLogSource{lines: []string{"daemon starting"}}),
	)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	m, cmd := actionKey(t, m, "r")
	if cmd == nil {
		t.Fatal("`r` must return the rotate request command")
	}
	content := m.View().Content
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) > 24 {
		t.Errorf("view with a pending action renders %d lines, want <= 24:\n%s",
			len(lines), content)
	}
	for _, want := range []string{"in flight", "quit"} {
		if !strings.Contains(content, want) {
			t.Errorf("view lacks %q while a request is pending:\n%s", want, content)
		}
	}
}

// --- s on a down daemon: the Spawner seam -----------------------------------

// recordingSpawner is the injected Spawner fake: it records the spawn call
// and the context it received — a process is never actually launched (the
// fake replaces exec/detachUp entirely, keeping the suite hermetic).
type recordingSpawner struct {
	calls       int
	gotDeadline bool
	err         error
}

func (s *recordingSpawner) spawn(ctx context.Context) error {
	s.calls++
	_, s.gotDeadline = ctx.Deadline()
	return s.err
}

// TestSpawnWhenDaemonDownViaSpawner: `s` with the daemon down fires the
// injected Spawner seam — the process-spawn path that StatusSource polling
// deliberately does not cover. The recording fake proves EXACTLY ONE spawn
// call and that no live process was ever launched; the ctx handed to it is
// bounded (actionTimeout).
func TestSpawnWhenDaemonDownViaSpawner(t *testing.T) {
	af := newActionFake(fakeResult{err: errDaemonDown})
	sp := &recordingSpawner{}
	m := New(af, WithSpawner(sp.spawn))
	m, _ = update(t, m, runCmd(t, m.Init()))
	if !strings.Contains(m.View().Content, "daemon: down") {
		t.Fatalf("fixture must start with the daemon down:\n%s", m.View().Content)
	}

	m, cmd := actionKey(t, m, "s")
	if cmd == nil {
		t.Fatal("`s` must return the spawn command when the daemon is down")
	}
	if sp.calls != 0 {
		t.Fatalf("spawner ran before its command executed: calls = %d, want 0", sp.calls)
	}
	if line := inflightLine(t, m); !strings.Contains(line, "start") {
		t.Errorf("in-flight line %q lacks the action label", line)
	}

	// In-flight guard: every action key is ignored while the spawn runs.
	before := m.View().Content
	for _, k := range []string{"s", "r", "d", "w"} {
		m, second := actionKey(t, m, k)
		if second != nil {
			t.Errorf("key %q fired while the spawn was in flight (want ignored)", k)
		}
		if after := m.View().Content; after != before {
			t.Errorf("key %q changed the view while the spawn was in flight", k)
		}
	}

	m, _ = update(t, m, runActionBatch(t, cmd))
	if sp.calls != 1 {
		t.Errorf("spawn calls = %d, want exactly ONE", sp.calls)
	}
	if !sp.gotDeadline {
		t.Error("spawn ctx must carry a deadline (actionTimeout)")
	}
	if sp.calls != af.rotateCalls+af.useCalls+af.stopCalls+1 {
		t.Errorf("control actions fired during spawn: rotate=%d use=%d stop=%d, want 0",
			af.rotateCalls, af.useCalls, af.stopCalls)
	}
	if strings.Contains(m.View().Content, "in flight") {
		t.Errorf("in-flight line must disappear after completion:\n%s", m.View().Content)
	}
}

// TestSpawnErrorSurfacesInView: a failed spawn (real detach path: daemon
// exited before readiness) renders in the status/error area like any other
// action error.
func TestSpawnErrorSurfacesInView(t *testing.T) {
	af := newActionFake(fakeResult{err: errDaemonDown})
	sp := &recordingSpawner{err: errors.New("daemon exited before becoming ready")}
	m := New(af, WithSpawner(sp.spawn))
	m, _ = update(t, m, runCmd(t, m.Init()))

	m, cmd := actionKey(t, m, "s")
	if cmd == nil {
		t.Fatal("`s` must return the spawn command when the daemon is down")
	}
	m, _ = update(t, m, runActionBatch(t, cmd))

	for _, want := range []string{"start daemon failed", "exited before becoming ready"} {
		if !strings.Contains(m.View().Content, want) {
			t.Errorf("view lacks %q after a failed spawn:\n%s", want, m.View().Content)
		}
	}
	if sp.calls != 1 {
		t.Errorf("spawn calls = %d, want exactly 1", sp.calls)
	}
}
