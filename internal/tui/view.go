package tui

// Dashboard rendering (plan Task 5 + the approved panel-grid redesign):
// the screen is a bordered panel grid — the two quota panels side by side
// at >=100 cols, stacked below — with a full-width log panel below it
// and a fixed footer (keys/help line, then the transient action line as
// the very last line). Panel titles keep the exact literals the hardening
// matrix asserts; the down daemon has no grid at all (its status block is
// a full-width panel instead).
//
// Every helper here is pure: it formats model state into strings. No I/O,
// no clock reads — time values arrive pre-stamped in m.now (set by Update
// when a status result lands), and every widget height/width is decided in
// layout() (also Update), so repeated View() calls are byte-identical.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"zen-router/internal/cli"
)

// Breakpoints for the panel grid (panelgrid_test.go).
const (
	midBreakpoint  = 80  // below: single column; at/above: quota panels stack
	wideBreakpoint = 100 // at/above: quota panels side by side
)

// Focusable panels in the tab order (tui.go tab / shift+tab): the focused
// panel's border/title is highlighted with the accent (focus_help_confirm_test).
const (
	focusEgress = iota
	focusKey
	focusLog
)

// focusablePanels is the cycle length tab/shift+tab walk.
const focusablePanels = focusLog + 1

// confirmPrompt is the first-press `s` line on an up daemon: the stop
// fires on the second press, esc cancels (focus_help_confirm_test).
const confirmPrompt = "stop the daemon? press s again to confirm, esc cancels"

// confirmDeletePrompt is the first-press `d` line on the keys tab: the
// delete fires on the second press, esc cancels (tabs_test).
const confirmDeletePrompt = "delete key? press d again to confirm, esc cancels"

// panelStyle returns the border/title styles for focusable panel idx: the
// focused panel gets the accent focus ring (theme.accent), every other
// panel keeps its dim border. Titles stay bold accent either way.
func (m Model) panelStyle(idx int) (border, title lipgloss.Style) {
	title = m.th.panelTitle
	if m.focus == idx {
		return lipgloss.NewStyle().Foreground(m.th.accent), title
	}
	return m.th.dimText, title
}

// layoutMode is the panel-grid shape chosen from the measured width.
type layoutMode int

const (
	modeSingle layoutMode = iota // <80 cols (and the unmeasured Phase A frame)
	modeMid                      // 80-99: quota panels stacked
	modeWide                     // >=100: quota panels side by side
)

func modeOf(width int) layoutMode {
	switch {
	case width >= wideBreakpoint:
		return modeWide
	case width >= midBreakpoint:
		return modeMid
	default:
		return modeSingle
	}
}

// viewWidth is the panel width View and layout share: the construction
// default before the first tea.WindowSizeMsg (m.width == 0), never below
// minLayoutWidth afterwards. The degenerate sizes ({0,0}/{1,1}) clamp here
// too — cosmetic only, the fit contract covers typical sizes.
func (m Model) viewWidth() int {
	if m.width <= 0 {
		return defaultWidth
	}
	if m.width < minLayoutWidth {
		return minLayoutWidth
	}
	return m.width
}

// gridMode is the layout of the two quota panels: the unmeasured frame
// (m.width == 0) stays single-column full-width like the pre-redesign
// default, so Phase A (hardening matrix, no WindowSizeMsg) renders every
// row at full interior width with the default column widths.
func (m Model) gridMode() layoutMode {
	if m.width <= 0 {
		return modeSingle
	}
	return modeOf(m.viewWidth())
}

// panelWidths returns the two panel widths for the mode (a 1-cell gutter
// separates side-by-side panels; stacked panels take the full width).
func panelWidths(mode layoutMode, w int) (egW, keyW int) {
	half := (w - 1) / 2
	right := w - 1 - half
	if mode == modeWide {
		return half, right
	}
	return w, w
}

// View assembles the dashboard screen. Pure render — model state only
// (widget content was pushed in Update). Line budget: layout() decides
// every widget height (coreLineCount measures the non-widget lines here),
// so the help line stays on-screen — the altscreen clips the bottom
// (TestViewFitsTerminalSize). The tab strip is the third header line
// (design_test pins only lines[0]/lines[1]); below it each tab renders
// its own body — dashboard grid, full-screen log, stats rollups, keys.
func (m Model) View() tea.View {
	vw := m.viewWidth()

	lines := []string{m.bandLine(), headerPoll, m.tabStrip()}
	if m.showHelp {
		// `?` overlay: the dashboard is replaced by the full key list.
		lines = append(lines, renderPanel("keyboard shortcuts", vw,
			helpOverlayRows(), lipgloss.NewStyle().Foreground(m.th.accent),
			m.th.header)...)
	} else {
		switch m.tab {
		case tabLogs:
			lines = append(lines, m.logFilterLine())
			lines = append(lines, m.logPanelLines()...)
		case tabStats:
			lines = append(lines, m.statsLines()...)
		case tabKeys:
			lines = append(lines, m.keysTabLines()...)
		default:
			if m.err != nil {
				// Daemon down: no grid at all — the status block becomes a
				// full-width bordered panel (hardening's absent markers).
				lines = append(lines, renderPanel("daemon status", vw, m.statusLines(),
					m.th.dimText, m.th.panelTitle)...)
			} else {
				lines = append(lines, m.statusLines()...)
				if m.gridPresent() {
					lines = append(lines, m.gridLines()...)
				}
			}
			lines = append(lines, m.logPanelLines()...)
		}
		lines = append(lines, m.help.View(m.keys))
	}
	if al := m.actionLine(); al != "" {
		lines = append(lines, al) // transient action line: frame's last line
	}

	var v tea.View
	v.SetContent(strings.Join(lines, "\n"))
	v.AltScreen = true
	return v
}

