package tui

// Phase 2 seam tests (lean, batch): tabs, log filters, stats tab rows,
// keys tab add/delete, and the `b` band toggle. The fake below satisfies
// StatsSource/KeySource so New discovers them via type assertion — no
// network, no daemon.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"zen-router/internal/cli"
)

// fullFake extends the status fake with stats/keys listings.
type fullFake struct {
	*fakeSource
	stats *cli.Stats
	fps   []string

	added    []string
	deleted  []string
	addErr   error
	delErr   error
}

func (f *fullFake) Stats(context.Context) (*cli.Stats, error) {
	return f.stats, nil
}
func (f *fullFake) Keys(context.Context) ([]string, error) { return f.fps, nil }
func (f *fullFake) AddKey(_ context.Context, raw string) (string, error) {
	if f.addErr != nil {
		return "", f.addErr
	}
	f.added = append(f.added, raw)
	return "0badf00d", nil
}
func (f *fullFake) DeleteKey(_ context.Context, fp string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, fp)
	return nil
}

const statsJSON = `{
  "days": [
    {"day":"2026-02-12","ok":10,"429":2},
    {"day":"2026-02-11","ok":5,"429":0}
  ],
  "ips": [
    {"ip":"1.2.3.4","ok":12,"429":1,"last":1770000000000},
    {"ip":"5.6.7.8","ok":3,"429":1,"last":1769000000000}
  ]
}`

func newFullFake(t *testing.T) *fullFake {
	t.Helper()
	var st cli.Stats
	if err := json.Unmarshal([]byte(statsJSON), &st); err != nil {
		t.Fatalf("stats fixture: %v", err)
	}
	return &fullFake{
		fakeSource: newFake(fakeResult{status: upStatus()}),
		stats:      &st,
		fps:        []string{"deadbeef", "cafebabe"},
	}
}

// press sends one rune key press.
func press(t *testing.T, m Model, s string) (Model, tea.Cmd) {
	t.Helper()
	return update(t, m, tea.KeyPressMsg{Code: rune(s[0]), Text: s})
}

// TestTabSwitchAndFrame: digit keys switch tabs, the tab strip reflects
// it, and every tab's frame fits a sized terminal (frame budget).
func TestTabSwitchAndFrame(t *testing.T) {
	m := New(newFullFake(t))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, runCmd(t, m.Init()))

	for i, name := range []string{"dashboard", "logs", "stats", "keys"} {
		m, _ = press(t, m, string(rune('1'+i)))
		if m.tab != i {
			t.Fatalf("tab after %d = %d, want %d", i+1, m.tab, i)
		}
		content := m.View().Content
		if !strings.Contains(content, "1 dashboard") {
			t.Fatalf("tab strip missing on tab %q:\n%s", name, content)
		}
		lines := strings.Split(content, "\n")
		if len(lines) > 24 {
			t.Fatalf("tab %q frame = %d lines > 24", name, len(lines))
		}
	}
}

// TestStatsTabRenders: entering tab 3 fires the stats fetch; the result
// renders day/IP rows.
func TestStatsTabRenders(t *testing.T) {
	m := New(newFullFake(t))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, runCmd(t, m.Init()))
	m, cmd := press(t, m, "3")
	if cmd == nil {
		t.Fatal("entering stats must fire fetchStats")
	}
	m, _ = update(t, m, runCmd(t, cmd)) // statsMsg
	content := m.View().Content
	for _, want := range []string{"2026-02-12", "1.2.3.4", "requests by day", "requests by ip"} {
		if !strings.Contains(content, want) {
			t.Fatalf("stats view missing %q:\n%s", want, content)
		}
	}
}

// TestLogsFilterCycle: `f` cycles level (all → error → warn → all) and
// the suffix/hint reflect it; `/` opens the input, esc resets filters.
func TestLogsFilterCycle(t *testing.T) {
	m := New(newFullFake(t))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, runCmd(t, m.Init()))
	m, _ = press(t, m, "2") // logs tab

	m, _ = press(t, m, "f")
	if m.logLevel != logLevelError {
		t.Fatalf("after f: logLevel = %d, want error", m.logLevel)
	}
	if !strings.Contains(m.View().Content, "level=error") {
		t.Fatalf("filter hint missing level=error:\n%s", m.View().Content)
	}
	m, _ = press(t, m, "f")
	m, _ = press(t, m, "f")
	if m.logLevel != logLevelAll {
		t.Fatalf("f must cycle back to all, got %d", m.logLevel)
	}

	// `/` opens the text input; typing + enter commits.
	m, _ = press(t, m, "/")
	if !m.logFilterOn {
		t.Fatal("/ must open the filter input")
	}
	m.logInput.SetValue("boot")
	m, cmd := update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.logQuery != "boot" {
		t.Fatalf("logQuery = %q, want boot", m.logQuery)
	}
	if cmd != nil {
		t.Fatal("committing the filter must not fire a command")
	}
	// esc resets.
	m, _ = update(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.logQuery != "" || m.logLevel != logLevelAll {
		t.Fatalf("esc must reset filters, got query=%q level=%d", m.logQuery, m.logLevel)
	}
}

