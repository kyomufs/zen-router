# dsh-opencode-zen Thin Adapter (Phase D) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the 1736-line installed DSH plugin with a ~250-line thin adapter that
delegates all business logic — SSE translation, quota, rotation, watchdogs, disguise,
session ids, gate tools, Responses routing, retry — to the `zen-router` daemon's OpenAI
surface (`POST /v1/chat/completions`, `GET /v1/models`), leaving only: provider/model
registration, one POST + SSE→StreamChunk translation, an HTTP→DSH error map, and an
`apply()` health ping.

**Architecture:** The plugin stays a plain CommonJS cordis service
(`{name, inject:['llm'], apply}`) that registers an `LlmAdapter` via
`ctx.llm.registerAdapter(['opencode'], adapter)`. The adapter's `stream()` is a single
fetch to the daemon with the host's `AbortSignal`; every daemon responsibility (body
shaping, watchdogs, staged rotation, quota counters, `[DONE]` synthesis) is already live
from Plan 1. The plugin holds **no mutable state**: the model table is static, the live
catalog (`GET /v1/models`) only refreshes ids, and sessions/identity are daemon-derived
from the conversation body.

**Tech Stack:** Node (CommonJS, no build step), `node:http` fixtures, existing
`node test/smoke.cjs` runner, `node:assert`. Host pins: nix store `dsh 0.2.0-rc.2`
(`dsh-llm`, `dsh-llm-retry`), `@deepseek-ai/cordis ^4.0.1`, `dsh-llm >=0.1.0-rc.6`,
`schemastery ^3.18.1`.

**Spec:** `docs/superpowers/specs/2026-10-06-zen-router-gateway-design.md` §9 (thin
contract), §9b (source repo state), §9d (host-verified DSH anchors), §2 goal 4 +
non-goals, §12 (отмашка fences), §13 phase D.

**Code location:** `/home/kyomufs/Projects/dsh-opencode-zen` (GitHub
`kyomufs/dsh-opencode-zen`) — a DIFFERENT repo from zen-router. The plan document lives
in zen-router's `docs/superpowers/plans/` beside its spec; executors work in the plugin
repo. Base the adapter on the installed `a416790` lineage's adapter knowledge; do NOT
blindly merge HEAD `c2471d2` (its `/zen/v1` default targets the transport-proxy variant
superseded by the OpenAI-shim decision).

## Global Constraints

- **Never touch `~/.dsh`** — no profile edits, no `dsh plugin` install, no
  `dsh --profile web --dump-config`, no DSH restart, no pin move off `a416790`.
  The installed v0.15.1 keeps running untouched until the user's отмашка (§12).
- **Never make a live call to `opencode.ai`** in tests; all fixtures are local
  `node:http` servers on the loopback interface (fake daemon + canned SSE + error
  envelopes + refused sockets).
- `cordis.patch.yml` fragment ships byte-identical to the installed one (spec non-goal:
  entry point stays as-is).
- `lib/` stays ≤ ~275 lines (target 250; the 9-model table is the main fixed cost —
  count is reported, not hard-failed, at ≤275).
- **No in-process retry loop** — host retry policy owns retries
  (`dsh-llm-retry/lib/index.js:160` gates on `policy.retryableCodes`).
- Map errors by **status + `error.type`**, never by message text (§9d).
- Preserve the 401 trap: `401 + ModelError` (upstream unknown-model) → non-retryable
  `PROVIDER_ERROR`, NOT terminal `INVALID_CREDENTIAL` (parity: installed
  `lib/index.js:737`). Terminal `INVALID_CREDENTIAL` is only `401 + AuthError`.
- `providerRetryPolicy()` MUST return `resolveRetryPolicy(RETRY_POLICY_CONFIG, '…')`
  from `dsh-llm` — hand-flattened shapes risk NaN backoff (§9d; installed plugin's
  fallback at `lib/index.js:138-152`).
- Tests pin host API expectations against the nix store path
  `/nix/store/qph9ndg6jv1q0gd819j7am0h7nhs8kpy-dsh-0.2.0-rc.2/lib/node_modules/@deepseek-ai/dsh`
  (API-drift risk §9d).
- All code, comments, commits in English; replies to the user in Russian. Commits via
  `git -c user.name=kyomufs -c user.email=kyomufs@localhost` after `git status`/
  `git diff`; explicit file paths only.

## Review Focus (reviewer checks these five classes hardest)

1. **Error mapping fidelity** — every row of the mapping table has a fixture asserting
   the EXACT DSH code string (and `providerRetryAfterMs` when `Retry-After` is present);
   `401 ModelError` vs `401 AuthError` split; 429+`FreeUsageLimitError` → `QUOTA`.