// tabStrip renders the third header line: one label per tab, the active
// one in the header style, the rest dim. SGR-only styling (hardening's
// escape audit), clipped to the view width on narrow terminals.
func (m Model) tabStrip() string {
	labels := [tabCount]string{"1 dashboard", "2 logs", "3 stats", "4 keys"}
	var b strings.Builder
	for i, label := range labels {
		if i > 0 {
			b.WriteString("  ")
		}
		if i == m.tab {
			b.WriteString(m.th.header.Render(label))
		} else {
			b.WriteString(m.th.dimText.Render(label))
		}
	}
	return clipLine(b.String(), m.viewWidth())
}

// statusPill is the header state chip: green while the poll answers, red
// when the daemon is unreachable, yellow before the first poll. The glyph
// runs through the theme's status styles so the state reads at a glance.
func (m Model) statusPill() string {
	switch {
	case m.err != nil:
		return m.th.statusErr.Render(pillDown)
	case m.status == nil:
		return m.th.statusWarn.Render(pillWait)
	default:
		return m.th.statusOK.Render(pillUp)
	}
}

// bandLine renders header line one: a full-width band (surface background)
// with the dashboard title on the left and the state pill right-aligned.
// It stays exactly one line — clipLine enforces the width budget even on
// windows too narrow for title and pill side by side. `b` (bandBG) drops
// the background: terminal-native text with a raw-space gap instead of the
// band fill; the cell width stays the view width either way.
func (m Model) bandLine() string {
	vw := m.viewWidth()
	title := m.th.bandTitle.Render(headerTitle)
	gapStyle := m.th.bandGap
	if !m.bandBG {
		title = m.th.header.Render(headerTitle)
		gapStyle = lipgloss.NewStyle()
	}
	pill := m.statusPill()
	gap := vw - cells(title) - cells(pill) - 1 // one band cell after the pill
	if gap < 0 {
		gap = 0
	}
	line := title +
		gapStyle.Render(strings.Repeat(" ", gap)) + pill +
		gapStyle.Render(" ")
	return clipLine(line, vw)
}

// helpOverlayRows lists every binding rendered by the `?` overlay — kept
// next to defaultKeyMap (compact help line) and the tab handling in tui.go.
// The %-10s column keeps the two-column layout even for "shift+tab".
// forbidden-string guard: no removed-panel phrases (rotate now / direct
// egress / warp egress / quota (per egress)...) — only live bindings.
func helpOverlayRows() []string {
	bindings := [][2]string{
		{"q", "quit"},
		{"s", "start/stop daemon"},
		{"1-4", "switch tab"},
		{"tab", "next panel"},
		{"shift+tab", "previous panel"},
		{"up/down", "log scroll line"},
		{"pgup/pgdn", "log page scroll"},
		{"g / G", "log top / bottom"},
		{"f", "log level filter"},
		{"/", "search log lines"},
		{"esc", "reset filters / cancel"},
		{"r", "refresh stats"},
		{"a", "add api key"},
		{"d", "delete api key"},
		{"b", "toggle band background"},
		{"?", "close help"},
	}
	out := make([]string, len(bindings))
	for i, kv := range bindings {
		out[i] = fmt.Sprintf("%-10s %s", kv[0], kv[1])
	}
	return out
}

// gridPresent reports whether the quota panels render (poll answered
// without error). Before the first status there is no grid; on error the
// down panel replaces it.
func (m Model) gridPresent() bool {
	return m.err == nil && m.status != nil
}

// gridLines renders the two quota panels in the mode's shape: paired
// panels are joined with a one-cell gutter on one line, stacked panels
// follow each other.
func (m Model) gridLines() []string {
	vw := m.viewWidth()
	mode := m.gridMode()
	egW, keyW := panelWidths(mode, vw)

	egB, egT := m.panelStyle(focusEgress)
	egP := renderPanel("quota (per egress)", egW, tableLines(m.egressTable), egB, egT)
	keyB, keyT := m.panelStyle(focusKey)
	keyP := renderPanel("quota (per key)", keyW, tableLines(m.keyTable), keyB, keyT)

	if mode == modeWide {
		return joinRow(egP, keyP, egW)
	}
	out := append([]string{}, egP...)
	return append(out, keyP...)
}

