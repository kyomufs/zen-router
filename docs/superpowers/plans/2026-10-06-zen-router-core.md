# zen-router Core (Gateway Shim + Staged Rotation) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the existing transport-only proxy into a full OpenAI-shim gateway in front of opencode.ai: Zen wire port (disguise, sessions, gate tools, effort clamps, Responses auto-routing, SSE translation, watchdogs, error classification) plus staged 429 rotation (key → identity → direct) with per-key and per-egress counters.

**Architecture:** A new `internal/gateway` package owns the client-facing OpenAI surface (`POST /v1/chat/completions`, `GET /v1/models`) and delegates every upstream attempt to a `Rotator` interface implemented by `internal/router`. Pure protocol logic (models, sessions, headers, error envelopes, SSE translation, watchdogs) lives in `internal/zen` as side-effect-free functions with table tests. The listener stays shared: `/_zenctl/*` control API and the legacy `/zen/v1` reverse proxy keep working untouched.

**Tech Stack:** Go 1.26, stdlib `net/http` + `httptest`, existing deps only (`golang.org/x/net`, `wgctrl`). No new modules for this plan (bubbletea arrives in Plan 2).

**Spec:** `docs/superpowers/specs/2026-10-06-zen-router-gateway-design.md` — this plan argues from it; executors read both. Protocol facts are §4 (upstream wire) and §6 (staged rotation); client surface is §5; phases A+B are §13.

## Global Constraints

- **No live gateway calls in tests.** All tests use `httptest.Server` fakes; `UpstreamURL` is already injectable (`proxy.Config.UpstreamURL`). Any smoke test against `opencode.ai` requires the user's отмашка and is NOT part of this plan.
- **Never touch the DSH profile or the installed plugin** (`~/.dsh/profiles/web/…`, `dsh-opencode-zen` v0.15.1, `cordis.patch.yml`). Rebuilds go to `./bin/zen-router` (gitignored) — never `go install` over `~/.local/bin/zen-router` while the live unit runs.
- Loopback default `127.0.0.1:8787` (`internal/cli.DefaultListen`), override `ZEN_ROUTER_LISTEN`.
- Watchdog defaults: first-event 30s, body-idle 120s (chat), responses-idle 300s (spec §4).
- Identity pool default 4 live + 1 spare, rotation cooldown 30s (spec §6).
- Responses-only auto-route for models with `responses: true` (only `muse-spark-1.3-contributor-free` today).
- Effort rule: clamp to the model's declared ladder; chat wire `off → none`, Responses wire omits `off`; `reasoningRequired` models default `high` when absent (spec §4).
- All code, comments, commits in English; replies to the user in Russian. Commits via `git -c user.name=kyomufs -c user.email=kyomufs@localhost` after `git status`/`git diff`.
- Every task ends green: `go build ./... && go vet ./... && go test -short ./...`.
- Existing CLI commands (`up/status/rotate/use/stop/__wgcfg`) keep their flags and behavior.

## Review Focus (reviewer checks these five classes hardest)