2. **StreamChunk contract** — exactly one terminal `finish`, `usage` emitted BEFORE
   `finish` and nothing after; `block-start`/`block-end` pairing for text/reasoning/tool
   blocks; premature close BEFORE content → retryable `TRANSPORT`, AFTER content →
   salvage-as-stop (no dangling `STREAM_TRUNCATED` on partial answers).
3. **Host-contract surface** — adapter methods match `LlmAdapter`
   (`dsh-llm/lib/types/index.d.ts:187+`): `providerInfo`, `providerRetryPolicy`,
   `imageRequestPricing`, `listModels`, `resolveModel`, `prepareCall`, `stream`;
   model metadata (`contextWindow`, `defaultMaxTokens`, `reasoning.efforts`, `vision`)
   byte-equal to the installed plugin's `MODELS` table.
4. **Deletability** — grep proves the forbidden subsystems are absent from `lib/`
   (panel, status server, quota store, key pool, family pool, disguise, session,
   Responses builder, watchdogs, retry loop); LOC budget reported.
5. **Safety** — no writes outside the plugin repo + temp dirs; zero `~/.dsh` access;
   tests loopback-only; the отмашка fence list (§H) is untouched.

## File Structure

| File | Responsibility |
|---|---|
| `lib/index.js` (rewrite) | cordis service + adapter: `apply`, `name`, `inject`, `PROVIDER`, `MODELS`, `stream`, error map, SSE translator |
| `test/smoke.cjs` (rewrite) | fixture fake-daemon tests: SSE sequences, error envelopes, down socket, parity vs installed MODELS |
| `test/fixtures/*.json` (new, optional) | canned daemon responses (error envelopes, model list) |
| `package.json` (modify) | version bump; `main`, `dsh.bundle.patch` unchanged |
| `cordis.patch.yml` (unchanged) | byte-identical to installed fragment |
| `README.md` (modify) | describe the thin adapter + daemon prerequisite |

`lib/quota.js`, `lib/client.js`, `lib/status.js` and friends are DELETED (the
`c2471d2` lineage already dropped `status.js` + `client.js`).

## Dependency-derived mapping table (binding — Task 5 implements exactly this)

| Daemon response | DSH code | Retryable? |
|---|---|---|
| 429 + `Retry-After` (any `error.type`) | `RATE_LIMIT` + `providerRetryAfterMs` | yes (host backoff overridden) |
| 429 + `FreeUsageLimitError`/quota body | `QUOTA` | no |
| 5xx / `ServerError` / relay failure | `SERVER` | yes |
| socket error, ECONNREFUSED, reset | `TRANSPORT` | yes |
| deadline / premature close, pre-content | `TIMEOUT` | yes |
| premature close, post-content | salvage: terminal `finish` (stop) | n/a |
| 401 + `AuthError` | `INVALID_CREDENTIAL` | no |
| 401 + `ModelError` (unknown model) | `PROVIDER_ERROR` | no |
| 400 + `InvalidRequestError` | `PROVIDER_ERROR` | no |
| 413 / context body | `CONTEXT_WINDOW_EXCEEDED` | no |
| stream ends, zero content, no finish | `EMPTY_RESPONSE` | yes |
| daemon down at `apply()` | warn + config-gated `zen-router up --detach`; registration still proceeds | n/a |

## Tasks

### Task 1 — Contract tests first (failing)

- [ ] In `test/smoke.cjs` (rewrite), stand up a local fake daemon (`node:http`) that
      serves: (a) canned OpenAI SSE chat streams (happy path with text+usage+`[DONE]`),
      (b) each row of the mapping table (status + envelope + `Retry-After`),
      (c) connection-refused (down socket), (d) premature close pre- and post-content.
- [ ] Write assertions FIRST against the thin adapter that does not exist yet: exact
      `StreamChunk` sequences (`block-start` → `text-delta`* → `block-end` → `usage` →
      `finish`), exact DSH error codes, `providerRetryAfterMs` values.
- [ ] Keep the runner contract: `npm test` → `node test/smoke.cjs`, plain
      `check`/`checkAsync` harness on `node:assert`, zero new deps.
- [ ] Quote the RED output; tests necessarily fail (old adapter expects direct
      `opencode.ai`, no `/v1` daemon contract).

### Task 2 — Module skeleton + static models

- [ ] `lib/index.js`: `name='dsh-opencode-zen'`, `inject=['llm']`, `apply()`
      registering via `ctx.llm.registerAdapter(['opencode'], adapter)`.
- [ ] `MODELS`: 9-entry static table with `contextWindow`, `defaultMaxTokens`,
      `reasoning.efforts` + `defaultEffort`, `vision`, `responses` flags — byte-equal
      to the installed plugin's table (parity test loads the installed
      `node_modules/.../dsh-opencode-zen/lib/index.js` read-only and deep-compares).
