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
// egress and per key, log tail, help line (Task 5, view.go) — and the
// daemon stop request through the ActionSource seam discovered on the
// status source, daemon start through the injected Spawner seam
// (production wiring: cmdTui passes detachUp).
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

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

// ActionSource is the action seam behind the `s` stop key: *cli.ControlClient
// satisfies it in production. New discovers it on the injected StatusSource
// by type assertion — the production client provides Status + stop from one
// object, so cmdTui cannot wire them inconsistently, while the plain
// StatusSource fakes keep the action key a silent no-op.
type ActionSource interface {
	Stop(ctx context.Context) error
}

// StatsSource is the request-history seam behind the stats tab (GET
// /_zenctl/stats): *cli.ControlClient satisfies it in production, discovered
// on the injected StatusSource exactly like ActionSource. A source without
// it leaves the stats tab on its "not wired" notice (no fetch ever fires).
type StatsSource interface {
	Stats(ctx context.Context) (*cli.Stats, error)
}

// KeySource is the pool-key management seam behind the keys tab
// (POST /_zenctl/keys and friends): *cli.ControlClient satisfies it in
// production, discovered on the injected StatusSource like ActionSource.
// The TUI only ever sees key FINGERPRINTS — raw keys travel through AddKey
// and are never echoed back or stored in the model.
type KeySource interface {
	// Keys lists the pool file's key fingerprints.
	Keys(ctx context.Context) ([]string, error)
	// AddKey appends one raw key to the pool file and returns its fingerprint.
	AddKey(ctx context.Context, raw string) (string, error)
	// DeleteKey removes the pool-file key matching the fingerprint.
	DeleteKey(ctx context.Context, fingerprint string) error
}

// Option customises a Model at construction time (New).
type Option func(*Model)

// WithLogTail injects the log-tail source feeding the dashboard's log
// viewport. A nil source leaves the tail disabled (the view then shows the
// empty state).
func WithLogTail(src LogSource) Option {
	return func(m *Model) { m.logSrc = src }
}

// Spawner is the daemon-start seam behind `s` on a down daemon: a process
// spawn, deliberately NOT part of StatusSource polling. cmdTui (main.go)
// injects the real `up --detach` path (reuse of detachUp), tests inject a
// recording fake — so no test ever launches a process. A nil Spawner
// leaves the start action disabled (silent no-op, like a source without
// ActionSource).
type Spawner func(ctx context.Context) error

// WithSpawner injects the daemon-start seam.
func WithSpawner(fn Spawner) Option {
	return func(m *Model) { m.spawn = fn }
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
	// Status pill glyphs: the right-aligned state chip on the header band.
	pillUp   = "● UP"
	pillDown = "● DOWN"
	pillWait = "● WAIT"
	// startOffer is the spec §7:219 daemon-down offer, verbatim in
	// substance: start it with `zen-router up` detached, log file in XDG
	// state.
	startOffer = "start the daemon: zen-router up --detach (log file in XDG state)"

	// defaultWidth/defaultLogHeight size the widgets before the first
	// tea.WindowSizeMsg arrives, so the sections render even without one.
	defaultWidth     = 80
	defaultLogHeight = 10
	defaultTableH    = 6

	// minLayoutWidth/minLayoutHeight clamp absurdly small terminals so the
	// widget math in layout() can never go negative.
	minLayoutWidth  = 30
	minLayoutHeight = 12

	// actionTimeout bounds one spec §7 action request so a wedged control
	// endpoint cannot leave the guard armed forever (only `q` would still
	// work). Comfortably above detachUp's own 30s readiness bound for the
	// `s`-down spawn; the real detach path ignores the context and relies
	// on that internal bound instead.
	actionTimeout = 60 * time.Second

	// tabCount is the number of `1`-`4` dashboard tabs (tui.go key
	// handling, view.go tab strip).
	tabCount = 4
)

// Dashboard tabs, switched by the digit keys `1`-`4` (view.go renders the
// strip; the tab index drives layout and View). tabDashboard is the boot
// default so every legacy layout test keeps its frame.
const (
	tabDashboard = iota
	tabLogs
	tabStats
	tabKeys
)

