# Stage 5 — Knowledge and Waypoints

Status: approved design, not yet implemented.
Date: 2026-09-08.

## Context

Stages 1-4 are live. The agent connects as a headless Bedrock client, runs
`!` commands through a plugin registry, welcomes players using profiles
persisted in Postgres, and answers `@server` questions with a single-shot
LLM completion.

That answer path has no access to anything the server actually knows. Asked
"where is the gold farm", the model invents one. Asked "where is my base",
it cannot know — nothing stores player coordinates.

Stage 5 closes both gaps with one mechanism: the model is given read-only
tools and decides for itself which to call.

The backend supports this. The production endpoint runs vLLM 0.24.0 serving
`qwen/qwen3-coder-30b-a3b`; a probe with an OpenAI-style `tools` array
returned a real `tool_calls` response with `finish_reason: "tool_calls"`.
No backend change is required.

## Goals

- Server knowledge (rules, farm locations, FAQ) is stored, operator-curated,
  and reachable both by an explicit command and by the LLM answer path.
- Players keep private named coordinates and can ask for them in plain
  language.
- The LLM answers from stored facts rather than invention, without gaining
  any ability to change those facts.

## Non-goals

- Vector search or embeddings. Postgres full-text search over what will be
  dozens of rows is sufficient, and a second datastore is not.
- Shared or promoted waypoints. Waypoints are per-player; a shared tier is a
  later stage if anyone asks for it.
- Any write tool. See Security.
- Multi-turn conversation memory. Each `@server` question stays independent.

## Architecture

```
@server question
  -> ratelimit -> answering goroutine (new; see Concurrency)
       -> llm.Answer loop  <---> tools.Registry
                                   |- knowledge.Store  (Postgres)
                                   |- waypoints.Store  (Postgres)
                                   |- adapters.Facts / ServerInfo (existing)

!kb / !wp commands
  -> plugin.Registry -> permission check -> same two stores
```

Commands and tools reach the same stores through the same interfaces, so the
two surfaces cannot drift apart: a fact added with `!kb set` is immediately
what the model reads.

## Components

Four new packages, each with one purpose and its own tests.

`internal/knowledge` — the curated fact store.

```go
type Entry struct {
    Topic      string
    Body       string
    AuthorXUID string
    UpdatedAt  time.Time
}

type Store interface {
    Lookup(ctx context.Context, query string, limit int) ([]Entry, error)
    Get(ctx context.Context, topic string) (Entry, bool, error)
    Upsert(ctx context.Context, topic, body, authorXUID string) error
    Delete(ctx context.Context, topic string) error
    List(ctx context.Context) ([]Entry, error)
    Enabled() bool
}
```

`internal/waypoints` — per-player named coordinates.

```go
type Waypoint struct {
    Name      string
    X, Y, Z   int
    Dimension string
    UpdatedAt time.Time
}

type Store interface {
    Get(ctx context.Context, xuid, name string) (Waypoint, bool, error)
    Set(ctx context.Context, xuid string, wp Waypoint) error
    Delete(ctx context.Context, xuid, name string) error
    List(ctx context.Context, xuid string) ([]Waypoint, error)
    Enabled() bool
}
```

Both follow the pattern `internal/store` established: an interface, a
Postgres implementation, and a `Nop` that answers "nothing known" so the
agent runs identically with no database configured.

`internal/tools` — the model-facing capability registry.

```go
type Tool struct {
    Name        string
    Description string
    Schema      json.RawMessage // JSON Schema for the arguments object
    Invoke      func(ctx context.Context, args json.RawMessage, caller string) (string, error)
}

type Registry struct{ ... }
func (r *Registry) Definitions() []Definition           // what is sent to the model
func (r *Registry) Invoke(ctx, name string, args json.RawMessage, caller string) (string, error)
```

`caller` is the asking player's XUID, injected by the loop rather than
supplied by the model. This is what makes `waypoint_lookup` safe: the model
names a waypoint, never an owner, so it cannot read another player's
coordinates even if it asks to.

`internal/plugins/knowledge.go` and `internal/plugins/waypoints.go` — the
`!kb` and `!wp` commands.

`plugin.Context` gains two nil-tolerant fields, `Knowledge` and `Waypoints`,
narrowed to what plugins may touch, exactly as `Profiles` is. Nil-tolerance
is not decoration: a capability documented as never nil, and believed, once
panicked the whole event dispatcher.

## Data model

A new platform migration, `V3__minecraft_knowledge.sql`, added to the
`jdwillmsen-migrations` ConfigMap alongside V1 and V2.

```sql
CREATE TABLE IF NOT EXISTS minecraft.knowledge
(
    topic       TEXT        NOT NULL PRIMARY KEY,   -- lowercased on write
    body        TEXT        NOT NULL,
    author_xuid TEXT        REFERENCES minecraft.players (xuid) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    search      TSVECTOR    GENERATED ALWAYS AS
        (to_tsvector('english', topic || ' ' || body)) STORED
);

CREATE INDEX IF NOT EXISTS knowledge_search_idx ON minecraft.knowledge USING GIN (search);

CREATE TABLE IF NOT EXISTS minecraft.waypoints
(
    xuid       TEXT        NOT NULL REFERENCES minecraft.players (xuid) ON DELETE CASCADE,
    name       TEXT        NOT NULL,                -- lowercased on write
    x          INTEGER     NOT NULL,
    y          INTEGER     NOT NULL,
    z          INTEGER     NOT NULL,
    dimension  TEXT        NOT NULL DEFAULT 'overworld'
        CHECK (dimension IN ('overworld', 'nether', 'end')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (xuid, name)
);
```

