package tui

// Dashboard section rendering (plan Task 5, spec §7 screen). Every helper
// here is pure: it formats model state into strings. No I/O, no clock
// reads — time values arrive pre-stamped in m.now (set by Update when a
// status result lands), so repeated View() calls are byte-identical.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"zen-router/internal/cli"
)

// rotationHistory is how many rotation rows the history table keeps
// (spec §14/§7: the store keeps the last 50; the view renders that tail).
const rotationHistory = 50

// View assembles the dashboard screen. Pure render — it reads model state
// only (file I/O for the log tail happens in fetch's command, see tui.go).
// Line budget: layout() fits the frame to the terminal height (coreLineCount
// measures every non-widget line here), so the help line stays on-screen —
// the altscreen clips the bottom (TestViewFitsTerminalSize).
func (m Model) View() tea.View {
	var b strings.Builder

	b.WriteString(titleStyle.Render(headerTitle))
	b.WriteByte('\n')
	b.WriteString(headerPoll)
	b.WriteByte('\n')

	for _, line := range m.statusLines() {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	if m.err == nil && m.status != nil {
		st := m.status
		writeSection(&b, "quota (per egress)", m.egressTable.View())
		writeSection(&b, "quota (per key)", m.keyTable.View())
		writeSection(&b, fmt.Sprintf("identity pool (%d, active %d)",
			len(st.State.Identities), st.State.Active), m.identityTable.View())
		writeSection(&b, "rotation history (last 50)", m.rotationTable.View())
	}

	b.WriteString(sectionStyle.Render("log tail"))
	b.WriteByte('\n')
	switch {
	case m.logErr != nil:
		fmt.Fprintf(&b, "log tail error: %v\n", m.logErr)
	case len(m.logLines) == 0:
		b.WriteString("no log output yet\n")
	default:
		b.WriteString(m.logVP.View())
		b.WriteByte('\n')
	}

	b.WriteString(m.help.View(m.keys))

	var v tea.View
	v.SetContent(b.String())
	v.AltScreen = true
	return v
}

// statusLines renders the block between the header and the first section —
// one entry per line (wrapped entries may span several rendered lines).
// View and coreLineCount share it so the layout budget can never drift
// from what is actually rendered.
func (m Model) statusLines() []string {
	var lines []string
	switch {
	case m.err != nil:
		lines = []string{
			fmt.Sprintf("daemon: down (%v)", m.err),
			m.wrap(startOffer),
		}
	case m.status == nil:
		lines = []string{"daemon: waiting for the first status poll..."}
	default:
		st := m.status
		lines = []string{
			fmt.Sprintf("daemon: up | listen: %s | pid: %d | uptime: %s",
				orDash(st.Listen), st.Pid, fmtUptime(st.UptimeSeconds)),
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
	// Task 6: the action line in the status/error area — the spinner while
	// a request is in flight, otherwise the last request error. A poll
	// result (statusMsg) never clears it; only the next action start does.
	switch {
	case m.pending:
		lines = append(lines, m.wrap(fmt.Sprintf("%s %s in flight",
			m.spinner.View(), m.actionLabel)))
	case m.actionErr != nil:
		lines = append(lines, m.wrap(fmt.Sprintf("%s failed: %v",
			m.actionLabel, m.actionErr)))
	}
	return lines
}

// writeSection appends one titled table section. table.View() ends without
// a newline when rows are visible and with one when it is header-only, so
// the body is trimmed and re-terminated: exactly table.Height()+header lines
// either way — the budget layout() reserved.
func writeSection(b *strings.Builder, title, body string) {
	b.WriteString(sectionStyle.Render(title))
	b.WriteByte('\n')
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteByte('\n')
}

// logViewportVisible reports whether the log tail renders as a viewport
// (its body height is budgeted by layout) rather than one notice line.
func (m Model) logViewportVisible() bool {
	return m.logErr == nil && len(m.logLines) > 0
}

// coreLineCount is the number of non-widget lines layout() must reserve
// before handing the remaining height to the tables and the log viewport:
// header, status block, section titles, log title/notice, help line. It
// shares statusLines() with View so the two cannot diverge.
func (m Model) coreLineCount() int {
	n := 2 // header title + poll line
	for _, line := range m.statusLines() {
		n += linesOf(line)
	}
	if m.err == nil && m.status != nil {
		n += 4 // section titles (quota egress/key, identity pool, rotation)
	}
	n++ // "log tail" section title
	if !m.logViewportVisible() {
		n++ // notice/error line replaces the viewport body
	}
	n += linesOf(m.help.View(m.keys))
	return n
}

// linesOf counts the lines a rendered string spans (wrapped status lines
// can contain embedded newlines).
func linesOf(s string) int {
	return strings.Count(s, "\n") + 1
}

// wrap bounds one status line to the window width once a tea.WindowSizeMsg
// has arrived (m.width == 0 = not yet measured: render unwrapped).
func (m Model) wrap(s string) string {
	if m.width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(m.width).Render(s)
}

// layout resizes every widget from the last tea.WindowSizeMsg and fits the
// frame to the terminal height. Budgeting (spec §7): the fixed lines are
// measured from what View will actually render (coreLineCount), the log
// tail viewport is reserved first (a fifth of the screen, trimmed under
// pressure), and the four tables absorb the remainder — collapsing to
// header-only rows when space is scarce. Invariant (checked by
// TestViewFitsTerminalSize): at typical sizes the frame never exceeds the
// terminal height, so the help line stays on-screen (the altscreen clips
// the bottom of the frame).
func (m *Model) layout() {
	w := m.width
	if w < minLayoutWidth {
		w = minLayoutWidth
	}
	h := m.height
	if h < minLayoutHeight {
		h = minLayoutHeight
	}

	const numTables = 4       // egress, key, identity, rotation
	const minTableH = 1       // header-only row: the floor under pressure
	core := m.coreLineCount() // headers, status, titles, log title/notice, help

	logH := 0
	if m.logViewportVisible() {
		logH = clamp(h/5, 2, 12)
		if need := h - core - logH - minTableH*numTables; need < 0 {
			logH += need // shrink the tail before touching the tables
			if logH < 1 {
				logH = 1 // keep one log line; below this the frame overflows anyway
			}
		}
	}

	avail := h - core - logH - minTableH*numTables // rows beyond each table's header line
	if avail < 0 {
		avail = 0
	}
	base := avail / numTables
	rem := avail % numTables
	var heights [numTables]int
	for i := range heights {
		heights[i] = minTableH + base
		if i < rem {
			heights[i]++
		}
	}

	m.egressTable.SetWidth(w)
	m.egressTable.SetHeight(heights[0])
	m.keyTable.SetWidth(w)
	m.keyTable.SetHeight(heights[1])
	m.identityTable.SetWidth(w)
	m.identityTable.SetHeight(heights[2])
	m.rotationTable.SetWidth(w)
	m.rotationTable.SetHeight(heights[3])
	m.rotationTable.GotoBottom() // newest rotation stays visible

	m.logVP.SetWidth(w)
	m.logVP.SetHeight(logH)
	m.logVP.GotoBottom() // tail stays pinned to the newest line
	m.help.SetWidth(w)
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
// midnight UTC (spec §9 daily windows). Renders as a string once, in
// Update's precompute — never in View.
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
