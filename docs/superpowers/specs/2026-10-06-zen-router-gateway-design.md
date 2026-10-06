# zen-router — Agent-Agnostic OpenCode Zen Gateway

Date: 2026-10-06
Status: awaiting spec review
Supersedes: implicit design of the legacy `zen-router` prototype (proxy/warp/quota/cli packages)

## 1. Background and motivation

The DSH plugin `dsh-opencode-zen` (v0.15.1, ~1736 lines) currently owns everything:
the OpenCode Zen wire protocol (disguise headers, canonical session ids, gate tools,
Responses-only model routing), stream translation into DSH chunks, stream watchdogs,
an in-process retry loop, an address-family pinning pool (undici), a quota store, a
local status HTTP server, a key pool, and a web settings panel.

Observed problems:

- **Latency.** Node's fetch drops keep-alive connections after ~4s idle; every request
  pays a fresh TCP+TLS handshake through the FlClash TUN. Steady-state request cadence
  (agent turns) loses seconds per turn.
- **Quota burn.** A single WARP identity exhausts its daily bucket almost instantly
  (state.json: 24 daily-429 vs 3 successes on 2026-10-05). Rotation must be staged:
  exhaust cheaper levers (keys on the same IP) before spending fresh identities.
- **Lock-in.** The protocol knowledge lives in JS inside one DSH plugin; opencode CLI
  and other agents cannot reuse it.

## 2. Goals / Non-goals

### Goals

1. **Latency**: a Go daemon keeps a warm connection pool (TCP+TLS keep-alive) to
   `opencode.ai`, so steady-state requests skip handshakes.
2. **Quota rotation**: staged rotation — next key on the same egress first, then a
   fresh WARP identity (fresh IP), then fall back to direct. A pool of N identities
   plus spare keys, with per-key and per-egress counters.
3. **Agent-agnostic**: the daemon exposes an OpenAI-compatible surface on loopback.
   Any agent that can point a `baseURL` at an OpenAI endpoint can use the free Zen
   tier through it. All Zen-specific behavior (disguise, session, gate tools,
   Responses routing, effort clamping, error classification, quota accounting)
   lives in the daemon.
4. **DSH as a thin entry point**: `dsh-opencode-zen` shrinks to ~250 lines —
   register the provider + models, POST to the local daemon, map HTTP errors to
   DSH failure codes, check/raise the daemon.

### Non-goals

- No changes to `cordis.patch.yml` (plugin entry stays as-is).
- No replacement `dsh web` server.
- No non-Zen upstreams (the gateway targets OpenCode Zen only).
- No Windows support (Linux/NixOS: `ip`, `sudo -n`, fwmark are assumed).
- The installed plugin v0.15.1 in the profile is NOT modified until the user's
  explicit go-ahead ("отмашка").

## 3. Architecture

```
agent (DSH plugin / opencode CLI / any OpenAI client)
   │   OpenAI HTTP on loopback   POST /v1/chat/completions, GET /v1/models
   ▼
┌── zen-router daemon ────────────────────────────────────────┐
│ gateway/     body shaping: disguise headers, canonical      │
│              session ids, gate tools (bash,read), effort     │
│              clamps, auto-routing Responses-only models      │
│              (/chat/completions ↔ /responses conversion)     │
│ watchdogs    first-event 30s, body-idle 120s/300s            │
│ quota/       counters per egress × per key, midnight-UTC,    │
│              x-real-ip bucket semantics                      │
│ router/      staged rotation: key → identity → direct        │
│ warp/        identity pool (N devices + spares), WG tunnel   │
│ proxy/       keep-alive transport, direct + warp egress      │
│ control API  /_zenctl/*   (same loopback listener)           │
│ tui/         bubbletea dashboard over the control API        │
└──────────────────────────────────────────────────────────────┘
   │ direct (family v4/v6)  or  warp (SO_BINDTODEVICE zenwarp)
   ▼
https://opencode.ai/zen/v1
```

New packages: `internal/gateway` (OpenAI shim + Zen wire), `internal/tui`
(bubbletea). Evolved packages: `internal/proxy`, `internal/warp`,
`internal/quota`, `internal/router`, `internal/cli`.

## 4. External wire contract (daemon → opencode.ai)

Ported from `dsh-opencode-zen` lib/index.js (live-audited 2026-10-02 against
`sst/opencode` sources; an independent cross-check against upstream
`sst/opencode` is running in the background for this spec — claims marked
unverified stay TBD and will be folded in when it reports):

- **Endpoints**: `POST https://opencode.ai/zen/v1/chat/completions`,
  `POST .../responses`, `GET .../models`. Auth: `Authorization: Bearer <key>`
  with key pool round-robin (`public` fallback).
- **Disguise headers** (union of every header the real CLI can send):
  `user-agent: opencode/1.18.34`, `x-opencode-session-id`, `x-opencode-client: cli`,
  `x-opencode-session`, `x-session-affinity`, `X-Session-Id`, `x-opencode-request`,
  `x-opencode-project`.
