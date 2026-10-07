package tui

// Task 4 (Phase C): `internal/tui` skeleton on Bubble Tea v2 — model /
// Init / Update / View, the spec §7 1s tea.Tick-driven status poll through
// the injected StatusSource seam, the daemon-down start offer, and `q` to
// quit.
//
// Hermeticity contract (task brief):
//   - StatusSource is always a fake with scripted results — no network, no
//     live daemon (127.0.0.1:8787) can ever be reached from here;
//   - tests feed tea.KeyPressMsg / pollMsg / statusMsg straight into
//     Update() — no tea.Program is ever constructed;
//   - the Tick command returned by Update is NEVER executed (it would sleep
//     for 1s); only fetch commands (fake source) and tea.Quit are run.

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"zen-router/internal/cli"
)

// --- fake StatusSource ------------------------------------------------------

type fakeResult struct {
	status *cli.Status
	err    error
}

// fakeSource scripts StatusSource results in order; the last result repeats.
// It never touches the network.
type fakeSource struct {
	results []fakeResult
	calls   int
}

func newFake(results ...fakeResult) *fakeSource {
	if len(results) == 0 {
		panic("newFake: need at least one scripted result")
	}
	return &fakeSource{results: results}
}

func (f *fakeSource) Status(context.Context) (*cli.Status, error) {
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	r := f.results[i]
	return r.status, r.err
}

var errDaemonDown = errors.New("zen-router daemon is not running (start it with `zen-router up`)")

func upStatus() *cli.Status {
	return &cli.Status{
		Up:            true,
		Listen:        "127.0.0.1:8787",
		Pid:           4242,
		UptimeSeconds: 7,
		Mode:          "direct",
		Current:       "direct",
	}
}

// runCmd executes a non-Tick command (fetch through the fake source, or
// tea.Quit) and returns the message it produces.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command, got nil")
	}
	return cmd()
}

// update feeds one message into Update and returns the re-stated model.
func update(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned model type %T, want tui.Model", next)
	}
	return nm, cmd
}

// --- poll chain -------------------------------------------------------------

// TestInitFetchesStatusImmediately: Init kicks the chain off with a
// StatusSource fetch (a statusMsg — NOT a Tick; the Tick command must never
// run in tests), and a status result schedules the next 1s tick.
func TestInitFetchesStatusImmediately(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))

	initCmd := m.Init()
	if initCmd == nil {
		t.Fatal("Init must return the initial status fetch command")
	}
	msg := runCmd(t, initCmd) // fake source only: no network
	sm, ok := msg.(statusMsg)
	if !ok {
		t.Fatalf("Init command message = %T, want statusMsg (Init must fetch, not tick)", msg)
	}
	if sm.err != nil || sm.status == nil || !sm.status.Up {
		t.Fatalf("Init statusMsg = %+v, want an up status", sm)
	}

	m, tick := update(t, m, sm)
	if tick == nil {
		t.Fatal("a status result must schedule the next 1s tea.Tick poll")
	}
	// tick IS the tea.Tick command — never run it (recipe).
	if !strings.Contains(m.View().Content, "daemon: up") {
		t.Fatalf("view after first status must show the up state, got:\n%s", m.View().Content)
	}
}

// TestPollChainDaemonUpThenDown: pollMsg triggers the next fetch; the second
// fetch fails (daemon down) and the view switches to the spec §7 start offer.
func TestPollChainDaemonUpThenDown(t *testing.T) {
	src := newFake(
		fakeResult{status: upStatus()},
		fakeResult{err: errDaemonDown},
	)
	m := New(src)

	// Cycle 1: Init fetch → status up → schedule tick (not run).
	msg := runCmd(t, m.Init())
	m, _ = update(t, m, msg)

	// The tick fired: pollMsg → the next fetch command.
	m, fetch := update(t, m, pollMsg(time.Now()))
	if fetch == nil {
		t.Fatal("pollMsg must trigger a status fetch command")
	}
	msg = runCmd(t, fetch) // fake #2: ErrNotRunning — still no network
	sm := msg.(statusMsg)
	if !errors.Is(sm.err, errDaemonDown) {
		t.Fatalf("second fetch err = %v, want the scripted daemon-down error", sm.err)
	}

	m, nextTick := update(t, m, sm)
	if nextTick == nil {
		t.Fatal("even a failed poll must schedule the next 1s tick")
	}
	if src.calls != 2 {
		t.Fatalf("StatusSource calls = %d, want 2", src.calls)
	}

	content := m.View().Content
	for _, want := range []string{"daemon: down", "zen-router up", "XDG state"} {
		if !strings.Contains(content, want) {
			t.Errorf("daemon-down view must contain %q, got:\n%s", want, content)
		}
	}
}

