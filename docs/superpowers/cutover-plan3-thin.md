# Plan 3 Cutover — `dsh-opencode-zen` thin adapter (Phase D)

> **PLAN 3 CUTOVER — DOCUMENTED, NOT EXECUTED. Requires explicit отмашка (spec §12).**

This is the Task 8 deliverable of
`docs/superpowers/plans/2026-10-07-dsh-opencode-zen-thin.md:301-315` (brief:
`.superpowers/sdd/plan3/task-8-brief.md:3-15`). Every command below is a **fenced
documentation string only** — none was executed to author this file, and none may be
executed before the user gives an explicit "отмашка"
(spec `docs/superpowers/specs/2026-10-06-zen-router-gateway-design.md:325-329`).
Until then the installed plugin v0.15.1 keeps running untouched
(spec `…2026-10-06-zen-router-gateway-design.md:331`).

Reference aliases used below: **plan** =
`docs/superpowers/plans/2026-10-07-dsh-opencode-zen-thin.md`, **spec** =
`docs/superpowers/specs/2026-10-06-zen-router-gateway-design.md`, **core plan** =
`docs/superpowers/plans/2026-10-06-zen-router-core.md`, **phase-e** =
`docs/superpowers/checklists/phase-e.md`.

## Prerequisites (state verified at write time — read-only checks, no cutover action)

1. **Rebuilt `~/.local/bin/zen-router` (Plan 1 deferred).** Plan 1 deferred restarting
   the live unit and rebuilding `~/.local/bin/zen-router` to отмашка
   (core plan `:198`); `go install` over that path is forbidden while the live unit runs
   (core plan `:16`, restated phase-e `:80` and plan-2 plan
   `docs/superpowers/plans/2026-10-07-zen-router-phase-c-tui.md:294-295`). Current state:
   the installed binary's mtime is `2026-10-06 01:19` — the very build spec §9c records
   as "built 01:19" (spec `:267-268`) and as **transport-only: `GET /v1/models` → 404**
   (spec `:273-274`). No rebuild has occurred since; Step 4 must land before Step 5.
2. **Live systemd unit — never touched by this cutover.**
   `~/.config/systemd/user/zen-router.service` is the hand-written unit (enabled and
   active, spec `:268-270`). This document never edits, reloads, or restarts it
   (core plan `:16`). Plan 2's `zen-router install-systemd` **overwrites** that unit
   (phase-e `:81-82`) and belongs to phase-e Part 2b — not to this cutover's steps.
