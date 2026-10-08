# Phase E Checklist — zen-router final gate (README + live verification + deferred minors)

> **Every "live" item below is отмашка-gated (spec §12). Nothing in this checklist may be
> executed without an explicit user go-ahead.** Deferred-minor items are docs/tests-only
> and may be executed at any time; they are batched here per ledger rulings.

**Sources:** `docs/superpowers/ledger-plan1.md` (deferred minors 13 = 7 + 6), whole-branch
re-run verdict (APPROVE-PLAN1, 3 nits), spec §12/§13 phase E, Plan 2 task 9 checklist,
Plan 3 §H cutover fence.

---

## Part 1 — Deferred minors (13) — safe to do anytime, no live surface

Batch rules: one commit for the test-only items, one for docs/comments; every change must
keep gates green (`gofmt -l .`, `go build ./...`, `go vet ./...`,
`timeout 180 go test -count=1 -short ./...`).

### From first whole-branch run (7)

- [x] **DM-1** (closed 0c44866) T7 two missing translation tests: probe-absent path + `reasoningRequired→high`
      (behavior implemented responses.go:346-365, clamp side covered models_test.go:218+).
- [x] **DM-2** (closed 0c44866) T7 ChatToResponses input-immutability test (body decoded fresh per request,
      handler.go:134 — pin by construction).
- [x] **DM-3** (closed 0c44866) T7 `max_completion_tokens:null` edge test (null → model cap, never
      over-budget; responses.go:379-398, doc :368-371).
- [x] **DM-4** (closed 0c44866) T2 ClampEffort tie-break test (ruled to plan ledger:13, documented
      models.go:97, unreachable today).
- [x] **DM-5** (closed ade6f88) T10 Active-remap: null-slot → remap `Active` (state.go:138-146 filters
      but does not remap; only hand-edited/corrupt JSON can trigger; clamped on read
      state.go:203-210).
- [x] **DM-6** (closed b6f53be, mark in this batch) T5 bare-429 → KindClient: KEEP (accepted T12,
      errors.go:235-239) — corpus check: zero bare-429-without-type instances anywhere (smoke.cjs
      `'429-catchall'` carries `InvalidRequestError`; gateway always types 429s per spec §4), so
      "no action expected" holds; end-to-end bare 429 → KindClient → errorClass InvalidRequestError
      → thin-plugin RATE_LIMIT row → host backoff matches golden lib/index.js:815. Revisit only if
      a real bare-429 corpus appears.
- [x] **DM-7** (closed b6f53be, mark in this batch) T5 daily-regex GATED on `status==429`
      (ledger:51(3)) — corpus check found zero non-429 + quota-class instances (errors_test.go,
      smoke.cjs, spec §4 are all 429), golden gates the same match (a416790 lib/index.js:1579
      guards the raw test at :1583); gated at BOTH sniff sites: zen.classify (errors.go:197) and
      proxy.wrapBody (proxy.go:185 — found during this corpus sweep; proxy feeds router.OnResult
      where the DailyLimit branch precedes 2xx success, so ungated it let a 200 SSE stream quoting
      a class name within the first 8 KiB mark the daily window and spawn rotation). Typed
      kindByErrorType path deliberately stays ungated (explicit error.type = strong evidence,
      spec §4 type-over-status). RED→GREEN both sites, no existing pin changed.

### From T14 re-review (6)

- [x] **DM-8** (closed ade6f88) buffer.go `merge` concatenates ALL choices (n>1 hypothetical — upstream is
      Zen/Anthropic-backed, no `n`): skip `c.Index > 0` or reject `n>1` in shaping + test.
- [x] **DM-9** (closed c1acea6) handler.go:571 comment cites "§5/§9d metadata rule" — actual rule is §4:140.
      Comment-only fix.
- [x] **DM-10** (closed c1acea6) errors.go:297 comment overstates plugin parity — plugin `Number()` ACCEPTS
      1.5, port rejects (spec-correct divergence); note it like errors_test.go:329 does.
