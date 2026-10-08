# zen-router

Agent-agnostic local gateway for **OpenCode Zen** (opencode.ai) with Cloudflare
WARP IP rotation, staged quota management, and a warm keep-alive connection pool.

Solves three problems the previous JS-only plugin could not:

1. **Latency** — a Go daemon holds a warm TCP+TLS keep-alive pool to `opencode.ai`;
   steady-state requests skip the handshake on every turn.
2. **Quota burn** — staged rotation exhausts cheap levers first (next key on the
   same egress → fresh WARP identity → direct v4/v6), with per-key and per-egress
   daily counters and a midday/midnight-UTC reset.
3. **Lock-in** — the OpenCode Zen wire protocol (disguise headers, canonical
   session ids, gate tools, Responses-only model routing, effort clamping, error
   classification) lives in the daemon and is reachable from **any OpenAI-compatible
   client** over loopback.

## Status

| Phase | Scope | State |
|---|---|---|
| A | Gateway shim (OpenAI surface, Zen wire port, watchdogs, error classification) | **done** |
| B | Staged rotation (key pool, identity pool, router stage machine) | **done** |
| C | TUI dashboard + systemd installer | **done** |
| D | Thin DSH plugin (entry point only) | **done** — rewritten in `dsh-opencode-zen` repo, not installed yet |
| E | README, deferred-minor cleanup, live verification | in progress — `docs/superpowers/checklists/phase-e.md` |

The **installed** `dsh-opencode-zen` plugin (v0.15.1, transport-proxy variant) is
**not** touched until the user's explicit go-ahead (see
`docs/superpowers/specs/…-gateway-design.md` §12); the rebuilt thin v0.16.0 waits
for the Phase E cutover (`docs/superpowers/cutover-plan3-thin.md`).

## Build and test

Requires Go 1.26.

```sh
go build ./...
go vet ./...
timeout 180 go test -count=1 -short ./...
```

Tests are hermetic: `httptest` fake upstreams, loopback only, no live network,
no `-race` (the target environment has no cgo).

## CLI

```
zen-router up      [--listen ADDR] [--detach]  start the daemon (foreground; --detach
                                               forks it into the background)
zen-router status  [--listen ADDR]     show egress, mode and quota counters
zen-router rotate  [--listen ADDR]     force an egress rotation now
zen-router use     <direct|warp>       force the active egress path
zen-router stop    [--listen ADDR]     gracefully stop the daemon
zen-router tui     [--listen ADDR]     interactive control dashboard (1s poll of the control API)
zen-router install-systemd [--remove]  write the user unit, daemon-reload + enable --now;
                                       --remove disables and deletes it
```

### Environment

| Variable | Default | Meaning |
|---|---|---|
| `ZEN_ROUTER_LISTEN` | `127.0.0.1:8787` | control + gateway listener |
| `ZEN_ROUTER_STATE` | — | state file path override |

## HTTP surface (same listener)

| Prefix | Purpose |
|---|---|
| `GET /v1/models`, `POST /v1/chat/completions` | OpenAI-compatible gateway (stream **and** non-stream) |
| `GET /_zenctl/status`, `POST /_zenctl/rotate\|use\|stop` | control API |
| `/zen/v1/*` | legacy reverse proxy (pre-gateway path, kept for the installed plugin) |

Client errors use the OpenAI envelope `{"error":{"message","type","code"}}`
(machine-readable `type`/`code`); quota `429`s additionally carry a `metadata`
object and a `Retry-After` header rendered as whole seconds (for daily limits:
distance to the next UTC midnight).

## Configuration and state (XDG)

- `$XDG_CONFIG_HOME/zen-router/config.json` — listen address, upstream base,
  address family (`auto|v4|v6`), rotation policy, key-pool file, timeouts.
- `$XDG_STATE_HOME/zen-router/state.json` — quota counters, WARP identities,
  rotation history (migrated one-shot from `~/.dsh/state/zen-router/state.json`).
- Keys: `pool-config.json` from `dsh-api-key-pool` when present, else
  `OPENCODE_ZEN_API_KEY`, else `public`.

## Architecture notes

- **Rotation stages** (spec §6): IP-lane 429 → next key on same egress → fresh
  WARP identity → direct; account-scoped limits advance keys only; key-rate-limits
  never spend identities; transport failures never re-issue. A client request is
  capped at **3 executed upstream attempts**.
- **Cloudflare WARP** runs as WireGuard interface `zenwarp` with `SO_BINDTODEVICE`
  routing and DoH; control API `api.cloudflareclient.com`.
- **Streaming** — the daemon relays upstream SSE through translation into OpenAI
  chat chunks for `stream:true` clients; for non-streaming clients it buffers the
  translated stream server-side and flushes one `chat.completion` JSON body.

## Repository layout

```
cmd/zen-router/        CLI entry point
internal/gateway/      OpenAI surface, SSE relay/buffer, error envelopes
internal/router/       staged rotation state machine
internal/quota/        quota/identity/rotation state (XDG)
internal/keys/         key pool
internal/warp/         WARP/WireGuard lifecycle
internal/zen/          OpenCode Zen wire: models, translation, classification
internal/tui/, systemd/
internal/proxy/, cli/, config/
docs/superpowers/      spec, plans, ledger, checklists
```

## Safety

Live-gateway calls, plugin installation, DSH restarts, and systemd unit installs
are gated on explicit user approval — see `docs/superpowers/checklists/phase-e.md`.