// Log-level filter states on the logs tab (`f` cycles all → error → warn →
// all). "error" keeps lines containing "error", "warn" keeps "error" OR
// "warn:" lines (severity-sorted filter, case-insensitive).
const (
	logLevelAll = iota
	logLevelError
	logLevelWarn
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

// statsMsg carries one stats-tab fetch (GET /_zenctl/stats) back into
// Update: the rollups and the fetch error, mirroring statusMsg's shape.
type statsMsg struct {
	stats *cli.Stats
	err   error
}

// keysMsg carries one keys-tab listing (GET /_zenctl/keys) back into
// Update: pool-file key fingerprints (never raw keys) and the fetch error.
type keysMsg struct {
	fps []string
	err error
}

// keyMap is the direct-only key set surfaced through bubbles help. `s` is
// the Task 6 action implemented in Update (startAction): the binding renders
// here, the handler lives in the key switch.
type keyMap struct {
	Quit   key.Binding
	Daemon key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Quit:   key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Daemon: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "start/stop daemon")),
	}
}

// ShortHelp implements help.KeyMap for the single-line help footer.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Quit, k.Daemon}
}

// FullHelp implements help.KeyMap for the expanded help view.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Quit, k.Daemon}}
}

// Model is the dashboard state: the last status snapshot (nil before the
// first poll), the last poll error (non-nil = daemon down), the log-tail
// lines, and the sized dashboard widgets (tables + log viewport + help).
type Model struct {
	src    StatusSource
	act    ActionSource
	spawn  Spawner
	logSrc LogSource
	status *cli.Status
	err    error

	// In-flight action state (Task 6): pending marks one spec §7 request
	// being executed by its command; actionLabel/actionErr render the
	// status-area line (spinner while pending, last error until the next
	// action starts). The 1s poll chain never touches these fields.
	pending     bool
	actionLabel string
	actionErr   error
	spinner     spinner.Model

	// Interaction state (approved redesign): focus is the highlighted
	// panel index in the tab order (view.go focusablePanels, 0 = the
	// egress quota panel, also the boot default); showHelp swaps the
	// dashboard for the `?` keyboard overlay; confirmStop arms the two-step
	// stop (`s` on an up daemon — the second press fires, esc cancels).
	focus       int
	showHelp    bool
	confirmStop bool

	// now stamps each status result; the reset countdowns are computed
	// from it in Update so View stays clock-free and deterministic.
	now time.Time

	// width/height are the last tea.WindowSizeMsg values (0 = not yet
	// received); layout() derives widget sizes from them.
	width  int
	height int

	keys keyMap
	help help.Model

	// th is the immutable style set built once in New() from the terminal
	// background; View renders through it and never detects anything.
	th theme

	egressTable table.Model
	keyTable    table.Model
	logVP       viewport.Model

	// egressCols/keyCols are the PRISTINE column definitions (the same
	// slices handed to table.WithColumns). Fitted columns are sticky —
	// table.Columns() returns whatever was last set — so layout always
	// refits from these bases: shrink-then-grow restores the full content
	// instead of compounding the shrink.
	egressCols []table.Column
	keyCols    []table.Column

	// Tab state (spec §7 four-tab redesign): tab is the active tab index
	// (tabDashboard = boot default, digit keys 1-4 switch). statsSrc/keySrc
	// are optional control-API seams discovered on src in New; a nil seam
	// leaves its tab on a static notice (no fetch is ever attempted).
	tab      int
	statsSrc StatsSource
	keySrc   KeySource

	// stats-tab state: last /_zenctl/stats result. stats==nil && statsErr==
	// nil means "no fetch yet" (loading), err non-nil renders the failure.
	stats    *cli.Stats
	statsErr error

	// keys-tab state: the pool file's key FINGERPRINTS (raw keys never
	// reach the model) and the last listing error. keyListErr/rendering
	// of add/delete happens through startAction like `s`.
	keyFPs     []string
	keyListErr error

	// Log-filter state (logs tab): logLevel cycles via `f`, logQuery is the
	// committed `/` text filter (case-insensitive substring, "" = off),
	// logFilterOn marks the text-input as active (keys route to it), and
	// logFollow pins the viewport to the newest line — cleared by scrolling
	// up, restored by `G` or by switching back to the dashboard tab.
	logLevel    int
	logQuery    string
	logFilterOn bool
	logFollow   bool
	logInput    textinput.Model

	// Keys-tab editing state: keyInput is the MASKED raw-key entry (the
	// raw key exists only inside this widget and the AddKey call — it is
	// never rendered by View, never stored in a field); keyInputOn routes
	// key presses to it; confirmDelete arms the two-step `d` (the first
	// press prompts in the action line, the second fires). keyRowFPs maps
	// the keyTable cursor row back to the fingerprint being deleted.
	keyInput      textinput.Model
	keyInputOn    bool
	confirmDelete bool
	keyRowFPs     []string

	// bandBG toggles the header band background (`b`): true = the
	// surface-fill band (boot default), false = bare terminal background
	// behind title and pill (view.go bandLine).
	bandBG bool

	// dayTable/ipTable render the stats tab (stacked day + IP rollups);
	// dayCols/ipCols are their pristine columns for layout's fitColumns.
	dayTable table.Model
	ipTable  table.Model
	dayCols  []table.Column
	ipCols   []table.Column

	logLines []string
	logErr   error
}

