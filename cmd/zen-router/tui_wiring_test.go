package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"zen-router/internal/cli"
	"zen-router/internal/tui"
)

// downStatusSource is a trivial StatusSource whose Status always fails: the
// daemon-down state that arms the `s` key's spawn branch. No network.
type downStatusSource struct{}

func (downStatusSource) Status(context.Context) (*cli.Status, error) {
	return nil, errors.New("dial tcp 127.0.0.1:8787: connect: connection refused")
}

// recordingSpawn counts spawn calls made through the wiring under test —
// the recorder stands in for the real detachUp path, so no process is ever
// launched (review F1).
type recordingSpawn struct {
	calls int
}

func (r *recordingSpawn) spawn(context.Context) error {
	r.calls++
	return nil
}

// TestTuiSpawnerWiringIsLive: cmdTui's option assembly must inject a live
// Spawner into the model — deleting the WithSpawner line in tuiOptions (or
// the option entirely) must fail this test instead of silently turning
// `s`-on-down into a no-op (review F1).
//
// Hermetic: down fake, XDG homes pointed at t.TempDir() (the log-tail
// option resolves there and reads an empty tail), a recorder instead of
// detachUp — no live daemon, no network, no process, no tea.Program. The
// model's own fetch command produces tui's unexported statusMsg; Update
// matches it across packages because matching is by dynamic type.
func TestTuiSpawnerWiringIsLive(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	rec := &recordingSpawn{}
	m := tui.New(downStatusSource{}, tuiOptions(rec.spawn)...)

	// Enter the down state through the model's own Init fetch.
	nm, _ := m.Update(m.Init()())

	// Press `s`: with the wiring live, the spawn branch must be armed —
	// a command plus the "start daemon" in-flight line in the view.
	model, cmd := nm.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd == nil {
		t.Fatal("`s` on a down daemon returned no command — the WithSpawner wiring in tuiOptions is dead (review F1)")
	}
	content := model.View().Content
	if !strings.Contains(content, "start daemon") || !strings.Contains(content, "in flight") {
		t.Fatalf("view must show the spawn action in flight, got:\n%s", content)
	}

	// Execute the returned command. It is a batch (spinner kick + request);
	// running the children must hit OUR recorder exactly once. The non-batch
	// fallback covers a future single-command shape where cmd() itself runs
	// the request.
	if out := cmd(); out != nil {
		if batch, ok := out.(tea.BatchMsg); ok {
			for _, child := range batch {
				child()
			}
		}
	}
	if rec.calls != 1 {
		t.Errorf("spawn calls = %d, want exactly 1 (the wired Spawner must be what runs)", rec.calls)
	}
}