3. **Daemon reachable at loopback:8787; existing pid must not be confused.** Both the
   CLI and the client default to `127.0.0.1:8787` — a single listener (spec `:265-266`);
   the thin plugin's base URL defaults to the same (plan `:215-217`). `pgrep -af
   zen-router` at write time shows one live daemon: **pid 1328**
   (`~/.local/bin/zen-router up`); spec §9c recorded pid 1459 on 2026-10-06 (spec `:267`)
   — pids change across restarts, so identify the current pid at cutover time and never
   start a second daemon beside it. "Running the daemon next to the live agent" is
   itself отмашка-gated (spec `:328-329`).
4. **Current plugin pin `a416790`.** `~/.dsh/profiles/web/package.json:13` reads
   `"dsh-opencode-zen": "github:kyomufs/dsh-opencode-zen#a416790a1487717bd4532ab9905788013e8fbaea"`
   (verified at write time). This is the state Step 1 moves OFF; the pin moves to the new
   commit only after отмашка (spec `:261`). Plugin source repo
   (`/home/kyomufs/Projects/dsh-opencode-zen`) is clean at `9890e55` (v0.16.0) and must
   stay clean — the cutover changes only the profile pin.

## Cutover steps — the 5 fenced checkboxes (brief `:3-11`, plan `:303-311`)

> **⛔ DO NOT RUN ANY STEP BELOW BEFORE THE USER'S ОТМАШКА (spec §12).**
> Every command in this section exists as fenced documentation text only.

### Step 1 — Profile pin move off `a416790`

> **⛔ DO NOT RUN before отмашка** (spec `:326-329`; plan `:43-45`).

**Command** (plan `:303-304`, verbatim):

```sh
dsh plugin --profile web <pnpm args>
```

`<pnpm args>` is a deliberate placeholder — no exact pnpm arguments are pinned anywhere
in the repo; they are chosen at отмашка time and must target the new thin-adapter commit
(plugin HEAD `9890e55` / v0.16.0 at write time).

**Expected result:** the command rewrites `profiles/web/package.json` (plan `:304`) so the
`dsh-opencode-zen` entry points at the new commit instead of `a416790…`.

**Verification:**
- `grep -n 'a416790' ~/.dsh/profiles/web/package.json` → **zero hits**.
- `grep -n 'dsh-opencode-zen' ~/.dsh/profiles/web/package.json` → exactly one line, now
  showing the new commit SHA.

### Step 2 — `dsh --profile web --dump-config` → zero stderr warnings

> **⛔ DO NOT RUN before отмашка** (spec `:327-328`; plan `:43-45`).

**Command** (plan `:305`, spec `:328`):

```sh
dsh --profile web --dump-config
```

**Expected result:** config dump on stdout; **zero stderr warnings** (plan `:305`).

**Verification:** route fd 2 to a file (`… 2>/tmp/dsh-dump-config.stderr`) and require
`wc -c` of that file = 0 (zero bytes on stderr).

### Step 3 — DSH restart

> **⛔ DO NOT RUN before отмашка** (spec `:328`; plan `:43-45`).

**Command:** none documented — restart DSH by its normal means; config/profile changes
apply only after restart (plan `:306`; HMR is not sufficient for a profile install,
phase-e `:97`). **Never start a replacement `dsh web` server** (plan `:306-307` — the GUI
owns the port; just note that a restart is needed).

**Expected result:** the restarted DSH session loads the profile with the Step-1 pin.

**Verification:** the `dsh-opencode-zen` plugin appears active in the new session, and a
re-run of Step 2's fenced dump-config stays at zero stderr bytes.

### Step 4 — Prerequisite: rebuilt `~/.local/bin/zen-router` + live unit serving the OpenAI surface

> **⛔ DO NOT RUN before отмашка** (Plan 1 deferred this to отмашка, brief `:8-9`,
> core plan `:198`).

**Commands** (fenced documentation only):

```sh
go install            # rebuild to ~/.local/bin/zen-router — phase-e :80; FORBIDDEN while
                      # the live hand-written unit runs (core plan :16, plan-2 :294-295)
