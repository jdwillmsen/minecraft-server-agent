# Announcements and Command Audit

Status: approved design, not yet implemented.
Date: 2026-09-09.
Covers: slice 1 of 4 (the delivery engine and its first source), plus the
command audit trail.

## Context

The agent can answer questions and remember facts. It cannot tell anyone
anything on its own initiative. Everything it says today is a reply: to a
`!` command, to an `@server` mention, or to a join.

Two requirements arrived together and turn out to be the same shape.

The first is announcements: a message the server wants delivered, to
everyone or to one player, now or when they next log in, from a human
typing a command or from a system with news. The list of things worth
announcing is long and will keep growing -- restart warnings, farm
relocations, backup outcomes, playtime milestones, deploy notices.

The second is compliance: a durable, queryable record of who ran what.
Today every command is logged to stdout and nowhere else. That record dies
with the pod, cannot be queried, and is not something an auditor would
accept.

Both are answered by writing rows rather than by writing chat lines.

## Goals

- One delivery path that every source shares, so a new source only has to
  write a row correctly.
- Targeting: everyone, one player, everyone at a permission level, or
  online-only.
- Durability: a message for an offline player waits, and arrives when they
  return -- unless it has stopped being worth saying.
- A record of every command and every delivery, durable and queryable.

## Non-goals for this slice

- Scheduled and API sources. Designed for, not built (slices 3 and 4).
- Event-driven announcements (slice 2), though the milestone data already
  exists in `minecraft.players`.
- Editing or recalling a sent announcement. An announcement is an event,
  not a document.
- Cross-server delivery. There is one server.

## Architecture

```
sources                     outbox                    deliverer
──────────────────────      ──────────────────        ──────────────────────
!announce (this slice) ┐
schedule (slice 3)     ├──► minecraft.announcements ─► target resolves to
agent events (slice 2) │    minecraft.announcement_    who is online now
HTTP API (slice 4)     ┘    deliveries               ─► rest wait for a join
```

Nothing writes to chat directly. A source's whole job is to insert a row;
the deliverer owns every question about who sees it, when, and whether it
is still worth saying. That is what keeps three further sources cheap.

## Data model

A platform migration, `V4__minecraft_announcements.sql`.

```sql
CREATE TABLE IF NOT EXISTS minecraft.announcements
(
    announcement_id BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    body            TEXT        NOT NULL,
    -- Which source produced this. Kept because the retention and expiry
    -- rules differ per source, and because "who told the server to say
    -- this" is the first question asked about anything it said.
    source          TEXT        NOT NULL
        CHECK (source IN ('command', 'schedule', 'event', 'api')),
    -- The actor for a command-sourced announcement; null for the rest,
    -- which have no human behind them.
    author_xuid     TEXT        REFERENCES minecraft.players (xuid) ON DELETE SET NULL,
    target_kind     TEXT        NOT NULL
        CHECK (target_kind IN ('everyone', 'player', 'permission', 'online_only')),
    -- The XUID for 'player', the level name for 'permission', unused
    -- otherwise. Deliberately one nullable column rather than three: no
    -- target kind needs two of them at once.
    target_value    TEXT,
    priority        TEXT        NOT NULL DEFAULT 'normal'
        CHECK (priority IN ('normal', 'expedited')),
    -- Whispered or broadcast. Derived from target_kind on insert rather
    -- than chosen freely: a 'player' announcement that broadcast would
    -- leak exactly what whispering waypoints protects.
    delivery        TEXT        NOT NULL
        CHECK (delivery IN ('broadcast', 'whisper')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Not before. Lets a source schedule without a scheduler existing yet.
    deliver_after   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- After this, never delivered. The single most important column here:
    -- see Decay.
    expires_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS announcements_pending_idx
    ON minecraft.announcements (deliver_after)
    WHERE expires_at IS NULL OR expires_at > now();

-- One row per person who actually received it. This is both the
-- idempotency key and the audit record: "delivered once, to whom, when"
-- is answerable only if the answer is written down.
CREATE TABLE IF NOT EXISTS minecraft.announcement_deliveries
(
    announcement_id BIGINT      NOT NULL REFERENCES minecraft.announcements (announcement_id) ON DELETE CASCADE,
    xuid            TEXT        NOT NULL REFERENCES minecraft.players (xuid) ON DELETE CASCADE,
    delivered_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (announcement_id, xuid)
);

-- Every command dispatch, whatever the outcome. Separate from
-- announcements because it answers a different question, and because its
-- retention will differ: this is the compliance trail.
CREATE TABLE IF NOT EXISTS minecraft.command_audit
(
    audit_id     BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    xuid         TEXT        NOT NULL,
    gamertag     TEXT        NOT NULL,
    -- The level the actor was resolved at when the command ran, not their
    -- level now. Permissions change; what they were allowed to do at the
    -- time is the auditable fact.
    permission   TEXT        NOT NULL,
    command      TEXT        NOT NULL,
    args         TEXT        NOT NULL,
    outcome      TEXT        NOT NULL
        CHECK (outcome IN ('ok', 'denied', 'unknown', 'error', 'rate_limited', 'timeout')),
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS command_audit_xuid_idx ON minecraft.command_audit (xuid, occurred_at DESC);
CREATE INDEX IF NOT EXISTS command_audit_time_idx ON minecraft.command_audit (occurred_at DESC);
```

