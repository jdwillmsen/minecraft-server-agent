# Actor presence design

Date: 2026-09-23
Status: approved design, not yet implemented

## Problem

Every account that puts a player into the FWB world keeps chunks loaded and
ticking around it: the server agent and both AFK bots. Bedrock's global limits
(ticking and spawning are counted around every player) mean those chunks
sometimes have to be released during play, then loaded again later.

Today the only switch is `replicas` in the `minecraft-fwb` chart values,
changed through git and ArgoCD. That is too slow for use mid-game, it restarts
pods (and with them the Xbox sign-in), it trips `JdwillmsenMinecraftAfkBotsDegraded`
after 30 minutes, and it cannot take the agent out of the world without also
stopping its monitoring. `kubectl scale` is not an option: ArgoCD selfHeal
reverts it within seconds.

None of the three processes has a runtime switch. The AFK bot has no API, no
health endpoint and a reconnect loop that never stops. The agent's monitoring
(TPS sampling, server watcher, scheduler, moderation pruning) only runs while
it holds a live in-world session.

## Goals

- Park or restore any actor, a group, or all of them, within seconds, from
  in-game chat, an HTTP API or the `tools/mc` CLI.
- The agent can leave the world while it keeps monitoring the server.
- A parked agent can always come back: by timer, by API/CLI, or when a player
  joins.
- The desired state survives pod restarts, rollouts and leader handovers.
- Git stays the source of truth for the baseline; runtime state is only ever a
  deviation from it, with an owner, a reason and usually an expiry.
- Adding a bot, or a new rule that decides presence, touches configuration or
  one policy, not every component.
- Deliberate parking never pages anyone; an actor that should be present and
  is not still does.

## Non-goals

- Moving an actor to a different location in the world. Parking means
  disconnecting.
- Relaying chat to a parked agent. Reading chat from a behavior-pack script
  needs the world's Beta APIs experiment, which permanently marks a survival
  world.
- Automatic policies such as "park bots while TPS is below 18" or
  "present only while a player is online". The model is built so they can be
  added as policies later; none ship in this work.
- Changing replica counts or any Kubernetes object at runtime.

## Why lowering view distance does not help

A bot's `MC_VIEW_DISTANCE` only changes how many chunks the server sends it.
What ticks is decided by the server's simulation distance around each player.
Only removing the player releases the chunks, so parking disconnects.

## Domain model

**Actor.** An account that puts a player into the world.

| Field | Meaning |
|---|---|
| `id` | Stable slug: `agent`, `afk-bot-1`, `afk-bot-2` |
| `gamertag` | The in-game name, used for `kick` and roster matching |
| `kind` | `agent` or `afk-bot` |
| `groups` | Named sets, e.g. `bots`. `all` is implicit |
| `default_state` | `present` or `parked`, from Helm values |

The actor list comes from one Helm values list. The chart renders both the bot
Deployments and the agent's actor registry (`PRESENCE_ACTORS`, JSON) from it,
so a bot cannot exist without being registered, or the reverse.

**Override.** A runtime deviation from an actor's default, one row per actor at
most.

| Field | Meaning |
|---|---|
| `actor_id` | The actor |
| `state` | `present` or `parked` |
| `until` | Optional. When it passes, the override is removed |
| `wake_on` | Optional. `any_player_join`, or a list of gamertags |
| `reason` | Free text, required from the API and CLI |
| `set_by` | `chat:<gamertag>` or `api:<token-name>`; the CLI is an API client and records `api:tools-mc` |
| `set_at` | Timestamp |
| `version` | Incremented on every write; writes carry the version they read |

The effective state is the override's state if a row exists, otherwise the
default. Clearing an override always falls back to what git says.

`wake_on` only removes an override whose state is `parked`. The actors'
own joins never count as a player join.

**Observed status.** What each actor last reported: `connected`,
`observed_state`, `last_seen`, `process_version`. Bots report through the API;
the agent writes its own directly. It feeds the status command, the API
listing and the alerts.

## Architecture

### Agent: split monitoring from presence

Today the leader lock, the in-world session and the monitoring loops share one
lifecycle. They separate into two:

1. **Leader lifecycle.** Holding the Postgres advisory lock starts
   `startLiveWork` (TPS sampling, pruning, server watcher, scheduler) and the
   presence policy loop. None of these needs the in-world session: they speak
   to the server through the console bridge.
2. **Session lifecycle.** A leader whose own effective state is `present` runs
   the connect loop. When it changes to `parked` the session closes and the
   loop waits for the state to change back.

While the session is down, the online roster comes from the bridge: a `list`
command to seed it, then `/events` connect and disconnect lines. The
announcement deliverer's `Online` and `SinceConnect` read from that roster
instead of the session.

Chat commands and `@server` questions are unavailable while the agent is
parked. That is the cost of option B and the reason every agent park carries
a way back.

### Agent: `internal/presence`

| Unit | Responsibility |
|---|---|
| `Registry` | Parsed `PRESENCE_ACTORS`; resolves ids, groups and `all` |
| `Store` | Postgres overrides and observed status; optimistic writes by version |
| `Policy` | Pure function: actors, overrides, now, recent joins → effective states and the overrides to remove |
| `Loop` | Leader-only, every 10s: runs `Policy`, removes expired and woken overrides, kicks actors that are parked but still on the server |
| `API` | HTTP handlers under `/v1` |
| `ChatPlugin` | `!presence`, `!park`, `!unpark`, `@server leave` |
| `Metrics` | `mc_presence_desired`, `mc_presence_observed` |

`Policy` has no I/O so every rule is a table test. Future automatic policies
are further inputs to it that produce overrides with `set_by: policy:<name>`.

Reads and writes go straight to Postgres, so the standby replica serves the API
as well as the leader. Only the loop is leader-only.

### Guaranteed unload

The Bedrock server keeps a session open for a while after a client leaves, so
the chunks do not always unload when a bot disconnects. About 20 seconds after
an actor's effective state becomes `parked`, if the bridge's `list` still shows
its gamertag, the loop sends `kick <gamertag>` through the bridge.

### Bridge: `kick`, limited to actors

`POST /command` gains `kick <gamertag>`. The bridge accepts it only for
gamertags in its own actor list (`BRIDGE_KICKABLE`, rendered by the chart from
the same values list). A leaked bridge token cannot kick a real player.

### AFK bot: desired-state reconciler

Each bot gains a small loop, enabled by `PRESENCE_URL` and `PRESENCE_TOKEN`:

- Every 10 seconds, `GET /v1/actors/{id}/presence` with an `If-None-Match`
  ETag, and `POST /v1/actors/{id}/status` with what it is doing.
- `parked`: close the session and pause the reconnect loop. `present`: resume.
- **Agent unreachable:** keep acting on the last answer. A bot that has never
  had an answer uses `PRESENCE_DEFAULT`, which the chart sets to the actor's
  default. An agent outage never makes the bots flap.
- With `PRESENCE_URL` unset the bot behaves exactly as today.

The bot's session setup already happens inside `runConnectLoop`; the
reconciler gates entry to it and cancels the session context on park.

### Shared contract

The request and response types live in one package the agent exports and the
bot imports. The bot's `docs/decisions.md` records that importing agent
packages raised 20 module versions and turned every agent release into a
Renovate bump, so the contract package gets its own Go module
(`minecraft-server-agent/presenceapi`) with no dependencies beyond the standard
library. Contract tests in both repos decode the same golden JSON files.

## Interfaces

### HTTP (agent, `HTTP_ADDR`)

| Method and path | Scope | Purpose |
|---|---|---|
| `GET /v1/actors` | `presence:read` | Every actor with default, override, effective and observed state |
| `GET /v1/actors/{id}/presence` | `presence:read` | Effective state for one actor; ETag |
| `PUT /v1/actors/{id}/presence` | `presence:write` | Set an override: `state`, `until` or `duration`, `wake_on`, `reason`, `version` |
| `DELETE /v1/actors/{id}/presence` | `presence:write` | Remove the override |
| `PUT /v1/groups/{group}/presence` | `presence:write` | Same body, applied to every member in one transaction |
| `POST /v1/actors/{id}/status` | `presence:report` | A bot reports its observed status; the token must belong to that actor |