// TestDownToUpRecovery: after a down poll, the next successful statusMsg
// must clear the error — the start offer disappears and `daemon: up`
// returns (review F1). Regression pin for the statusMsg arm of Update; the
// pre-fix code already behaved this way, so no RED was possible — the test
// is mutation-verified instead (breaking err-clearing makes it fail; see
// the fix report).
func TestDownToUpRecovery(t *testing.T) {
	src := newFake(
		fakeResult{status: upStatus()},
		fakeResult{err: errDaemonDown},
		fakeResult{status: upStatus()},
	)
	m := New(src)

	// Cycle 1: Init fetch → up.
	msg := runCmd(t, m.Init())
	m, _ = update(t, m, msg)
	if !strings.Contains(m.View().Content, "daemon: up") {
		t.Fatalf("cycle 1 must be up, got:\n%s", m.View().Content)
	}

	// Cycle 2: tick → fetch → down (start offer appears).
	m, fetch := update(t, m, pollMsg(time.Now()))
	if fetch == nil {
		t.Fatal("pollMsg must trigger a status fetch command")
	}
	m, _ = update(t, m, runCmd(t, fetch))
	down := m.View().Content
	if !strings.Contains(down, "daemon: down") || !strings.Contains(down, "XDG state") {
		t.Fatalf("cycle 2 must show the down state and start offer, got:\n%s", down)
	}

	// Cycle 3: tick → fetch → recovered.
	m, fetch = update(t, m, pollMsg(time.Now()))
	if fetch == nil {
		t.Fatal("pollMsg must trigger a status fetch command after recovery")
	}
	m, nextTick := update(t, m, runCmd(t, fetch))
	if nextTick == nil {
		t.Fatal("a recovered poll must schedule the next 1s tick")
	}
	if src.calls != 3 {
		t.Fatalf("StatusSource calls = %d, want 3", src.calls)
	}

	content := m.View().Content
	if !strings.Contains(content, "daemon: up") {
		t.Errorf("recovered view must show `daemon: up`, got:\n%s", content)
	}
	if strings.Contains(content, "XDG state") {
		t.Errorf("start offer must disappear after recovery, got:\n%s", content)
	}
}

// --- view --------------------------------------------------------------------

// TestViewHeader: View().Content always carries the dashboard header, and
// the view declares the alternate screen (v2 tea.View).
func TestViewHeader(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))

	view := m.View()
	if !view.AltScreen {
		t.Error("dashboard view must declare AltScreen")
	}
	for _, want := range []string{"zen-router control dashboard", "poll: 1s"} {
		if !strings.Contains(view.Content, want) {
			t.Errorf("initial view must contain header %q, got:\n%s", want, view.Content)
		}
	}

	// After a poll the up state and its header fields are rendered.
	msg := runCmd(t, m.Init())
	m, _ = update(t, m, msg)
	content := m.View().Content
	for _, want := range []string{"daemon: up", "127.0.0.1:8787", "pid: 4242"} {
		if !strings.Contains(content, want) {
			t.Errorf("up view must contain %q, got:\n%s", want, content)
		}
	}
}

// --- keys --------------------------------------------------------------------

// TestQuitOnQ: `q` (spec §7) returns the tea.Quit command; ctrl+c is the
// conventional escape hatch and quits too.
func TestQuitOnQ(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"q", tea.KeyPressMsg{Code: 'q', Text: "q"}},
		{"ctrl+c", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(newFake(fakeResult{status: upStatus()}))
			m, cmd := update(t, m, tc.msg)
			if cmd == nil {
				t.Fatalf("%q must return the quit command", tc.name)
			}
			got := runCmd(t, cmd) // tea.Quit: instant, no Tick involved
			if _, ok := got.(tea.QuitMsg); !ok {
				t.Fatalf("%q command message = %T, want tea.QuitMsg", tc.name, got)
			}
		})
	}
}

// TestStubKeysAreNoOps: r/d/w/s are spec §7 keys owned by Tasks 5/6 — in the
// skeleton they must not quit, not emit commands, and not alter state.
func TestStubKeysAreNoOps(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))
	msg := runCmd(t, m.Init())
	m, _ = update(t, m, msg)
	before := m.View().Content

	for _, key := range []string{"r", "d", "w", "s"} {
		m, cmd := update(t, m, tea.KeyPressMsg{Code: rune(key[0]), Text: key})
		if cmd != nil {
			t.Errorf("key %q must be a no-op stub until Task 6, got a command", key)
		}
		if after := m.View().Content; after != before {
			t.Errorf("key %q must not change the view yet:\nbefore:\n%s\nafter:\n%s", key, before, after)
		}
	}
}

// TestUnknownKeyIgnored: anything else is silently ignored.
func TestUnknownKeyIgnored(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))
	before := m.View().Content
	m, cmd := update(t, m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	if cmd != nil {
		t.Errorf("unknown key must not emit a command, got %v", cmd)
	}
	if m.View().Content != before {
		t.Errorf("unknown key must not change the view")
	}
}

// --- architecture boundary ---------------------------------------------------

// TestImportBoundaries: the TUI is a pure control-API client — internal/cli
// (client types) is allowed; the daemon-side packages are not.
func TestImportBoundaries(t *testing.T) {
	forbidden := map[string]bool{
		"zen-router/internal/router":  true,
		"zen-router/internal/quota":   true,
		"zen-router/internal/gateway": true,
		"zen-router/internal/proxy":   true,
		"zen-router/internal/systemd": true,
		"zen-router/internal/config":  true, // daemon-side config paths (XDG dirs, unit files)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if forbidden[path] {
				t.Errorf("%s imports forbidden daemon-side package %q (tui may only import internal/cli)", name, path)
				continue
			}
			// Positive allowlist (Task 5): stdlib, the charm.land UI
			// stack, and the control client — nothing else. Keeps the
			// tui package a pure control-API client.
			first, _, _ := strings.Cut(path, "/")
			var allowed bool
			switch {
			case strings.HasPrefix(path, "zen-router/"):
				allowed = path == "zen-router/internal/cli"
			case strings.HasPrefix(path, "charm.land/"):
				allowed = true
			default:
				allowed = !strings.Contains(first, ".") // stdlib: no dot in first segment
			}
			if !allowed {
				t.Errorf("%s imports %q: tui may only import stdlib, charm.land/*, zen-router/internal/cli", name, path)
			}
		}
	}
}
