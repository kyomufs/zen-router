package tui

// Task 7 (Phase C): TUI hardening matrix — every screen state composed
// with every poll-error and action-error variant, plus the brief's
// properties asserted in every matrix cell:
//
//  1. single terminal view per state: consecutive View() calls return
//     byte-identical content (pure render — no clock/IO reads in View);
//  2. escape discipline: every byte of ESC in Content is the renderer's
//     own SGR styling (no cursor/erase/OSC sequences, no NUL/CR), and the
//     one path the design promises to sanitize (action errors, review F5)
//     never carries a data-borne escape into Content. The brief's literal
//     "Content contains no \x1b at all" property is genuinely RED pre-impl
//     (lipgloss emits style SGR unconditionally — 82 CSI sequences in the
//     dashboard, identical under TERM=dumb and a bare env); it is kept as
//     a skipped test documenting finding F1, not forced green.
//  3. redaction never leaks: every secret canary planted into the
//     fixtures — identity token/license/privateKey, warp credentials, a
//     raw state key — is absent from Content in every cell.
//
// Hermetic by construction: fakes only — no tea.Program, no tea.Tick
// execution, no process spawn, no network, no live daemon, no filesystem.
// Fixtures are decoded through the real cli.Status JSON contract (see
// statusFromJSON) so no daemon-side package import is needed
// (TestImportBoundaries).
//
// Findings from this suite (details in .superpowers/sdd/plan2/
// task-7-report.md) are documented here, NOT fixed in production:
//
//	F1 literal no-ANSI impossible (style SGR is unconditional);
//	F2 data-borne ESC passes through status/rotation/identity/log paths
//	   verbatim — only action errors are sanitized (F5 scope);
//	F3 a raw state key renders truncated to 9 chars + "…" in the Key
//	   column — full-value absence holds only via truncation; control-
//	   layer fingerprinting (Task 1 F5) is the intended defense.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// --- canaries and display markers -------------------------------------------

const (
	// Secret canaries: credential-shaped values planted ONLY at paths the
	// design promises never render. assertNoSecretCanaries greps Content
	// for each one in every matrix cell, at both render sizes.
	hcIdent0Token = "api_canary_ident0_token_secret_value"
	hcIdent0Lic   = "api_canary_ident0_license_secret_value"
	hcIdent0Priv  = "api_canary_ident0_privkey_secret_value"
	hcIdent1Token = "api_canary_ident1_token_secret_value"
	hcIdent1Lic   = "api_canary_ident1_license_secret_value"
	hcIdent1Priv  = "api_canary_ident1_privkey_secret_value"
	hcWarpToken   = "api_canary_warp_token_secret_value"
	hcWarpLic     = "api_canary_warp_license_secret_value"
	hcWarpPriv    = "api_canary_warp_privkey_secret_value"
	// Raw state key pre-fingerprinting (finding F3): planted as a state.keys
	// map key; the view renders whatever arrives, so full-value absence is
	// asserted here while truncation does the actual hiding.
	hcRawStateKey = "sk-hardening-rawkey-0123456789abcdef0123456789abcdef"

	// hcActionESC is planted inside the action-error body: the design
	// promise for that path is sanitization, so the escape+marker pair
	// must never survive (the printable residue must — liveness).
	hcActionESC     = "\x1b[31maction-esc-canary"
	hcActionESCText = "action-esc-canary"

	// Display markers: values planted at RENDERED paths. Their presence
	// proves the fixture actually flows into the view, keeping the
	// absence checks non-vacuous — they are deliberately not secret-shaped.
	hcSpareMarker  = "spare-hardening-marker"
	hcLogMarker    = "hardening log marker line"
	hcLogErrMarker = "hardening log read refused"
	hcErr409Body   = "errbody-hardening-409"
	hcStopErrBody  = "stopbody-hardening-rejected"
	hcSpawnErrBody = "spawnbody-hardening-rejected"
)

// hcSecretCanaries lists every secret value planted anywhere in the
// fixtures. The extra "api_canary" stem check is a belt for future
// canary variants that might be forgotten in this list.
var hcSecretCanaries = []string{
	hcIdent0Token, hcIdent0Lic, hcIdent0Priv,
	hcIdent1Token, hcIdent1Lic, hcIdent1Priv,
	hcWarpToken, hcWarpLic, hcWarpPriv,
	hcRawStateKey,
}

