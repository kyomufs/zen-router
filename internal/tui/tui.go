// Package tui implements the zen-router interactive control dashboard
// (plan Task 4, spec §7) as a Bubble Tea v2 skeleton.
//
// Architecture: a pure control-API client. This package may import
// internal/cli (client types) but never the daemon-side packages
// (router/quota/gateway/proxy/systemd — see TestImportBoundaries). All I/O
// happens inside tea.Cmd closures produced by Init/Update; View() only
// renders model state and has no side effects.
//
// Scope of the skeleton: the model, the spec §7 1s poll chain, the
// daemon-down start offer, and `q` to quit. Dashboard sections are Task 5;
// the r/d/w/s actions are Task 6 (stubbed here with TODOs).
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"zen-router/internal/cli"
)

// StatusSource is the polling seam: *cli.ControlClient satisfies it in
// production (see cmdTui in main.go); tests inject a fake so no live daemon
// is ever reached.
type StatusSource interface {
	Status(ctx context.Context) (*cli.Status, error)
}

const (
	// pollInterval is the spec §7 control-API poll cadence (1s tea.Tick).
	pollInterval = time.Second
	// pollTimeout bounds one status fetch so a wedged daemon cannot stall
	// the poll chain; a timeout surfaces as the daemon-down state.
	pollTimeout = 2 * time.Second

	// headerTitle is the dashboard header line asserted by the tests.
	headerTitle = "zen-router control dashboard"
	headerPoll  = "poll: 1s (control API)"
	// startOffer is the spec §7:219 daemon-down offer, verbatim in
	// substance: start it with `zen-router up` detached, log file in XDG
	// state.
	startOffer = "start the daemon: zen-router up --detach (log file in XDG state)"
)

// pollMsg is produced by the 1s tea.Tick timer; Update answers it with the
// next StatusSource fetch.
type pollMsg time.Time

// statusMsg carries one StatusSource result back into Update.
type statusMsg struct {
	status *cli.Status
	err    error
}

// Model is the dashboard state: the last status snapshot (nil before the
// first poll) and the last poll error (non-nil = daemon down).
type Model struct {
	src    StatusSource
	status *cli.Status
	err    error
}

// New builds the model over the injected status source.
func New(src StatusSource) Model {
	return Model{src: src}
}

// Init kicks off the first status fetch. The chain then runs itself:
// statusMsg schedules the next tick, pollMsg triggers the next fetch — at
// most one fetch in flight, one poll per second plus fetch time.
func (m Model) Init() tea.Cmd {
	return m.fetch()
}

// fetch polls the control API once. The I/O lives inside the returned
// command (never in View); tests run it only against a fake source.
func (m Model) fetch() tea.Cmd {
	src := m.src
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		st, err := src.Status(ctx)
		return statusMsg{status: st, err: err}
	}
}

// tick arms the 1s poll timer (spec §7). Tests never execute this command.
func (m Model) tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return pollMsg(t) })
}

// Update is the single I/O/decision point of the model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case statusMsg:
		m.status, m.err = msg.status, msg.err
		return m, m.tick()
	case pollMsg:
		return m, m.fetch()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r", "d", "w", "s":
			// TODO(Task 6): spec §7 keys — r rotate now, d/w force
			// direct|warp, s start/stop the daemon. Deliberate no-op
			// stubs in the skeleton; Task 5 owns dashboard sections.
			return m, nil
		}
	}
	return m, nil
}

// titleStyle emphasizes the header line (lipgloss v2; pure rendering).
var titleStyle = lipgloss.NewStyle().Bold(true)

// View renders the current state. It is side-effect free: no I/O, no clock
// reads, no status fetches — those belong to Init/Update and their commands.
func (m Model) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render(headerTitle))
	b.WriteByte('\n')
	b.WriteString(headerPoll)
	b.WriteByte('\n')

	switch {
	case m.err != nil:
		fmt.Fprintf(&b, "daemon: down (%v)\n", m.err)
		b.WriteString(startOffer)
		b.WriteByte('\n')
	case m.status == nil:
		b.WriteString("daemon: waiting for the first status poll...\n")
	default:
		st := m.status
		fmt.Fprintf(&b, "daemon: up | listen: %s | pid: %d | uptime: %ds\n",
			st.Listen, st.Pid, st.UptimeSeconds)
	}

	b.WriteString("\nsections (Task 5): egress + latency, quota, identity pool, log tail\n")
	b.WriteString("keys: q quit | r rotate, d/w egress, s daemon — stubs until Task 6\n")

	var v tea.View
	v.SetContent(b.String())
	v.AltScreen = true
	return v
}
