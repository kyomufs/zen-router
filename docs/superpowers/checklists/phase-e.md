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
- [ ] **DM-6** T5 bare-429 → KindClient: keep (accepted T12, errors.go:225-227) — revisit
      only if a real bare-429 corpus appears; no action expected.
- [ ] **DM-7** T5 daily-regex not gated on `status==429` (ledger:51(3)) — low-risk
      port-fidelity divergence, recorded; decide keep-vs-gate with a corpus check.

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

- [ ] T7 two tests + `max_completion_tokens:null` (now tracked as DM-1/DM-3).
- [ ] T2 ClampEffort tie-break (DM-4).
- [ ] T10 Active-remap (DM-5).
- [ ] T5 bare-429 (DM-6), errors.go wording (done in T14), snippet UTF-8 (done in T14).

### 2b. Build + install + daemon (Plan 1 + Plan 2 surface)

- [ ] Rebuild `~/.local/bin/zen-router` (go install — forbidden while live unit runs).
- [ ] `zen-router install-systemd` (Plan 2 Task 8) — **overwrites the live hand-written
      unit** `~/.config/systemd/user/zen-router.service`.
- [ ] `systemctl --user daemon-reload && systemctl --user enable --now zen-router` — may
      restart the running daemon.
- [ ] Verify: `zen-router help` lists `tui`, `install-systemd [--remove]`, `up --detach`.
- [ ] Live smoke: `GET /_zenctl/status` (expect mode/egress/quota), `GET /v1/models`,
      TUI opens and polls, `r/d/w/s` actions work against the live daemon.
- [ ] Live egress-IP echo (Plan 2 Task 2) through the active transport.
- [ ] Log tee: `$XDG_STATE_HOME/zen-router/zen.log` receives daemon output; TUI tails it.
- [ ] Journal: `journalctl --user -u zen-router` shows daemon logs (journald default kept).

### 2c. Plan 3 cutover fence (§H — thin plugin)

- [ ] Install rebuilt thin plugin into `~/.dsh/profiles/web` (currently pinned `a416790`
      transport-proxy variant); move the pin off `a416790`.
- [ ] `dsh --profile web --dump-config` — no stderr warnings.
- [ ] DSH restart (HMR not sufficient for profile install).
- [ ] Live opencode.ai calls through the daemon via the thin plugin (stream + non-stream).
- [ ] Wire check: `Authorization: Bearer`, stickyId, error envelope reads
      `error.type` (new OpenAI shape confirmed compatible with thin-plugin parser).
- [ ] Rebuilt `~/.local/bin/zen-router` matching the daemon the plugin talks to.

### 2d. README (Phase E per spec §13)

- [x] Repo README: purpose, install, CLI surface (`up/status/rotate/use/stop/tui/
      install-systemd`), config/XDG paths, systemd notes, отмашка-safe defaults,
      architecture diagram (three prefixes, D1 rotation). — DONE (45f75b3): docs-only,
      no live surface, executed before отмашка; `tui`/`install-systemd`/`up --detach`
      marked "planned (phase C)" until Plan 2 lands — re-check those lines after C.

---

## Part 3 — final gate run (after Parts 1-2, at HEAD)

- [ ] `gofmt -l .` empty; `go build ./...`; `go vet ./...`;
      `timeout 180 go test -count=1 -short ./...` all green.
- [ ] `go mod tidy` diff reviewed (bubbletea v2 from Plan 2 only — no other additions).
- [ ] `git status` clean; ledger updated; plan workspace deleted (per SDD skill) once all
      plans are closed.
- [ ] Goal: mark complete only when the WHOLE objective (all plans + live phases) is done.
