# dsh-opencode-zen Thin Adapter (Phase D) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the 1736-line installed DSH plugin with a ~250-line thin adapter
(compact, no-image path; ≤400 lines with the image-capable OpenAI serializer kept —
see the LOC budget) that delegates all business logic — SSE translation, quota,
rotation, watchdogs, disguise, session ids, gate tools, Responses routing, retry — to
the `zen-router` daemon's OpenAI surface (`POST /v1/chat/completions`,
`GET /v1/models`), leaving only: provider/model registration, one POST + SSE→StreamChunk
translation, an HTTP→DSH error map, and an `apply()` health ping.

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
repo. **Base: the plugin repo's current clean HEAD `c2471d2`** (confirm `git status`
clean before starting); at this base `package.json` already dropped the `./client`
export and `lib/` holds only `index.js` + `quota.js`. Base the adapter on the installed
`a416790` lineage's adapter knowledge; do NOT blindly merge HEAD `c2471d2` (its
`/zen/v1` default targets the transport-proxy variant superseded by the OpenAI-shim
decision).

## Global Constraints

- **Never touch `~/.dsh`** — no profile edits, no `dsh plugin` install, no
  `dsh --profile web --dump-config`, no DSH restart, no pin move off `a416790`.
  The installed v0.15.1 keeps running untouched until the user's отмашка (§12).
- **Never make a live call to `opencode.ai`** in tests; all fixtures are local
  `node:http` servers on the loopback interface (fake daemon + canned SSE + error
  envelopes + refused sockets).
- `cordis.patch.yml` fragment ships byte-identical to the installed one (spec non-goal:
  entry point stays as-is).
- `lib/` LOC budget: target ~250 (compact, no-image path), acceptable ≤400 with the
  image-capable OpenAI serializer kept — installed cost `serializeMessages` ~77 + image
  helpers ~40 + `serializeTools` ~10 ≈ 130; the 9-model table is the other main fixed
  cost. `wc -l lib/*.js` is reported; only >400 hard-fails.
- **No in-process retry loop** — host retry policy owns retries
  (`dsh-llm-retry/lib/index.js:160` gates on `policy.retryableCodes`).
- Map errors by **status + `error.type`**, never by message text (§9d). The daemon's
  client envelope is the OpenAI shape `{"error":{"message","type","code"},"metadata"?}`
  (T14 fix `6d50b7e`; `error.type` sits at the same JSON path the plugin parser reads,
  `code` mirrors `type`, `metadata` only on 429, never `null` after `ade6f88`) —
  contract tests in Task 1 must fixture this exact shape.
- Preserve the 401 trap: `401 + ModelError` (upstream unknown-model; daemon emits it at
  `internal/gateway/handler.go:160`) → non-retryable `PROVIDER_ERROR`, NOT terminal
  `INVALID_CREDENTIAL` (parity: installed `lib/index.js:821`). Terminal
  `INVALID_CREDENTIAL` is only `401 + AuthError` (plus the other auth-class 401 types).
- **Failure-throw contract:** every thrown error carries BOTH own props — `code` AND
  `failure = {message, code, status?, providerRetryAfterMs?}` — agreeing with each
  other, set by a `typedError`-style helper (parity: installed `lib/index.js:752-758`).
  Host `dsh-llm/lib/types/adapter-failure.js normalizeLlmFailure` degrades a bare `.code`
  (no matching `failure` snapshot) to `UNKNOWN`, which is in no retryable set — the turn
  dies on the first glitch.
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
   `401 ModelError` vs `401 AuthError` split; 429 type-first: `429 +
   FreeUsageLimitError|GoUsageLimitError|BlackUsageLimitError` → `QUOTA` BEFORE the
   generic 429 → `RATE_LIMIT` row (the daemon sets `Retry-After` on EVERY 429 —
   `handler.go:397-400`, synthesized midnight/60s `errors.go:206-214` — so status-first
   matching would steal the quota rows).
2. **StreamChunk contract** — exactly one terminal `finish`, `usage` emitted BEFORE
   `finish` and nothing after; `block-start`/`block-end` pairing for text/reasoning/tool
   blocks; premature close (HTTP response received) BEFORE content → retryable `TIMEOUT`,
   AFTER content → salvage-as-stop with every open block closed first (no dangling
   `STREAM_TRUNCATED` on partial answers).
