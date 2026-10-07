// Package tui implements the zen-router interactive control dashboard
// (plan Task 4/5, spec §7) on Bubble Tea v2.
//
// Architecture: a pure control-API client. This package may import
// internal/cli (client types) but never the daemon-side packages
// (router/quota/gateway/proxy/systemd/config — see TestImportBoundaries:
// stdlib, charm.land/* and zen-router/internal/cli only). All I/O happens
// inside tea.Cmd closures produced by Init/Update; View() only renders
// model state and has no side effects (pure render: no file reads, no
// clock reads, no status fetches).
//
// Lifecycle: the spec §7 1s poll chain and the daemon-down start offer
// (Task 4), the dashboard sections — status header, quota tables per
// egress and per key, identity pool, rotation history, log tail, help
// line (Task 5, view.go) — and the r/d/w/s actions, which remain Task-6
// stubs.
package tui

import (
	"context"
	"fmt"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
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

// LogSource is the log-tail seam: NewFileLogTail(path) satisfies it in
// production (cmdTui wires the daemon's XDG file log); tests inject a fake
// so no real file is ever opened. Tail reads the last maxLines lines of the
// daemon log; a missing file is not an error (empty tail — daemon never
// logged yet).
type LogSource interface {
	Tail(maxLines int) ([]string, error)
}

// Option customises a Model at construction time (New).
type Option func(*Model)

// WithLogTail injects the log-tail source feeding the dashboard's log
// viewport. A nil source leaves the tail disabled (the view then shows the
// empty state).
func WithLogTail(src LogSource) Option {
	return func(m *Model) { m.logSrc = src }
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

	// defaultWidth/defaultLogHeight size the widgets before the first
	// tea.WindowSizeMsg arrives, so the sections render even without one.
	defaultWidth      = 80
	defaultLogHeight  = 10
	defaultTableH     = 6
	defaultRotationsH = 51 // header + the last-50 rotation rows

	// minLayoutWidth/minLayoutHeight clamp absurdly small terminals so the
	// widget math in layout() can never go negative.
	minLayoutWidth  = 30
	minLayoutHeight = 12
)

// pollMsg is produced by the 1s tea.Tick timer; Update answers it with the
// next StatusSource fetch.
type pollMsg time.Time

// statusMsg carries one poll-cycle result back into Update: the status
// snapshot and the log-tail read produced by the same fetch command (both
// I/Os live inside that command — never in View).
type statusMsg struct {
	status  *cli.Status
	err     error
	logTail []string
	logErr  error
}

// keyMap is the spec §7 key set surfaced through bubbles help. r/d/w/s are
// Task 6: their bindings render here, Update keeps them as deliberate
// no-op stubs.
type keyMap struct {
	Quit   key.Binding
	Rotate key.Binding
	Direct key.Binding
	Warp   key.Binding
	Daemon key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Quit:   key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Rotate: key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rotate now")),
		Direct: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "direct egress")),
		Warp:   key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "warp egress")),
		Daemon: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start/stop daemon")),
	}
}

// ShortHelp implements help.KeyMap for the single-line help footer.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Quit, k.Rotate, k.Direct, k.Warp, k.Daemon}
}

// FullHelp implements help.KeyMap for the expanded help view.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Quit, k.Rotate, k.Direct, k.Warp, k.Daemon}}
}

// Model is the dashboard state: the last status snapshot (nil before the
// first poll), the last poll error (non-nil = daemon down), the log-tail
// lines, and the sized dashboard widgets (tables + log viewport + help).
type Model struct {
	src    StatusSource
	logSrc LogSource
	status *cli.Status
	err    error

	// now stamps each status result; the reset countdowns are computed
	// from it in Update so View stays clock-free and deterministic.
	now time.Time

	// width/height are the last tea.WindowSizeMsg values (0 = not yet
	// received); layout() derives widget sizes from them.
	width  int
	height int

	keys keyMap
	help help.Model

	egressTable   table.Model
	keyTable      table.Model
	identityTable table.Model
	rotationTable table.Model
	logVP         viewport.Model

	logLines []string
	logErr   error
}

// New builds the model over the injected status source (and optional
// extras such as WithLogTail).
func New(src StatusSource, opts ...Option) Model {
	m := Model{
		src:  src,
		keys: defaultKeyMap(),
		help: help.New(),
		egressTable: table.New(
			table.WithColumns([]table.Column{
				{Title: "Egress", Width: 8},
				{Title: "OK", Width: 7},
				{Title: "429", Width: 6},
				{Title: "Resets in", Width: 12},
			}),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		keyTable: table.New(
			table.WithColumns([]table.Column{
				{Title: "Key", Width: 10},
				{Title: "OK", Width: 7},
				{Title: "429", Width: 6},
				{Title: "Resets in", Width: 12},
			}),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		identityTable: table.New(
			table.WithColumns([]table.Column{
				{Title: "#", Width: 3},
				{Title: "Active", Width: 7},
				{Title: "Device ID", Width: 20},
				{Title: "IPv4", Width: 16},
				{Title: "Registered", Width: 17},
			}),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		rotationTable: table.New(
			table.WithColumns([]table.Column{
				{Title: "At", Width: 16},
				{Title: "From", Width: 8},
				{Title: "To", Width: 8},
				{Title: "Reason", Width: 40},
			}),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultRotationsH),
		),
		logVP: viewport.New(
			viewport.WithWidth(defaultWidth),
			viewport.WithHeight(defaultLogHeight),
		),
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Init kicks off the first status fetch. The chain then runs itself:
// statusMsg schedules the next tick, pollMsg triggers the next fetch — at
// most one fetch in flight, one poll per second plus fetch time.
func (m Model) Init() tea.Cmd {
	return m.fetch()
}

// fetch polls the control API and reads the log tail in one command (the
// single I/O cycle per second). The I/O lives inside the returned command
// (never in View); tests run it only against fake sources.
func (m Model) fetch() tea.Cmd {
	src, logSrc := m.src, m.logSrc
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		st, err := src.Status(ctx)
		var tail []string
		var logErr error
		if logSrc != nil {
			tail, logErr = logSrc.Tail(logTailLines)
		}
		return statusMsg{status: st, err: err, logTail: tail, logErr: logErr}
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
		m.now = time.Now()
		m.logLines, m.logErr = msg.logTail, msg.logErr
		m.applyLog()
		if msg.status != nil {
			m.applyStatus(msg.status)
		}
		return m, m.tick()
	case pollMsg:
		return m, m.fetch()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r", "d", "w", "s":
			// TODO(Task 6): spec §7 keys — r rotate now, d/w force
			// direct|warp, s start/stop the daemon. Deliberate no-op
			// stubs; Task 5 owns dashboard sections only.
			return m, nil
		}
	}
	return m, nil
}

// titleStyle emphasizes the header line (lipgloss v2; pure rendering).
var titleStyle = lipgloss.NewStyle().Bold(true)

// sectionStyle emphasizes the dashboard section titles.
var sectionStyle = lipgloss.NewStyle().Bold(true)

// fmtUptime renders the daemon uptime the way the header line shows it.
func fmtUptime(sec int64) string {
	return fmt.Sprintf("%ds", sec)
}