// logPanelLines renders the full-width log panel: the viewport body when
// a tail is visible, one notice line otherwise. The title carries the
// active filter state (suffix after the literal "log tail" — Contains
// markers still match). Filtered-empty tails notice instead of an empty
// box (only reachable on the logs tab; the dashboard is unfiltered).
func (m Model) logPanelLines() []string {
	var body []string
	switch {
	case m.logErr != nil:
		body = []string{m.logLevelStyle(fmt.Sprintf("log tail error: %v", m.logErr))}
	case len(m.logLines) == 0:
		body = []string{"no log output yet"}
	default:
		filtered := m.filteredLogLines()
		if len(filtered) == 0 {
			body = []string{m.th.dimText.Render("no log lines match the filter")}
		} else {
			body = splitBody(m.logVP.View())
		}
	}
	logB, logT := m.panelStyle(focusLog)
	return renderPanel("log tail"+m.logFilterSuffix(), m.viewWidth(), body, logB, logT)
}

// logFilterSuffix tags the log panel title with the active filters, e.g.
// " [level=error] [/timeout]". Empty when nothing is filtered — the
// dashboard frame keeps its byte-identical title.
func (m Model) logFilterSuffix() string {
	var parts []string
	switch m.logLevel {
	case logLevelError:
		parts = append(parts, "level=error")
	case logLevelWarn:
		parts = append(parts, "level=warn")
	}
	if m.logQuery != "" {
		parts = append(parts, "/"+m.logQuery)
	}
	if len(parts) == 0 {
		return ""
	}
	return " [" + strings.Join(parts, " ") + "]"
}

// logFilterLine is the logs-tab chrome line under the tab strip: the text
// filter input while it is open, otherwise a one-line hint of the active
// state (f cycles, / searches, esc resets). Exactly one line — the tab's
// coreLineCount accounts for it.
func (m Model) logFilterLine() string {
	if m.logFilterOn {
		return m.logInput.View()
	}
	return m.th.dimText.Render(fmt.Sprintf(
		"filter: level=%s text=%s (f cycle, / search, esc reset)",
		logLevelName(m.logLevel), orEmpty(m.logQuery)))
}

// logLevelName maps the level constant to its hint token.
func logLevelName(level int) string {
	switch level {
	case logLevelError:
		return "error"
	case logLevelWarn:
		return "warn"
	default:
		return "all"
	}
}

// orEmpty renders the active text query for the hint line.
func orEmpty(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// statsLines renders the stats tab: the two rollup tables (day, IP) as
// stacked full-width panels, or one notice panel before the first load.
// Rows arrive pre-sorted from /_zenctl/stats (day DESC, IP by last
// request) and pre-formatted in applyStats (view purity).
func (m Model) statsLines() []string {
	vw := m.viewWidth()
	notice := m.statsNotice()
	if notice != nil {
		return renderPanel("request stats", vw, notice, m.th.dimText, m.th.panelTitle)
	}
	dayB, dayT := m.panelStyle(0)
	out := renderPanel("requests by day", vw, tableLines(m.dayTable), dayB, dayT)
	ipB, ipT := m.panelStyle(1)
	return append(out, renderPanel("requests by ip", vw, tableLines(m.ipTable), ipB, ipT)...)
}

// statsNotice is the stats tab's pre-load body: nil once data landed.
// Seam absent → static notice (no fetch ever fires), loading → spinner
// line, fetch error → styled error line, empty rollups → hint.
func (m Model) statsNotice() []string {
	switch {
	case m.statsSrc == nil:
		return []string{m.th.dimText.Render("stats source not wired")}
	case m.statsErr != nil:
		return []string{m.logLevelStyle(fmt.Sprintf("stats error: %v", m.statsErr))}
	case m.stats == nil:
		return []string{m.th.dimText.Render("loading stats...")}
	case len(m.stats.Days) == 0 && len(m.stats.IPs) == 0:
		return []string{m.th.dimText.Render("no requests recorded in the last 30 days")}
	}
	return nil
}

// keysTabLines renders the keys tab: the masked add-key input when open,
// then the full-width key table (fingerprint counters merged with the
// pool listing — same rows as the dashboard's quota panel, full height).
func (m Model) keysTabLines() []string {
	vw := m.viewWidth()
	var out []string
	if m.keyInputOn {
		out = append(out, m.keyInput.View())
	}
	keyB, keyT := m.panelStyle(0)
	body := tableLines(m.keyTable)
	switch {
	case m.keyListErr != nil:
		body = []string{m.logLevelStyle(fmt.Sprintf("keys error: %v", m.keyListErr))}
	case len(body) == 0:
		body = []string{m.th.dimText.Render("no api keys yet (a to add one)")}
	}
	return append(out, renderPanel("api keys", vw, body, keyB, keyT)...)
}

// tableLines splits a table render into body lines (table.View may end
// with or without a trailing newline depending on rows).
func tableLines(t table.Model) []string { return splitBody(t.View()) }

// splitBody splits any widget render (table or viewport) into body lines.
func splitBody(rendered string) []string {
	raw := strings.TrimRight(rendered, "\n")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

// logLevelStyle colors one log line by substring: "error" first (severity
// order), then "warn:", everything else stays raw text. It runs when the
// tail is pushed into the viewport (applyLog) and on the tail-error
// notice, so every line stays one whole style run through viewport
// wrapping and panel padding.
func (m Model) logLevelStyle(line string) string {
	switch {
	case strings.Contains(line, "error"):
		return m.th.statusErr.Render(line)
	case strings.Contains(line, "warn:"):
		return m.th.statusWarn.Render(line)
	}
	return line
}

// joinRow places two panels side by side with a one-cell gutter: both
// start on the same line (titles share it), and whichever side ends
// earlier is padded with blanks so the borders stay aligned.
func joinRow(left, right []string, leftWidth int) []string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		l := strings.Repeat(" ", leftWidth)
		if i < len(left) {
			l = left[i]
		}
		r := ""
		if i < len(right) {
			r = right[i]
		}
		out[i] = l + " " + r
	}
	return out
}