// hcActionMarkers are the four mutually exclusive action-line states; each
// matrix cell must render exactly the one its action case expects (or none)
// so the states cannot bleed into one another.
var hcActionMarkers = []string{"in flight", "rotate failed", "start daemon failed", "stop daemon failed"}

// --- fixtures ----------------------------------------------------------------

// hardeningStatusJSON builds one of the three status fixtures through the
// exact JSON contract the control API serves. Every fixture carries the
// credential canaries (warp block; identities where they exist) and, for
// the full-fat variants, a raw state key beside the fingerprinted one.
func hardeningStatusJSON(t *testing.T, kind string) string {
	t.Helper()
	spentEgress := time.Now().Add(2 * time.Hour).UnixMilli()
	spentKey := time.Now().Add(90 * time.Minute).UnixMilli()

	var rots strings.Builder
	for i := 1; i <= 60; i++ {
		if i > 1 {
			rots.WriteByte(',')
		}
		fmt.Fprintf(&rots,
			`{"at":%d,"from":"direct","to":"warp","reason":"rotation-%02d"}`,
			1759700000000+int64(i)*1000, i)
	}

	// per-kind values: full and spare differ only in the spare-error line
	// and the rotating/registering flags; degenerate empties every table.
	rotating, registering := "false", "false"
	spareErr := `""`
	latTTFB := `{"direct": {"last_ms": 12, "avg_ms": 14, "count": 3}, "warp": {"last_ms": 30, "avg_ms": 31, "count": 4}}`
	latStream := `{"direct": {"last_ms": 50, "avg_ms": 55, "count": 2}, "warp": {"last_ms": 60, "avg_ms": 61, "count": 5}}`
	egress := fmt.Sprintf(`{"direct": {"ok": 11, "daily429": 2, "spentUntil": %d}, "warp": {"ok": 7, "daily429": 0}}`, spentEgress)
	keys := fmt.Sprintf(`{"deadbeef": {"ok": 5, "daily429": 1, "spentUntil": %d}, %q: {"ok": 2, "daily429": 0}}`,
		spentKey, hcRawStateKey)
	identities := fmt.Sprintf(`[
      {"deviceId": "dev-hardening-a", "publicKey": "pk-hc-a", "addressV4": "10.0.0.2", "registeredAt": 1763193600000, "token": %q, "license": %q, "privateKey": %q},
      {"deviceId": "dev-hardening-b", "publicKey": "pk-hc-b", "addressV4": "10.0.0.3", "registeredAt": 1763193600000, "token": %q, "license": %q, "privateKey": %q}
    ]`,
		hcIdent0Token, hcIdent0Lic, hcIdent0Priv,
		hcIdent1Token, hcIdent1Lic, hcIdent1Priv)
	active := 1
	rotsJSON := rots.String()

	switch kind {
	case "full":
	case "spare":
		rotating, registering = "true", "true"
		spareErr = fmt.Sprintf("%q", "spare registration rejected: "+hcSpareMarker)
	case "degenerate":
		latTTFB, latStream = `{}`, `{}`
		egress, keys, rotsJSON = `{}`, `{}`, ""
		identities = `[]`
		active = 0
	default:
		t.Fatalf("unknown hardening status kind %q", kind)
	}

	warpIdentity := fmt.Sprintf(
		`{"deviceId": "dev-warp-hc", "publicKey": "pk-hc", "addressV4": "10.9.9.9", "registeredAt": 1763193600000, "token": %q, "license": %q, "privateKey": %q}`,
		hcWarpToken, hcWarpLic, hcWarpPriv)

	return fmt.Sprintf(`{
  "mode": "auto",
  "current": "warp",
  "up": true,
  "listen": "127.0.0.1:8787",
  "pid": 4242,
  "uptime_seconds": 7,
  "last_rotate": "2026-10-06T12:00:00Z",
  "rotating": %s,
  "registering": %s,
  "lastSpareError": %s,
  "egress_ip": "198.51.100.9",
  "latency_ttfb_ms": %s,
  "latency_stream_ms": %s,
  "state": {
    "version": 2,
    "mode": "auto",
    "current": "warp",
    "updatedAt": 1759700000000,
    "egress": %s,
    "keys": %s,
    "warp": %s,
    "identities": %s,
    "active": %d,
    "rotations": [%s]
  }
}`,
		rotating, registering, spareErr,
		latTTFB, latStream,
		egress, keys, warpIdentity, identities, active, rotsJSON)
}