- [ ] Adapter shell: `providerInfo`, `providerRetryPolicy` (via `resolveRetryPolicy`),
      `imageRequestPricing → undefined`, `listModels` (static table + live-id refresh
      from `GET /v1/models` when reachable, falling back to the static set),
      `resolveModel`, `prepareCall`.
- [ ] RED→GREEN: model-parity + adapter-surface tests.

### Task 3 — `stream()` transport

- [ ] `stream(options)` → `POST ${OPENCODE_ZEN_BASE:-http://127.0.0.1:8787}/v1/chat/completions`
      with `options.signal` wired to `fetch` (host AbortSignal owns cancellation).
- [ ] Body: host messages/tools/`maxTokens`/`temperature`/`reasoningEffort` pass
      through as OpenAI-shaped fields (`stream:true`, `stream_options.include_usage`
      is appended by the daemon — the plugin must NOT double-set semantics the daemon
      owns; set `stream:true` and the `include_usage` flag exactly as the daemon's
      handler expects — verify against `internal/gateway/handler.go` force-stream path).
- [ ] **Zero retry loop** — a single attempt; failures throw the mapped code.
- [ ] Test: fixture daemon records the received request; assert method, path, body
      shape, that `AbortSignal` abort propagates (mid-flight abort → `TIMEOUT`/abort).

### Task 4 — Minimal SSE translator

- [ ] Parse `data:` lines → `StreamChunk`: `block-start` (first delta per role),
      `text-delta`, `reasoning-delta`, `tool-call-delta` (id/name deltas on
      `delta.tool_calls`), `block-end` (role block end), `usage` (from the
      `include_usage` frame, `choices: []`), terminal `finish`.
- [ ] Rules: ignore non-`data` lines and cost/ping lines; `data: [DONE]` and clean
      stream end both terminate; **exactly one** `finish`; `usage` before `finish`;
      nothing after `finish`.
- [ ] Tests: happy stream, usage-frame handling, `[DONE]` missing, comment lines,
      tool-call deltas, reasoning deltas.

### Task 5 — Error mapping table

- [ ] Implement exactly the mapping table above: `status` + `error.type` +
      `Retry-After` header → DSH code (+`providerRetryAfterMs`), using the canonical
      `*_CODE` constants exported from `dsh-llm/lib/index.js`.
- [ ] **No message-text routing** anywhere.
- [ ] Premature-close rule: before any content → `TIMEOUT`; after content → salvage
      terminal `finish`.
- [ ] One fixture per table row asserting the exact code (Task 1's RED goes GREEN).

### Task 6 — `apply()` health-check + registration

- [ ] Ping `GET ${base}/_zenctl/status`; on failure log a warning and, when
      config-gated (`OPENCODE_ZEN_AUTOSTART=1` or config field), spawn
      `zen-router up --detach`; register the adapter REGARDLESS of ping result.
- [ ] Keep the `cordis.patch.yml` fragment byte-identical (spec non-goal).
- [ ] Test: fake control endpoint up → no spawn; down → spawn command recorded via
      injectable spawner; registration always happens.

### Task 7 — Verify + budget + commit

- [ ] `node test/smoke.cjs` green end-to-end (all Task-1 fixtures).
- [ ] Report `wc -l lib/*.js` — target ~250, acceptable ≤275; list any forbidden
      subsystem greps as ZERO hits.
- [ ] `git status` / `git diff` review; bump version; commit(s) with exact subjects
      from each task's plan line (English).
- [ ] **STOP — no install, no pin move, no `~/.dsh` touch.**

### Task 8 — Отмашка-gated cutover checklist (documented, NOT executed here)

- [ ] (fenced) Profile pin move off `a416790` → new commit:
      `dsh plugin --profile web <pnpm args>` rewrites `profiles/web/package.json`.
- [ ] (fenced) `dsh --profile web --dump-config` → zero stderr warnings.
- [ ] (fenced) DSH restart (config applies on restart; never start a replacement
      `dsh web` server).
- [ ] (fenced) Prerequisite: rebuilt `~/.local/bin/zen-router` + live systemd unit
      serving the OpenAI surface (Plan 1 deferred this to отмашка).
- [ ] (fenced) One live smoke: `GET /v1/models` against the running daemon, then a
      DSH chat round-trip.

**Explicitly deferred (not in this plan):** everything in §H's fence list — profile
install, pin move, dump-config, DSH restart, live opencode.ai calls (Phase E/отмашка);
TUI (Plan 2); README of zen-router (Phase E).

---

## Task dependency notes

- Task 1 before Tasks 2–6 (fixtures define the contract).
- Tasks 2–5 are independent against Task 1's fixtures and parallelizable (different
  `lib/` regions — serialize file writes: one implementer at a time in `lib/index.js`).
- Task 6 after 2 (registration needs the adapter); Task 7 last; Task 8 never runs in
  this plan.