- [x] **DM-11** (closed 0c44866) No test pins `buf.reset()` after a partially-buffered failed attempt +
      re-issue (unreachable with live rotator: post-buffer failures are KindTransport →
      NextAttempt false). Defense-in-depth: fake-Rotator test — attempt 1 emits a frame
      then dies, attempt 2 succeeds, assert final JSON has only attempt-2 content.
- [x] **DM-12** (closed 0c44866) Zero-frame flush untested: 2xx upstream stream with only [DONE] →
      200 + `content:""` + finish `"stop"` (valid semantics, pin it).
- [x] **DM-13** (closed ade6f88) Upstream `"metadata":null` passes `json.Valid` → client gets
      `metadata:null` instead of `{}` (handler.go:586-591 + probeEnvelope:505-507).
      Treat `null` as absent.

### Whole-branch-2 nits (batch with DM-1/DM-3)

- [x] **N-1** (closed c1acea6) `TestResponsesAutoRoute` subtest-1 is no longer a force-proof (client already
      sends `stream:true`); note in test comment — force pinned by
      TestNonStreamingResponsesLane instead.
- [x] **N-2** (closed ade6f88) `review-package.sh`: empty `=== PLAN SLICE ===` header when a plan arg is
      absent — emit the header only when content follows.
- [x] **N-3** (closed 0c44866) No responses-lane test with `stream` key *omitted* (redundant — same
      `body["stream"].(bool)` path as `false`; add for symmetry if touching the file anyway).

---

## Part 2 — отмашка-gated live items (spec §12)

**Gate: user says "отмашка" explicitly for this phase. Do not run any item below before that.**

### 2a. Plan 1 deferred-minors batch (from earlier ledger)

- [x] T7 two tests + `max_completion_tokens:null` — closed as DM-1/DM-3 (0c44866).
- [x] T2 ClampEffort tie-break — closed as DM-4 (0c44866).
- [x] T10 Active-remap — closed as DM-5 (ade6f88).
- [x] T5 bare-429 — closed as DM-6 (b6f53be); errors.go wording (done in T14), snippet UTF-8 (done in T14).

### 2b. Build + install + daemon (Plan 1 + Plan 2 surface)

- [x] Rebuild `~/.local/bin/zen-router` (go install — forbidden while live unit runs).
      — DONE 2026-10-08: atomic stop→install→start, mtime 14:55, unit restarted.
- [x] `zen-router install-systemd` (Plan 2 Task 8) — **overwrites the live hand-written
      unit** `~/.config/systemd/user/zen-router.service`. — DONE: handwritten unit
      backed up to /tmp/zen-router.service.handwritten.bak, new unit installed
      (`ExecStart=~/.local/bin/zen-router up`, Restart=on-failure/5s, default.target).
- [x] `systemctl --user daemon-reload && systemctl --user enable --now zen-router` — may
      restart the running daemon. — DONE: unit active, WantedBy=default.target enabled.
- [x] Verify: `zen-router help` lists `tui`, `install-systemd [--remove]`, `up --detach`.
      — DONE: all three present in help output.