- **Canonical session id**: `ses_<6 hex timestamp><14 base62>` derived from
  sha256("ses\x00" + conversation seed); stable per conversation, refreshed
  request id per attempt (`req_<hex>`).
- **Gate tools**: the anonymous lane 403s bodies without a bash+read agent shape;
  `ensureFreeLaneShape` injects reserved `bash`/`read` function tools
  ("Reserved for the host runtime; do not call it."), `tool_choice: none` only
  when the caller had zero tools.
- **Models table** (9 ids, verified against `GET /zen/v1/models`): per-model
  `contextWindow`, `maxOutput`, `efforts` ladder, `vision`, `reasoningRequired`,
  `responses` flag (muse-spark-* answers only on `/responses`).
  `resolveReasoningEffort` clamps to the declared ladder (`off → none` on the
  chat wire, omitted on Responses; `reasoningRequired` models default to `high`).
- **Error classification**: `FreeUsageLimitError|GoUsageLimitError|BlackUsageLimitError`
  → daily-limit (429, `Retry-After` honored, midnight-UTC window), `ModelError`,
  `RegionError`, 401 → invalid credential, 5xx → server, network/abort →
  transport/timeout/aborted. `Retry-After` > 60s ⇒ long daily window, do not
  sleep-and-retry inside the request — surface to the caller.
- **Quota bucket key**: raw `x-real-ip`, truncated to the first four IPv6 groups —
  IPv4 and IPv6 egresses are independent daily buckets; addresses in one /64 share
  a bucket. Models without their own `rateLimit` share one per-IP bucket.
  The gateway publishes **no** rate-limit headers; counters are observed
  successes before the first daily-429 (lower bound), keyed by UTC day, kept 3 days.

## 5. Client wire contract (agents → daemon)

- `POST /v1/chat/completions` — standard OpenAI chat (stream SSE included).
  The daemon internally converts to `/responses` for Responses-only models and
  converts the SSE back to chat format, so clients never need to know.
- `GET /v1/models` — OpenAI model list, served from the daemon's models table.
- `POST /v1/responses` — passed through for agents that speak Responses natively (optional, low priority).
- Errors: OpenAI-style JSON errors (`{"error": {"message", "type", "code"}}`)
  mapped from Zen classification; `429` carries `Retry-After`.
- Loopback only by default (`127.0.0.1:8787`, override `ZEN_ROUTER_LISTEN`).

## 6. Staged rotation (approved strategy: pooled identity + keys, staged)

1. **429 daily-limit arrives** → record against current egress AND current key.
2. **Step 1 — key rotation**: draw the next key from the key pool on the SAME
   egress; one automatic in-daemon re-issue of the request (body is buffered
   up to a cap before first byte; a re-issue only happens if no chunk has been
   forwarded to the client yet).
3. **Step 2 — identity rotation**: when keys are exhausted or the re-issue 429s
   again → fresh WARP identity from the identity pool (hot-spares registered
   lazily; default pool 4 live + 1 spare, cooldown 30s preserved).
4. **Step 3 — direct**: warp fully spent → direct egress (respecting the
   configured family), and back when cooldown/reset elapses.

Identity pool: `WarpIdentity` records already exist in state.json; the pool
extends this to N entries with per-identity health (registered-at, spent-until,
last-429). Spare registration is background, jittered, to respect CF rate limits.

Counters (visible in TUI): per egress (`ok`, `daily429`, `spentUntil`) AND per
key (`ok`, `daily429`, `last429`).

## 7. TUI (bubbletea) — dashboard + background daemon (approved model)

- `zen-router tui` connects to the control API with a 1s poll. If the daemon is
  not running, offer to start it (`zen-router up` detached, log file in XDG state).
- Screen: daemon status, current egress + latency, quota table (per egress and
  per key with countdown to reset), identity pool table, log tail.
- Keys: `r` rotate now, `d` direct / `w` warp, `s` start/stop daemon, `q` quit.
- Headless: `zen-router up` (foreground or `--detach`) for systemd autostart.

## 8. Daemon autostart — systemd user unit (approved)

`zen-router install-systemd` writes `~/.config/systemd/user/zen-router.service`
(`ExecStart=<binary> up`, `Restart=on-failure`), then `daemon-reload` +
`enable --now`. Logs go to journald; the file log in XDG state is for the TUI tail.
Uninstall via `zen-router install-systemd --remove`.

## 9. DSH plugin — thin adapter (~250 lines, replaces 1736)

- Registers provider `opencode` + the 9 models (static table as fallback; live
  catalog from `GET /v1/models` when the daemon is up).
- `stream()` → `POST http://127.0.0.1:8787/v1/chat/completions` with the host's
  `AbortSignal`; minimal SSE → StreamChunk translation (text/reasoning/tool deltas,
  usage, finish), no inner retry loop (host retry policy owns it).