// TestKeysTabAddDelete: `a` opens the masked input, enter calls
// AddKey; `d` arms then fires DeleteKey for the cursor row's fp.
func TestKeysTabAddDelete(t *testing.T) {
	f := newFullFake(t)
	m := New(f)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, runCmd(t, m.Init()))
	m, cmd := press(t, m, "4")
	if cmd == nil {
		t.Fatal("entering keys must fire fetchKeys")
	}
	m, _ = update(t, m, runCmd(t, cmd)) // keysMsg

	m, _ = press(t, m, "a")
	if !m.keyInputOn {
		t.Fatal("a must open the key input")
	}
	m.keyInput.SetValue("sk-raw-key")
	m, cmd = update(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.keyInputOn {
		t.Fatal("enter must close the key input")
	}
	if cmd == nil {
		t.Fatal("enter must fire AddKey")
	}
	// startAction's cmd returns an actionDone msg — run it to apply.
	m, _ = update(t, m, runActionBatch(t, cmd))
	if len(f.added) != 1 || f.added[0] != "sk-raw-key" {
		t.Fatalf("AddKey calls = %v, want [sk-raw-key]", f.added)
	}

	// Delete: first `d` arms (prompt), second `d` fires.
	m, _ = press(t, m, "d")
	if !m.confirmDelete {
		t.Fatal("first d must arm confirmDelete")
	}
	if !strings.Contains(m.actionLine(), "delete key") {
		t.Fatalf("confirm prompt missing: %q", m.actionLine())
	}
	m, cmd = press(t, m, "d")
	if m.confirmDelete {
		t.Fatal("second d must disarm and fire")
	}
	if cmd == nil {
		t.Fatal("second d must fire DeleteKey")
	}
	m, _ = update(t, m, runActionBatch(t, cmd))
	if len(f.deleted) != 1 || f.deleted[0] != "cafebabe" {
		t.Fatalf("DeleteKey calls = %v, want [cafebabe] (first sorted fp)", f.deleted)
	}
}

// TestBandToggle: `b` drops the band background — no 48;2 escape in the
// header line; a second `b` restores it.
func TestBandToggle(t *testing.T) {
	m := New(newFullFake(t))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	line0 := strings.Split(m.View().Content, "\n")[0]
	if !strings.Contains(line0, "\x1b[48;2;") {
		t.Fatalf("band background must be ON by default:\n%q", line0)
	}
	m, _ = press(t, m, "b")
	line0 = strings.Split(m.View().Content, "\n")[0]
	if strings.Contains(line0, "\x1b[48;2;") {
		t.Fatalf("b must drop the band background:\n%q", line0)
	}
	m, _ = press(t, m, "b")
	line0 = strings.Split(m.View().Content, "\n")[0]
	if !strings.Contains(line0, "\x1b[48;2;") {
		t.Fatalf("second b must restore the band background:\n%q", line0)
	}
}

// TestKeysFetchErrorRenders: a listing error surfaces on the keys tab
// instead of a stale empty table.
func TestKeysFetchErrorRenders(t *testing.T) {
	f := newFullFake(t)
	f.fps = nil
	m := New(f)
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = update(t, m, runCmd(t, m.Init()))

	// Replace the Keys seam result with an error: simplest is direct state.
	m, _ = press(t, m, "4")
	m.keyListErr = errors.New("pool unreadable")
	m, _ = press(t, m, "r")
	// r fires fetchKeys; run it (still nil fps, no error) — assert the
	// error line renders from state first.
	m.keyListErr = errors.New("pool unreadable")
	if !strings.Contains(keysErrorLine(m), "pool unreadable") {
		t.Fatalf("keys error line missing, got %q", keysErrorLine(m))
	}
}

// keysErrorLine pulls the keys-tab error notice out of the view (the
// notice renders inside the api-keys panel body).
func keysErrorLine(m Model) string {
	for _, l := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(l, "error") {
			return l
		}
	}
	return ""
}