zen-router help       # must list: tui, install-systemd [--remove], up --detach (phase-e :85)
```

The exact `go install` package arguments are **not pinned in-repo** — only the target
path `~/.local/bin/zen-router` and the "forbidden while the live unit runs" constraint
are documented (core plan `:16`, phase-e `:80`); the stop/rebuild/start sequence is
decided at отмашка. This step does **not** include `install-systemd` or any `systemctl`
invocation — those live in phase-e Part 2b (`:81-84`) and would overwrite/restart the
hand-written unit.

**Expected result:** `~/.local/bin/zen-router` is rebuilt from the current zen-router HEAD
(mtime newer than the newest commit), and the unchanged live unit is now serving that
binary with the OpenAI surface mounted.

**Verification:** `stat` shows a fresh mtime; `zen-router help` matches phase-e `:85`;
the OpenAI surface itself is proven by Step 5 (`GET /v1/models` → 200, not the
pre-rebuild 404 of spec `:273-274`).

### Step 5 — One live smoke: `GET /v1/models` + DSH chat round-trip

> **⛔ DO NOT RUN before отмашка** — this includes the first live `opencode.ai` traffic
> (spec `:326`).

**Commands** (fenced documentation only):

```sh
curl -sS http://127.0.0.1:8787/v1/models
```

then one **DSH chat round-trip**: send a single chat message in the DSH GUI using an
`opencode` model and confirm a streamed reply end-to-end.

**Expected result:** `GET /v1/models` returns 200 with the OpenAI-shaped model list the
thin plugin refreshes from (plan `:9-10`, `:209-210`); the chat message streams back
through the daemon → thin plugin → DSH. A 404 from `/v1/models` means Step 4 did not land
(spec `:273-274` documents 404 as the pre-rebuild symptom).

**Verification:** non-empty model list; chat completes; wire expectations —
`Authorization: Bearer`, stickyId, error envelope read via `error.type` — checked per
phase-e `:99-100`. This is the only sanctioned live smoke; repeat live calls afterwards
only under the Phase E / отмашка umbrella (phase-e Part 2 `:67-69`).

## Explicitly deferred (not in this plan) — brief `:13-15`, verbatim-in-spirit

- **Everything in §H's fence list** — profile install, pin move, dump-config, DSH
  restart, live opencode.ai calls (Phase E/отмашка).
- **TUI** (Plan 2).
- **README of zen-router** (Phase E).

Note on §H: no literal "§H" heading exists in this repo (repo-wide grep returns only the
references plan `:111`, plan `:313`, phase-e `:9`, phase-e `:92`). Its concrete fence
list is phase-e Part 2c "Plan 3 cutover fence (§H — thin plugin)" (phase-e `:92-101`)
plus the plan's Global Constraints (plan `:43-45`). README status cross-check: the base
README is already closed `[x]` at phase-e `:105-109` (commit `45f75b3`), with a residual
"re-check `tui`/`install-systemd`/`up --detach` lines after Plan 2" note (`:108-109`).

## Rollback

**No automated rollback documented** — a repo-wide grep for `rollback` across `docs/` and
`.superpowers/` returns zero hits at write time. If the cutover fails, **restore
`profiles/web/package.json` pin to `a416790` manually**: set the `dsh-opencode-zen` entry
in `~/.dsh/profiles/web/package.json` back to
`"github:kyomufs/dsh-opencode-zen#a416790a1487717bd4532ab9905788013e8fbaea"`, then re-run
the fenced Step 2 (dump-config must stay at zero stderr) and fenced Step 3 (DSH restart),
and re-verify with `grep -n 'a416790' ~/.dsh/profiles/web/package.json` (one hit). The
daemon/unit side needs no rollback: this cutover never touches the systemd unit or the
binary (Steps 4+ are prerequisites, not mutations of the plugin).

## Cross-references

- **Plan Task 8 + §H:** plan `:301-315` (5 checkboxes + deferred list), `:111` and `:313`
  (§H fence references), `:324-325` ("Task 8 never runs in this plan"), `:43-45`
  (Global Constraints: never touch `~/.dsh`).
- **Spec:** §12 spec `:317-331` (отмашка gates: live opencode.ai `:326`, profile install
  / dump-config / DSH restart / daemon-beside-agent `:327-329`, v0.15.1 untouched `:331`);
  §9b spec `:261` (pin moves only after отмашка); §9c spec `:263-276` (live runtime
  state); §13 spec `:333-341` (phase D: "do not install it before отмашка" `:340-341`).
- **phase-e.md open items (all §12-gated):** DM-6 phase-e `:32-33`, DM-7 `:34-35`,
  Part 2a `:71-76`, Part 2b `:78-90`, Part 2c `:92-101`, Part 3 final gate `:113-120`.
- **Pattern precedent:** plan-2's отмashка checklist was documented by Task 9 and never
  executed (`docs/superpowers/ledger-plan2.md:58`); Plan 1's deferrals sit in
  `docs/superpowers/ledger-plan1.md` (`:105`, `:110`, `:120`).

## Execution record (authoring session)

- Zero cutover commands executed: no `dsh`, no `systemctl`, no profile install, no live
  `opencode.ai` call, no DSH restart, no daemon/unit touch. Only read-only
  grep/sed/stat/pgrep plus the loopback test suite were run.
- Final gate: plugin suite `npm test` at `9890e55` → **64 passed, 0 failed, exit 0**.
