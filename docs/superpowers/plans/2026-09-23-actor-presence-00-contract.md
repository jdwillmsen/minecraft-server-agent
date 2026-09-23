# Actor presence — shared contract and plan index

> **For agentic workers:** This file is not a plan. It fixes every name that
> crosses a repo boundary. Each phase plan implements part of it and must not
> rename anything here. A change to this file is a change to every plan.

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md`

## Plan index and order

| # | Plan | Repo | Depends on |
|---|---|---|---|
| 1 | `2026-09-23-actor-presence-01-lifecycle-split.md` | minecraft-server-agent | — |
| 2 | `2026-09-23-actor-presence-02-schema.md` | jdwlabs/platform | — |
| 3 | `2026-09-23-actor-presence-03-agent-presence.md` | minecraft-server-agent | 1, 2 |
| 4 | `2026-09-23-actor-presence-04-bridge-kick.md` | mc-console-bridge | — |
| 5 | `2026-09-23-actor-presence-05-bot-reconciler.md` | minecraft-afk-bot | 3 (tagged `presenceapi` module) |
| 6 | `2026-09-23-actor-presence-06-chart-cli.md` | jdw-deployments | 3, 4, 5 (released images) |
| 7 | `2026-09-23-actor-presence-07-alerts.md` | jdwlabs/platform | 6 live in prd |

Plans 1, 2 and 4 can run in parallel, each in its own worktree.

`<ticket>` in a plan's branch names is that phase's Jira key, handed over at
dispatch; keys stay out of repo content.

Known gap left for a follow-up: the agent image is built without its release
version, so the agent reports `process_version` as `(devel)`. Plan 5 adds a
version build argument for the bot only.

## Vocabulary

- **Actor ids:** `agent`, `afk-bot-1`, `afk-bot-2`. Regex `^[a-z0-9][a-z0-9-]{0,62}$`.
- **Kinds:** `agent`, `afk-bot`.
- **Groups:** `bots` (both AFK bots). `all` is implicit and never listed.
- **States:** `present`, `parked`. Nothing else.
- **Wake triggers:** `{"any_player_join": true}` or `{"players": ["Gamertag", ...]}`.
  Actors' own joins never count.
- **set_by prefixes:** `chat:<gamertag>`, `api:<token-name>`, `policy:<name>`
  (reserved, unused). The CLI is an API client, so its writes record
  `api:tools-mc`.
- **Token names:** `tools-mc` (scopes `presence:read`, `presence:write`);
  one per bot named after its actor id (scopes `presence:read`,
  `presence:report`, bound with `actor`).

## Go contract module

Path `github.com/jdwillmsen/minecraft-server-agent/presenceapi`, directory
`presenceapi/` in the agent repo with its **own `go.mod`** (`go 1.27`, standard
library only). Tagged `presenceapi/v0.1.0` by plan 3 (Go's subdirectory-module
tag convention). The agent's root module uses it through a `replace
=> ./presenceapi` directive plus a normal require.

```go
package presenceapi

type State string

const (
	StatePresent State = "present"
	StateParked  State = "parked"
)

type WakeOn struct {
	AnyPlayerJoin bool     `json:"any_player_join,omitempty"`
	Players       []string `json:"players,omitempty"`
}

// Override is a runtime deviation from an actor's default.
type Override struct {
	State   State      `json:"state"`
	Until   *time.Time `json:"until,omitempty"`
	WakeOn  *WakeOn    `json:"wake_on,omitempty"`
	Reason  string     `json:"reason"`
	SetBy   string     `json:"set_by"`
	SetAt   time.Time  `json:"set_at"`
	Version int64      `json:"version"`
}

// Presence is GET /v1/actors/{id}/presence.
type Presence struct {
	ActorID   string    `json:"actor_id"`
	Effective State     `json:"effective"`
	Default   State     `json:"default"`
	Override  *Override `json:"override,omitempty"`
}

// SetRequest is the body of PUT /v1/actors/{id}/presence and
// PUT /v1/groups/{group}/presence. At most one of Until and Duration may be
// set. Version is required for a single actor (0 = "expect no override") and
// ignored for a group.
type SetRequest struct {
	State    State      `json:"state"`
	Until    *time.Time `json:"until,omitempty"`
	Duration string     `json:"duration,omitempty"` // Go duration, e.g. "2h"
	WakeOn   *WakeOn    `json:"wake_on,omitempty"`
	Reason   string     `json:"reason"`
	Version  int64      `json:"version"`
}

// Status is the body of POST /v1/actors/{id}/status and part of ActorView.
type Status struct {
	Connected      bool      `json:"connected"`
	ObservedState  State     `json:"observed_state"`
	LastSeen       time.Time `json:"last_seen"`
	ProcessVersion string    `json:"process_version"`
}

// ActorView is one element of GET /v1/actors.
type ActorView struct {
	ID       string   `json:"id"`
	Gamertag string   `json:"gamertag"`
	Kind     string   `json:"kind"`
	Groups   []string `json:"groups"`
	Presence
	Status *Status `json:"status,omitempty"`
}