// New builds the model over the injected status source (and optional
// extras such as WithLogTail). When the source also exposes the control
// actions (*cli.ControlClient in production), the daemon stop half of `s`
// is enabled through that discovered ActionSource; otherwise it stays a
// no-op.
func New(src StatusSource, opts ...Option) Model {
	// Pristine column definitions: shared with the tables below and kept
	// for layout's fitColumns (see Model.egressCols).
	egressCols := []table.Column{
		{Title: "Egress", Width: 8},
		{Title: "OK", Width: 7},
		{Title: "429", Width: 6},
		{Title: "Resets in", Width: 12},
	}
	keyCols := []table.Column{
		{Title: "Key", Width: 10},
		{Title: "OK", Width: 7},
		{Title: "429", Width: 6},
		{Title: "Resets in", Width: 12},
	}
	dayCols := []table.Column{
		{Title: "Day", Width: 11},
		{Title: "OK", Width: 7},
		{Title: "429", Width: 6},
	}
	ipCols := []table.Column{
		{Title: "IP", Width: 16},
		{Title: "OK", Width: 7},
		{Title: "429", Width: 6},
		{Title: "Last seen", Width: 14},
	}
	logInput := textinput.New()
	logInput.Prompt = "/ "
	logInput.Placeholder = "type to filter, enter applies, esc cancels"
	keyInput := textinput.New()
	keyInput.Prompt = "api key: "
	keyInput.Placeholder = "paste the raw key (masked)"
	keyInput.EchoMode = textinput.EchoPassword
	keyInput.EchoCharacter = '*'
	keyInput.CharLimit = 512
	m := Model{
		src:     src,
		act:     discoverActions(src),
		keys:    defaultKeyMap(),
		help:    help.New(),
		th:      newTheme(detectBackground()),
		spinner: spinner.New(), // Line frames; advanced only while pending

		statsSrc:   discoverStats(src),
		keySrc:     discoverKeys(src),
		logFollow:  true, // tail pinned to the newest line until scrolled up
		bandBG:     true, // header band background on (toggle with `b`)
		logInput:   logInput,
		keyInput:   keyInput,
		egressCols: egressCols,
		keyCols:    keyCols,
		dayCols:    dayCols,
		ipCols:     ipCols,
		egressTable: table.New(
			table.WithColumns(egressCols),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		keyTable: table.New(
			table.WithColumns(keyCols),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		dayTable: table.New(
			table.WithColumns(dayCols),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		ipTable: table.New(
			table.WithColumns(ipCols),
			table.WithWidth(defaultWidth),
			table.WithHeight(defaultTableH),
		),
		logVP: viewport.New(
			viewport.WithWidth(defaultWidth),
			viewport.WithHeight(defaultLogHeight),
		),
	}
	// Help footer styling from the theme (approved redesign): keys in the
	// accent-bold weight, descriptions and the " • " separator dimmed —
	// never the bubbles defaults.
	m.help.Styles.ShortKey = m.th.header
	m.help.Styles.ShortDesc = m.th.dimText
	m.help.Styles.ShortSeparator = m.th.dimText
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// discoverActions returns the ActionSource exposed by src, or nil when the
// source only polls (plain StatusSource fakes → action keys disabled).
func discoverActions(src StatusSource) ActionSource {
	act, _ := src.(ActionSource)
	return act
}

// discoverStats returns the StatsSource exposed by src, or nil when the
// source only polls (plain fakes → stats tab shows its static notice).
func discoverStats(src StatusSource) StatsSource {
	st, _ := src.(StatsSource)
	return st
}

// discoverKeys returns the KeySource exposed by src, or nil when the
// source only polls (plain fakes → keys tab is read-only via status).
func discoverKeys(src StatusSource) KeySource {
	ks, _ := src.(KeySource)
	return ks
}

// actionDoneMsg carries one spec §7 action's result back into Update: it
// clears the in-flight guard and surfaces err (nil = success) in the view.
type actionDoneMsg struct {
	label string
	err   error
}

// startAction arms one action request: it flips the in-flight guard,
// stamps the label (clearing any previous error), and returns the request
// command batched with the spinner kick. Chosen semantics (asserted by
// tests): a disabled capability (no ActionSource / no Spawner) or an
// already-pending request IGNORES the key — nil command, no state change,
// nothing queued. The command carries all I/O under actionTimeout; View
// stays pure.
func (m Model) startAction(enabled bool, label string, fn func(context.Context) error) (tea.Model, tea.Cmd) {
	if !enabled || m.pending {
		return m, nil
	}
	m.pending = true
	m.actionLabel, m.actionErr = label, nil
	m.relayout() // the status block gains the action line
	req := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
		defer cancel()
		return actionDoneMsg{label: label, err: fn(ctx)}
	}
	// Batch: the spinner kick returns its message without sleeping (the
	// frame animation re-arms through spinner.Update), the request runs
	// the seam. Tests execute both children — fakes only, no tea.Tick.
	return m, tea.Batch(m.spinner.Tick, req)
}

// relayout re-runs the height budget when the status block changes size
// (an action starts or completes), keeping the help line on-screen.
// Before the first tea.WindowSizeMsg (width == 0) the widgets still hold
// their construction defaults — there is no measured frame to refit.
func (m *Model) relayout() {
	if m.width > 0 {
		m.layout()
	}
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

// fetchStats loads the stats tab rollups (entering tab 3 or `r`). One
// request per invocation, guarded by the caller — the poll chain never
// re-fires it. A nil seam returns no command (tab stays on its notice).
func (m Model) fetchStats() tea.Cmd {
	src := m.statsSrc
	if src == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		st, err := src.Stats(ctx)
		return statsMsg{stats: st, err: err}
	}
}

// fetchKeys lists the pool file's key fingerprints (entering tab 4, `r`,
// or after a successful add/delete). A nil seam returns no command.
func (m Model) fetchKeys() tea.Cmd {
	src := m.keySrc
	if src == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		fps, err := src.Keys(ctx)
		return keysMsg{fps: fps, err: err}
	}
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
		// Row counts feed the panel heights (the grid hugs its content),
		// so every poll result re-budgets; width > 0 keeps Phase A at the
		// construction defaults.
		m.relayout()
		return m, m.tick()
	case pollMsg:
		return m, m.fetch()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil
	case spinner.TickMsg:
		// The frame animation only runs while a request is in flight; a
		// late tick after completion is swallowed (no orphan tick loop).
		if !m.pending {
			return m, nil
		}
		var spinCmd tea.Cmd
		m.spinner, spinCmd = m.spinner.Update(msg)
		return m, spinCmd
	case actionDoneMsg:
		m.pending = false
		m.actionLabel, m.actionErr = msg.label, msg.err
		m.relayout() // the status block loses the action line
		// No command: the poll chain keeps its own tick schedule — an
		// action result neither pauses nor re-arms it.
		return m, nil
	case statsMsg:
		m.stats, m.statsErr = msg.stats, msg.err
		m.applyStats()
		m.relayout()
		return m, nil
	case keysMsg:
		m.keyFPs, m.keyListErr = msg.fps, msg.err
		m.applyKeys()
		m.relayout()
		return m, nil
	case tea.KeyPressMsg:
		// Text input active (logs `/` filter or keys `a` add): every key
		// belongs to the input — enter commits and re-lays out, esc
		// cancels, ctrl+c quits. `q`, digits and friends are typed, not
		// bound.
		if m.logFilterOn || m.keyInputOn {
			switch msg.String() {
			case "enter":
				if m.logFilterOn {
					m.logQuery = strings.ToLower(strings.TrimSpace(m.logInput.Value()))
					m.logFilterOn = false
					m.logInput.Blur()
					m.logInput.Reset()
					m.applyLog()
					m.relayout()
					return m, nil
				}
				// keys add: commit the masked value through KeySource.AddKey
				raw := strings.TrimSpace(m.keyInput.Value())
				m.keyInputOn = false
				m.keyInput.Blur()
				m.keyInput.Reset()
				m.relayout()
				if raw == "" {
					return m, nil
				}
				src := m.keySrc
				return m.startAction(src != nil, "add key", func(ctx context.Context) error {
					_, err := src.AddKey(ctx, raw)
					return err
				})
			case "esc":
				if m.logFilterOn {
					m.logFilterOn = false
					m.logInput.Blur()
					m.logInput.Reset()
				} else {
					m.keyInputOn = false
					m.keyInput.Blur()
					m.keyInput.Reset()
				}
				m.relayout()
				return m, nil
			case "ctrl+c":
				return m, tea.Quit
			}
			if m.logFilterOn {
				var cmd tea.Cmd
				m.logInput, cmd = m.logInput.Update(msg)
				return m, cmd
			}
			var cmd tea.Cmd
			m.keyInput, cmd = m.keyInput.Update(msg)
			return m, cmd
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "1", "2", "3", "4":
			return m.switchTab(int(msg.String()[0] - '1'))
		case "tab":
			m.focus = (m.focus + 1) % panelsOn(m.tab)
			m.confirmStop = false
			return m, nil
		case "shift+tab":
			m.focus = (m.focus + panelsOn(m.tab) - 1) % panelsOn(m.tab)
			m.confirmStop = false
			return m, nil
		case "?":
			m.showHelp = !m.showHelp
			m.confirmStop = false
			return m, nil
		case "esc":
			// Priority: open filter input (handled above) → reset the
			// logs-tab filters → disarm delete then stop confirmation.
			if m.tab == tabLogs && (m.logLevel != logLevelAll || m.logQuery != "") {
				m.logLevel = logLevelAll
				m.logQuery = ""
				m.applyLog()
				m.relayout()
				return m, nil
			}
			m.confirmStop, m.confirmDelete = false, false
			return m, nil
		case "b":
			// Header band background on/off — pure display state, no
			// layout change (the band line stays one line either way).
			m.bandBG = !m.bandBG
			return m, nil
		case "s":
			// Spec §7: s = start/stop. Daemon down → spawn `up --detach`
			// through the Spawner seam on the FIRST press (starting is
			// not destructive); daemon up → stop through the control
			// client, but only after a two-step confirmation — the first
			// press arms confirmStop (prompt, no command), the second
			// fires, esc cancels. First poll not answered → up/down
			// unknown, the key does nothing.
			if m.pending {
				return m, nil // in-flight: strictly ignored, no state change
			}
			switch {
			case m.err != nil:
				m.confirmStop = false
				return m.startAction(m.spawn != nil, "start daemon", func(ctx context.Context) error {
					return m.spawn(ctx)
				})
			case m.status != nil && m.act != nil:
				if m.confirmStop {
					m.confirmStop = false
					return m.startAction(true, "stop daemon", func(ctx context.Context) error {
						return m.act.Stop(ctx)
					})
				}
				m.confirmStop = true
				return m, nil
			default:
				return m, nil
			}
		}
		return m.handleTabKey(msg)
	}
	return m, nil
}

// panelsOn reports the focusable panel count of a tab (tab/shift+tab cycle
// length): the dashboard walks its 3 panels, the stats tab walks day/ip,
// logs and keys are single-panel.
func panelsOn(tab int) int {
	switch tab {
	case tabDashboard:
		return focusablePanels
	case tabStats:
		return 2
	default:
		return 1
	}
}

// switchTab moves to tab n (digit keys 1-4), resets focus, disarms the
// stop confirmation, and re-budgets the frame. Entering stats/keys fires
// their fetch; every move re-pins the log tail (scroll position belongs to
// the logs tab only — the dashboard mini-viewport is always bottom-anchored).
func (m Model) switchTab(n int) (tea.Model, tea.Cmd) {
	m.tab = n
	m.focus = 0
	m.confirmStop, m.confirmDelete = false, false
	m.showHelp = false
	m.logFollow = true
	m.relayout()
	switch n {
	case tabStats:
		return m, m.fetchStats()
	case tabKeys:
		return m, m.fetchKeys()
	}
	return m, nil
}

// handleTabKey routes the keys only one tab binds: logs (scroll, `f`,
// `/`), stats (`r` refresh), keys (`a` add, `d` delete, `r` refresh).
// Unbound keys are a silent no-op (nil command, no state change). The
// receiver is a VALUE: viewport mutations land on the returned model.
func (m Model) handleTabKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.tab {
	case tabLogs:
		switch k {
		case "up":
			m.logFollow = false
			m.logVP.ScrollUp(1)
		case "down":
			m.logVP.ScrollDown(1)
			m.logFollow = m.logVP.AtBottom()
		case "pgup":
			m.logFollow = false
			m.logVP.PageUp()
		case "pgdown":
			m.logVP.PageDown()
			m.logFollow = m.logVP.AtBottom()
		case "g":
			m.logFollow = false
			m.logVP.GotoTop()
		case "G":
			m.logFollow = true
			m.logVP.GotoBottom()
		case "f":
			m.logLevel = (m.logLevel + 1) % 3
			m.applyLog()
			m.relayout()
		case "/":
			m.logFilterOn = true
			m.relayout()
			return m, m.logInput.Focus()
		}
		return m, nil
	case tabStats:
		switch k {
		case "r":
			return m, m.fetchStats()
		case "up", "down":
			m.dayTable, _ = m.dayTable.Update(msg)
		}
		return m, nil
	case tabKeys:
		switch k {
		case "r":
			return m, m.fetchKeys()
		case "up", "down":
			m.keyTable, _ = m.keyTable.Update(msg)
		case "a":
			if m.keySrc == nil || m.pending || m.keyInputOn {
				return m, nil
			}
			m.keyInputOn = true
			m.confirmDelete, m.confirmStop = false, false
			m.relayout()
			return m, m.keyInput.Focus()
		case "d":
			return m.deleteSelectedKey()
		}
		return m, nil
	}
	return m, nil
}

// deleteSelectedKey is the two-step keys-tab delete: the first press arms
// confirmDelete (prompt line, no command), the second fires KeySource.
// DeleteKey for the fingerprint of the cursor row; esc cancels (Update's
// esc priority). Disabled without a KeySource or while pending.
func (m Model) deleteSelectedKey() (tea.Model, tea.Cmd) {
	if m.keySrc == nil || m.pending || m.keyInputOn {
		return m, nil
	}
	rows := m.keyTable.Rows()
	idx := m.keyTable.Cursor()
	if idx < 0 {
		idx = 0 // bubbles' table cursor is -1 until the first selection
	}
	if idx >= len(rows) || idx >= len(m.keyRowFPs) {
		return m, nil
	}
	if !m.confirmDelete {
		m.confirmDelete = true
		m.confirmStop = false
		return m, nil
	}
	fp := m.keyRowFPs[idx]
	m.confirmDelete = false
	return m.startAction(true, "delete key", func(ctx context.Context) error {
		return m.keySrc.DeleteKey(ctx, fp)
	})
}

// fmtUptime renders the daemon uptime the way the header line shows it.
func fmtUptime(sec int64) string {
	return fmt.Sprintf("%ds", sec)
}