`xuid` on `command_audit` is deliberately **not** a foreign key: an audit
row must survive the deletion of the player it describes, which is the
opposite of what waypoints need.

### What the audit does not store

The reply text is not recorded. Replies to `!wp` contain coordinates the
agent goes out of its way to whisper, and an audit trail that transcribes
every private reply is a bigger privacy exposure than the one it is meant
to close. Who ran what, at what level, and whether it succeeded is the
compliance question. The current stdout logging keeps its behaviour
unchanged for live debugging.

## Decay

Every announcement may carry `expires_at`, defaulted by source and
overridable by the sender. This is the difference between a queue and a
nag.

| Class | Default lifetime | Why |
|---|---|---|
| `online_only` | never queues | A restart countdown is meaningless the moment it has passed |
| Operator whisper to a player | 7 days | "The farm moved" is still true next week |
| Operator broadcast | 24 hours | Aimed at the people who were around |
| Event-driven (slice 2) | 24 hours | A milestone is worth mentioning on the next visit, not a fortnight later |
| Scheduled (slice 3) | until the next occurrence | A missed reminder is superseded, never stacked |

An expired announcement is never delivered and is never deleted; it stays
for audit. "Why did nobody get told" is a question the table should be able
to answer.

## Delivery

**Immediately after insert**, the deliverer resolves the target against the
live roster and sends to everyone matching who is online. `everyone` and
`online_only` broadcast once through `Voice.Say`; `player` and `permission`
whisper per recipient through `Voice.Tell`, writing one delivery row each.

`permission` targeting resolves through the same `PermissionResolver` the
commands already use, so "all operators" means exactly what
`permissions.json` says at that moment.

**On a join event**, the deliverer drains what is pending for that player:
anything not expired, whose `deliver_after` has passed, that they have no
delivery row for, and whose target includes them. `online_only` is excluded
by construction -- it has no queue.

Order and volume at join:

1. Every `expedited` message, uncapped. Expedited exists precisely to
   bypass the cap.
2. Up to three `normal` messages, oldest first.
3. If any remain, one summary line naming the count and `!inbox`.

The cap exists because chat is scarce and the join moment is already
contested: the welcome message lands in the same second. A wall of text is
a worse experience than a trickle, and `!inbox` gives back control to the
player who wants it all now.

`!inbox` drains the remainder on demand, same rules. Implementation note:
it delivers a few at a time and reports the remainder rather than draining
without limit -- the command is answered inside a dispatch timeout, and an
uncapped drain of a real backlog exceeds it mid-delivery, which costs the
player their reply as well as the rest of their messages.

## Broadcast deduplication

A broadcast announcement is sent once to the whole server, but the delivery
table records one row per player online at the time. Without that, a player
who was online when it broadcast would receive it again on their next join.
The roster is the source of truth for who was present.

## Sources in this slice

`!announce`, operator only:

- `!announce <message>` -- everyone, broadcast, 24h
- `!announce @<player> <message>` -- whispered, 7 days, queued if offline
- `!announce !now <message>` -- `online_only`, never queues
- `!announce !urgent ...` -- marks it expedited

Flags parse before the message body and are refused mid-message, so a
message that happens to contain `!now` is not silently reinterpreted --
the same class of ambiguity the waypoint parser already refuses rather than
guesses.

`!inbox`, any member: drains their own queue. A player can only ever drain
their own; there is no argument naming someone else, for the same reason
no waypoint command takes an owner.

## Failure behaviour

- No database: `!announce` reports that announcements need persistence and
  changes nothing. Immediate broadcast could work without a store, but a
  half-working announcement system that silently forgets is worse than one
  that says it is off.
- A delivery that fails to send is not recorded as delivered, so it is
  retried on the next join rather than lost. A whisper to a player whose
  gamertag the roster cannot resolve fails closed, as `Voice.Tell` already
  does.
- The join drain runs off the packet read loop, like answering: it makes
  bridge calls and must not deafen the agent.
- An audit write that fails is logged and never blocks the command. A
  compliance record that can take the server down is a worse liability than
  a gap in the record -- but the gap is logged loudly.

## Testing

- Targeting resolution: each of the four kinds against a roster and a
  permission map, including a `permission` target that matches nobody.
- Decay: an expired announcement is never delivered; a live one is.
- Join drain: expedited bypasses the cap, normal respects it, the summary
  line appears only when something remains, and a second join delivers the
  remainder rather than repeating what was already sent.
- Idempotency: the same announcement is never delivered twice to the same
  player, including across a broadcast followed by a join.
- Flag parsing: `!now` and `!urgent` before the body are flags, the same
  words inside the body are text.
- Audit: a row per dispatch with the right outcome for each of ok, denied,
  unknown, error and rate-limited; and a failing audit write does not fail
  the command.
- Live-database tests behind `livedb`, following the existing convention.

## Rollout

The migration merges and syncs before the agent release that uses it, as
Stage 5 did. Against a database without these tables `!announce` and
`!inbox` degrade to their unconfigured replies; nothing else is affected.