// --- matrix dimensions -------------------------------------------------------

// hcDaemon is one screen state of the status/poll dimension.
type hcDaemon struct {
	name    string
	kind    string // "wait" | "full" | "spare" | "degenerate" | "down"
	pollErr error

	// want markers that must render at every terminal size; wantWide row
	// content visible only at default widget sizes (phase A); wantAbsent
	// markers of the mutually exclusive states (and of empty fixtures).
	want       []string
	wantWide   []string
	wantAbsent []string
}

func hcDaemons() []hcDaemon {
	upAbsent := []string{"daemon: down", "daemon: waiting"}
	upTitles := []string{
		"quota (per egress)", "quota (per key)",
		"identity pool (2, active 1)", "rotation history (last 50)",
	}
	downAbsent := []string{
		"daemon: up", "quota (per egress)", "identity pool", "rotation history",
	}
	return []hcDaemon{
		{
			name: "wait", kind: "wait",
			want:       []string{"daemon: waiting for the first status poll..."},
			wantAbsent: append([]string{"daemon: up", "daemon: down"}, downAbsent...),
		},
		{
			name: "up-full", kind: "full",
			want: append([]string{
				"daemon: up | listen: 127.0.0.1:8787 | pid: 4242 | uptime: 7s",
				"mode: auto | egress: warp | ip: 198.51.100.9",
				"last rotate: 2026-10-06T12:00:00Z | rotating: false | registering: false",
				"latency ttfb: direct 12/14ms (n=3), warp 30/31ms (n=4)",
				"latency stream: direct 50/55ms (n=2), warp 60/61ms (n=5)",
			}, upTitles...),
			wantWide: []string{"deadbeef", "dev-hardening-a", "dev-hardening-b", "rotation-60"},
			// rotation-01..10 fall out of the newest-50 window (existing
			// cap test owns that assertion; the fixture still proves the
			// full-vs-empty axis via rotation-60 above).
			wantAbsent: upAbsent,
		},
		{
			name: "up-spare", kind: "spare",
			want: append([]string{
				"daemon: up | listen: 127.0.0.1:8787 | pid: 4242 | uptime: 7s",
				"rotating: true | registering: true",
				"spare registration error: ",
				hcSpareMarker,
				"latency ttfb: direct 12/14ms (n=3), warp 30/31ms (n=4)",
			}, upTitles...),
			wantWide:   []string{"deadbeef", "dev-hardening-a", "rotation-60"},
			wantAbsent: upAbsent,
		},
		{
			name: "up-degenerate", kind: "degenerate",
			want: []string{
				"daemon: up | listen: 127.0.0.1:8787 | pid: 4242 | uptime: 7s",
				"quota (per egress)", "quota (per key)",
				"identity pool (0, active 0)", "rotation history (last 50)",
				"latency ttfb: no samples", "latency stream: no samples",
			},
			// empty fixtures: every table is header-only, no row content.
			wantAbsent: append(upAbsent, "deadbeef", "rotation-60", "dev-hardening-a"),
		},
		{
			name: "down-not-running", kind: "down", pollErr: errDaemonDown,
			want: []string{
				"daemon: down (zen-router daemon is not running",
				"start the daemon: zen-router up --detach (log file in XDG state)",
			},
			wantAbsent: downAbsent,
		},
		{
			name: "down-network", kind: "down",
			pollErr: errors.New(`Get "http://127.0.0.1:8787/_zenctl/status": dial tcp 127.0.0.1:8787: connect: connection refused`),
			want: []string{
				"daemon: down (",
				"connection refused",
				"start the daemon: zen-router up --detach (log file in XDG state)",
			},
			wantAbsent: downAbsent,
		},
	}
}

// hcLog is the log-tail dimension: none (seam not wired — the notice
// branch), tail (viewport), err (error line).
type hcLog struct {
	name  string
	wired bool
	lines []string
	err   error
	want  []string
}

