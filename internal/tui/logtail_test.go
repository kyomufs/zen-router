package tui

// Task 5 (Phase C): the log-tail seam. The viewport is fed exclusively
// through the fetch command (Init/Update path); View only renders model
// state. Tests inject a fake LogSource — the file reader itself is
// exercised only inside t.TempDir().

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fakeLogSource counts Tail calls so purity of View can be asserted.
type fakeLogSource struct {
	lines []string
	err   error
	calls int
}

func (f *fakeLogSource) Tail(maxLines int) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.lines, nil
}

// TestLogTailRenderedFromSeam: lines injected through WithLogTail reach
// the rendered viewport after one fetch cycle.
func TestLogTailRenderedFromSeam(t *testing.T) {
	log := &fakeLogSource{lines: []string{
		"first log line",
		"rotated direct -> warp",
		"last log line",
	}}
	m := New(newFake(fakeResult{status: upStatus()}), WithLogTail(log))
	nm, _ := update(t, m, runCmd(t, m.Init()))

	content := nm.View().Content
	for _, want := range []string{"first log line", "rotated direct -> warp", "last log line"} {
		if !strings.Contains(content, want) {
			t.Errorf("log tail lacks %q:\n%s", want, content)
		}
	}
	if log.calls != 1 {
		t.Errorf("seam read count = %d after one poll, want 1", log.calls)
	}
}

// TestViewDoesNotReadLogSource: View is pure — rendering never re-invokes
// the seam (file I/O would live behind it in production).
func TestViewDoesNotReadLogSource(t *testing.T) {
	log := &fakeLogSource{lines: []string{"boot: gateway ready"}}
	m := New(newFake(fakeResult{status: upStatus()}), WithLogTail(log))
	nm, _ := update(t, m, runCmd(t, m.Init()))

	before := log.calls
	for i := 0; i < 3; i++ {
		_ = nm.View().Content
	}
	if log.calls != before {
		t.Errorf("View() read the log seam %d times, want 0", log.calls-before)
	}
}

// TestLogTailErrorSurfaces: a failing tail read is surfaced by the view,
// not swallowed.
func TestLogTailErrorSurfaces(t *testing.T) {
	log := &fakeLogSource{err: errors.New("EIO: unreadable")}
	m := New(newFake(fakeResult{status: upStatus()}), WithLogTail(log))
	nm, _ := update(t, m, runCmd(t, m.Init()))

	content := nm.View().Content
	if !strings.Contains(content, "log tail error") || !strings.Contains(content, "EIO") {
		t.Errorf("view does not surface the log tail error:\n%s", content)
	}
}

// TestLogTailEmptyShowsNotice: no seam wired → deterministic empty state.
func TestLogTailEmptyShowsNotice(t *testing.T) {
	m := New(newFake(fakeResult{status: upStatus()}))
	nm, _ := update(t, m, runCmd(t, m.Init()))
	if content := nm.View().Content; !strings.Contains(content, "no log output yet") {
		t.Errorf("view lacks the empty-tail notice:\n%s", content)
	}
}

// TestFileLogTailReadsLastLines: the production seam reads the trailing
// lines of the daemon log (t.TempDir only — hermetic).
func TestFileLogTailReadsLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zen.log")
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, "line-%03d\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write fixture log: %v", err)
	}

	tail, err := NewFileLogTail(path).Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(tail) != 10 {
		t.Fatalf("tail lines = %d, want 10: %q", len(tail), tail)
	}
	if tail[0] != "line-091" || tail[9] != "line-100" {
		t.Errorf("tail = first %q last %q, want line-091 … line-100", tail[0], tail[9])
	}

	// A missing file is an empty tail, not an error (daemon not logged yet).
	got, err := NewFileLogTail(filepath.Join(dir, "missing.log")).Tail(10)
	if err != nil {
		t.Errorf("missing file: err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("missing file: lines = %q, want none", got)
	}
}

// TestWindowSizeSetsWidgetSizes: the resize message propagates
// SetWidth/SetHeight to every widget (field-level companion to the
// behavioural TestViewWindowSizeAdaptsLayout).
func TestWindowSizeSetsWidgetSizes(t *testing.T) {
	m := dashboardModel(t)
	nm, _ := update(t, m, tea.WindowSizeMsg{Width: 60, Height: 30})
	mm := nm

	if w := mm.egressTable.Width(); w != 60 {
		t.Errorf("egress table width = %d, want 60", w)
	}
	if w := mm.keyTable.Width(); w != 60 {
		t.Errorf("key table width = %d, want 60", w)
	}
	if w := mm.logVP.Width(); w != 60 {
		t.Errorf("log viewport width = %d, want 60", w)
	}
	if h := mm.logVP.Height(); h < 3 {
		t.Errorf("log viewport height = %d, want >= 3", h)
	}
	// table.Height() is the viewport height (SetHeight minus the header):
	// at 30 rows a table gets header + one visible row — header-only is
	// allowed only under tighter pressure (F1 budget).
	if h := mm.keyTable.Height(); h < 1 {
		t.Errorf("key table viewport height = %d, want >= 1", h)
	}
}
