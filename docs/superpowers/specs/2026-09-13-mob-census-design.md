# Mob census design

Date: 2026-09-13
Status: approved design, not yet implemented

## Problem

FWB has no reproducible way to answer "what mobs exist, where, and why is
nothing spawning". The question has been answered twice by hand — once for a
drowned sweep on 2026-09-10, once for a full spawn audit on 2026-09-13 — both
times with throwaway Python against an ad-hoc copy of the world. Neither run is
repeatable, neither is stored, and the second contradicted the first on entity
counts with no way to tell whether the world had changed or the method had.

The 2026-09-13 audit established what the answer needs to contain. Bedrock gates
every spawn on a regional population cap counted over 9x9 chunk regions, and
mobs in unloaded chunks keep their slot as long as they were once loaded. So a
handful of permanent mobs — piglin brutes, name-tagged pets, farm villagers —
can close a region to spawning forever. Finding that requires bucketing every
entity in the world by region and category and comparing against the cap table.
That is a computation, not an observation, and it needs to run the same way
every time.

## Goals

- One census engine, four consumers: operator report, Prometheus metrics,
  in-game command, entity-leak deltas.
- Every result carries the world timestamp it came from. A report can never
  present stale data as current.
- The census never writes to the world and never risks the running server.
- Reproducible: same world bytes in, same census out.

## Non-goals

- Live per-tick mob tracking. The census is a point-in-time snapshot.
- Reproducing the game's own cap arithmetic exactly. See "Surface and cave"
  below — the input for that is not in the save.
- Block-level scanning (beds, workstations, spawn surfaces). The iron-farm
  diagnosis wants this, but it reads chunk block palettes rather than entity
  records and is a separate piece of work.

## Approach

Census logic lives in the agent repo as `internal/census`, compiled into a
second binary `cmd/census`. The chart runs that binary as a CronJob, following
the pattern already established by the backup, version-check and
volume-recovery jobs. Results land in Postgres. The agent imports the same
package to serve metrics, the chat command and the report.

This keeps one codebase, one image and one review surface — "baked into the
server agent" in the sense that matters — without putting a 568 MB batch parse
inside the pod that answers players in chat. `cmd/evalllm` already establishes
the second-entrypoint pattern in this repo.

## Acquiring world bytes

The census reads from one of two sources behind a single interface. The engine
above never knows which it was given.

**`ArchiveSource` — scheduled runs.** Reads the newest
`/backup/fwb-<stamp>.tar.gz` written by the backup CronJob. That job already
performs Bedrock's documented snapshot protocol: `send-command save hold`, then
`save query` to get the file list and the byte length to truncate each file to,
then the copy, then `save resume`. The census becomes a pure consumer of an
already-consistent artifact: no contention with the live server, no new RBAC,
and no path by which a census bug reaches the world. Staleness is bounded by
the backup schedule and is irrelevant for population-cap analysis, which moves
over days.

**`LiveSource` — on-demand runs.** Performs the same hold/query/copy/resume
sequence against the running server, reusing the existing `backup-rbac.yaml`
exec permission. For "why is nothing spawning right now".

Reading the live world PVC directly while the server writes to it is
deliberately not an option. It is technically possible and it is how a torn
read arrives looking like a valid parse. `save hold` exists to prevent exactly
that. The manual 2026-09-13 snapshot skipped it and got a clean parse by luck,
not method.