// Error is every non-2xx body.
type Error struct {
	Code    string    `json:"code"` // not_found, conflict, forbidden, unauthorized, invalid, unavailable
	Message string    `json:"message"`
	Current *Presence `json:"current,omitempty"` // set on 409
}
```

Golden files in `presenceapi/testdata/`: `presence_parked.json`,
`presence_default.json`, `set_request.json`, `status.json`,
`error_conflict.json`, `actors.json`. Both repos round-trip them.

## HTTP routes (agent, existing `HTTP_ADDR`)

| Method, path | Scope | 2xx | Errors |
|---|---|---|---|
| `GET /v1/actors` | `presence:read` | 200 `[]ActorView` | 401, 503 |
| `GET /v1/actors/{id}/presence` | `presence:read` | 200 `Presence`, `ETag: "<version>-<effective>"`; 304 on `If-None-Match` match | 401, 404, 503 |
| `PUT /v1/actors/{id}/presence` | `presence:write` | 200 `Presence` | 400, 401, 403, 404, 409, 503 |
| `DELETE /v1/actors/{id}/presence` | `presence:write` | 200 `Presence` | 401, 403, 404, 503 |
| `PUT /v1/groups/{group}/presence` | `presence:write` | 200 `[]Presence` | 400, 401, 403, 404, 503 |
| `POST /v1/actors/{id}/status` | `presence:report` | 204 | 400, 401, 403 (token bound to another actor), 404, 503 |

Auth header `Authorization: Bearer <token>`. Routes are mounted only when
`PRESENCE_TOKENS` is non-empty.

## Environment variables

| Variable | Process | Format |
|---|---|---|
| `PRESENCE_ACTORS` | agent | JSON `[{"id","gamertag","kind","groups":[...],"default_state"}]` |
| `PRESENCE_TOKENS` | agent | JSON `[{"name","token","scopes":[...],"actor":"<id, optional>"}]` |
| `PRESENCE_SELF_ID` | agent | actor id of the agent itself, default `agent` |
| `PRESENCE_URL` | afk-bot | base URL, e.g. `http://<release>-server-agent:8080`; unset = feature off |
| `PRESENCE_TOKEN` | afk-bot | bearer token with `presence:read` + `presence:report`, bound to its actor |
| `PRESENCE_ACTOR_ID` | afk-bot | its actor id |
| `PRESENCE_DEFAULT` | afk-bot | `present` or `parked`; used until the first answer |
| `PRESENCE_POLL_MS` | afk-bot | default `10000` |
| `BRIDGE_KICKABLE` | bridge | comma-separated gamertags; empty = `kick` refused for everyone |

## Database (schema `minecraft`, migration `V8__minecraft_presence.sql`)

```sql
CREATE TABLE IF NOT EXISTS minecraft.presence_overrides (
    actor_id  TEXT        NOT NULL PRIMARY KEY,
    state     TEXT        NOT NULL CHECK (state IN ('present','parked')),
    until     TIMESTAMPTZ NULL,
    wake_on   JSONB       NULL,
    reason    TEXT        NOT NULL,
    set_by    TEXT        NOT NULL,
    set_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    version   BIGINT      NOT NULL
);
CREATE TABLE IF NOT EXISTS minecraft.presence_status (
    actor_id        TEXT        NOT NULL PRIMARY KEY,
    connected       BOOLEAN     NOT NULL,
    observed_state  TEXT        NOT NULL CHECK (observed_state IN ('present','parked')),
    last_seen       TIMESTAMPTZ NOT NULL,
    process_version TEXT        NOT NULL DEFAULT ''
);
```

`version` only detects concurrent edits. A new row starts at 1 and every
update increments it. `DELETE` removes the row, so the next override starts at
1 again. `SetRequest.Version == 0` means "I expect no override"; a mismatch
returns 409 with the current `Presence`. A client that read "no override" while
another client created and then deleted one still succeeds — the end state is
what it asked for.

## Metrics (agent, leader only)

- `mc_presence_desired{actor}` gauge: 1 present, 0 parked.
- `mc_presence_observed{actor}` gauge: 1 connected, 0 otherwise.
- `mc_presence_override_age_seconds{actor}` gauge: age of an override with no
  `until`; series absent otherwise.
- `mc_presence_kicks_total{actor}` counter.

## Chat

| Command | Permission |
|---|---|
| `!presence` | member |
| `!park <target> [duration]` | operator |
| `!unpark <target>` | operator |
| `@server leave [duration]` | operator |

Target = actor id, group name or `all`. Durations use Go syntax (`30m`, `2h`).
Agent parked from chat: `until = now + (duration or 1h)`, `wake_on =
{"any_player_join": true}`.

## Timings

- Policy loop tick: 10s. Kick grace after park: 20s. Bot poll: 10s.
- "Recent joins" consumed by the policy loop: events since the last tick.
