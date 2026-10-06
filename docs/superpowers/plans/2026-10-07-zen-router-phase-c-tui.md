# zen-router Phase C (TUI Dashboard + systemd Installer) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close phase C of the spec: extend the control API with the data the dashboard
needs (gateway success counters, latency, spare-registration errors, secret redaction),
add `up --detach` + a daemon file log, build the `zen-router tui` Bubbletea dashboard
(1s poll of the control API, status/egress/quota/identities/rotations/log-tail, keys
`r/d/w/s/q`), and add `zen-router install-systemd` for the user unit.

**Architecture:** The TUI is a pure client: `internal/tui` models + views talk to the
daemon exclusively through `cli.ControlClient` (`GET /_zenctl/status`, `POST rotate/use/
stop`) over loopback — no in-process router access, so the dashboard works against any
running daemon. All new dashboard data is added server-side into `Status` (quota state
extensions + redacted identities). `install-systemd` renders a unit file and shells out
to `systemctl --user` behind an injectable exec seam (tests never invoke real systemd).

**Tech Stack:** Go 1.26; **Bubbletea v2** — `charm.land/bubbletea/v2` (v2.0.10,
2026-09-24, min Go 1.26.0 = our go.mod), `charm.land/bubbles/v2` (v2.2.1),
`charm.land/lipgloss/v2` (v2.0.6). New module deps are allowed for this plan (Plan 1
had none). No `teatest` (v2 has none — it is v1-only) → hermetic tests are pure
`Update()`/`View().Content` assertions.

**Spec:** `docs/superpowers/specs/2026-10-06-zen-router-gateway-design.md` — §7 (TUI),
§8 (systemd), §6 (counters visible in TUI), §10 (XDG), §11 (CLI surface), §12 (safety
gates), §13 phase C, §14 (registration-failure surfacing).

## Global Constraints

- **Never touch the live daemon/unit in tests.** No real `systemctl`, no writes outside
  the repo + `t.TempDir()`, no `~/.config/systemd/user` access, no
  `~/.local/bin/zen-router` rebuild (`go install` forbidden while the live unit runs).
  The **live install step is отмашка-gated** (§12): actually running
  `zen-router install-systemd`, `daemon-reload`, `enable --now` against the hand-written
  live unit — documented as a user action, never executed by executors.
- **Live egress-IP echo calls** (public IP through the active transport) are
  §12-gated: tests use an injectable echo seam + local fake; production calls only
  after отмашка.
- Loopback default `127.0.0.1:8787`, override `ZEN_ROUTER_LISTEN` — unchanged.
- Watchdog/quota/rotation semantics unchanged; `Status.State` stays the full
  `quota.State` snapshot (with NEW redaction at the control layer, `state.json` keeps
  full fidelity).
- Keys: `r` rotate now, `d` direct, `w` warp, `s` start/stop daemon, `q` quit (spec
  §7:222). 1s control-API poll (§7:218). Daemon-down screen offers start via
  `up --detach` (§7:219).
- systemd unit: `~/.config/systemd/user/zen-router.service`,
  `ExecStart=<binary> up`, **`Restart=on-failure`** (spec §8:226 — intentional
  `stop` exits 0 so it is not restarted), `WantedBy=default.target`, keep the live
  unit's NixOS `Environment=PATH=/run/wrappers/bin:…` (spec §14:352). Logs → journald;
  the XDG file log (`$XDG_STATE_HOME/zen-router/zen.log`, already in
  `config.Paths.LogFile`) is written by a daemon-side tee for the TUI tail (NOT
  `StandardOutput=append:` — that would sacrifice journald).
- Uninstall: `zen-router install-systemd --remove`.
- All code, comments, commits in English; replies to the user in Russian. Commits via
  `git -c user.name=kyomufs -c user.email=kyomufs@localhost` after `git status`/
  `git diff`; explicit file paths only.
- Every task ends green: `gofmt -l .` EMPTY && `go build ./...` && `go vet ./...` &&
  `timeout 180 go test -count=1 -short ./...`. `-race` unavailable (no cgo) — out of
  gate.

## Review Focus (reviewer checks these five classes hardest)

1. **Control-data correctness** — gateway 2xx records egress AND key success
   (today `RecordKeySuccess` has zero production callers); latency recorded from
   both proxy and gateway paths; identity `token`/`privateKey` NEVER serialized into
   the status payload (grep the JSON output).
2. **TUI as pure client** — every byte of dashboard data flows through
   `cli.ControlClient`; no imports of `internal/router`/`internal/quota` from
   `internal/tui`; daemon-down path surfaces a start offer without crashing.
3. **Bubbletea v2 API** — `View() tea.View` (+`tea.NewView`/`SetContent`),
   `tea.KeyPressMsg` (not `tea.KeyMsg`), declarative `v.AltScreen` (no program
   options), `tea.Tick` for the 1s poll returned from `Init()`, bubbles v2
   getters/setters (`SetWidth`); no ANSI assertions in tests (no-TTY profile is
   deterministic plain text).
