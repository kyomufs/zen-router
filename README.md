# zen-router

Agent-agnostic local gateway for **OpenCode Zen** (opencode.ai) with direct-only
egress, key-pool quota rotation, and a warm keep-alive connection pool.

Solves three problems the previous JS-only plugin could not:

1. **Latency** — a Go daemon holds a warm TCP+TLS keep-alive pool to `opencode.ai`;
   steady-state requests skip the handshake on every turn.
2. **Quota burn** — when the anonymous daily quota is spent, the daemon rotates
   through the API key pool (next key on the same direct lane), with per-key and
   per-egress daily counters and a midday/midnight-UTC reset.
3. **Lock-in** — the OpenCode Zen wire protocol (disguise headers, canonical
   session ids, gate tools, Responses-only model routing, effort clamping, error
   classification) lives in the daemon and is reachable from **any OpenAI-compatible
   client** over loopback.

IP rotation happens at the network layer (system routing); the daemon itself
holds **no TUN/VPN device** — the gateway is direct-only.

## Status

| Phase | Scope | State |
|---|---|---|
| A | Gateway shim (OpenAI surface, Zen wire port, watchdogs, error classification) | **done** |
| B | Key-pool rotation (key pool, router stage machine) | **done** |
| C | TUI dashboard + systemd installer | **done** |
| D | Thin DSH plugin (entry point only) | **done** — rewritten in `dsh-opencode-zen` repo, installed as v0.17.0 |
| E | README, deferred-minor cleanup, live verification | **done** — `docs/superpowers/checklists/phase-e.md` (all boxes ticked, 2026-10-08) |
| F | WARP excision — direct-only gateway | **done** — `internal/warp` removed, control API narrowed to `status`/`stop`, TUN device deleted (2026-10-09) |

The installed `dsh-opencode-zen` plugin is the thin adapter, v0.17.0 (direct-only
pairing; the health ping targets the surviving `GET /_zenctl/status`).

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
zen-router status  [--listen ADDR]     show egress, quota counters and latency
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
| `GET /_zenctl/status`, `POST /_zenctl/stop` | control API (`/rotate` and `/use` are gone — 404) |
| `/zen/v1/*` | legacy reverse proxy (pre-gateway path, kept for the installed plugin) |

Client errors use the OpenAI envelope `{"error":{"message","type","code"}}`
(machine-readable `type`/`code`); quota `429`s additionally carry a `metadata`
object and a `Retry-After` header rendered as whole seconds (for daily limits:
distance to the next UTC midnight).

## Configuration and state (XDG)

- `$XDG_CONFIG_HOME/zen-router/config.json` — listen address, upstream base,
  address family (`auto|v4|v6`), key-pool file, timeouts.
- `$XDG_STATE_HOME/zen-router/state.json` — quota counters (migrated one-shot
  from `~/.dsh/state/zen-router/state.json`; pre-excision files carrying
  `mode`/`current`/`warp`/`identities`/`rotations` load as-is and those keys
  drop on the next save).
- Keys: `pool-config.json` from `dsh-api-key-pool` when present, else
  `OPENCODE_ZEN_API_KEY`, else `public`.

## Architecture notes

- **Rotation stages** (spec §6): daily-limit 429 → next key from the pool on the
  same (direct) lane → request exhausted; account-scoped limits advance keys only;
  transport failures never re-issue. A client request is capped at **3 executed
  upstream attempts** (the stage machine spends at most 2 with the default pool).
- **Direct-only egress** — one lane (`direct`); there is no WARP identity stage,
  no egress switching and no TUN interface. Outbound traffic follows the system
  routing table.
- **Streaming** — the daemon relays upstream SSE through translation into OpenAI
  chat chunks for `stream:true` clients; for non-streaming clients it buffers the
  translated stream server-side and flushes one `chat.completion` JSON body.

## Repository layout

```
cmd/zen-router/        CLI entry point
internal/gateway/      OpenAI surface, SSE relay/buffer, error envelopes
internal/router/       direct-only rotation state machine
internal/quota/        quota state (XDG)
internal/keys/         key pool
internal/zen/          OpenCode Zen wire: models, translation, classification
internal/tui/, systemd/
internal/proxy/, cli/, config/
docs/superpowers/      spec, plans, ledgers, checklists
```

## Safety

Live-gateway calls, plugin installation, DSH restarts, and systemd unit installs
were gated on explicit user approval and executed on 2026-10-08 — see
`docs/superpowers/checklists/phase-e.md`. The WARP excision was executed
autonomously on 2026-10-09 under a standing goal (user absent).