Tokens come from `PRESENCE_TOKENS` (JSON: name, scopes, optional actor). The
routes are mounted only when it is set, the same way `POST /announcements`
depends on `ANNOUNCE_API_TOKEN`. A stale `version` returns 409 with the
current row.

### Chat

| Command | Permission | Effect |
|---|---|---|
| `!presence` | member | Each actor: effective state, source, expiry |
| `!park <actor\|group\|all> [duration]` | operator | Park; without a duration it lasts until unparked |
| `!unpark <actor\|group\|all>` | operator | Remove the override |
| `@server leave [duration]` | operator | Park the agent |

Parking the agent from chat always gets a way back: `until` defaults to 1 hour
and `wake_on` to `any_player_join`, and an explicit duration only replaces the
timer. `!park all` includes the agent under the same rule.

The agent confirms in chat before it leaves, and announces through the bridge's
`say` when it rejoins.

### CLI

`tools/mc` in jdw-deployments gains `presence ls`, `presence park <target> [--for 2h] --reason <text>` and
`presence unpark <target>`. A thin client over the HTTP API that
follows the AXI conventions: structured output, exit codes 0 and 1, no
prompts.

## Data

Two tables in the `minecraft` schema, migrated by `jdwlabs/platform`'s
jdwillmsen-schemas service like the rest of the agent's schema:
`presence_overrides` (primary key `actor_id`) and `presence_status` (primary
key `actor_id`). Every override write, removal and wake also goes into the
existing audit log with actor, old state, new state, source and reason.

## Operations

- **Metrics.** `mc_presence_desired{actor}` and `mc_presence_observed{actor}`
  (1 present, 0 parked or absent), exported by the leader.
- **Alerts.** `JdwillmsenMinecraftAfkBotsDegraded` and
  `JdwillmsenMinecraftAgentDisconnected` are rewritten to fire only when
  desired is 1 and observed is 0 for their existing windows. A new
  `JdwillmsenMinecraftActorParkedLong` warns when any override without `until`
  is older than 24 hours.
- **Runbook.** The chart README gets a "Parking actors" section replacing the
  `replicas: 0` advice for gameplay use. `replicas: 0` stays documented for
  maintenance.

## Error handling

| Situation | Behaviour |
|---|---|
| Postgres unavailable | API returns 503; the loop skips its tick; bots keep their last answer |
| Bridge unavailable | Kicks and the bridge roster are retried next tick; parking still stops the bot's own reconnects |
| Agent has no leader | API still answers from Postgres; expiries and wakes wait for a leader |
| Two edits race | The second gets 409 and the current row |
| Unknown actor or group | 404 from the API; a usage reply in chat |
| Bot token used for another actor | 403 |

## Testing

- `Policy` table tests: expiry, wake on any join, wake on named gamertags,
  actors' joins ignored, group expansion, the agent-park default.
- `Store` tests against Postgres, following `postgres_live_test.go`, including
  version conflicts.
- API tests for scopes, ETags, 409 and 403.
- Chat plugin tests for permissions and argument parsing.
- Bot reconciler tests against a fake API: park, resume, unreachable agent,
  never-answered start.
- Bridge tests: `kick` accepted for an actor, rejected for anyone else.
- A leader-handover test: overrides and pending expiries survive it.

## Rollout

Each step is its own PR and safe to ship alone.

1. **Agent: split lifecycles.** Monitoring follows the leader lock; the roster
   comes from the bridge when there is no session. No visible change.
2. **Platform: schema.** The two tables, through jdwillmsen-schemas.
3. **Agent: presence.** Registry, store, policy, loop, API, chat, metrics,
   contract module. Bots ignore it until step 5.
4. **Bridge: `kick` for actors.**
5. **AFK bot: reconciler**, off unless `PRESENCE_URL` is set.
6. **jdw-deployments: chart and CLI.** Actor list in values, tokens, bot
   presence variables, `tools/mc presence`, runbook.
7. **Platform: alerts.** Rewrite the two alerts in the jdwillmsen-alerts
   PrometheusRule and add `JdwillmsenMinecraftActorParkedLong`, with rule
   tests under `tests/prometheus-rules/`. Ships after step 6, once the
   metrics exist in production.