4. **systemd installer safety** — unit written only under `HOME`/`XDG_CONFIG_HOME`
   from the env; `systemctl` behind an exec seam (fake on PATH in tests) asserting
   argv order `daemon-reload` → `enable --now`; `--remove` = stop/disable + unlink;
   real HOME untouched in tests.
5. **Safety** — no live systemctl, no live echo-IP call, no `~/.dsh`, no
   `~/.local/bin` rebuild; отмашка checklist present and unexecuted.

## File Structure

| File | Responsibility |
|---|---|
| `internal/quota/state.go` (modify) | latency + success additions if they live in state; `Redacted()` view of identities (strip `token`, `privateKey`) |
| `internal/gateway/handler.go` (modify) | record egress+key success on 2xx (`RecordSuccess`/`RecordKeySuccess`) and latency |
| `internal/router/router.go` (modify) | record `Result.LatencyMS` (currently discarded at `OnResult`) into state |
| `internal/cli/control.go` (modify) | serve redacted state; new fields (uptime, listen, `lastSpareError`, `registering`, egress IP, latency) |
| `internal/cli/ip.go` (new, optional) | injectable egress-IP echo seam (fake in tests; live call отмашка-gated) |
| `internal/config/config.go` (modify) | none expected — `LogFile` exists; document reuse |
| `cmd/zen-router/main.go` (modify) | `up --detach`, `tui`, `install-systemd [--remove]` subcommands; logger tee to `Paths.LogFile` |
| `internal/tui/model.go` (new) | Elm model: 1s poll loop, daemon-down state, key map, actions |
| `internal/tui/view.go` (new) | lipgloss layout: header, quota/identity/rotation tables, log viewport, help |
| `internal/tui/model_test.go` (new) | pure `Update`/`View().Content` suites (no PTY, no timers) |
| `internal/tui/render_test.go` (new) | per-screen layout assertions from canned `Status` fixtures |
| `internal/systemd/unit.go` (new) | unit template render (PATH env, on-failure, ExecStart resolution) |
| `internal/systemd/install.go` (new) | write/remove + `systemctl --user` exec seam |
| `internal/systemd/install_test.go` (new) | temp `HOME`, fake `systemctl` on PATH, exact unit-file assertions |

## Binding decisions (surfaced at review — recorded here so executors don't re-decide)

- **D1 — Bubbletea v2 on `charm.land/*`** (recommended: exact Go 1.26 match, v1 frozen
  2025-09, we need nothing teatest-only). Import paths `charm.land/bubbletea/v2`,
  `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`.
- **D2 — `s` semantics:** stop = `POST /_zenctl/stop` when the daemon responds;
  start = spawn `zen-router up --detach`. When installed under systemd, the TUI
  reports the unit as the supervisor (start/stop still via control API + spawn —
  systemctl stays installer-only, keeping the TUI free of systemd coupling).
- **D3 — egress IP:** new `egress_ip` field refreshed by an injectable echo
  (Cloudflare `cdn-cgi/trace`-style) through the active transport, refreshed on
  rotation and at most once per TUI poll interval (daemon-side, debounced); tests use
  a fake, live calls отмашка-gated.
- **D4 — log tail:** daemon tees its logger to `config.Paths.LogFile`; the TUI tails
  the file (not journald). systemd keeps `StandardOutput` default (journald).
- **D5 — dependencies install via `go get charm.land/bubbletea/v2@v2.0.10`
  (+bubbles/lipgloss) + `go mod tidy`, never by hand-editing `go.mod`** (skill rule);
  versions cross-checked against proxy.golang.org in the research pass.
- **D6 — layout rules (skill `bubbletea`):** terminal-cell/ANSI-aware measurement —
  no byte-length string slicing (clamp small terminals; measure borders per
  `references/golden-rules.md`); effectful work (HTTP polls, file tails) stays OUT of
  `View()` — only in `Update()` commands; validate wide + narrow window sizes.

## Tasks

### Task 1 — Control API data extensions

- [ ] RED first: `control_test.go` cases for gateway 2xx → `RecordSuccess` +
      `RecordKeySuccess` called (counter increments visible in `/status`), latency
      recorded per egress (last/avg), `lastSpareError` + `registering` surfaced,
      status payload has NO `token`/`privateKey` (assert via JSON round-trip grep).
- [ ] Implement: gateway 2xx path records success + latency; proxy `OnResult`
      records `Result.LatencyMS` (today discarded); `Redacted()` identity view used
      by `/_zenctl/status` only (state.json keeps full fidelity); add
      `uptime_seconds`, `listen`, `last_rotate`, `rotating` header fields.
- [ ] Green + `gofmt -l .` empty + all gates; commit
      `Expose dashboard data in the control API`.

### Task 2 — Egress IP observation

