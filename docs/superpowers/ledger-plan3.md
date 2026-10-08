# Plan 3 Ledger — dsh-opencode-zen thin adapter

Status: **CLOSED — APPROVED** (whole-branch review, no Critical/Important).
Base: `c2471d2` → HEAD `9890e55` (10 commits, 5 files, +1500/−2372).
Docs branch (this repo): `2fe7fd9` + `f30fd1f` + `bd047e7` (cutover checklist).
Suite: 64 passed / 0 failed, exit 0 (2 whole-branch runs + every task review).
LOC: `lib/index.js` 396 (cap 400); `test/smoke.cjs` 1411; version 0.16.0.

## Tasks

| # | Subject | Commit | Review |
|---|---------|--------|--------|
| 1 | thin-adapter contract tests (RED at c2471d2) | `c768070` + fix `2e87478` | fix 1 → APPROVE |
| 2 | module skeleton, static MODELS parity, adapter shell | `8f67dbc` | APPROVE |
| 3 | stream() transport — serializer, attribution, zero retry | `1884f7b` + fixes `788abc1` | fix 2 → APPROVE |
| 4 | minimal SSE translator | `4f7257d` | APPROVE (attempts 1–2 died, 3 salvaged) |
| 5 | error mapping table with typed failure snapshots | `2c26ea8` + fix `8a476ee` | fix 1 → APPROVE |
| 6 | apply() health ping with gated autostart | `9497e72` | APPROVE first pass |
| 7 | verify + budget + commit (quota prune, forbidden greps, ref sweep, bump) | `9890e55` | APPROVE first pass |
| 8 | gated cutover checklist (documented, not executed) | `2fe7fd9` + fixes `f30fd1f`, `bd047e7` | fix 2 → 0 OPEN |

## Constraints verified at HEAD (whole-branch review)

- No mutable state; no in-process retry (`hits===1` pinned).
- MODELS 9 entries byte-parity vs `a416790` (entries-only diff EMPTY).
- Error mapping by status + `error.type` only; no message-text routing.
- `stream:true`, no `stream_options`; base `http://127.0.0.1:8787`; API key optional.
- Single POST `/v1/chat/completions`; zero new deps; `ls lib/` = index.js only.
- Mapping table 16/16 rows conformance (plan:128-163 → lib lines cited in review).
- Failure snapshot contract (code + failure agree) checked per row via host `normalizeLlmFailure`.
- `cordis.patch.yml` 0-byte diff; export pin byte-identical across 5 commits.

## Deferred minors (report-only / Phase E, non-blocking)

1. T4 M1-M2: task-4-report:51/:63 `types.d.ts` refs `:335-365`/`:330` (actual `:417-443`/`:412`) — trivial 2-line fix by choice.
2. T4 M3: thinning-log 10+1→9+1 reflow wording.
3. T4 M4: MODELS rationale uncaptured (HEAD provenance comment lib:12 preserved).
4. T4 M5: tool-call-delta absent-wire edge + parallel fragmentation — optional test-only in Phase E.
5. T6 M1: report LOC log −4 off-by-one.
6. T6 M2: 3 uncaptured health-warn lines in suite stdout — optional `withWarnCapture` in Phase E.
7. T6 M3: theoretical straggler-ping flake (not reproduced in 2 runs).
8. T7 M1-M5: task-2-report:23/:17 era-ref wording; task-7-report:32 `grep -i` note, :10 fixture-coverage note, :17 verbatim-subject note.
9. Whole-branch Minors 1-6: no literal `[Task 1]` header (naming); module-level `let dshLlm` (load-time binding); vestigial `OPENCODE_ZEN_POOL_FILE` pin (isolation belt); literal `opencode.ai` in a smoke comment (zero traffic — fixtures bind 127.0.0.1); health-warn noise; README.md 0-byte diff (Phase-E deferral, honest in Task 8).
10. CLEARED in T7: stale quota header + `QUOTA_FILE` (grep = 0). T5 F2 6xx: closed in task-5-report:121.

## Fence (hard)

Cutover documented in `docs/superpowers/cutover-plan3-thin.md` — **NOT EXECUTED**.
Requires explicit отмашка (spec §12): profile pin move off `a416790`, dump-config,
DSH restart, rebuilt zen-router + live systemd, live smoke + DSH round-trip.
Phase E open items: DM-6, DM-7, Part 2a/2b/2c, Part 3 final gate — all §12-gated.

## Rulings (extracted from plan3 workspace before archiving)

- Ledger ruling (progress.md:2): serial order T1→T8 — one implementer at a time in `lib/index.js`, even though plan allowed 2-5 parallel.
- Ruling A: LOC table + thinning log discipline for ≤400 budget (task-3-report §4, 17-row log).
- Ruling B: body-shape gate — tests assert `stream:true` only, `stream_options` OMITTED; fixes were tests-only add/extend (task-3-report §3).
- Ruling C: HTTP status rows stay RED until Task 5; `error.response` stashed for Task 5 to map (task-3-report:199).

Workspace `.superpowers/sdd/plan3/` archived to Trash after close (reports/review packages recoverable there).

## ⚠️ Cannot verify (recorded)

- Full recount of T7 ref-sweep (~289 replacements; ~54-ref sample, 0 errors).
- RED-at-base chronology and per-fix "ok-lost NONE" historical claims.
- Long-run flake rate; host-pin checks depend on nix-store dsh-llm (not re-read independently).

## Phase E Part 2c execution (2026-10-08, отмашка active)

- Step 1 DONE: pin moved `a416790` → `9890e557734e780f406232c6d85ec2e2427c1c96` via `dsh plugin --profile web add`; package.json:13 = new SHA, `a416790` hits = 0, node_modules version 0.16.0 (thin, main lib/index.js).
- Step 2 DONE: `dsh --profile web --dump-config` rc=0, stderr 0 bytes (1613-line dump, plugin present).
- Step 3 OPEN: DSH restart — user-managed (signal given 2026-10-08).
- Step 4-5 OPEN: live opencode.ai stream + non-stream through thin plugin, wire checks (Bearer, stickyId, error.type) — after restart.
- Step 6 DONE: rebuilt binary mtime 14:55:01 == running daemon (pid 31217, same path), phase-e.md:112 ticked.