- Error mapping: HTTP → DSH codes (429 → `RATE_LIMIT` + `providerRetryAfterMs`,
  5xx → `SERVER`, socket → `TRANSPORT`, deadline → `TIMEOUT`, 401 → terminal
  `INVALID_CREDENTIAL`, daily-limit body → `QUOTA`).
- `apply()` health-check: ping `/_zenctl/status`; if down — log a warning and
  (config-gated) spawn `zen-router up --detach`.
- Removed from the plugin: web settings panel (`client.js`), status server
  (`status.js`, port 47821), quota store (`quota.js`), key pool, undici family
  pool, disguise/session code, Responses routing, watchdogs, in-process retry.
  All of it moves to the daemon. The panel's replacement is the TUI.

## 9b. Plugin source repo state (discovered 2026-10-06)

- Source repo cloned to `/home/kyomufs/Projects/dsh-opencode-zen` (GitHub
  `kyomufs/dsh-opencode-zen`); the profile pins
  `github:kyomufs/dsh-opencode-zen#a416790` in `profiles/web/package.json`.
- Repo HEAD `c2471d2` ("drop the status panel and client seat; route traffic
  through the zen-router proxy") is an intermediate iteration of THIS project
  that is NOT installed: it removed `lib/status.js` + `lib/client.js` and
  defaulted `OPENCODE_BASE` to `127.0.0.1:8787/zen/v1` (the transport-proxy
  variant, superseded by the OpenAI-shim decision in §3).
- Plan implication: phase D builds the thin adapter from the pinned
  `a416790` lineage's protocol knowledge (or reworks `c2471d2`), and the
  profile pin moves to the new commit only after the user's отмашка.

## 10. Config and paths (approved: XDG)

- `$XDG_CONFIG_HOME/zen-router/config.json` (default `~/.config/zen-router/config.json`):
  listen address, upstream base, address family, rotation policy (cooldown,
  pool sizes), key-pool file path, timeouts. Env overrides: `ZEN_ROUTER_*`.
- `$XDG_STATE_HOME/zen-router/state.json` (+ `zen.log`):
  migrated automatically from `~/.dsh/state/zen-router/state.json` on first run
  (one-shot copy, original left in place).
- Keys: read `pool-config.json` from `dsh-api-key-pool` when present (backward
  compatible), else `OPENCODE_ZEN_API_KEY` env, else `['public']`.

## 11. CLI surface

`zen-router up [--detach] | tui | status | rotate | use <direct|warp> | stop | install-systemd [--remove]`
(legacy `__wgcfg` privileged helper preserved).

## 12. Testing strategy and safety gates

**Allowed now (no DSH, no live gateway):**
- Go unit tests with `httptest` fake upstream: body shaping, header set,
  session derivation, effort clamps, error classification, staged rotation with
  fake clocks, state/migration, SSE translation, keep-alive reuse.
- `go build ./...` + `go vet`.

**Only after the user's explicit "отмашка":**
- Any live call to `opencode.ai` (probe, E2E, quota).
- Installing the new plugin into the profile (`dsh plugin ...`), config edits,
  `dsh --profile web --dump-config`, restarting DSH, running the daemon next to
  the live agent.

Until then the installed plugin v0.15.1 keeps running untouched.

## 13. Implementation phases (plan to follow via writing-plans)

- **A. Gateway shim**: `internal/gateway` — OpenAI surface, Zen wire port,
  watchdogs, error classification, over the existing `internal/proxy` transport.
- **B. Staged rotation**: key pool + identity pool + router stage machine.
- **C. TUI**: control API extension (per-key counters, daemon start/stop),
  bubbletea dashboard, systemd installer.
- **D. Thin plugin**: rewrite `dsh-opencode-zen` (~250 lines) — but do not
  install it before отмашка.
- **E. Docs**: README for the Go project; user go-ahead for live tests.

## 14. Risks and mitigations

- **Protocol drift** (upstream changes Zen behavior): all protocol knowledge in
  one Go package; verified against the live-audited plugin behavior and a
  background cross-check of `sst/opencode` sources (§4); models table
  refreshable from `GET /models`.
- **Cloudflare rate-limits identity registration**: lazy spare registration with
  jitter; on registration failure fall back to direct and surface in TUI.
- **sudo/ip dependencies**: already verified (`sudo -n` NOPASSWD ALL,
  `/run/current-system/sw/bin/ip`); wgctrl fallback via `__wgcfg` helper.
- **Two sources of truth during transition** (old plugin counters vs daemon
  counters): accepted — plugin is untouched until отмашка; after cutover the
  daemon is the only counter.
- **Same-listener control API**: `/_zenctl/*` on the loopback listener only;
  no mutating route without loopback origin (TUI and plugin are local).