`author_xuid` is `ON DELETE SET NULL` rather than `CASCADE`: deleting a
player should not silently delete the server rules they happened to write.
Waypoints are the opposite — they are personal data and go with the player.

No grants migration accompanies this. The V2 permissions migration set
`ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA minecraft ... TO app`
precisely so that the next migration adding a table would not reintroduce
the silent-empty-table failure V2 was written to fix. V3 is the first test
of that; verification that `app` can write to both new tables is an explicit
step in the rollout, not an assumption.

## The tool loop

`llm.Answer` becomes a bounded loop:

1. Send the question, the system prompt, and the tool definitions.
2. If the reply is text, return it.
3. If the reply is `tool_calls`, invoke each through `tools.Registry`, append
   the results as `role: "tool"` messages, and send again.
4. After 2 tool rounds, send once more with tools omitted, forcing a text
   answer.

The cap is 2 because the questions this serves ("where is the gold farm",
"where is my base", "who is on") need one lookup, occasionally two. An
uncapped loop against a chat trigger is an unbounded cost per message.

Tools exposed, all read-only:

| Tool | Backed by |
|---|---|
| `knowledge_lookup(query)` | `knowledge.Store.Lookup`, top 3 |
| `waypoint_lookup(name)` | `waypoints.Store.Get` for the caller |
| `waypoint_list()` | `waypoints.Store.List` for the caller |
| `players_online()` | existing `Facts` |
| `server_status()` | existing `ServerInfo` |
| `server_version()` | existing `ServerInfo` |
| `backup_status()` | existing `ServerInfo` |
| `player_playtime()` | `minecraft.player_playtime` view, caller only |

A tool whose backing capability is absent is not registered at all. The
model cannot call what it was never offered, which is a stronger guarantee
than refusing the call afterwards.

## Concurrency

`handleMention` currently runs synchronously inside the Bedrock packet read
loop (`cmd/agent/main.go`). One 8-second LLM timeout already stops the agent
reading packets for 8 seconds; a two-round tool loop would multiply that.
Stage 5 moves mention answering into its own goroutine.

That goroutine gets a total budget covering the whole loop, distinct from
the existing per-request timeout. The per-actor rate limiter continues to
bound how many can be in flight.

Ordering is not a concern: replies are broadcast, independent, and already
arrive whenever the backend finishes.

## Commands

`!wp`, member level, replies whispered — coordinates are personal, and
broadcasting a base location to everyone online is a griefing vector.

- `!wp` — list the caller's waypoints
- `!wp <name>` — one waypoint
- `!wp set <name> <x> <y> <z> [dimension]`
- `!wp del <name>`

`!kb`, split by level — reads visitor, writes operator:

- `!kb <topic>` / `!kb list` — visitor
- `!kb set <topic> <text>` / `!kb del <topic>` — operator

Operator-only writes keep the model's fact source trustworthy: everything
`knowledge_lookup` returns was curated by someone the server already trusts
with operator rights.

## Configuration

Chart changes required:

- `LLM_MAX_TOKENS` 96 to 192 — the current ceiling was sized for a single
  chat line and cannot hold tool-call arguments plus an answer.
- `LLM_TOTAL_TIMEOUT_MS`, new, default 20000 — the budget for one whole
  answering goroutine including every round trip. Added rather than widening
  `LLM_TIMEOUT_MS`, which must keep bounding each individual call: without a
  per-call bound, one stalled request consumes the entire loop budget.

## Failure behaviour

- No database: knowledge and waypoint tools are unregistered; `!kb` and
  `!wp` reply that the feature is not configured. Everything else is
  unaffected.
- A tool returns an error: the error text is returned to the model as the
  tool result so it can answer around the gap. It is logged, never spoken.
- The model requests an unknown tool: the loop stops and takes the plain
  answer. A malformed arguments object is treated the same way.
- The loop exhausts its budget: nothing is said, matching the existing
  convention that a backend failure is an operator's problem rather than an
  announcement in chat.

## Security

The invariant is that **the model has no write path**. It reads; it never
mutates. Every mutation goes through a `!` command, which carries a real
actor XUID and a permission check resolved from the server's own
`permissions.json`.

This matters because `@server` input is untrusted player text. With no write
tool registered, a prompt-injection attempt to rewrite the rules or move
another player's waypoint has nothing to call. Scoping `waypoint_lookup` to
the injected caller closes the read side of the same question.

## Testing

- `internal/tools`: registry dispatch, argument decoding, unknown-name and
  malformed-argument handling, absent-capability omission.
- The loop, against an `httptest` backend: no tool call; one round; two
  rounds; cap exceeded; malformed arguments; a tool erroring mid-loop.
- `internal/knowledge` and `internal/waypoints`: unit tests plus live-Postgres
  tests, following the pattern already used for the profile store.
- Command tests for permission gating, in particular that a member cannot
  reach `!kb set` and that one player cannot read another's waypoints.
- Live verification in-game after deploy: `!wp set`, `!wp`, `!kb set`, then
  `@server where is my base` and `@server where is the gold farm`.

## Rollout

1. Platform migration V3 merged and synced; confirm the `app` role can
   insert into both new tables before the agent depends on it.
2. Agent implementation, tagged release, chart bump with the configuration
   changes above.
3. Live verification as listed under Testing.

The platform migration touches a path under codeowner gating and needs a
real approval; it cannot be self-merged.