Every `Census` record carries `TakenAt` (the world's own save timestamp) and
`SourceKind`. Consumers render both.

## Engine

`internal/census`, two passes over the LevelDB key space:

1. `digp*` keys map actor UUID to dimension. Keys are 8 bytes (overworld) or 12
   bytes with a trailing little-endian dimension int.
2. `actorprefix*` keys hold one entity each as Bedrock NBT — little-endian,
   length-prefixed UTF-8 strings.

NBT decoding uses `github.com/sandertv/gophertunnel/minecraft/nbt`, already a
direct dependency. LevelDB access adds one new direct dependency,
`github.com/df-mc/goleveldb/leveldb`, the Mojang-flavoured fork that
dragonfly's `mcdb` package sits on. Pulling all of dragonfly is not necessary
and is not proposed.

Per entity the census extracts: `identifier`, `UniqueID`, dimension, `Pos`,
`CustomName`, `CustomNameVisible`, `Persistent`, `Health`, `Age`, `Tags`,
`OwnerNew`.

Aggregation produces:

- Totals by dimension and identifier.
- Region buckets: 9x9 chunks, 144x144 blocks, keyed on `floor(x/144)`,
  `floor(z/144)`, with per-category counts.
- Clusters: union-find at a configurable radius. This is what located the
  4,147-item nether pile and the 80-villager iron farm; it earns its place.
- Named entities, in full. There are five on FWB today; storing all of them
  costs nothing.
- Persistent counts by identifier.

Category classification is table-driven with one source of truth and its own
test. Categories: monster, animal, water animal, ambient, pillager, other,
ignored. "Ignored" covers items, XP orbs, projectiles, minecarts and boats —
entities that tick but never count toward a spawn cap.

### Surface and cave

Bedrock's population cap differs for surface and cave spawns, and which one a
mob counts as is fixed when it spawns, not by where it currently is. That input
is not written to the save. The census therefore cannot reproduce the engine's
own arithmetic.

It reports the raw regional count against the applicable range instead — for
overworld monsters, 8 to 16 — and classifies each region:

- count > upper bound: certainly capped
- count > lower bound: at risk
- otherwise: headroom

This is the honest limit of what the save supports. Any single number claiming
to be "the" cap utilisation would be invented.

### Cap table

Encoded as data, per dimension and category, surface and cave:

Each cell is `surface / cave`.

| Category | Overworld | Nether | End |
|---|---|---|---|
| Monster | 8 / 16 | 0 / 16 | 10 / 8 |
| Animal | 4 / 0 | 0 / 4 | 4 / 0 |
| Water animal | 36 / 0 | 0 / 0 | 36 / 0 |
| Ambient | 0 / 2 | 0 / 0 | 0 / 2 |
| Pillager | 8 / 8 | 0 / 0 | 8 / 8 |

Global cap is 200 across all environmental spawning, not scaled by player
count. The census reports world totals against it as context; it cannot
measure the live loaded-chunk figure the game actually checks.

## Storage

Rollups, not raw entities. Four tables in the `minecraft` schema:

- `census_run` — id, taken_at, source_kind, world_stamp, entity_total,
  created_at
- `census_rollup` — run_id, dimension, identifier, category, count
- `census_region` — run_id, dimension, region_x, region_z, category, count
- `census_named` — run_id, name, identifier, dimension, x, y, z, persistent

The agent does not reshape its own schema — `internal/store/postgres.go` is
explicit that migrations belong to `jdwlabs/platform`'s `jdwillmsen-schemas`
service. **The migration PR in that repo lands before any agent code that reads
these tables.** This is a cross-repo ordering dependency, not an afterthought.

Persistence follows the existing `Store` interface and `Nop` pattern, so the
agent continues to run with the store disabled and the census degrades to
report-only.

## Consumers

**Operator report.** A renderer over a `Census` producing the 2026-09-13 audit
in reproducible form: entity totals by dimension, over-cap regions ranked,
named and persistent breakdown, top clusters. `cmd/census --report` prints it;
the agent serves the latest over its existing httpapi.

**Prometheus metrics.** The agent reads the latest run and exports gauges:
`mc_census_entities{dimension,identifier}`,
`mc_census_region_over_cap{dimension,category}`, `mc_census_named_total`,
`mc_census_persistent_total`, and `mc_census_age_seconds` so a stale census is
visible on the dashboard rather than silently believed.

**Leak deltas.** Diff of the latest run against the previous one, exported as
`mc_census_entity_delta{identifier}` and rendered as a "top growers" report
section. This is the signal that would have caught chest minecarts going 2,483
to 4,122 in three days.

**In-game command.** A plugin under `internal/plugins`, following the existing
plugin shape: a summary command and a lookup for named mobs, reading stored
census rows. Rate-limited through the existing `internal/ratelimit`, replying
through the existing voice path.

## Testing

Fixtures are constructed programmatically — tests write known NBT records into
a temporary LevelDB and assert the census over them. No trimmed real world is
committed; the repo stays small and the tests stay hermetic.

Coverage targets the logic that can be wrong in a way nobody notices:

- Category classification, including entities that must be ignored.
- Region bucketing at negative coordinates, where `floor` and integer
  truncation disagree.
- Dimension resolution for both 8-byte and 12-byte `digp` keys.
- Cap classification at the boundaries of each range.
- Name-tag extraction, including a tag containing non-ASCII text.
- Delta computation across two runs, including an identifier present in one
  run and absent from the other.

## Phasing

Each phase is its own PR.

1. Census core: source interface, both implementations, engine, classifier,
   `cmd/census`, chart CronJob, operator report.
2. Schema migration in `jdwlabs/platform`, then storage, metrics and leak
   deltas in the agent.
3. In-game command plugin.

## Parked

A live console query for mob counts is deferred. `mc-console-bridge`'s
allowlist is a closed list of six templates with no free-text fallback and no
entity query. Adding one means a new rule for `/testfor @e[type=...,r=...]`,
whose availability under this server's `allow-cheats=false` /
`cheatsEnabled=0` is unverified. That probe is a live production console action
and has not been authorised. Nothing in phases 1 to 3 depends on it.