- [ ] RED: fake echo server → `/_zenctl/status` gains `egress_ip`; rotation refreshes
      it; debounce prevents per-poll fan-out; no live network in tests.
- [ ] Implement the injectable seam (interface + production echo impl, отмашка-gated
      live call); record IP per egress on rotation.
- [ ] Green + gates; commit `Track egress IP per rotation`.

### Task 3 — XDG file log + `up --detach`

- [ ] RED: daemon tees logger to `Paths.LogFile` (temp XDG state dir); `up --detach`
      starts a child, parent waits until `/_zenctl/status` answers (or timeout),
      exits 0; child handles SIGTERM cleanly. Never spawn against the live unit.
- [ ] Implement tee + `--detach` flag in `cmdUp` (parse only; usage text updated).
- [ ] Green + gates; commit `Add detached start and daemon file log`.

### Task 4 — `internal/tui` skeleton

- [ ] RED: model with 1s `tea.Tick` poll through an injected `StatusSource` (wraps
      `cli.ControlClient`); daemon-down state shows the start offer; `q` quits;
      `View().Content` contains expected header strings; tests feed
      `tea.KeyPressMsg`/poll msgs directly (never run the Tick command).
- [ ] Implement model/Init/Update/View skeleton + `zen-router tui` subcommand wiring
      (refuses with a clear message when not a TTY? — no: run normally; tests don't
      construct the program).
- [ ] Green + gates; commit `Add TUI skeleton with control API polling`.

### Task 5 — TUI dashboard sections

- [ ] RED: canned `Status` fixtures → view contains: status header (mode, egress,
      IP, latency), quota table per egress AND per key with reset countdown, identity
      pool table (redacted fields only), rotation history table (last N), log-tail
      viewport, help line.
- [ ] Implement `view.go` with lipgloss v2 + `bubbles/v2` table/viewport/help;
      layout adapts to `tea.WindowSizeMsg` (SetWidth/SetHeight).
- [ ] Green + gates; commit `Render TUI dashboard sections`.

### Task 6 — TUI actions

- [ ] RED: `r` → Rotate request; `d`/`w` → Use; `s` → stop when up / spawn
      `up --detach` when down; spinner while a request is in flight; action errors
      surface in the view (e.g. rotate 409 → message).
- [ ] Implement key handlers with in-flight guard (no double-fire during 1s poll).
- [ ] Green + gates; commit `Add TUI rotation and daemon actions`.

### Task 7 — TUI hardening tests

- [ ] Suite over every screen state × poll-error × action-error; property-ish checks:
      single terminal view per state, no ANSI escapes asserted (deterministic
      no-TTY output), redaction never leaks under any fixture.
- [ ] Green + gates; commit `Cover TUI states in hermetic tests`.

### Task 8 — `install-systemd` subcommand

- [ ] RED: unit rendered with `ExecStart=<resolved binary> up`,
      `Restart=on-failure`, `RestartSec`, NixOS `Environment=PATH=…`,
      `WantedBy=default.target`; exec seam records `systemctl --user daemon-reload`
      then `enable --now`; `--remove` records `disable --now` + unlinks the file.
      **Fake `systemctl` on PATH, temp `HOME`.**
- [ ] Implement `internal/systemd` + subcommand + usage (spec §11). Binary path
      resolution: current executable path by default (never overwrites
      `~/.local/bin` — that is the отмашка-time user action).
- [ ] Green + gates; commit `Add systemd user unit installer`.

### Task 9 — Verification gate + отмашка checklist

- [ ] Full gates: `gofmt -l .` EMPTY, `go build ./...`, `go vet ./...`,
      `timeout 180 go test -count=1 -short ./...`, new packages included;
      `go mod tidy` diff reviewed (adds `charm.land/*` v2 only).
- [ ] `./bin/zen-router help` lists `tui` and `install-systemd [--remove]` (§11).
- [ ] Document the отмашка checklist in the plan (NOT executed): rebuild
      `~/.local/bin/zen-router`, run `zen-router install-systemd` against the live
      hand-written unit (overwrites it), `daemon-reload`+`enable --now` may restart
      the running daemon, live egress-IP echo enables, README (Phase E).
- [ ] Commit `Verify phase C gates and document live steps`.

**Explicitly deferred (not in this plan):** executing the отмashка checklist (live
unit overwrite, `~/.local/bin` rebuild, live echo-IP calls), README (Phase E), thin
plugin (Plan 3), live DSH integration.

---

## Task dependency notes

- Tasks 1, 3, 8 are parallel-startable (control API / detach+log / systemd — disjoint
  files; serialize `cmd/zen-router/main.go` edits: 3 and 8 both touch it — run 3 then
  8, or split the subcommand registry edit).
- 2 after 1 (status fields); 4 after 1+3; 5 after 4 (+2 for IP field); 6 after 5;
  7 after 6; 9 last.
- `internal/tui` depends only on `internal/cli` + `charm.land/*` — no router/quota
  imports (review focus 2).
