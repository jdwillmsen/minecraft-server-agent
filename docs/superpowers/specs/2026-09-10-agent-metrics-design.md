# Agent Metrics and Dashboard

Status: approved for build.
Date: 2026-09-10.
Covers: the metrics track of Stage 6.

## Context

The agent exports two metrics: `mc_agent_connected` and
`mc_agent_reconnects_total`. Everything else it does -- commands, `@server`
answers, tool calls, announcement deliveries, audit writes, deaths, and the
TPS that `!ping` now measures -- exists only as log lines. The tenant
overview dashboard's "Chat answering" row still queries the retired AFK
bot's container, so nothing on any dashboard shows the agent at all.

## Goals

- Count every outcome an operator would ask about, with labels bounded by
  construction.
- One dashboard that answers "is the agent healthy and useful right now".
- Alerts only for states a human must act on, each with a promtool test.

## Non-goals

- Tracing. One process, one server; logs carry the request detail.
- Per-player metrics. A player label is unbounded and is a privacy leak
  on a dashboard; the audit table answers per-player questions.

## Metrics

All live in a new `internal/metrics` package, registered once per process on
the default registry the existing `/metrics` handler already serves. Domain
packages call small functions there rather than touching Prometheus types, so
a test can read a value back without scraping.

| Name | Type | Labels | Recorded |
|---|---|---|---|
| `mc_agent_commands_total` | counter | `command`, `outcome` | once per dispatch, beside the audit write |
| `mc_agent_mentions_total` | counter | `outcome` | once per `@server` mention |
| `mc_agent_answer_duration_seconds` | histogram | `outcome` | per answer attempt that reached the model |
| `mc_agent_tool_calls_total` | counter | `tool`, `outcome` | per tool invocation |
| `mc_agent_announce_deliveries_total` | counter | `delivery`, `outcome` | per send attempt |
| `mc_agent_audit_write_failures_total` | counter | none | per failed audit write |
| `mc_agent_auth_rejections_total` | counter | none | per Xbox Live account rejection |
| `mc_agent_deaths_total` | counter | none | per death the respawner handles |
| `mc_agent_server_tps` | gauge | none | per successful TPS measurement |
| `mc_agent_tps_last_success_timestamp_seconds` | gauge | none | same moment |
| `mc_agent_link_rtt_seconds` | gauge | none | per background sample while a session exists |

Label values:

- `command`: the registered command name, or `unregistered` for anything
  else. Players type arbitrary `!words`; passing them through is an
  unbounded label a single player could inflate.
- command `outcome`: `ok`, `denied`, `unknown`, `error`, `rate_limited`,
  `timeout` -- the audit outcomes, deliberately the same set.
- mention `outcome`: `answered`, `failed`, `empty`, `rate_limited`, `busy`,
  `undeliverable`, `send_failed`, `disabled`.
- answer `outcome`: `answered`, `failed`. Buckets 0.5, 1, 2, 4, 8, 15, 30
  seconds, matching the 30s total answer budget.
- `tool`: a name from the tool registry, or `unregistered` for a name the
  model invented.
- tool `outcome`: `ok`, `error`.
- `delivery`: `broadcast`, `whisper`, `summary`. `outcome`: `sent`,
  `failed`.

Every known label combination of `mentions_total`, the audit counter, the
auth and death counters, and `announce_deliveries_total` is initialised to
zero at startup. `increase()` over a series that springs into existence at 1
reads as 0, so an alert on the first failure after a restart would never
fire without this.

TPS is a gauge of the last good measurement, so it cannot say on its own that
measurement stopped. The success timestamp is what an alert reads for that.
The TPS gauge is never reset to a sentinel on failure: a dashboard showing
the last real value beside how old it is tells the truth, and a made-up zero
reads as a crashed server.

## Dashboard

`observability/dashboards/jdwillmsen/minecraft-agent.json` in the platform
repo, uid `jdwillmsen-minecraft-agent`, provisioned by the existing
jdwillmsen Git Sync folder. It uses the same datasource variables as
`tenant-overview.json`.

Rows: Health (session, reconnects, auth rejections, deaths), Server (TPS,
link round trip, TPS measurement age), Commands (rate by command and
outcome), Answering (outcomes, latency p50/p95, tool calls), Announcements
and audit (deliveries by outcome, audit write failures), Logs (agent
container, Loki).

The tenant overview's "Chat answering" row is repointed at the agent's own
series, since the container it queries no longer answers anyone.

## Alerts

In `jdwillmsen-alerts`, each with a promtool test under
`tests/prometheus-rules/`:

| Alert | Expression (shape) | For | Severity |
|---|---|---|---|
| `JdwillmsenMinecraftAgentReconnectStorm` | `increase(mc_agent_reconnects_total[30m]) > 6` | 5m | warning |
| `JdwillmsenMinecraftServerTpsLow` | `mc_agent_server_tps < 15` | 10m | warning |
| `JdwillmsenMinecraftServerTpsStale` | `time() - mc_agent_tps_last_success_timestamp_seconds > 600` | 5m | warning |
| `JdwillmsenMinecraftAgentAuditWritesFailing` | `increase(mc_agent_audit_write_failures_total[15m]) > 0` | 0m | warning |
| `JdwillmsenMinecraftAgentAnswersFailing` | failed share of answered+failed over 30m > 0.5, at least 3 failures | 10m | warning |

Warnings rather than pages: none of these is the server down, which the
existing critical alerts already cover. The reconnect storm is the leading
indicator of the abuse-mode hold that took the account down once; it is
worth knowing about before the login provider decides for us.

## Rollout

The agent change ships in the next batched release. The platform change can
merge first: alerts over series that do not exist yet are silent, and the
`absent()` style is deliberately not used for these new series.