// renderPanel builds one bordered box: a rounded top border embedding the
// title, the body clipped/padded to the interior, and a rounded bottom.
// The widgets themselves do not truncate (viewport returns raw lines),
// so clipping happens here — every rendered panel line is exactly width
// cells wide.
func renderPanel(title string, width int, body []string, border, tstyle lipgloss.Style) []string {
	if width < 8 {
		width = 8
	}
	interior := width - 2
	if cells(title) > width-4 {
		title = clipLine(title, width-4)
	}
	top := border.Render("╭─") + tstyle.Render(title) +
		border.Render(strings.Repeat("─", width-3-cells(title))+"╮")

	lines := make([]string, 0, len(body)+2)
	lines = append(lines, top)
	for _, ln := range body {
		// Wrapped status lines carry embedded newlines: one element can
		// span several rendered panel lines (coreLineCount budgets them
		// via linesOf, so the count always matches).
		for _, part := range strings.Split(ln, "\n") {
			c := clipLine(part, interior)
			lines = append(lines, border.Render("│")+padCells(c, interior)+border.Render("│"))
		}
	}
	lines = append(lines, border.Render("╰"+strings.Repeat("─", width-2)+"╯"))
	return lines
}

// cells counts the display cells of a styled line — ANSI escape runs cost
// zero cells (clipLine/padCells rely on this).
func cells(s string) int {
	n := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\x1b' {
			for i++; i < len(rs) && rs[i] != 'm'; i++ {
			}
			continue
		}
		n++
	}
	return n
}

// clipLine bounds one styled line to at most w cells (ANSI-aware), marking
// a cut with a unicode ellipsis. Lines already within the budget come back
// byte-identical — the fitted table rows therefore never grow an ellipsis.
func clipLine(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if cells(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\x1b' { // copy the escape run untouched (free cells)
			b.WriteRune(rs[i])
			for i++; i < len(rs); i++ {
				b.WriteRune(rs[i])
				if rs[i] == 'm' {
					break
				}
			}
			continue
		}
		if n == w-1 { // reserve the last cell for the ellipsis
			break
		}
		b.WriteRune(rs[i])
		n++
	}
	b.WriteRune('…')
	return b.String()
}

