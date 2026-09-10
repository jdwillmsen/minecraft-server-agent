# Stage 6 Remainder and Stage 7

Status: approved for build.
Date: 2026-09-10.
Covers: announcement slices 2-4, the moderation audit, and the local-model
evaluation. The metrics track has its own document.

Every decision below that could have gone to a question was taken on the
conservative side and is marked **Decision**, so it can be revisited
without archaeology.

## Announcement slice 2: event-driven

Sources that write `source = 'event'` rows through the same outbox as
`!announce`. The deliverer does not change. Each source's only job is to
write a row correctly, as the slice 1 design promised.

| Event | Target | Delivery | Lifetime |
|---|---|---|---|
| A player's first-ever join | `permission` = operator | whisper | 24h |
| A player's total playtime crosses 10h, 24h, 100h or 500h | `everyone` | broadcast | 24h |
| The server's Bedrock version changes | `everyone` | broadcast | 24h |
| The world backup is older than the exporter's max age | `permission` = operator | whisper | 24h |

- Playtime is measured at `RecordLeave`, the only moment a session's
  length is known. `RecordLeave` returns the totals before and after, so
  "crossed a threshold" is a comparison of two numbers the store computed
  in one statement, never a guess.
- Version and backup come from the exporters `ServerInfo` already reads,
  polled every five minutes.
- **Decision:** a version or backup condition announces once per change,
  not once per poll. The last value seen is kept in memory. After an agent
  restart the first poll only records a baseline, so a restart never
  re-announces old news. A backup that stays stale is announced once, and
  announced again only after it has recovered and gone stale again.
- **Decision:** a first-join notice goes to operators, not the whole
  server. The welcome plugin already greets the player in public, and a
  second public line about the same arrival is noise.

## Announcement slice 3: scheduled

A platform migration, `V5__minecraft_announcement_schedules.sql`:

```sql
CREATE TABLE IF NOT EXISTS minecraft.announcement_schedules
(
    schedule_id   BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    body          TEXT        NOT NULL,
    author_xuid   TEXT        REFERENCES minecraft.players (xuid) ON DELETE SET NULL,
    -- 'player' is excluded on purpose: a recurring whisper to one person is
    -- a nag, and !announce @player already covers the one-off.
    target_kind   TEXT        NOT NULL
        CHECK (target_kind IN ('everyone', 'permission', 'online_only')),
    target_value  TEXT,
    priority      TEXT        NOT NULL DEFAULT 'normal'
        CHECK (priority IN ('normal', 'expedited')),
    -- Exactly one cadence. The floor on every_seconds keeps a typo from
    -- turning a reminder into spam.
    every_seconds INTEGER     CHECK (every_seconds >= 900),
    daily_at      TIME,
    CHECK ((every_seconds IS NULL) <> (daily_at IS NULL)),
    next_fire_at  TIMESTAMPTZ NOT NULL,
    active        BOOLEAN     NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS announcement_schedules_due_idx
    ON minecraft.announcement_schedules (next_fire_at) WHERE active;

ALTER TABLE minecraft.announcements
    ADD COLUMN IF NOT EXISTS schedule_id BIGINT
        REFERENCES minecraft.announcement_schedules (schedule_id) ON DELETE SET NULL;
```

- `daily_at` is UTC. **Decision:** UTC rather than a configured zone, so
  that no DST rule is built into this. `!schedule list` prints UTC
  explicitly so nobody has to guess.
- The agent checks for due schedules once a minute and claims each one with
  `UPDATE ... SET next_fire_at = <next> WHERE schedule_id = $1 AND
  next_fire_at = $2`. A row changes hands exactly once, even if two agents
  ever ran at the same time. A claimed schedule inserts one announcement
  with `source = 'schedule'`, its `schedule_id`, and `expires_at` equal to
  the next occurrence, so a missed reminder is replaced by the next one
  rather than stacked on top of it.
- A schedule that fell behind (the agent was down) fires once and moves
  to the next future occurrence. It never replays each missed slot.
- Operator commands:
  - `!schedule add daily HH:MM [flags] <message>`
  - `!schedule add every <N>m|<N>h [flags] <message>`
  - `!schedule list`
  - `!schedule del <id>`
  - The flags are `!announce`'s: `!now` for online_only and `!urgent` for
    expedited, parsed before the body with the same refusal rules. A
    `@player` target is refused with the reason.
  - `del` sets `active = false` rather than deleting, so announcements that
    already reference the row keep their provenance.

## Announcement slice 4: HTTP API

`POST /announcements` on the agent's existing HTTP server, for in-cluster
systems such as deploy hooks and CI:

```json
{"body": "...", "target": {"kind": "everyone|player|permission|online_only", "value": "..."},
 "priority": "normal|expedited", "expires_in_seconds": 3600}
```

