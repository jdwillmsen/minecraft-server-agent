# minecraft-server-agent

Minecraft Bedrock server chat agent: tool-calling LLM assistant, welcomes, stats, knowledge lookup.

The chat "ear" and brain for the FWB Bedrock server. Connects as a headless
`bedrock-protocol` client (via `sandertv/gophertunnel`), reads chat, and
dispatches `!` commands and `@server` mentions to a small plugin host.

## What this is not

This is **not** a rewrite of `minecraft-afk-bot`. That repo stays a pure
AFK presence bot with no chat logic once this agent is live (see the design
doc). This agent owns everything interactive: commands, welcomes, the
tool-calling LLM answer path, knowledge lookup, moderation audit.

It also does **not** write to the server console directly. All server-voice
output (`tellraw`, `say`, ...) goes through
[`mc-console-bridge`](https://github.com/jdwillmsen/mc-console-bridge), a
separate sidecar living in the server pod. Splitting the two repos makes
that boundary a repo boundary, not just a code boundary: this repo can carry
an LLM and read-only tools; only the bridge can ever write to the console.

## Status

**Stage 1** (see the design doc: connect loop, event bus, plugin host, the
`core` plugin's `!help`/`!ping`). No real console output yet - replies are
logged, not sent - and no permission resolution, database, or LLM. Those
land in later stages.

## Architecture

```
gophertunnel client --> chat.ParseTrigger --> plugin.Registry --> plugin.Voice
                                                                    (logging stand-in for now,
                                                                     mc-console-bridge in Stage 2)
```

- `cmd/agent` - entry point: config, wiring, connect/reconnect loop
- `internal/config` - environment variable parsing
- `internal/logging` - structured JSON stdout logging (Loki-compatible)
- `internal/chat` - packet parsing, XUID-based identity, command/mention
  detection, self/sibling loop guard
- `internal/bus` - typed pub/sub event bus (for future join/leave/welcome
  plugins)
- `internal/plugin` - the `Plugin`/`Context`/`Registry` extension surface
- `internal/plugins` - concrete plugins (`core` today; `welcome`, `stats`,
  `ask`, etc. in later stages)
- `internal/adapters` - implementations of the plugin package's capability
  interfaces (`NoopVoice` today; a real bridge-backed `Voice` in Stage 2)
- `internal/mcauth` - Xbox Live device-code login with on-disk token
  caching, so a restart doesn't require a fresh interactive login
- `internal/httpapi` - `/healthz` and `/metrics`

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `MC_HOST` | *(required)* | Bedrock server hostname |
| `MC_USERNAME` | *(required)* | Token-cache identity; not the gamertag |
| `MC_PORT` | `19132` | Bedrock server port |
| `RECONNECT_MIN_MS` | `5000` | Initial reconnect backoff |
| `RECONNECT_MAX_MS` | `300000` | Reconnect backoff ceiling |
| `HTTP_ADDR` | `:8080` | `/healthz` + `/metrics` listen address |
| `AUTH_CACHE_DIR` | `/data/auth` | Where the Xbox Live token is cached, one file per `MC_USERNAME` |
| `LOG_LEVEL` | `info` | `info` or `debug` |

## Identity model

Chat identity is always resolved from **XUID**, never the gamertag
(`SourceName`) - a player can set an arbitrary display name, but not an
arbitrary XUID. A message with both an empty XUID and an empty name is
treated as console-originated (`send-command say ...`); a message with an
empty XUID but a non-empty name is rejected outright rather than trusted,
since accepting it would let a player impersonate the console.

## First-run login

On first connect (and whenever the cached token can't be refreshed), the
agent prints a Microsoft device-code login URL and code to stdout - in a
container, that means the pod logs. Complete the login once; the resulting
token is cached under `AUTH_CACHE_DIR` (a persistent volume in production)
and refreshed automatically on subsequent runs.

## Development

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

## Design doc

The full architecture, staged delivery plan, and risk register live in the
approved design (not in this repo - see the `jdwlabs` planning history).