1. **Wire fidelity** — request headers, session-id shape, gate-tool injection, effort clamping match spec §4 exactly (each has a dedicated test asserting literals).
2. **Stream correctness** — SSE translation produces well-formed frames, exactly one terminal `[DONE]`, cost lines dropped, no re-issue after the first forwarded byte.
3. **Rotation state machine** — per-egress AND per-key counters both recorded; cooldown enforced; no infinite 429 loops; error-kind → step routing correct (daily vs key-lane vs account).
4. **Error mapping** — upstream envelope `{"type":"error","error":{"type":…}}` parsed by `error.type`, not status alone (`ModelError` is an upstream 401 but must NOT surface as INVALID_CREDENTIAL-class; `Retry-After` parsed from header and synthesized to UTC midnight when absent).
5. **Safety** — no network beyond loopback in tests; no writes outside the repo and temp dirs; profile/plugin untouched.

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` (new) | XDG paths (config/state/log), `config.json` load + `ZEN_ROUTER_*` env overrides, one-shot state migration from `~/.dsh/state/zen-router/` |
| `internal/config/config_test.go` (new) | path/migration/env-override tests |
| `internal/zen/models.go` (new) | 9-model table, `ResolveModel`, `ClampEffort` |
| `internal/zen/session.go` (new) | canonical `ses_` id, conversation seed, stable `prj_`, random `req_` |
| `internal/zen/headers.go` (new) | disguise header set builder |
| `internal/zen/shape.go` (new) | gate tools (`bash`/`read`) injection, `tool_choice: none` rule |
| `internal/zen/errors.go` (new) | upstream error-envelope parse + `Kind` classification + `Retry-After` extraction |
| `internal/zen/watchdog.go` (new) | first-event / body-idle reader watchdog |
| `internal/zen/responses.go` (new) | chat body → Responses body (`instructions`, `input_text`, `max_output_tokens`, no reasoning replay) |
| `internal/zen/sse.go` (new) | Responses SSE → chat SSE translation; chat passthrough filter (drop cost lines, single `[DONE]`) |
| `internal/zen/*_test.go` (new) | table tests per file |
| `internal/keys/pool.go` (new) | key pool: `pool-config.json` + env + `public` fallback, round-robin, mtime reload |
| `internal/keys/pool_test.go` (new) | fixture-based pool tests |
| `internal/quota/state.go` (modify) | v2 schema: XDG path default, per-key counters, identity pool array; migrate v1 files |
| `internal/quota/state_test.go` (new) | round-trip + v1-migration tests |
| `internal/router/stage.go` (new) | staged rotation: `Attempt`/`Report`/`NextAttempt` decision logic |
| `internal/router/stage_test.go` (new) | decision table tests with fake clock |
| `internal/gateway/handler.go` (new) | OpenAI surface: chat completions + models, upstream attempt loop with bounded re-issue |
| `internal/gateway/stream.go` (new) | response streaming to client: headers, body pump, watchdog wiring, first-byte tracking |
| `internal/gateway/handler_test.go` (new) | httptest end-to-end: happy stream, 429 rotation sequence, budget exhaustion, mid-stream no-reissue |
| `cmd/zen-router/main.go` (modify) | mount gateway on `/v1/*` in `up`, keep `/_zenctl/*` + `/zen/v1` legacy proxy |

`internal/proxy` (transports, `Result`, `OnResult` hook) and `internal/warp` are consumed as-is; the reverse-proxy `Server` stays for the legacy path — no deletions.

## Key decision to confirm at review: **D1 — re-issue attempt budget**

Spec §6 mandates exactly one re-issue at step 1 (key rotation) but does not say whether the request that triggered step 2/3 may benefit. **This plan allows up to 3 upstream attempts per client request**: original → key re-issue → (only if the key re-issue 429'd with a daily-limit error) identity/egress re-issue. Each rotation is applied persistently regardless, so later requests benefit even when the budget is spent or a byte was already forwarded. Rationale: otherwise the rotation this 429 paid for never serves the triggering request, and `RateLimitError` (key lane, `retry-after: 60`) is deliberately **not** rotated to a new identity — it surfaces as 429 for host backoff. Flag this paragraph if the budget should be 2.

---

## Tasks

### Task 1: XDG config with migration

**Files:** `internal/config/config.go`, `internal/config/config_test.go`; modify `internal/quota/state.go` default path only (loading stays lazy until Task 10).

- [ ] Write failing tests: `TestDefaultPaths` (honors `XDG_CONFIG_HOME`/`XDG_STATE_HOME`, falls back to `~/.config`/`~/.local/state`), `TestConfigEnvOverridesFile` (`ZEN_ROUTER_LISTEN` beats `config.json`), `TestMigrateLegacyState` (legacy `~/.dsh/state/zen-router/state.json` copied once to XDG state, original untouched, second run no-op).
- [ ] Run tests, confirm they fail (no package).
- [ ] Implement `config.go`: `Paths` struct (`ConfigDir`, `StateDir`, `StateFile`, `LogFile`), `Load() (*Config, error)` with fields `Listen`, `Upstream`, `KeyPoolFile`, `PoolSize`, `PoolSpare`, `RotationCooldown`, `FirstEventTimeout`, `IdleTimeout`, `ResponsesIdleTimeout` (defaults per Global Constraints), empty file = all defaults; `MigrateLegacyState(paths)`.
- [ ] Point `quota.DefaultPath()` at `config.Paths.StateFile` (keep `ZEN_ROUTER_STATE` override first).
- [ ] Tests green; `go vet`; commit `Add XDG config with legacy state migration`.

### Task 2: models table and effort clamp

**Files:** `internal/zen/models.go`, `internal/zen/models_test.go`.

- [ ] Write failing tests: `TestResolveModel` (all 9 ids resolve with exact `ContextWindow`/`MaxOutput`/`Vision`/`Responses`/`ReasoningRequired` values copied from spec §4 / plugin MODELS; unknown id → error), `TestClampEffort` (mimo `xhigh → high`, `minimal → off`… verify against each ladder: mimo `[off low medium high]`, muse `[minimal low medium high xhigh]`, space-bunny `[low medium high xhigh max]`, default ladder `[off low high max]` for models without one), `TestClampEffortReasoningRequired` (absent effort → `high` for `space-bunny-free`; `off` on chat → `none` mapping exposed as separate `ToChatWire` helper; Responses `off` → omit).
- [ ] Run, confirm fail.
- [ ] Implement: `Model` struct + `MODELS` table (9 rows, literal values), `ResolveModel(id)`, `ClampEffort(m, effort string) string`, `ToChatWire(effort) string`.
- [ ] Green; commit `Add Zen models table with effort clamping`.

### Task 3: session id derivation

**Files:** `internal/zen/session.go`, `internal/zen/session_test.go`.

- [ ] Failing tests: `TestCanonicalSessionID` (seed "hello" → matches `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`; same seed → same id; different seed → different id; an already-canonical id passes through), `TestProjectIDStable` (fixed string → fixed `prj_` id, 12 hex), `TestRequestIDFresh` (two calls differ, matches `^req_[0-9a-f]{32}$`), `TestConversationSeed` (first non-empty user message JSON content; empty conversation → empty string, caller falls back to random — test the exported fallback path).
- [ ] Run, fail.
- [ ] Implement mirroring plugin `lib/index.js:165-205`: sha256(`"ses\x00"+seed`), first 6 bytes hex (12 chars), next 10 bytes → 14 base62 chars (alphabet `0-9A-Za-z`, `byte % 62`); `StableID(prefix, value)` sha256 12 hex; `RandomID(prefix, bytes)`.
- [ ] Green; commit `Add canonical session id derivation`.

### Task 4: disguise headers and gate tools

**Files:** `internal/zen/headers.go`, `internal/zen/shape.go`, `internal/zen/headers_test.go`, `internal/zen/shape_test.go`.

- [ ] Failing tests: `TestBuildHeaders` (asserts the full map: `user-agent: opencode/1.18.34`, `x-opencode-session-id`, `x-opencode-session`, `x-opencode-request`, `x-opencode-client: cli`, `x-opencode-project` only when non-empty, `x-session-affinity` + `X-Session-Id` present per spec §4 union — literal key/value assertions), `TestGateTools` (zero tools → adds `bash`+`read` function tools with description `Reserved for the host runtime; do not call it.` and `tool_choice: none`; caller has other tools → gate tools appended, `tool_choice` untouched; caller already has `bash` → only `read` added; non-object body → unchanged).
- [ ] Run, fail.
- [ ] Implement `BuildHeaders(HeaderInput)` and `EnsureFreeLaneShape(map[string]any)` mirroring plugin `index.js:240-273`.
- [ ] Green; commit `Add disguise headers and free-lane gate tools`.

### Task 5: upstream error classification

**Files:** `internal/zen/errors.go`, `internal/zen/errors_test.go`.

- [ ] Failing tests with fixture envelopes from spec §4: `TestClassifyDailyLimit` (`FreeUsageLimitError`/`GoUsageLimitError`/`BlackUsageLimitError` body → `KindDailyLimit`; `Retry-After` header parsed; header absent → synthesized seconds-to-UTC-midnight), `TestClassifyKeyRateLimit` (`RateLimitError` + `retry-after: 60` → `KindKeyRateLimit`), `TestClassifyModel` (upstream **401** + `ModelError` → `KindModel`, NOT credential-class — this guards the §4 trap), `TestClassifyAuth` (401 + `AuthError` → `KindAuth`), `TestClassifyRegion` (403 + `RegionError`/`DataPolicyError` → `KindRegion`), `TestClassifyProviderRelay` (429 body `Error from provider (OpenAI): …` with no gateway envelope → `KindProviderRelay`, 429 status preserved), `TestClassify5xx` → `KindServer`, malformed/empty body → `KindServer` by status.
- [ ] Run, fail.
- [ ] Implement: `Kind` enum, `Classify(resp *http.Response, body []byte) *UpstreamError` (reads `error.type` from `{"type":"error","error":{"type","message"}}`, falls back to status; captures `retryAfter time.Duration`, `Message`, `Type`).
- [ ] Green; commit `Add upstream error envelope classification`.

### Task 6: stream watchdog

**Files:** `internal/zen/watchdog.go`, `internal/zen/watchdog_test.go`.

- [ ] Failing tests: `TestWatchdogFirstEventTimeout` (reader that blocks > budget → `ErrWatchdogTimeout` on first `Read`), `TestWatchdogIdleTimeout` (first data flows, then block → timeout fires only after idle budget), `TestWatchdogDisarms` (steady reader completes with `io.EOF`, no error; budget configurable — tests use 20ms).
- [ ] Run, fail.
- [ ] Implement `NewWatchdog(ctx, r io.Reader, firstEvent, idle time.Duration) io.Reader` — re-arm timer around every `Read`, `ErrWatchdogTimeout` sentinel, context-cancellable.
- [ ] Green; commit `Add stream watchdog reader`.

### Task 7: chat → Responses body translation

**Files:** `internal/zen/responses.go`, `internal/zen/responses_test.go`.

- [ ] Failing tests: `TestChatToResponsesBody` (system message → `instructions`; user text → `input_text` item; image attachment part → `input_image`; `max_tokens` → `max_output_tokens`; `reasoning_effort: off` → field absent; tool messages converted per plugin shape; `tool_choice` allowed — plugin sets `auto` only when caller tools exist), `TestChatToResponsesKeepsModel` (model id unchanged; endpoint path chosen by caller).
- [ ] Run, fail.
- [ ] Implement `ChatToResponses(chatBody map[string]any) (map[string]any, error)` mirroring plugin `index.js:1004-1080`.
- [ ] Green; commit `Add chat to Responses body translation`.

### Task 8: SSE translation and filtering

**Files:** `internal/zen/sse.go`, `internal/zen/sse_test.go`.

- [ ] Failing tests: `TestResponsesSSEToChat` (fixture: `response.created`, `response.output_text.delta`×2, reasoning delta → `delta.reasoning_content`, `response.completed` with usage + `max_output_tokens` cutoff → `finish_reason: length`; emits standard `chat.completion.chunk` frames and exactly one `data: [DONE]`), `TestChatPassthroughFilter` (chat frames forwarded verbatim; `data: {"choices":[],"cost":…}` dropped; `event: ping` cost frames dropped; upstream `[DONE]` deduplicated — never two terminators), `TestChatPassthroughUsageFrame` (final `usage` chunk preserved).
- [ ] Run, fail.
- [ ] Implement SSE line parser (handles `data:`/`event:`/blank separators, partial reads) + `TranslateResponsesStream(io.Reader, io.Writer)` + `FilterChatStream(io.Reader, io.Writer)`; writer flushes per frame (`http.Flusher` handled in Task 12).
- [ ] Green; commit `Add SSE translation and chat stream filtering`.

### Task 9: key pool

**Files:** `internal/keys/pool.go`, `internal/keys/pool_test.go`.

- [ ] Failing tests with JSON fixtures in `testdata/`: `TestPoolFromFile` (`pools.opencode.keys` + `pools["opencode-zen"].keys`, `public` entries filtered, dedup, order preserved), `TestPoolEnvAppends` (`ZEN_ROUTER_API_KEYS`-style env — use `OPENCODE_ZEN_API_KEY` to mirror plugin precedence: file first, env after), `TestPoolFallbackPublic` (missing/empty file → `["public"]`), `TestPoolRoundRobin` (N draws cycle, never out of range), `TestPoolMtimeReload` (rewrite fixture → next draw sees new keys without reconstructing pool).
- [ ] Run, fail.
- [ ] Implement `Pool` with `Next() (string, bool)` (bool = has real keys beyond `public`), `Len()`, `HasAlternatives(current string)` — the last one feeds router step 1.
- [ ] Green; commit `Add key pool with mtime reload`.

### Task 10: state v2 — per-key counters and identity pool

**Files:** `internal/quota/state.go` (modify), `internal/quota/state_test.go` (new).

- [ ] Failing tests: `TestStateV2RoundTrip` (per-key `{ok, daily429, last429at}` map + identities array with `{registeredAt, spentUntil, last429at}` health fields persist and reload), `TestStateMigratesV1` (v1 file with single `warp` identity and egress stats → v2: identity becomes `identities[0]`, egress/rotations preserved, `version: 2`), `TestRecordKey429`/`TestRecordKeySuccess` counters.
- [ ] Run, fail.
- [ ] Implement: `State.Version` → 2; `Keys map[string]*KeyStats`; `Identities []*WarpIdentity` (each with health fields, `Active int`); `Warp` field migrates into `Identities[0]`; keep all existing accessor signatures working (`Current`, `SetWarp`, `EgressStats`, `RecordDaily429`, `NextReset`…), add `RecordKeyDaily429`, `RecordKeySuccess`, `KeyStats(key)`, `Identity(i)`, `ActiveIdentity()`.
- [ ] Green; commit `Extend state with per-key counters and identity pool`.

### Task 11: staged rotation state machine

**Files:** `internal/router/stage.go` (new), `internal/router/stage_test.go` (new); small additions to `internal/router/router.go`.

- [ ] Failing decision-table tests: `TestNextAttemptKeyStep` (daily-limit report on egress E with pool of ≥2 keys → re-issue on same egress with next key), `TestNextAttemptSkipsKeyStepWhenSingleKey` (pool `["public"]` → no key re-issue, straight to rotation decision), `TestNextAttemptIdentityStep` (key re-issue also daily-limited → `SwitchEgress` to warp + fresh identity from pool; cooldown respected — inside 30s cooldown → no re-issue, persist rotation for later requests), `TestNextAttemptDirectStep` (all identities spent — every `spentUntil` in future — → switch to direct), `TestKeyRateLimitNoIdentityRotate` (`KindKeyRateLimit` → next key only, never identity; report budget exhausted surfaces to client), `TestAccountLimitNoRotation` (`GoUsageLimitError`/`BlackUsageLimitError` with different workspace → key rotation only), `TestReportRecordsBothCounters` (one daily-limit report increments egress stats AND key stats).
- [ ] Run, fail.
- [ ] Implement: `Report` struct (`Kind`, `Egress`, `Key`, `RetryAfter`), `NextAttempt(rep Report) (Attempt, bool)` on Router (budget bookkeeping: which steps already spent for this request — tracked by `Attempt.Step int` carried by the caller), identity switch inside `NextAttempt` is synchronous but uses the pre-registered hot-spare (reconfigure transport, no registration; background registration of the next spare kicked off), `Attempt{Key, Egress, Transport, Step}`.
- [ ] Green; commit `Add staged rotation state machine`.

### Task 12: gateway handler — OpenAI surface with bounded re-issue

**Files:** `internal/gateway/handler.go`, `internal/gateway/stream.go`, `internal/gateway/handler_test.go`.

- [ ] Failing end-to-end tests (`httptest.Server` fake upstream speaking the Zen wire, injected via `Upstream` field):
  - `TestChatHappyPath` — client POST → handler shapes body (gate tools visible upstream), sets disguise headers upstream, streams SSE through filter, terminates with single `[DONE]`; assert `Flushing` after each frame.
  - `TestModelsEndpoint` — `GET /v1/models` returns OpenAI list from `zen.MODELS`.
  - `TestDailyLimitRotationSequence` — fake upstream 429s first two attempts (`FreeUsageLimitError`), succeeds on third with a different key and (after step 2) different egress transport label; assert ≤3 upstream calls, both counters recorded, response streams normally to client.
  - `TestBudgetExhaustedSurfaces429` — upstream always 429 → client receives exactly one429 with `Retry-After` mapped from classification (synthesized to UTC midnight), exactly 3 attempts, no infinite loop.
  - `TestNoReissueAfterFirstByte` — upstream 429s on attempt 2 while a chunk was already flushed on attempt 1 (simulate: first attempt streams one frame then errors… simpler: assert guard by unit — `reissueAllowed` false once `firstByteWritten`) → client stream just ends/errors; rotation still recorded for later requests.
  - `TestResponsesAutoRoute` — model `muse-spark-1.3-contributor-free` → upstream receives `POST /zen/v1/responses` with translated body; chat frames out.
  - `TestUpstreamDownSurfaces502` — dial error → client gets `KindTransport`-class JSON error, not a hang.
- [ ] Run, fail.
- [ ] Implement `handler.go`: route `/v1/chat/completions` (read + buffer body ≤ 4 MiB, `ResolveModel`, `ClampEffort`, `EnsureFreeLaneShape`, build upstream URL by lane, attempt loop `for att := rot.Attempt(); …` with `rot.NextAttempt` per D1, body re-send from buffer), `GET /v1/models`; `stream.go`: watchdog-wrapped response reader, flush-per-frame, first-byte flag feeding the no-reissue guard, error mapping before first byte → OpenAI error envelope JSON.
- [ ] Green; commit `Add OpenAI gateway surface with staged re-issue`.

### Task 13: wire into `cmd/zen-router`

**Files:** `cmd/zen-router/main.go` (modify only).

- [ ] Add failing test? `main` stays thin — verify by build + existing suites; add a `TestGatewayMounts` in `internal/gateway` that exercises the same mux composition helper (`gateway.Mux(rotator, cfg)`) used by `main.go`, so mount wiring is covered.
- [ ] Implement: `cmdUp` builds config (`config.Load` + migration), pool, gateway handler via `gateway.Mux`; listener serves: `/_zenctl/*` (unchanged), `/v1/*` → gateway, `/zen/v1/*` → legacy reverse proxy (kept). `--listen` flag unchanged.
- [ ] `go build ./... && go vet ./... && go test -short ./...` all green; `./bin/zen-router help` prints updated usage (add a line for the OpenAI surface).
- [ ] Commit `Mount OpenAI gateway on the daemon listener`.

**Explicitly deferred (not in this plan):** restarting the live systemd unit or rebuilding `~/.local/bin/zen-router` (отмашка), TUI/control-API extension (Plan 2), thin plugin (Plan 3), README (Phase E).

---

## Task dependency notes

- Tasks 1–10 are independent leaf work — any order; 1 before 10 (paths), 10 before 11 (state accessors), 11 before 12 (Rotator interface), 12 before 13.
- Tasks 2–8 are all `internal/zen` pure functions: parallelizable across subagents with zero shared mutable state.
