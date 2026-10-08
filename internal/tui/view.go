package tui

// Dashboard rendering (plan Task 5 + the approved panel-grid redesign):
// the screen is a bordered panel grid — 2x2 at >=100 cols, stacked quotas
// at 80-99, single column below 80 — with a full-width log panel below it
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

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"zen-router/internal/cli"
)

// rotationHistory is how many rotation rows the history table keeps
// (plan Task 5: an accepted extra beyond spec — data in state.Rotations;
// the quota store keeps the last 50 rotations, quota.RecordRotation — the
// view renders that tail).
const rotationHistory = 50

// Breakpoints for the panel grid (panelgrid_test.go).
const (
	midBreakpoint  = 80  // below: single column; at/above: quotas stack, id/rot pair
	wideBreakpoint = 100 // at/above: full 2x2
)

// noIdentities explains an empty pool inside its panel (one budgeted line).
const noIdentities = "no identities"

// Focusable panels in the tab order (tui.go tab / shift+tab): the focused
// panel's border/title is highlighted with the accent (focus_help_confirm_test).
const (
	focusEgress = iota
	focusKey
	focusIdentity
	focusRotation
	focusLog
)

// focusablePanels is the cycle length tab/shift+tab walk.
const focusablePanels = focusLog + 1

// confirmPrompt is the first-press `s` line on an up daemon: the stop
// fires on the second press, esc cancels (focus_help_confirm_test).
const confirmPrompt = "stop the daemon? press s again to confirm, esc cancels"

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
	modeMid                      // 80-99: quotas stacked, identity/rotation paired
	modeWide                     // >=100: full 2x2
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

// gridMode is the layout of the four quota panels: the unmeasured frame
// (m.width == 0) stays single-column full-width like the pre-redesign
// default, so Phase A (hardening matrix, no WindowSizeMsg) renders every
// row at full interior width with the default column widths.
func (m Model) gridMode() layoutMode {
	if m.width <= 0 {
		return modeSingle
	}
	return modeOf(m.viewWidth())
}

// panelWidths returns the four panel widths for the mode (a 1-cell gutter
// separates side-by-side panels; stacked panels take the full width).
func panelWidths(mode layoutMode, w int) (egW, keyW, idW, rotW int) {
	half := (w - 1) / 2
	right := w - 1 - half
	switch mode {
	case modeWide:
		return half, right, half, right
	case modeMid:
		return w, w, half, right
	default:
		return w, w, w, w
	}
}