- [x] Live smoke: `GET /_zenctl/status` (expect mode/egress/quota), `GET /v1/models`,
      TUI opens and polls, `r/d/w/s` actions work against the live daemon.
      — DONE: status 200 (mode/egress/state.egress quotas + egress_ip; no literal
      `quota` key — quota lives in state.egress/per-key tables, surfaced by the TUI
      "quota (per egress/per key)" widgets); models 200; TUI under `script` pty
      renders the live dashboard (recipe: `stty rows/cols` + `TERM=xterm-256color`
      inside the pty — bare pty defaults to 0x0 → empty frame, inherited
      `TERM=dumb` skips alt-screen). Keys: r → "rotate in flight" + daemon journal
      attempt (register blocked: api.cloudflareclient.com unreachable from this
      host, cf_http:000 7s timeout — env fact, direct path healthy); w → control
      API busy during warp switch attempt (serialized handler; status polls time
      out while register is in flight); s → stop branch (journal "stopped",
      TUI "down (…not running)" + hint) and spawn branch ("start daemon in
      flight" → new pid, dashboard back to "up"); d → direct confirmed; q → clean
      exit rc=0. Daemon reconciled to systemd unit + current=direct after runs.
- [x] Live egress-IP echo (Plan 2 Task 2) through the active transport.
      — DONE: `egress_ip: 176.212.216.125` via ACTIVE transport (ipify), flag
      `~/.config/zen-router/config.json {"egressIPEcho": true}`.
- [x] Log tee: `$XDG_STATE_HOME/zen-router/zen.log` receives daemon output; TUI tails it.
      — DONE: zen.log grows on start/stop lines; TUI log-tail widget shows them live.
- [x] Journal: `journalctl --user -u zen-router` shows daemon logs (journald default kept).
      — DONE: unit logs + control lines visible (stop/start/rotation entries).

### 2c. Plan 3 cutover fence (§H — thin plugin)

- [x] Install rebuilt thin plugin into `~/.dsh/profiles/web` (currently pinned `a416790`
      transport-proxy variant); move the pin off `a416790`.
      — DONE 2026-10-08: `dsh plugin --profile web add github:kyomufs/dsh-opencode-zen#9890e557734e780f406232c6d85ec2e2427c1c96`,
      package.json:13 has new SHA, `a416790` hits = 0, installed node_modules
      reports version 0.16.0 (thin, main lib/index.js).
- [x] `dsh --profile web --dump-config` — no stderr warnings.
      — DONE: rc=0, stderr 0 bytes (1613-line dump), plugin name present in output.
- [ ] DSH restart (HMR not sufficient for profile install).
- [ ] Live opencode.ai calls through the daemon via the thin plugin (stream + non-stream).
- [ ] Wire check: `Authorization: Bearer`, stickyId, error envelope reads
      `error.type` (new OpenAI shape confirmed compatible with thin-plugin parser).
- [x] Rebuilt `~/.local/bin/zen-router` matching the daemon the plugin talks to.
      — DONE: binary mtime 2026-10-08 14:55:01, live daemon pid 31217 runs
      `/home/kyomufs/.local/bin/zen-router up` (same path, started 15:24:37).

### 2d. README (Phase E per spec §13)

- [x] Repo README: purpose, install, CLI surface (`up/status/rotate/use/stop/tui/
      install-systemd`), config/XDG paths, systemd notes, отмашка-safe defaults,
      architecture diagram (three prefixes, D1 rotation). — DONE (45f75b3): docs-only,
      no live surface, executed before отмашка; `tui`/`install-systemd`/`up --detach`
      marked "planned (phase C)" until Plan 2 lands — **re-check DONE** (see 2e).

### 2e. README re-check after Plans C/D close (docs-only, from 2d)

- [x] Removed "planned (phase C/D)" markers: status table C/D now **done**
      (D: rewritten, not installed); CLI block merged with the canonical
      `usage()` text (`up --detach`, `tui [--listen ADDR]`, `install-systemd [--remove]`);
      repo layout adds `internal/tui/, systemd/`; installed-plugin paragraph names
      v0.15.1 (pinned) vs rebuilt thin v0.16.0 awaiting
      `docs/superpowers/cutover-plan3-thin.md`.

---

## Part 3 — final gate run (after Parts 1-2, at HEAD)

- [x] `gofmt -l .` empty; `go build ./...`; `go vet ./...`;
      `timeout 180 go test -count=1 -short ./...` all green.
      — DONE 2026-10-08: gofmt 0 files, build/vet OK, 12 packages ok / 0 FAIL
      (re-run once more at finale after docs commits).
- [x] `go mod tidy` diff reviewed (bubbletea v2 from Plan 2 only — no other additions).
      — DONE: `go mod tidy` → `git diff go.mod go.sum` EMPTY; go.mod carries
      `charm.land/bubbletea/v2 v2.0.10` (Plan 2 T4 addition, with lipgloss v2 + bubbles v2).
- [ ] `git status` clean; ledger updated; plan workspace deleted (per SDD skill) once all
      plans are closed.
- [ ] Goal: mark complete only when the WHOLE objective (all plans + live phases) is done.