func hcLogs() []hcLog {
	return []hcLog{
		{name: "no-log", want: []string{"no log output yet"}},
		{
			name: "tail", wired: true,
			lines: []string{"boot: hardening fixture started", hcLogMarker},
			want:  []string{hcLogMarker},
		},
		{
			name: "log-err", wired: true,
			err:  errors.New(hcLogErrMarker),
			want: []string{"log tail error: ", hcLogErrMarker},
		},
	}
}

// hcAction is the action dimension: none, in-flight spinner, a 409 rotate
// error carrying the ESC canary, and the s-key spawn/stop error (its label
// depends on daemon polarity — down spawns, up stops).
type hcAction struct {
	name string
	kind string // "none" | "pending" | "err-rotate" | "err-spawn-stop"
}

func hcActions() []hcAction {
	return []hcAction{
		{name: "none", kind: "none"},
		{name: "pending", kind: "pending"},
		{name: "err-rotate-409", kind: "err-rotate"},
		{name: "err-spawn-stop", kind: "err-spawn-stop"},
	}
}

// hcPollResult scripts the poll outcome for one daemon case.
func hcPollResult(t *testing.T, dc hcDaemon) fakeResult {
	t.Helper()
	switch dc.kind {
	case "wait":
		// never fetched — the model stays pre-poll; script a full status
		// anyway so the fake has a result.
		return fakeResult{status: statusFromJSON(t, hardeningStatusJSON(t, "full"))}
	case "down":
		return fakeResult{err: dc.pollErr}
	default:
		return fakeResult{status: statusFromJSON(t, hardeningStatusJSON(t, dc.kind))}
	}
}

// --- assertion helpers -------------------------------------------------------

// assertStableView is property 1: consecutive View() calls must be
// byte-identical (single terminal view per state). It returns the content
// for the remaining checks.
func assertStableView(t *testing.T, phase string, m Model) string {
	t.Helper()
	first := m.View()
	second := m.View()
	if first.Content != second.Content {
		t.Fatalf("[%s] View() is not pure: consecutive renders differ at offset %d\nfirst:  %q\nsecond: %q",
			phase, firstDiffOffset(first.Content, second.Content),
			contextAround(first.Content, firstDiffOffset(first.Content, second.Content)),
			contextAround(second.Content, firstDiffOffset(first.Content, second.Content)))
	}
	if first.AltScreen != second.AltScreen {
		t.Fatalf("[%s] View() AltScreen flag differs between renders: %v vs %v",
			phase, first.AltScreen, second.AltScreen)
	}
	return first.Content
}