// padCells appends spaces until the line spans w cells (no-op when the
// line is already at budget — clipLine guarantees <= w).
func padCells(s string, w int) string {
	if pad := w - cells(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// statusLines renders the block between the header and the first panel —
// one entry per line (wrapped entries may span several rendered lines).
// View and coreLineCount share it so the layout budget can never drift
// from what is actually rendered. Labels and values render as separate
// style runs; the runs reassemble the historical display text verbatim,
// so pinned markers still match on display text (stripANSI).
func (m Model) statusLines() []string {
	var lines []string
	switch {
	case m.err != nil:
		// Inside the full-width down panel: wrap at the panel interior so
		// renderPanel never has to clip a status line (the down marker
		// stays one whole style run, text unchanged).
		interior := m.viewWidth() - 2
		lines = []string{
			m.wrapAt(m.th.statusErr.Render(fmt.Sprintf("daemon: down (%v)", m.err)), interior),
			m.wrapAt(startOffer, interior),
		}
	case m.status == nil:
		lines = []string{m.wrap(m.th.statusWarn.Render(
			"daemon: waiting for the first status poll..."))}
	default:
		lines = m.upStatusLines(m.status)
	}
	return lines
}

// upStatusLines renders the up-state stats block: the state/listen/pid/
// uptime line, the observed egress IP, and both latency windows sharing
// one stats line. Labels render dim, values accent-bold (th.value), the
// state chunk carries the ok color. Each run wraps whole substrings —
// concatenating the runs reproduces the pinned markers character for
// character (design_test.go pins both directions).
func (m Model) upStatusLines(st *cli.Status) []string {
	dim, val := m.th.dimText, m.th.value
	line1 := m.th.statusOK.Render("daemon: up") +
		dim.Render(" | listen: ") + val.Render(orDash(st.Listen)) +
		dim.Render(" | pid: ") + val.Render(strconv.Itoa(st.Pid)) +
		dim.Render(" | uptime: ") + val.Render(fmtUptime(st.UptimeSeconds))
	line2 := dim.Render("ip: ") + val.Render(orDash(st.EgressIP))
	ttfbLabel, ttfb := latencyRuns(st, "ttfb", false)
	streamLabel, stream := latencyRuns(st, "stream", true)
	line3 := dim.Render(ttfbLabel) + val.Render(ttfb) +
		dim.Render("  ") + dim.Render(streamLabel) + val.Render(stream)
	return []string{m.wrap(line1), m.wrap(line2), m.wrap(line3)}
}

// actionLine is the transient footer line — the spinner while a request
// is in flight, the last request error otherwise, plus the two-step
// confirm prompts. A poll result never clears an error; only the next
// action start does. It renders as the very last line of the frame
// (below the keys line) and disappears when idle.
func (m Model) actionLine() string {
	switch {
	case m.pending:
		return m.wrap(fmt.Sprintf("%s %s in flight", m.spinner.View(), m.actionLabel))
	case m.confirmStop:
		return m.wrap(confirmPrompt)
	case m.confirmDelete:
		return m.wrap(confirmDeletePrompt)
	case m.actionErr != nil:
		return m.wrap(fmt.Sprintf("%s failed: %s",
			m.actionLabel, sanitizeActionErr(m.actionErr)))
	}
	return ""
}

// actionErrMaxRunes bounds the rendered action-error content: control-API
// errors echo response bodies verbatim (409/502 replies, HTML error
// pages), so the line is truncated to keep the dashboard single-line
// (review F5).
const actionErrMaxRunes = 200

// sanitizeActionErr renders an action error safe for one dashboard line:
// newlines/tabs flatten to spaces, other control runes (ESC, NUL, ...) are
// dropped, and oversized text is truncated with a unicode ellipsis. Pure
// display hygiene — no content inspection (review F5).
func sanitizeActionErr(err error) string {
	if err == nil {
		return ""
	}
	var b strings.Builder
	for _, r := range err.Error() {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			// dropped: escapes and other non-printable runes
		default:
			b.WriteRune(r)
		}
	}
	runes := []rune(b.String())
	if len(runes) > actionErrMaxRunes {
		return string(runes[:actionErrMaxRunes-1]) + "…"
	}
	return string(runes)
}

// logViewportVisible reports whether the log tail renders as a viewport
// (its body height is budgeted by layout) rather than one notice line.
func (m Model) logViewportVisible() bool {
	return m.logErr == nil && len(m.logLines) > 0
}

// coreLineCount is the number of lines layout() must reserve that are NOT
// panel bodies: header (band + poll + tab strip), the status block
// (dashboard only — bordered in the down case, counted as chrome by
// layout), the per-tab chrome line (logs filter hint / keys add input),
// the footer help line and the transient action line. Panel chrome
// (grid/log/down/stats/keys borders) lives in layout's frameTotal; titles
// are embedded in the top borders and therefore part of the panels. It
// shares statusLines/actionLine/View's tab structure with View so the
// budget cannot drift from what is rendered.
func (m Model) coreLineCount() int {
	n := 3 // band title + poll line + tab strip
	if m.tab == tabDashboard {
		for _, line := range m.statusLines() {
			n += linesOf(line)
		}
	}
	switch m.tab {
	case tabLogs:
		if m.logFilterOn {
			n += linesOf(m.logInput.View())
		} else {
			n += 1 // the filter hint line
		}
	case tabKeys:
		if m.keyInputOn {
			n += linesOf(m.keyInput.View())
		}
	}
	n += linesOf(m.help.View(m.keys))
	if al := m.actionLine(); al != "" {
		n += linesOf(al)
	}
	return n
}

// linesOf counts the lines a rendered string spans (wrapped status lines
// can contain embedded newlines).
func linesOf(s string) int {
	return strings.Count(s, "\n") + 1
}

// wrap bounds one status line to the window width once a tea.WindowSizeMsg
// has arrived (m.width == 0 = not yet measured: render unwrapped). Widths
// below the layout floor clamp up so no line can wrap into a pathological
// stack of 1-cell rows.
func (m Model) wrap(s string) string {
	if m.width <= 0 {
		return s
	}
	w := m.width
	if w < minLayoutWidth {
		w = minLayoutWidth
	}
	return lipgloss.NewStyle().Width(w).Render(s)
}

// wrapAt bounds a line to an explicit width (the down status panel wraps
// at its interior; callers pass viewWidth()-2, never below the floor).
func (m Model) wrapAt(s string, w int) string {
	if w <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(w).Render(s)
}

// layout sizes every widget from the last tea.WindowSizeMsg. Per tab:
// the dashboard keeps the panel-grid invariants (panelgrid_test.go —
// content hug, frame fit, shrink order log -> keys -> egress); the logs
// tab gives the viewport everything below its chrome; stats/keys hug
// their tables and shrink to fit. Heights land here (Update), View stays
// a pure render.
func (m *Model) layout() {
	vw := m.viewWidth()
	h := m.height
	if h < minLayoutHeight {
		h = minLayoutHeight
	}
	m.logVP.SetWidth(vw)
	m.help.SetWidth(vw)

	switch m.tab {
	case tabLogs:
		m.layoutLogs(vw, h)
	case tabStats:
		m.layoutStats(vw, h)
	case tabKeys:
		m.layoutKeys(vw, h)
	default:
		m.layoutDashboard(vw, h)
	}
}

// layoutLogs gives the log viewport every line between the chrome and
// the panel borders (a real fullscreen tail), pinned when following.
func (m *Model) layoutLogs(vw, h int) {
	core := m.coreLineCount()
	body := h - core - 2 // panel borders
	if body < 1 {
		body = 1
	}
	m.logVP.SetHeight(body)
	if m.logFollow {
		m.logVP.GotoBottom()
	}
}

// layoutStats stacks the two rollup panels: each hugs its rows (header
// included), and the pair shrinks the taller table first until the frame
// fits; leftover height goes back, capped at content. The notice state
// (no data yet) needs only its single body line.
func (m *Model) layoutStats(vw, h int) {
	m.dayTable.SetWidth(vw)
	m.dayTable.SetColumns(fitColumns(m.dayCols, vw-2))
	m.ipTable.SetWidth(vw)
	m.ipTable.SetColumns(fitColumns(m.ipCols, vw-2))

	core := m.coreLineCount()
	if m.statsNotice() != nil {
		return // frame = core + borders + one notice line; no heights to set
	}
	nDay, nIp := len(m.dayTable.Rows()), len(m.ipTable.Rows())
	dayT, ipT := 1+nDay, 1+nIp
	total := func() int { return core + 4 + dayT + ipT } // two panel borders × 2
	for total() > h && (dayT > 1 || ipT > 1) {
		if dayT >= ipT && dayT > 1 {
			dayT--
		} else {
			ipT--
		}
	}
	if spare := h - total(); spare > 0 {
		if g := min(spare, 1+nDay-dayT); g > 0 {
			dayT += g
			spare -= g
		}
		if g := min(spare, 1+nIp-ipT); g > 0 {
			ipT += g
		}
	}
	m.dayTable.SetHeight(dayT)
	m.ipTable.SetHeight(ipT)
}

// layoutKeys hugs the full-width key table: exact rows when they fit,
// shrunk from the bottom when the frame would overflow. The add-key
// input (coreLineCount) is already accounted.
func (m *Model) layoutKeys(vw, h int) {
	m.keyTable.SetWidth(vw)
	m.keyTable.SetColumns(fitColumns(m.keyCols, vw-2))

	core := m.coreLineCount()
	nKey := len(m.keyTable.Rows())
	keyT := 1 + nKey
	if keyT < 1 {
		keyT = 1
	}
	total := func() int { return core + 2 + keyT }
	for total() > h && keyT > 1 {
		keyT--
	}
	if spare := h - total(); spare > 0 {
		keyT += min(spare, 1+nKey-keyT)
	}
	m.keyTable.SetHeight(keyT)
}

// layoutDashboard sizes the panel grid and the dashboard's mini log tail
// (panelgrid_test.go invariants):
//
//   - content hug: each table gets exactly its rows (+ header), leftover
//     height goes back to the quota tables (capped at content) and then
//     the log — never split evenly (the old layout's "blank void" bug);
//   - frame fit: core + chrome + body lines <= terminal height at every
//     breakpoint; the shrink order is log -> keys -> egress, floors be
//     damned only at degenerate sizes.
func (m *Model) layoutDashboard(vw, h int) {
	mode := m.gridMode()
	grid := m.gridPresent()

	// Widths first: column fitting reads the panel width the mode gives
	// each table, and every later count sees the fitted rows.
	if grid {
		egW, keyW := panelWidths(mode, vw)
		m.egressTable.SetWidth(vw)
		m.egressTable.SetColumns(fitColumns(m.egressCols, egW-2))
		m.keyTable.SetWidth(vw)
		m.keyTable.SetColumns(fitColumns(m.keyCols, keyW-2))
	}

	logVisible := m.logViewportVisible()
	logSet := 0
	if logVisible {
		logSet = clamp(h/5, 2, 12)
	}

	core := m.coreLineCount()
	nEg, nKey := len(m.egressTable.Rows()), len(m.keyTable.Rows())

	// Content-hug targets: one line per rendered table line (header row
	// included).
	egT, keyT := 1+nEg, 1+nKey

	logBody := func() int {
		if !logVisible {
			return 1 // the notice/error line replaces the viewport body
		}
		return min(logSet, m.visibleLogLines())
	}

	// frameTotal mirrors View() line-for-line for the current heights.
	frameTotal := func() int {
		total := core + 2 + logBody() // log panel borders + body
		if !grid {
			if m.err != nil {
				total += 2 // down status panel borders (its lines are in core)
			}
			return total
		}
		egBody, keyBody := min(egT, nEg+1), min(keyT, nKey+1)
		switch mode {
		case modeWide:
			total += 2 + max(egBody, keyBody) // one paired row
		default:
			total += 4 + egBody + keyBody // two stacked rows
		}
		return total
	}

	total := frameTotal()
	if grid {
	shrink:
		for total > h {
			switch {
			case logVisible && logSet > 3:
				logSet--
			case keyT > 1:
				keyT--
			case egT > 1:
				egT--
			case logVisible && logSet > 1:
				logSet--
			default:
				break shrink // degenerate size: accept the overflow
			}
			total = frameTotal()
		}
	} else {
		for total > h && logVisible && logSet > 1 {
			logSet--
			total = frameTotal()
		}
	}

	// Leftover height flows back to the quota tables (capped at their
	// content), then the log (never beyond its lines) — panels stay
	// content-hugged.
	if spare := h - total; spare > 0 {
		if grid {
			if g := min(spare, 1+nEg-egT); g > 0 {
				egT += g
				spare -= g
			}
			if g := min(spare, 1+nKey-keyT); g > 0 {
				keyT += g
				spare -= g
			}
		}
		if spare > 0 && logVisible {
			if g := min(spare, m.visibleLogLines()-logSet); g > 0 {
				logSet += g
			}
		}
	}

	if grid {
		m.egressTable.SetHeight(egT)
		m.keyTable.SetHeight(keyT)
	}
	m.logVP.SetHeight(logSet)
	m.logVP.GotoBottom() // tail stays pinned to the newest line
}

// fitColumns scales a table's column widths so its padded rows
// (sum(width) + 2 cells per column) fit the panel interior. Shrinks only,
// and never below 1 cell per column — the pristine definitions live on
// the Model (m.egressCols, ...), so a shrink-then-grow resize restores
// the full content instead of compounding the shrink.
func fitColumns(base []table.Column, interior int) []table.Column {
	n := len(base)
	if n == 0 || interior <= 0 {
		return base
	}
	target := interior - 2*n
	if target < n {
		target = n
	}

	// Titles never ellipsize: every column keeps at least its title's
	// rune width. The layout clamps viewWidth to >=30, where the egress
	// title floors (6+2+3+9 = 20) exactly fit the minimum target of 20,
	// so the floors always fit the budget.
	mins := make([]int, n)
	out := make([]table.Column, n)
	copy(out, base)
	total := 0
	for i := range base {
		mins[i] = utf8.RuneCountInString(base[i].Title)
		if out[i].Width < mins[i] {
			out[i].Width = mins[i]
		}
		total += out[i].Width
	}
	if total <= target {
		return out
	}

	sum := 0
	for i := range out {
		w := out[i].Width * target / total // proportional shrink, floored at the title
		if w < mins[i] {
			w = mins[i]
		}
		out[i].Width = w
		sum += w
	}
	for sum != target { // rounding drift lands on the widest column
		if sum < target {
			widest := 0
			for i := range out {
				if out[i].Width > out[widest].Width {
					widest = i
				}
			}
			out[widest].Width++
			sum++
			continue
		}
		// Shrink the widest column that still sits above its title floor;
		// when every column is at its floor, accept the overflow.
		widest := -1
		for i := range out {
			if out[i].Width > mins[i] && (widest < 0 || out[i].Width > out[widest].Width) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		out[widest].Width--
		sum--
	}
	return out
}

// applyStatus rebuilds every table from one status snapshot: rows are
// sorted (maps iterate randomly) and countdowns are precomputed from
// m.now so View never touches the clock.
func (m *Model) applyStatus(st *cli.Status) {
	m.egressTable.SetRows(egressRows(st, m.now))
	m.rebuildKeyTable()
}

// applyStats pushes one stats result into both rollup tables (row
// building is pure and sorted; called from Update only).
func (m *Model) applyStats() {
	if m.stats == nil {
		m.dayTable.SetRows(nil)
		m.ipTable.SetRows(nil)
		return
	}
	m.dayTable.SetRows(dayRows(m.stats))
	m.ipTable.SetRows(ipRows(m.stats))
}

// applyKeys pushes the pool listing into the merged key table.
func (m *Model) applyKeys() {
	m.rebuildKeyTable()
}

// keyCounters is the TUI-local view of one quota key state: the two
// counters the table shows plus the reset stamp, copied out of the
// status snapshot (the quota type stays unnamed — import boundary).
type keyCounters struct {
	ok      int64
	n429    int64
	spentAt int64
	seen    bool // false = pool-only key the status has not counted yet
}

// rebuildKeyTable merges the status quota counters with the pool-file
// listing: every fingerprint from either source gets one row (counters
// fall back to 0 / "-" when the status has not seen the key yet) and
// keyRowFPs mirrors the row order for the keys-tab delete cursor.
func (m *Model) rebuildKeyTable() {
	counters := make(map[string]keyCounters)
	if m.status != nil {
		for fp, ks := range m.status.State.Keys {
			if ks == nil {
				continue
			}
			counters[fp] = keyCounters{ok: ks.OK, n429: ks.Daily429, spentAt: ks.SpentUntil, seen: true}
		}
	}
	for _, fp := range m.keyFPs {
		if _, ok := counters[fp]; !ok {
			counters[fp] = keyCounters{}
		}
	}
	rows := make([]table.Row, 0, len(counters))
	m.keyRowFPs = make([]string, 0, len(counters))
	for _, fp := range sortedKeys(counters) {
		c := counters[fp]
		m.keyRowFPs = append(m.keyRowFPs, fp)
		if !c.seen {
			rows = append(rows, table.Row{fp, "0", "0", "-"})
			continue
		}
		rows = append(rows, table.Row{
			fp,
			strconv.FormatInt(c.ok, 10),
			strconv.FormatInt(c.n429, 10),
			resetCountdown(c.spentAt, m.now),
		})
	}
	m.keyTable.SetRows(rows)
}

// filteredLogLines applies the logs-tab filters to the tail in render
// order: level first (case-sensitive severity substrings, matching the
// coloring rules), then the lowercased text query as a substring match.
// An empty query means no text filter (the fallback keeps everything).
func (m Model) filteredLogLines() []string {
	if m.logLevel == logLevelAll && m.logQuery == "" {
		return m.logLines
	}
	out := make([]string, 0, len(m.logLines))
	for _, line := range m.logLines {
		switch m.logLevel {
		case logLevelError:
			if !strings.Contains(line, "error") {
				continue
			}
		case logLevelWarn:
			if !strings.Contains(line, "error") && !strings.Contains(line, "warn:") {
				continue
			}
		}
		if m.logQuery != "" && !strings.Contains(strings.ToLower(line), m.logQuery) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// visibleLogLines is the filtered tail length (the line budget in
// layout reads the same list View renders).
func (m Model) visibleLogLines() int {
	return len(m.filteredLogLines())
}

// applyLog pushes the FILTERED tail into the viewport (still inside
// Update; View only renders the widget). Lines are colored by level here
// — before the viewport sees them — so each line reaches it as one whole
// run. Scroll pin: the dashboard's mini tail is always bottom-anchored;
// the logs tab follows only while logFollow is set (cleared by scrolling
// up, restored by `G` or a tab switch).
func (m *Model) applyLog() {
	visible := m.filteredLogLines()
	if m.logErr != nil || len(visible) == 0 {
		m.logVP.SetContent("")
		return
	}
	colored := make([]string, len(visible))
	for i, line := range visible {
		colored[i] = m.logLevelStyle(line)
	}
	m.logVP.SetContent(strings.Join(colored, "\n"))
	if m.tab == tabDashboard || m.logFollow {
		m.logVP.GotoBottom()
	}
}

// egressRows renders one row per quota egress with its reset countdown.
func egressRows(st *cli.Status, now time.Time) []table.Row {
	rows := make([]table.Row, 0, len(st.State.Egress))
	for _, name := range sortedKeys(st.State.Egress) {
		es := st.State.Egress[name]
		if es == nil {
			continue
		}
		rows = append(rows, table.Row{
			name,
			strconv.FormatInt(es.OK, 10),
			strconv.FormatInt(es.Daily429, 10),
			resetCountdown(es.SpentUntil, now),
		})
	}
	return rows
}

// keyRows renders one row per fingerprinted API key with its reset
// countdown — the status-only variant folded into rebuildKeyTable above
// (kept conceptually: fingerprint + counters + countdown).
//
// dayRows renders the stats "by day" rollup (already sorted by the
// store: newest day first). Columns mirror the quota tables' OK/429.
func dayRows(st *cli.Stats) []table.Row {
	rows := make([]table.Row, 0, len(st.Days))
	for _, d := range st.Days {
		rows = append(rows, table.Row{d.Day, strconv.FormatInt(d.OK, 10), strconv.FormatInt(d.N429, 10)})
	}
	return rows
}

// ipRows renders the stats "by ip" rollup (sorted by last request). The
// LastTS stamp is formatted ONCE here (Update-side precompute — View
// stays pure); cells clip in the table, never wrap.
func ipRows(st *cli.Stats) []table.Row {
	rows := make([]table.Row, 0, len(st.IPs))
	for _, p := range st.IPs {
		last := "-"
		if p.LastTS > 0 {
			last = time.UnixMilli(p.LastTS).Format("2006-01-02 15:04")
		}
		rows = append(rows, table.Row{
			p.IP,
			strconv.FormatInt(p.OK, 10),
			strconv.FormatInt(p.N429, 10),
			last,
		})
	}
	return rows
}

// latencyRuns splits one latency window per egress into a dim label and
// a value run, e.g. "latency ttfb: " + "direct 12/14ms (n=3)". The
// concatenation equals the historical latencyLine text character for
// character — label+value styling never changes the display text, and
// the windows still render as separate windows (never a merged number).
func latencyRuns(st *cli.Status, kind string, stream bool) (label, value string) {
	lat := st.LatencyTTFB
	if stream {
		lat = st.LatencyStream
	}
	label = fmt.Sprintf("latency %s: ", kind)
	if len(lat) == 0 {
		return label, "no samples"
	}
	var b strings.Builder
	for i, eg := range sortedKeys(lat) {
		e := lat[eg]
		if i > 0 {
			b.WriteString(", ")
		}
		if e.Count == 0 {
			fmt.Fprintf(&b, "%s none (n=0)", eg)
			continue
		}
		fmt.Fprintf(&b, "%s %d/%dms (n=%d)", eg, e.LastMS, e.AvgMS, e.Count)
	}
	return label, b.String()
}

// resetCountdown is the time until the quota window resets: the key/egress
// spentUntil stamp when it is still in the future, otherwise the next
// midnight UTC (spec §3:65 architecture + §4:161 quota semantics — quota
// windows are midnight-UTC daily). Renders as a string once, in Update's
// precompute — never in View.
func resetCountdown(spentUntil int64, now time.Time) string {
	at := time.UnixMilli(spentUntil)
	if !at.After(now) {
		u := now.UTC()
		at = time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	}
	return formatCountdown(at.Sub(now))
}

// formatCountdown renders a duration as 1h59m / 29m30s / 42s (floored, so
// a 2h fixture 0.1ms in the past reads 1h59m — deterministic in tests).
func formatCountdown(d time.Duration) string {
	if d < time.Second {
		return "now"
	}
	h := int(d.Hours())
	mins := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, mins)
	case mins > 0:
		return fmt.Sprintf("%dm%ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// sortedKeys gives deterministic map iteration for row building.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// orDash substitutes "-" for an empty display string.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// clamp bounds v to [low, high].
func clamp(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}