// View assembles the dashboard screen. Pure render — model state only
// (widget content was pushed in Update). Line budget: layout() decides
// every widget height (coreLineCount measures the non-widget lines here),
// so the help line stays on-screen — the altscreen clips the bottom
// (TestViewFitsTerminalSize).
func (m Model) View() tea.View {
	vw := m.viewWidth()

	lines := []string{m.th.header.Render(headerTitle), headerPoll}
	if m.showHelp {
		// `?` overlay: the dashboard is replaced by the full key list.
		lines = append(lines, renderPanel("keyboard shortcuts", vw,
			helpOverlayRows(), lipgloss.NewStyle().Foreground(m.th.accent),
			m.th.header)...)
	} else {
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

// helpOverlayRows lists every binding rendered by the `?` overlay — kept
// next to defaultKeyMap (compact help line) and the tab handling in tui.go.
// The %-10s column keeps the two-column layout even for "shift+tab".
func helpOverlayRows() []string {
	bindings := [][2]string{
		{"q", "quit"},
		{"r", "rotate now"},
		{"d", "direct egress"},
		{"w", "warp egress"},
		{"s", "start/stop daemon"},
		{"tab", "next panel"},
		{"shift+tab", "previous panel"},
		{"?", "close help"},
	}
	out := make([]string, len(bindings))
	for i, kv := range bindings {
		out[i] = fmt.Sprintf("%-10s %s", kv[0], kv[1])
	}
	return out
}

// gridPresent reports whether the four quota panels render (poll answered
// without error). Before the first status there is no grid; on error the
// down panel replaces it.
func (m Model) gridPresent() bool {
	return m.err == nil && m.status != nil
}

// gridLines renders the four quota panels in the mode's shape: paired
// panels are joined with a one-cell gutter on one line, stacked panels
// follow each other.
func (m Model) gridLines() []string {
	st := m.status
	vw := m.viewWidth()
	mode := m.gridMode()
	egW, keyW, idW, rotW := panelWidths(mode, vw)

	egB, egT := m.panelStyle(focusEgress)
	egP := renderPanel("quota (per egress)", egW, tableLines(m.egressTable), egB, egT)
	keyB, keyT := m.panelStyle(focusKey)
	keyP := renderPanel("quota (per key)", keyW, tableLines(m.keyTable), keyB, keyT)

	idBody := tableLines(m.identityTable)
	if len(m.identityTable.Rows()) == 0 {
		idBody = append(idBody, m.th.dimText.Render(noIdentities))
	}
	idB, idT := m.panelStyle(focusIdentity)
	idP := renderPanel(fmt.Sprintf("identity pool (%d, active %d)",
		len(st.State.Identities), st.State.Active), idW, idBody, idB, idT)

	rotB, rotT := m.panelStyle(focusRotation)
	rotP := renderPanel("rotation history (last 50)", rotW,
		tableLines(m.rotationTable), rotB, rotT)

	var out []string
	switch mode {
	case modeWide:
		out = append(out, joinRow(egP, keyP, egW)...)
		out = append(out, joinRow(idP, rotP, idW)...)
	case modeMid:
		out = append(out, egP...)
		out = append(out, keyP...)
		out = append(out, joinRow(idP, rotP, idW)...)
	default:
		out = append(out, egP...)
		out = append(out, keyP...)
		out = append(out, idP...)
		out = append(out, rotP...)
	}
	return out
}

// logPanelLines renders the full-width log panel: the viewport body when
// a tail is visible, one notice line otherwise.
func (m Model) logPanelLines() []string {
	var body []string
	switch {
	case m.logErr != nil:
		body = []string{fmt.Sprintf("log tail error: %v", m.logErr)}
	case len(m.logLines) == 0:
		body = []string{"no log output yet"}
	default:
		body = splitBody(m.logVP.View())
	}
	logB, logT := m.panelStyle(focusLog)
	return renderPanel("log tail", m.viewWidth(), body, logB, logT)
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
// from what is actually rendered.
func (m Model) statusLines() []string {
	var lines []string
	switch {
	case m.err != nil:
		// Inside the full-width down panel: wrap at the panel interior so
		// renderPanel never has to clip a status line (hardening markers
		// must stay whole).
		interior := m.viewWidth() - 2
		lines = []string{
			m.wrapAt(fmt.Sprintf("daemon: down (%v)", m.err), interior),
			m.wrapAt(startOffer, interior),
		}
	case m.status == nil:
		lines = []string{m.wrap("daemon: waiting for the first status poll...")}
	default:
		st := m.status
		lines = []string{
			m.wrap(fmt.Sprintf("daemon: up | listen: %s | pid: %d | uptime: %s",
				orDash(st.Listen), st.Pid, fmtUptime(st.UptimeSeconds))),
			m.wrap(fmt.Sprintf("mode: %s | egress: %s | ip: %s",
				orDash(st.Mode), orDash(st.Current), orDash(st.EgressIP))),
			m.wrap(fmt.Sprintf("last rotate: %s | rotating: %t | registering: %t",
				orDash(st.LastRotate), st.Rotating, st.Registering)),
		}
		if st.LastSpareError != "" {
			lines = append(lines, m.wrap("spare registration error: "+st.LastSpareError))
		}
		lines = append(lines,
			m.wrap(latencyLine(st, "ttfb", false)),
			m.wrap(latencyLine(st, "stream", true)))
	}
	return lines
}

// actionLine is the transient footer line — the spinner while a request
// is in flight, the last request error otherwise. A poll result never
// clears an error; only the next action start does. It renders as the
// very last line of the frame (below the keys line) and disappears when
// idle.
func (m Model) actionLine() string {
	switch {
	case m.pending:
		return m.wrap(fmt.Sprintf("%s %s in flight", m.spinner.View(), m.actionLabel))
	case m.confirmStop:
		return m.wrap(confirmPrompt)
	case m.actionErr != nil:
		return m.wrap(fmt.Sprintf("%s failed: %s",
			m.actionLabel, sanitizeActionErr(m.actionErr)))
	}
	return ""
}

// actionErrMaxRunes bounds the rendered action-error content: control-API
// errors echo response bodies verbatim (409/502 replies, HTML error
// pages), so the line is truncated like LastSpareError's 300-byte cap
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
// panel bodies: header (2), the status block (bordered only in the down
// case — those borders are counted as chrome by layout), the footer help
// line and the transient action line. Panel chrome (grid/log/down borders)
// lives in layout's frameTotal; titles are embedded in the top borders and
// therefore part of the panels. It shares statusLines/actionLine with
// View so the budget cannot drift from what is rendered.
func (m Model) coreLineCount() int {
	n := 2 // header title + poll line
	for _, line := range m.statusLines() {
		n += linesOf(line)
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

// layout sizes every widget from the last tea.WindowSizeMsg. The panel
// grid invariants (panelgrid_test.go):
//
//   - content hug: each table gets exactly its rows (+ header), leftover
//     height goes back to the deepest panel (rotation), then the log —
//     never split evenly (the old layout's "blank void" bug);
//   - frame fit: core + chrome + body lines <= terminal height at every
//     breakpoint; the shrink order is log -> rotation -> identity -> keys
//     -> egress, floors be damned only at degenerate sizes.
//
// Heights land here (Update), View stays a pure render.
func (m *Model) layout() {
	vw := m.viewWidth()
	h := m.height
	if h < minLayoutHeight {
		h = minLayoutHeight
	}
	mode := m.gridMode()
	grid := m.gridPresent()

	// Widths first: column fitting reads the panel width the mode gives
	// each table, and every later count sees the fitted rows.
	if grid {
		egW, keyW, idW, rotW := panelWidths(mode, vw)
		m.egressTable.SetWidth(vw)
		m.egressTable.SetColumns(fitColumns(m.egressCols, egW-2))
		m.keyTable.SetWidth(vw)
		m.keyTable.SetColumns(fitColumns(m.keyCols, keyW-2))
		m.identityTable.SetWidth(vw)
		m.identityTable.SetColumns(fitColumns(m.idCols, idW-2))
		m.rotationTable.SetWidth(vw)
		m.rotationTable.SetColumns(fitColumns(m.rotCols, rotW-2))
	}
	m.logVP.SetWidth(vw)
	m.help.SetWidth(vw)

	logVisible := m.logViewportVisible()
	logSet := 0
	if logVisible {
		logSet = clamp(h/5, 2, 12)
	}

	core := m.coreLineCount()
	nEg, nKey := len(m.egressTable.Rows()), len(m.keyTable.Rows())
	nID, nRot := len(m.identityTable.Rows()), len(m.rotationTable.Rows())

	// Content-hug targets: one line per rendered table line (header row
	// included); identity grows one notice line when the pool is empty.
	egT, keyT, idT := 1+nEg, 1+nKey, 1+nID
	rotFloor := 1
	if nRot > 0 {
		rotFloor = 2 // header + the newest row stays visible
	}
	rotT := 1 + nRot
	if rotT < rotFloor {
		rotT = rotFloor
	}

	logBody := func() int {
		if !logVisible {
			return 1 // the notice/error line replaces the viewport body
		}
		return min(logSet, len(m.logLines))
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
		idBody, rotBody := min(idT, nID+1), min(rotT, nRot+1)
		if nID == 0 {
			idBody++ // the "no identities" notice under the header-only table
		}
		switch mode {
		case modeWide:
			total += 6 + max(egBody, keyBody) + max(idBody, rotBody)
		case modeMid:
			total += 8 + egBody + keyBody + max(idBody, rotBody)
		default:
			total += 10 + egBody + keyBody + idBody + rotBody
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
			case rotT > rotFloor:
				rotT--
			case idT > 1:
				idT--
			case keyT > 1:
				keyT--
			case egT > 1:
				egT--
			case logVisible && logSet > 1:
				logSet--
			case rotT > 1:
				rotT--
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

	// Leftover height flows back to the deepest panel (rotation history,
	// capped at its content), then the log (never beyond its lines) —
	// panels stay content-hugged.
	if spare := h - total; spare > 0 {
		if grid {
			if g := min(spare, 1+nRot-rotT); g > 0 {
				rotT += g
				spare -= g
			}
		}
		if spare > 0 && logVisible {
			if g := min(spare, len(m.logLines)-logSet); g > 0 {
				logSet += g
			}
		}
	}

	if grid {
		m.egressTable.SetHeight(egT)
		m.keyTable.SetHeight(keyT)
		m.identityTable.SetHeight(idT)
		m.rotationTable.SetHeight(rotT)
		m.rotationTable.GotoBottom() // newest rotation stays visible
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
	total := 0
	for _, c := range base {
		total += c.Width
	}
	target := interior - 2*n
	if target < n {
		target = n
	}
	if total <= target {
		return base
	}
	out := make([]table.Column, n)
	copy(out, base)
	sum := 0
	for i := range out {
		w := out[i].Width * target / total // proportional shrink, floored at 1
		if w < 1 {
			w = 1
		}
		out[i].Width = w
		sum += w
	}
	for sum != target { // rounding drift lands on the widest column
		widest := 0
		for i := range out {
			if out[i].Width > out[widest].Width {
				widest = i
			}
		}
		if sum < target {
			out[widest].Width++
			sum++
		} else {
			if out[widest].Width <= 1 {
				break
			}
			out[widest].Width--
			sum--
		}
	}
	return out
}

// applyStatus rebuilds every table from one status snapshot: rows are
// sorted (maps iterate randomly) and countdowns are precomputed from
// m.now so View never touches the clock.
func (m *Model) applyStatus(st *cli.Status) {
	m.egressTable.SetRows(egressRows(st, m.now))
	m.keyTable.SetRows(keyRows(st, m.now))
	m.identityTable.SetRows(identityRows(st))
	m.rotationTable.SetRows(rotationRows(st))
	m.rotationTable.GotoBottom()
}

// applyLog pushes the fetched tail into the viewport (still inside Update;
// View only renders the widget).
func (m *Model) applyLog() {
	if m.logErr != nil || len(m.logLines) == 0 {
		m.logVP.SetContent("")
		return
	}
	m.logVP.SetContent(strings.Join(m.logLines, "\n"))
	m.logVP.GotoBottom()
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
// countdown. The key column is the fingerprint only — the raw key never
// exists in the decoded status (control-layer fingerprinting) and never
// reaches the table.
func keyRows(st *cli.Status, now time.Time) []table.Row {
	rows := make([]table.Row, 0, len(st.State.Keys))
	for _, fp := range sortedKeys(st.State.Keys) {
		ks := st.State.Keys[fp]
		if ks == nil {
			continue
		}
		rows = append(rows, table.Row{
			fp,
			strconv.FormatInt(ks.OK, 10),
			strconv.FormatInt(ks.Daily429, 10),
			resetCountdown(ks.SpentUntil, now),
		})
	}
	return rows
}

// identityRows renders the identity pool from DISPLAY FIELDS ONLY: index,
// active marker, DeviceID, AddressV4 and RegisteredAt. Credential fields
// (Token, PrivateKey) are never read here — redaction is by construction,
// not by filtering.
func identityRows(st *cli.Status) []table.Row {
	rows := make([]table.Row, 0, len(st.State.Identities))
	for i, id := range st.State.Identities {
		if id == nil {
			continue
		}
		active := ""
		if i == st.State.Active {
			active = "*"
		}
		registered := "-"
		if id.RegisteredAt > 0 {
			registered = time.UnixMilli(id.RegisteredAt).Format("2006-01-02 15:04")
		}
		rows = append(rows, table.Row{
			strconv.Itoa(i + 1),
			active,
			id.DeviceID,
			orDash(id.AddressV4),
			registered,
		})
	}
	return rows
}

// rotationRows renders state.Rotations capped at the newest 50 rows.
// Rotation rows carry display-safe strings only (at/from/to/reason).
func rotationRows(st *cli.Status) []table.Row {
	rots := st.State.Rotations
	if len(rots) > rotationHistory {
		rots = rots[len(rots)-rotationHistory:]
	}
	rows := make([]table.Row, 0, len(rots))
	for _, r := range rots {
		rows = append(rows, table.Row{
			time.UnixMilli(r.At).Format("2006-01-02 15:04"),
			r.From,
			r.To,
			r.Reason,
		})
	}
	return rows
}

// latencyLine renders one latency window per egress as last/avg/sample
// count, e.g. "latency ttfb: direct 12/14ms (n=3), warp 30/31ms (n=4)".
// The ttfb and stream windows stay separate lines — never a merged number.
func latencyLine(st *cli.Status, kind string, stream bool) string {
	lat := st.LatencyTTFB
	if stream {
		lat = st.LatencyStream
	}
	if len(lat) == 0 {
		return fmt.Sprintf("latency %s: no samples", kind)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "latency %s:", kind)
	for i, eg := range sortedKeys(lat) {
		e := lat[eg]
		if i > 0 {
			b.WriteByte(',')
		}
		if e.Count == 0 {
			fmt.Fprintf(&b, " %s none (n=0)", eg)
			continue
		}
		fmt.Fprintf(&b, " %s %d/%dms (n=%d)", eg, e.LastMS, e.AvgMS, e.Count)
	}
	return b.String()
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