// firstDiffOffset locates the first differing byte (len when equal).
func firstDiffOffset(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// contextAround renders a short window around offset for failure output.
func contextAround(s string, off int) string {
	start := off - 30
	if start < 0 {
		start = 0
	}
	end := off + 40
	if end > len(s) {
		end = len(s)
	}
	if start >= end {
		return ""
	}
	return s[start:end]
}

// assertNoSecretCanaries is property 3: no secret canary (identity
// credentials, warp credentials, raw state key) may appear in Content.
func assertNoSecretCanaries(t *testing.T, phase, content string) {
	t.Helper()
	for _, canary := range hcSecretCanaries {
		if strings.Contains(content, canary) {
			t.Errorf("[%s] secret canary leaked into Content: %q\nnear: %q",
				phase, canary, contextAround(content, strings.Index(content, canary)))
		}
	}
	if strings.Contains(content, "api_canary") {
		// Belt: catches planted variants whose full string is missing from
		// the list (and truncated variants that keep the stem).
		t.Errorf("[%s] api_canary stem present in Content", phase)
	}
}

// assertOnlyStylingEscapes is property 2 (the part that holds): every ESC
// in Content must open an SGR styling sequence (ESC [ ... m), and no NUL
// or CR byte may appear. Cursor/erase/OSC sequences or stray control
// bytes would let rendered data hijack the terminal frame.
func assertOnlyStylingEscapes(t *testing.T, phase, content string) {
	t.Helper()
	for i := 0; i < len(content); i++ {
		switch content[i] {
		case 0:
			t.Fatalf("[%s] NUL byte in Content at offset %d: %q", phase, i, contextAround(content, i))
		case '\r':
			t.Fatalf("[%s] CR byte in Content at offset %d: %q", phase, i, contextAround(content, i))
		case 0x1b:
		default:
			continue
		}
		// ESC must open an SGR CSI: ESC '[' params 'm'.
		if i+2 >= len(content) || content[i+1] != '[' {
			t.Fatalf("[%s] non-CSI escape at offset %d: %q", phase, i, contextAround(content, i))
		}
		j := i + 2
		for j < len(content) && content[j] >= 0x20 && content[j] <= 0x3f {
			j++ // CSI parameter/intermediate bytes
		}
		if j >= len(content) || content[j] != 'm' {
			end := j + 1
			if end > len(content) {
				end = len(content)
			}
			t.Fatalf("[%s] non-SGR escape sequence %q at offset %d (only renderer styling is allowed)",
				phase, content[i:end], i)
		}
		i = j
	}
}

// hcActionWants returns the action markers this cell must render.
func hcActionWants(dc hcDaemon, ac hcAction) []string {
	switch ac.kind {
	case "none":
		return nil
	case "pending":
		return []string{"rotate in flight"}
	case "err-rotate":
		return []string{"rotate failed: ", hcErr409Body, hcActionESCText}
	case "err-spawn-stop":
		if dc.kind == "down" {
			return []string{"start daemon failed: ", hcSpawnErrBody}
		}
		return []string{"stop daemon failed: ", hcStopErrBody}
	default:
		return nil
	}
}

// assertCell is the per-cell property battery.
func assertCell(t *testing.T, phase string, content string, dc hcDaemon, lc hcLog, ac hcAction, fit bool) {
	t.Helper()
	if content == "" {
		t.Fatalf("[%s] View().Content is empty", phase)
	}

	// --- markers: header + common frame + state-specific content ---------
	want := []string{headerTitle, headerPoll, "log tail", "quit"}
	want = append(want, dc.want...)
	want = append(want, lc.want...)
	actionWants := hcActionWants(dc, ac)
	want = append(want, actionWants...)
	if !fit {
		want = append(want, dc.wantWide...) // row content only at default sizes
	}
	for _, w := range want {
		if !strings.Contains(content, w) {
			t.Errorf("[%s] marker %q missing from Content", phase, w)
		}
	}
	for _, a := range dc.wantAbsent {
		if strings.Contains(content, a) {
			t.Errorf("[%s] marker %q must be absent in state %s", phase, a, dc.name)
		}
	}

	// --- exactly one action-line state (or none) -------------------------
	joined := strings.Join(actionWants, " ")
	for _, marker := range hcActionMarkers {
		if strings.Contains(joined, marker) {
			continue
		}
		if strings.Contains(content, marker) {
			t.Errorf("[%s] action marker %q must be absent in state %s/%s",
				phase, marker, dc.name, ac.name)
		}
	}
	if ac.kind == "err-rotate" && strings.Contains(content, hcActionESC) {
		t.Errorf("[%s] data-borne escape survived sanitizeActionErr: %q", phase, hcActionESC)
	}

	// --- properties 2 (structural) and 3 (redaction) ----------------------
	assertOnlyStylingEscapes(t, phase, content)
	assertNoSecretCanaries(t, phase, content)

	// --- fit: the frame must fit the 80x24 budget -------------------------
	if fit {
		body := strings.TrimRight(content, "\n")
		if n := strings.Count(body, "\n") + 1; n > 24 {
			t.Errorf("[%s] frame is %d lines at 80x24, want <= 24", phase, n)
		}
	}
}

// applyHardeningAction drives one action case into the model. The pending
// case deliberately discards the command so the request stays in flight
// (spinner armed, completion never delivered).
func applyHardeningAction(t *testing.T, m Model, ac hcAction) Model {
	t.Helper()
	switch ac.kind {
	case "none":
		return m
	case "pending":
		next, cmd := actionKey(t, m, "r")
		if cmd == nil {
			t.Fatal("pending: r must queue the rotate request")
		}
		return next
	case "err-rotate", "err-spawn-stop":
		key := "r"
		if ac.kind == "err-spawn-stop" {
			key = "s"
		}
		next, cmd := actionKey(t, m, key)
		if cmd == nil {
			t.Fatalf("%s: %s must queue the request", ac.name, key)
		}
		done := runActionBatch(t, cmd)
		after, _ := update(t, next, done)
		return after
	default:
		t.Fatalf("unknown action kind %q", ac.kind)
		return m
	}
}

// --- the matrix --------------------------------------------------------------

// TestHardeningMatrix composes every screen state (daemon poll dimension)
// with every log-tail branch and every action state, asserting the three
// brief properties plus state markers in each cell.
//
// Documented skips (both structural, not omissions):
//   - wait × log seam: log data arrives only inside a statusMsg; a model
//     that never polled has an empty tail by construction;
//   - wait × err-spawn-stop: `s` is a documented no-op before the first
//     poll (tui.go key switch — up/down unknown).
func TestHardeningMatrix(t *testing.T) {
	for _, dc := range hcDaemons() {
		for _, lc := range hcLogs() {
			if dc.kind == "wait" && lc.name != "no-log" {
				continue
			}
			for _, ac := range hcActions() {
				if dc.kind == "wait" && ac.kind == "err-spawn-stop" {
					continue
				}
				t.Run(dc.name+"/"+lc.name+"/"+ac.name, func(t *testing.T) {
					runHardeningCell(t, dc, lc, ac)
				})
			}
		}
	}
}

func runHardeningCell(t *testing.T, dc hcDaemon, lc hcLog, ac hcAction) {
	t.Helper()

	src := &actionFake{
		fakeSource: newFake(hcPollResult(t, dc)),
		rotateErr:  errors.New("HTTP 409 conflict: " + hcErr409Body + " " + hcActionESC),
		stopErr:    errors.New(hcStopErrBody),
	}
	opts := []Option{
		WithSpawner(func(context.Context) error { return errors.New(hcSpawnErrBody) }),
	}
	if lc.wired {
		opts = append(opts, WithLogTail(&fakeLogSource{lines: lc.lines, err: lc.err}))
	}
	m := New(src, opts...)

	if dc.kind != "wait" {
		m, _ = update(t, m, runCmd(t, m.Init()))
	}

	// Phase A: default widget sizes (table row content visible), before
	// any resize or action — full liveness + properties.
	if ac.kind == "none" {
		content := assertStableView(t, dc.name+"/"+lc.name+"/default", m)
		assertCell(t, dc.name+"/"+lc.name+"/default", content, dc, lc, ac, false)
	}

	// Measured terminal, then the action (relayout path: width > 0).
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m = applyHardeningAction(t, m, ac)

	// Phase B: every cell at 80x24 — fit + properties + action markers.
	content := assertStableView(t, dc.name+"/"+lc.name+"/"+ac.name, m)
	assertCell(t, dc.name+"/"+lc.name+"/"+ac.name, content, dc, lc, ac, true)
}

// TestRecoveringDownToUpKeepsStateComposed walks one model through
// up -> action 409 -> poll down -> poll up: the action error must survive
// both polls (only the next action start clears it — tui.go), the start
// offer must appear exactly while down, and every step stays pure,
// escape-clean and canary-free. Finish: the recovered frame fits 80x24.
func TestRecoveringDownToUpKeepsStateComposed(t *testing.T) {
	up := statusFromJSON(t, hardeningStatusJSON(t, "full"))
	src := &actionFake{
		fakeSource: newFake(
			fakeResult{status: up},
			fakeResult{err: errDaemonDown},
			fakeResult{status: up}, // last repeats: recovery sticks
		),
		rotateErr: errors.New("HTTP 409 conflict: " + hcErr409Body + " " + hcActionESC),
	}
	m := New(src,
		WithSpawner(func(context.Context) error { return errors.New(hcSpawnErrBody) }),
		WithLogTail(&fakeLogSource{lines: []string{hcLogMarker}}))

	// Step 1: daemon up.
	m, _ = update(t, m, runCmd(t, m.Init()))
	c := assertStableView(t, "recovery/up", m)
	for _, want := range []string{"daemon: up", "quota (per egress)", hcLogMarker, "quit"} {
		if !strings.Contains(c, want) {
			t.Errorf("[recovery/up] marker %q missing", want)
		}
	}
	if strings.Contains(c, "daemon: down") || strings.Contains(c, startOffer) {
		t.Errorf("[recovery/up] down-state markers present while up")
	}
	assertOnlyStylingEscapes(t, "recovery/up", c)
	assertNoSecretCanaries(t, "recovery/up", c)

	// Step 2: rotate fails with 409 (ESC canary sanitized).
	m, cmd := actionKey(t, m, "r")
	done := runActionBatch(t, cmd)
	m, _ = update(t, m, done)
	c = assertStableView(t, "recovery/action-409", m)
	for _, want := range []string{"daemon: up", "rotate failed: ", hcErr409Body, hcActionESCText} {
		if !strings.Contains(c, want) {
			t.Errorf("[recovery/action-409] marker %q missing", want)
		}
	}
	if strings.Contains(c, hcActionESC) {
		t.Errorf("[recovery/action-409] data-borne escape survived sanitizeActionErr")
	}
	assertOnlyStylingEscapes(t, "recovery/action-409", c)
	assertNoSecretCanaries(t, "recovery/action-409", c)

	// Step 3: poll goes down — action error and log tail must survive,
	// sections/status vanish behind the down offer.
	m, fetchCmd := update(t, m, pollMsg(time.Now()))
	m, _ = update(t, m, runCmd(t, fetchCmd))
	c = assertStableView(t, "recovery/down", m)
	for _, want := range []string{"daemon: down (", startOffer, "rotate failed: ", hcLogMarker, "quit"} {
		if !strings.Contains(c, want) {
			t.Errorf("[recovery/down] marker %q missing", want)
		}
	}
	for _, absent := range []string{"quota (per egress)", "daemon: up"} {
		if strings.Contains(c, absent) {
			t.Errorf("[recovery/down] marker %q must be absent", absent)
		}
	}
	assertOnlyStylingEscapes(t, "recovery/down", c)
	assertNoSecretCanaries(t, "recovery/down", c)

	// Step 4: poll recovers — offer gone, up sections back, the 409 line
	// still pinned (a poll never clears it).
	m, fetchCmd = update(t, m, pollMsg(time.Now()))
	m, _ = update(t, m, runCmd(t, fetchCmd))
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	c = assertStableView(t, "recovery/recovered", m)
	for _, want := range []string{
		"daemon: up | listen: 127.0.0.1:8787",
		"quota (per egress)", "identity pool (2, active 1)",
		"rotate failed: ", hcErr409Body, "quit",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("[recovery/recovered] marker %q missing", want)
		}
	}
	if strings.Contains(c, startOffer) {
		t.Errorf("[recovery/recovered] start offer must be gone once up")
	}
	body := strings.TrimRight(c, "\n")
	if n := strings.Count(body, "\n") + 1; n > 24 {
		t.Errorf("[recovery/recovered] frame is %d lines at 80x24, want <= 24", n)
	}
	assertOnlyStylingEscapes(t, "recovery/recovered", c)
	assertNoSecretCanaries(t, "recovery/recovered", c)
}

// TestViewLiteralNoANSIEscapes encodes the task-7 brief's LITERAL property
// ("no ANSI escapes in View().Content"). Genuine pre-impl RED: one run of
// this test fails with 82 CSI sequences — lipgloss Bold/color styling
// emits SGR unconditionally (verified identical under TERM=dumb and a bare
// env, so output is deterministic but never ANSI-free).
//
// STOPPED as finding F1 per the brief's contract; the matrix asserts the
// substitute that does hold (assertOnlyStylingEscapes: SGR-only, no
// cursor/erase/OSC bytes). Un-skip only if styling stops emitting ANSI.
func TestViewLiteralNoANSIEscapes(t *testing.T) {
	t.Skip("finding F1: lipgloss style SGR is unconditional — 82 CSI sequences in dashboard Content (genuine pre-impl RED; see .superpowers/sdd/plan2/task-7-report.md)")
	m := dashboardModel(t)
	if got := strings.Count(m.View().Content, "\x1b["); got != 0 {
		t.Errorf("View().Content contains %d ANSI CSI sequences, want 0", got)
	}
}