- Bearer auth against `ANNOUNCE_API_TOKEN`, compared in constant time.
  **Decision:** when the token is unset, the endpoint is not mounted and
  answers 404. A disabled API must be indistinguishable from an absent one
  and never open.
- A `player` target takes a gamertag and resolves it through the same
  two-tier lookup `!announce @player` uses. An unknown name is a 422, not a
  silently dropped message.
- The response is `201` with the announcement id and how many recipients
  were reached immediately.
- Body length is capped at the same limit `!announce` enforces. Requests
  are capped at 16 KiB.
- **Decision:** the token is never minted by an agent. The chart takes an
  optional existing Secret name. Until a human creates that Secret in a
  terminal outside any agent session, the API stays off, and everything
  else ships and runs without it.

## Moderation audit

A platform migration, `V6__minecraft_moderation.sql`:

```sql
CREATE TABLE IF NOT EXISTS minecraft.moderation_events
(
    event_id    BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- Not a foreign key, for the same reason command_audit's is not: the
    -- record must outlive the player it describes.
    xuid        TEXT        NOT NULL,
    gamertag    TEXT        NOT NULL,
    message     TEXT        NOT NULL,
    rule        TEXT        NOT NULL CHECK (rule IN ('term', 'flood', 'caps')),
    detail      TEXT        NOT NULL DEFAULT '',
    action      TEXT        NOT NULL CHECK (action IN ('logged', 'warned')),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS moderation_events_xuid_idx ON minecraft.moderation_events (xuid, occurred_at DESC);
CREATE INDEX IF NOT EXISTS moderation_events_time_idx ON minecraft.moderation_events (occurred_at DESC);
```

- **Decision: only flagged messages are stored, never all chat.** A
  complete chat log is a much larger privacy exposure than the problem this
  solves, and nothing here needs one. Flagged rows are kept for 90 days.
  The agent prunes older rows once a day.
- Rules, evaluated on public chat only:
  - `term`: the message contains a configured term. Terms come from
    `MODERATION_TERMS` (comma-separated, set from chart values). Matching
    is case-insensitive and on word boundaries. With no terms configured
    the rule is off.
  - `flood`: more than 5 messages from one player in 10 seconds.
  - `caps`: at least 20 letters, 80% or more of them capitals.
- Actions:
  - Every flag is written as `logged`.
  - A `term` flag also whispers the player a short warning (`warned`).
  - The first flag in each 10-minute window per player also whispers
    online operators, through an announcement targeted at the operator
    permission, so an operator who is offline sees it when they join.
- **Decision:** no kicks, mutes or bans. The console bridge's allowlist does
  not permit them, and widening it is a security decision for a human, not
  a side effect of this feature. The audit is what makes that later
  decision informed.
- Messages from operators are still flagged and recorded. The record is
  only useful if it applies to everyone.
- `!modlog [player] [n]` (operator only): the newest flags, default 5, at
  most 10, whispered, each with its rule, detail, time and a truncated
  message.
- The moderation check never blocks the command or answer path. It runs
  after dispatch on the same message.

## Stage 7: local-model evaluation

An offline harness, `cmd/evalllm`, that runs a fixed case file against the
configured LLM endpoint through the real `AnswerWithTools` path, with
fixture-backed capabilities (knowledge, waypoints, server info) so results
are repeatable.

- Case file `eval/cases.yaml`. Each case has:
  - asker gamertag and XUID
  - question
  - expected tool names (all of, any of, or none)
  - `must_contain` and `must_not_contain` substrings
  - `private` (the answer must be whispered because it used the asker's
    waypoints)
- Scored per case:
  - tool selection
  - content checks
  - reply length within the chat limit
  - the reply does not end with a question (the anti-loop rule in the
    system prompt)
  - latency
- The report covers pass rate per dimension, p50/p95 latency and failures
  with the actual reply. It goes to stdout as markdown and, with `-out`,
  to a file.
- At least 25 cases, covering:
  - server status and version
  - knowledge hits and misses
  - own waypoints, including a question about another player's waypoints,
    which must not leak
  - a prompt-injection attempt that tries to make the model call a tool or
    announce something
  - small talk that needs no tool
  - an unanswerable question, where the model must say it does not know
    rather than invent
- The deliverable is the harness, the cases, and a first report run
  against the production endpoint and model, committed under `docs/eval/`
  with any prompt or parameter change it justifies. If the endpoint serves
  more than one model, compare them.
- **Decision:** not in CI. It needs a GPU endpoint and is slow. It is run
  by hand (`make eval`) before any model or prompt change.

## Release

The agent changes ship together as one release, on top of the metrics
track. Each restart is a fresh Xbox Live login, and batching is deliberate
after the abuse-mode hold. Migrations V5 and V6 merge and sync first. Every
new feature degrades to a plain "not available" reply against a database
without its tables, as slices 1 and Stage 5 did.
