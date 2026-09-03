# minecraft-server-agent

Minecraft Bedrock server chat agent: tool-calling LLM assistant, welcomes, stats, knowledge lookup.

The chat "ear" and brain for the FWB Bedrock server. Connects as a headless
Bedrock client (via `sandertv/gophertunnel`), reads chat, and dispatches
`!` commands and `@server` mentions to a small plugin host.

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

**Stage 2** (see the design doc: real console-bridge-backed `Voice`/`Facts`,
live permission resolution from `permissions.json`, and a join-triggered
welcome). `!help`, `!ping`, and `!players` all reach real players now, and
an operator-only command is actually gated by the server's own
`permissions.json` rather than treating everyone as a visitor. No database
or LLM yet - those land in later stages.

## Architecture

```
gophertunnel client --> chat.ParseTrigger --> plugin.Registry --> plugin.Voice (mc-console-bridge)
                    \-> roster.Roster (join detection, XUID->gamertag) --> plugin.EventHandler (welcome)
```

- `cmd/agent` - entry point: config, wiring, connect/reconnect loop, the
  event dispatcher that pumps bus events to `plugin.EventHandler`s
- `internal/config` - environment variable parsing
- `internal/logging` - structured JSON stdout logging (Loki-compatible)
- `internal/chat` - packet parsing, XUID-based identity, command/mention
  detection, self/sibling loop guard
- `internal/roster` - live XUID<->gamertag mapping from `PlayerList`
  packets; the authoritative join/leave signal (not chat, not the raw
  `add_player` proximity packet) and the name source `Voice.Tell` resolves
  a reply target from
- `internal/bus` - typed pub/sub event bus; every answerable chat message
  (`chat.MessageEvent`) and every genuinely new arrival
  (`roster.JoinEvent`) is published here, so event-driven plugins subscribe
  instead of touching the connection
- `internal/plugin` - the `Plugin`/`Context`/`Registry` extension surface
- `internal/plugins` - concrete plugins: `core` (`!help`/`!ping`), `stats`
  (`!players`), `welcome` (event-driven, no commands)
- `internal/adapters` - implementations of the plugin package's capability
  interfaces: `BridgeClient` (shared HTTP transport to mc-console-bridge),
  `BridgeVoice`, `BridgeFacts`, `PermissionResolver` (cached
  `GET /permissions` lookups); `NoopVoice` remains for tests
- `internal/mcauth` - Xbox Live device-code login with on-disk token
  caching, so a restart doesn't require a fresh interactive login
- `internal/httpapi` - `/healthz`, `/readyz` (reflects real Bedrock session
  state), and `/metrics`
- `internal/ratelimit` - per-actor sliding-window command rate limiting

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `MC_HOST` | *(required)* | Bedrock server hostname |
| `MC_USERNAME` | *(required)* | Token-cache identity; not the gamertag |
| `MC_PORT` | `19132` | Bedrock server port |
| `RECONNECT_MIN_MS` | `5000` | Initial reconnect backoff |
| `RECONNECT_MAX_MS` | `300000` | Reconnect backoff ceiling |
| `HTTP_ADDR` | `:8080` | `/healthz` + `/readyz` + `/metrics` listen address |
| `AUTH_CACHE_DIR` | `/data/auth` | Where the Xbox Live token is cached, one file per `MC_USERNAME` |
| `COMMAND_RATE_LIMIT_PER_MINUTE` | `10` | Max `!` commands a single actor (XUID) may trigger per rolling minute |
| `CONSOLE_BRIDGE_URL` | *(required)* | Base URL of `mc-console-bridge`'s HTTP API |
| `CONSOLE_BRIDGE_TOKEN` | *(required)* | Bearer token the bridge authenticates every request against |
| `CONSOLE_BRIDGE_TIMEOUT_MS` | `5000` | Timeout for each individual bridge HTTP call |
| `LOG_LEVEL` | `info` | `info` or `debug` |

## Identity model

Chat identity is always resolved from **XUID**, never the gamertag
(`SourceName`) - a player can set an arbitrary display name, but not an
arbitrary XUID. A message with both an empty XUID and an empty name is
treated as console-originated (`send-command say ...`); a message with an
empty XUID but a non-empty name is rejected outright rather than trusted,
since accepting it would let a player impersonate the console.

## Permission model

Every `!` command's actor permission is resolved from `mc-console-bridge`'s
`GET /permissions` (a cached read of the server's real `permissions.json`),
except one sentinel: `chat.ServerOrigin` (console-originated messages) is
trusted at operator level without ever reaching the bridge, since it isn't a
real XUID and will never appear in that file. Any XUID absent from the map -
never seen by the server, or a bridge lookup failure - resolves to the
least-privileged real level (`visitor`), so an unrecognised player is never
granted more trust than a stranger, and a bridge outage fails closed.

## Targeting a reply

`Voice.Tell` only ever receives an XUID, but Bedrock's `tellraw` needs a
selector or a name. The agent resolves the XUID to a gamertag via
`internal/roster` (fed from the server's own `PlayerList` packets - the
authoritative live roster, never the spoofable chat `SourceName`) and
targets the reply with `@a[name="<gamertag>"]`, a real Bedrock selector -
not a bare name token, which `mc-console-bridge`'s allowlist deliberately
keeps whitespace-free and which a gamertag containing a space (Xbox
gamertags may) couldn't satisfy anyway.

## First-run login

The device-code login runs in exactly one case: no cache file exists yet for
`MC_USERNAME` under `AUTH_CACHE_DIR`. The agent then prints a Microsoft
device-code login URL and code to stdout - in a container, that means the pod
logs. Complete the login once; the resulting token is cached under
`AUTH_CACHE_DIR` (a persistent volume in production) and refreshed
automatically on subsequent runs.

Any other cache problem is deliberately *not* an interactive re-login, since
a container would otherwise block on a device code nobody is watching for:

- **Corrupt, unreadable, or refresh-token-less cache file** - startup fails
  loudly and the process exits non-zero. Recover by deleting the
  `token-*.json` file for that username under `AUTH_CACHE_DIR` and
  restarting, which takes the first-run path above.
- **Expired or revoked refresh token** - surfaces as a dial failure and the
  connect loop retries with backoff indefinitely; no login prompt is ever
  printed. Recover the same way: delete the cache file and restart.

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .
```

## Releases

Pushing a version tag - a `v` followed by a digit, matching `v[0-9]*` -
publishes a container image to GitHub Container Registry:

```sh
git tag v0.1.0
git push origin v0.1.0
# -> ghcr.io/jdwillmsen/minecraft-server-agent:0.1.0
```

The leading `v` is stripped, so the git tag `v0.1.0` becomes the image tag
`0.1.0`. The same release can also be published from the Actions tab via the
`Release` workflow's manual trigger, which takes the version with or without
the leading `v`.

No `latest` tag is published - consumers (the Helm chart) pin an exact
version, so there is no moving tag that could silently upgrade a running
agent. Images are `linux/amd64` only.

## Design doc

The full architecture, staged delivery plan, and risk register live in the
approved design (not in this repo - see the `jdwlabs` planning history).