3. **Host-contract surface** — adapter methods match `LlmAdapter`
   (`dsh-llm/lib/types/index.d.ts:187+`): `providerInfo`, `providerRetryPolicy`,
   `imageRequestPricing`, `listModels`, `resolveModel`, `prepareCall`, `stream`;
   `MODELS` entries byte-equal to `a416790`'s table vocabulary (`id,name,contextWindow,
   maxOutput,description,vision?,efforts?,responses?,reasoningRequired?`), and
   `resolveModel()` output (`defaultMaxTokens`, `reasoning.efforts`, `defaultEffort`)
   derived by the installed mapping — those are NOT table fields.
4. **Deletability** — grep proves the forbidden subsystems are absent from `lib/`
   (panel, status server, quota store, key pool, family pool, disguise, session,
   Responses builder, watchdogs, retry loop); LOC budget reported.
5. **Safety** — no writes outside the plugin repo + temp dirs; zero `~/.dsh` access
   (MODELS parity reads a committed `test/fixtures/` snapshot extracted once from
   `a416790:lib/index.js`, never the installed `~/.dsh` copy — fallback only: reword to
   "zero `~/.dsh` WRITES" if the fixture cannot be extracted); tests loopback-only; the
   отмашка fence list (§H) is untouched.

## File Structure

| File | Responsibility |
|---|---|
| `lib/index.js` (rewrite) | cordis service + adapter: `apply`, `name`, `inject`, `PROVIDER`, `MODELS`, `stream`, error map, SSE translator, OpenAI message/tool serializer (DSH content blocks → chat messages, `tool_calls`, `role:'tool'`, `image_url` parts) |
| `test/smoke.cjs` (rewrite) | fixture fake-daemon tests: SSE sequences, error envelopes, down socket, host-abort, failure-snapshot shape, parity vs the committed MODELS fixture |
| `test/fixtures/*` (new) | canned daemon responses (error envelopes, model list) + `models-a416790.cjs` parity snapshot extracted at test-authoring time via `git show a416790:lib/index.js` |
| `package.json` (modify) | version bump; `main`, `dsh.bundle.patch` unchanged (`./client` export already removed at base `c2471d2`) |
| `cordis.patch.yml` (unchanged) | byte-identical to installed fragment |
| `README.md` (modify) | describe the thin adapter + daemon prerequisite |

`lib/quota.js` is to be deleted by THIS plan — it still EXISTS at base `c2471d2` (the
`c2471d2` lineage already dropped `client.js` + `status.js`; only `index.js` and
`quota.js` remain, and Task 7 verifies `lib/` ends up with `index.js` only).

## Dependency-derived mapping table (binding — Task 5 implements exactly this)

**Precedence: topmost match wins, order as listed** (type-first on 401/429 — status
alone never decides). All daemon-reachable envelopes are enumerated here; any status not
matching a row below must fail a Task-1 fixture rather than pass silently.

| Daemon response | DSH code | Retryable? |
|---|---|---|
| 401 + `ModelError` (upstream unknown-model; emitted `internal/gateway/handler.go:160`) | `PROVIDER_ERROR` | no (the 401 trap) |
| 401 else (`AuthError` \| `CreditsError` \| `MonthlyLimitError` \| `UserLimitError`, bare 401 → `AuthError`) | `INVALID_CREDENTIAL` | no |
| 429 + `FreeUsageLimitError` \| `GoUsageLimitError` \| `BlackUsageLimitError` | `QUOTA` | no |
| other 429 (`RateLimitError`; bare-429 typed `InvalidRequestError` via the `kindByStatus` catch-all `internal/zen/errors.go:224-231`; any other type) | `RATE_LIMIT` + `providerRetryAfterMs` when `Retry-After` present | yes ¹ |
| 403 + `RegionError` \| `DataPolicyError` | `PROVIDER_ERROR` | no |
| 404 / 405 (`NotFoundError` \| `MethodNotAllowedError`) | `PROVIDER_ERROR` | no |
| 400 + `InvalidRequestError` | `PROVIDER_ERROR` | no |
| 413 `PayloadTooLargeError` (request body >4 MiB, `handler.go:126-129`) | `PROVIDER_ERROR` | no |
| ≥5xx (`ServerError` \| `TransportError` \| `ProviderRelayError` \| `InternalError`, incl. the pre-first-byte clamp to 500 `handler.go:395-401`) | `SERVER` | yes |
| no HTTP response (ECONNREFUSED / reset / socket error) | `TRANSPORT` | yes |
| any other unmatched status (unreachable per spec §9 — fixture must prove it) | `PROVIDER_ERROR` | no (safety default; no envelope falls through unmapped) |
| host `AbortSignal` fired (caller abort, any phase) | `ABORTED` | no (parity `lib/index.js:877/889/1452`) |
| HTTP response, stream death BEFORE any content (watchdog/upstream kill or abnormal EOF; daemon's bare post-first-byte exit `handler.go:387-393`) | `TIMEOUT` | yes |
| clean termination (`[DONE]` or clean end) with ZERO content | `EMPTY_RESPONSE` | yes |
| EOF AFTER content | salvage: `block-end` for each open block (partial block), then terminal `finish` (stop) | n/a |
| daemon down at `apply()` | warn + config-gated `zen-router up --detach`; registration still proceeds | n/a |

¹ `providerRetryAfterMs` only takes effect while ≤ `policy.maxDelayMs` (default 10s,
`dsh-llm/lib/index.js:249`); a longer window makes the host skip the retry entirely
(`dsh-llm-retry/lib/index.js:165-167`). Moot for daily limits — the QUOTA row above
takes them first (non-retryable) anyway.

**413 note:** the daemon's only 413 is the 4 MiB request-body cap
(`PayloadTooLargeError`, `handler.go:126-129`) — NOT context overflow; the daemon does
zero context classification. Spec §9 does not require `CONTEXT_WINDOW_EXCEEDED`, and
message-text sniffing (how the installed plugin detected overflow) is forbidden;
daemon-side context classification = follow-up, out of plan scope.

## Tasks

### Task 1 — Contract tests first (failing)

- [ ] In `test/smoke.cjs` (rewrite), stand up a local fake daemon (`node:http`) that
      serves: (a) canned OpenAI SSE chat streams (happy path with text+usage+`[DONE]`),
      (b) each row of the mapping table (status + envelope + `Retry-After`),
      (c) connection-refused (down socket), (d) premature close pre- and post-content,
      (e) host `AbortSignal` fired mid-flight (adapter-side; no server involvement).
- [ ] Write assertions FIRST against the thin adapter that does not exist yet: exact
      `StreamChunk` sequences (`block-start` → `text-delta`* → `block-end` → `usage` →
      `finish`), exact DSH error codes, `providerRetryAfterMs` values — and for EVERY
      thrown error assert the failure-snapshot shape: own `code` AND own
      `failure = {message, code, status?, providerRetryAfterMs?}` with both codes
      agreeing (a bare `err.code` degrades to `UNKNOWN` in host `normalizeLlmFailure`
      and is never retried; parity installed `lib/index.js:752-758`).
- [ ] Keep the runner contract: `npm test` → `node test/smoke.cjs`, plain
      `check`/`checkAsync` harness on `node:assert`, zero new deps.
- [ ] Quote the RED output; tests necessarily fail at base `c2471d2`: its adapter
      defaults to the transport-proxy `http://127.0.0.1:8787/zen/v1` and exports a
      different surface, so RED holds via path/export-shape mismatch against this plan's
      `/v1/chat/completions` contract (a direct-`opencode.ai` default was the older
      `a416790` config, not HEAD).

### Task 2 — Module skeleton + static models

- [ ] `lib/index.js`: `name='dsh-opencode-zen'`, `inject=['llm']`, `apply()`
      registering via `ctx.llm.registerAdapter(['opencode'], adapter)`.
- [ ] `MODELS`: copy the 9-entry array VERBATIM from `git show a416790:lib/index.js`
      — entry keys are `id,name,contextWindow,maxOutput,description,vision?,efforts?,
      responses?,reasoningRequired?` (INCLUDING `description`, `reasoningRequired`,
      `efforts`); NO `defaultMaxTokens` and NO `reasoning.*` on entries: those are
      `resolveModel()` OUTPUT (`maxOutput` → `defaultMaxTokens`, `efforts` →
      `reasoning.efforts`, `defaultEffort` from the `DEFAULT_REASONING` const —
      document this mapping next to `resolveModel`). The parity test deep-compares the
      table against a committed fixture (`test/fixtures/models-a416790.cjs`, extracted
      ONCE at test-authoring time via `git show a416790:lib/index.js`) — tests read
      ZERO `~/.dsh` paths (fallback only: if the fixture cannot be extracted, reword
      Review Focus #5 to "zero `~/.dsh` WRITES" and read the installed copy read-only).
- [ ] Adapter shell: `providerInfo`, `providerRetryPolicy` (via `resolveRetryPolicy`),
      `imageRequestPricing → undefined`, `listModels` (static table + live-id refresh
      from `GET /v1/models`: the refresh SWALLOWS ALL errors and falls back to the
      static table — `listModels()` must NEVER reject), `resolveModel`, `prepareCall`.
- [ ] RED→GREEN: model-parity + adapter-surface tests.

### Task 3 — `stream()` transport

- [ ] `stream(options)` → `POST ${OPENCODE_ZEN_BASE:-http://127.0.0.1:8787}/v1/chat/completions`
      with `options.signal` wired to `fetch` (host AbortSignal owns cancellation).
      `OPENCODE_ZEN_BASE` defaults to `http://127.0.0.1:8787`; `OPENCODE_ZEN_API_KEY`
      is accepted but NOT required — the daemon's inbound lane has no auth check.
- [ ] Send `stream:true` only; OMIT `stream_options` (the daemon force-writes it on the
      chat lane `internal/gateway/handler.go:204`; the Responses lane never takes it
      client-side; usage frames are synthesized daemon-side for both lanes).
- [ ] Message serializer (inside `lib/index.js`): DSH content blocks → OpenAI chat —
      `tool-call` → assistant `tool_calls`, `tool-result` → `role:'tool'` messages,
      `image` → `image_url` parts (data URLs), `reasoning` folded into the assistant
      message (installed: `reasoning_content`) or dropped — never its own role; plus
      `serializeTools`. Installed cost ≈ `serializeMessages` ~77 + image helpers ~40 +
      `serializeTools` ~10 ≈ 130 LOC — this is what moves the ceiling from ~250 to ≤400.
- [ ] Attribution: send the `attributionHeaders()` `user-agent` (from `dsh-llm`,
      defaults to `APP_IDENTITY`) on the loopback hop — that one header, nothing else
      from the old disguise union.
- [ ] **Zero retry loop** — a single attempt; failures throw the mapped code.
- [ ] Test: fixture daemon records the received request; assert method, path, body
      shape (incl. that the body NEVER carries `stream:false` or `stream_options` — the
      T14 buffered path is never exercised), and that `AbortSignal` abort propagates
      (mid-flight abort → `ABORTED`).

### Task 4 — Minimal SSE translator

- [ ] Parse `data:` lines → `StreamChunk`: `block-start` (first delta per role),
      `text-delta`, `reasoning-delta`, `tool-call-delta` (id/name deltas on
      `delta.tool_calls`), `block-end` (role block end), `usage` (from the
      `include_usage` frame, `choices: []`), terminal `finish`.
- [ ] Rules: ignore non-`data` lines and cost/ping lines; `data: [DONE]` and clean
      stream end both terminate; **exactly one** `finish`; `usage` before `finish`;
      nothing after `finish`; on premature close AFTER content the translator first
      emits `block-end` for every open block (carrying the partial block) so
      `block-start`/`block-end` pairing holds through salvage.
- [ ] Tests: happy stream, usage-frame handling, `[DONE]` missing, comment lines,
      tool-call deltas, reasoning deltas, salvage closes open blocks before `finish`.

### Task 5 — Error mapping table

- [ ] Implement exactly the mapping table above — **precedence: topmost match wins, in
      the listed order**; `status` + `error.type` + `Retry-After` header → DSH code
      (+`providerRetryAfterMs`). Code values: use the exported `*_CODE` constants where
      `dsh-llm/lib/index.js` exports them (`QUOTA_EXCEEDED_CODE`,
      `ACCOUNT_QUOTA_EXCEEDED_CODE`, `EMPTY_RESPONSE_CODE`, `INVALID_CREDENTIAL_CODE`,
      `CONTEXT_WINDOW_EXCEEDED_CODE`, `IMAGE_OFFLOAD_REQUIRED_CODE`); every other code
      is a plain literal IDENTICAL to the host default (the installed plugin uses
      literals; `DEFAULT_RETRYABLE_CODES` at `dsh-llm/lib/index.js:251-257` mixes
      `EMPTY_RESPONSE_CODE` with the strings `"RATE_LIMIT"`, `"SERVER"`, `"TIMEOUT"`,
      `"TRANSPORT"`).
- [ ] Every error is thrown through a `typedError(failure)` helper that sets BOTH own
      props — `error.code = failure.code` and `error.failure = {...failure}` — kept in
      agreement (parity installed `lib/index.js:752-758`); a bare `.code` degrades to
      `UNKNOWN` in host `normalizeLlmFailure` → never retried.
- [ ] **No message-text routing** anywhere (the installed plugin's message-text
      overflow sniff is explicitly NOT ported; `CONTEXT_WINDOW_EXCEEDED` stays
      unmapped — see the 413 note).
- [ ] Premature-close contract — one rule per observable: no HTTP response →
      `TRANSPORT`; HTTP response then stream death (watchdog kill / abnormal EOF)
      BEFORE any content → `TIMEOUT`; clean termination (`[DONE]`/clean end) with ZERO
      content → `EMPTY_RESPONSE`; EOF AFTER content → salvage terminal `finish` with
      open blocks closed; host-signal abort at any phase → `ABORTED` (non-retryable,
      parity `lib/index.js:877/889/1452`). `TIMEOUT` is reserved for the stream-death
      case only, `TRANSPORT` for sockets only — never for caller aborts.
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
- [ ] Report `wc -l lib/*.js` — target ~250 (compact path), acceptable ≤400 with the
      image-capable serializer; report the ACTUAL count; list any forbidden subsystem
      greps as ZERO hits.
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
