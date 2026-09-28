# gameops monorepo and live world map design

Date: 2026-09-28
Status: approved design, not yet implemented

This spec has two parts. Part 1 is the umbrella: why the Minecraft repos are
merging, what the live world map is, and the order the work lands in. Part 2
is the detailed design for the first sub-project, the monorepo migration. Each
map sub-project gets its own spec when its turn comes; Part 1 records only the
decisions and evidence they build on.

# Part 1 — Umbrella

## Problem

Players want to see FWB the way chunkbase shows a seed: a zoomable map with
structures, spawn, biomes and coordinates. But they want *their* world — the
terrain they explored, the bases they built — with live players and mobs moving
on it. A seed map cannot do that; it regenerates what the world generator would
produce and knows nothing about the save.

Nothing in the current stack captures live positions. The agent handles only
`Text` and `PlayerList` packets, the AFK bot only death/respawn, and the mob
census reads entity positions once a night but keeps only aggregates.

Building the map would touch the console bridge (snapshot, allowlist), the
agent (`!map` login), a new map service, a script pack and the chart, and it
needs the census LevelDB reader that is locked inside the agent's `internal/`.
That is the same cross-repo shape actor presence had, which took 7 PRs across 4
repos. So the repos merge first.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Repo | `jdwillmsen/gameops`, top level split by game, `minecraft/` first | Auth, presence and Discord code are game-agnostic; a second game reuses them |
| What moves in | agent, console bridge, AFK bot, plus the new map | Same language, same server, same chart |
| Chart | Stays in `jdw-deployments` | Keeps the GitOps split: code repo publishes images, deploy repo pins them |
| Releases | semantic-release per component, automatic on merge | Keeps today's flow; no release PRs, no new token |
| Image names | Unchanged; new map image is `minecraft-map` | Chart and tag history keep working |
| Map audience | Players, behind a login | Live positions and base locations are not public |
| Login | Pluggable providers, one session model keyed by XUID; in-game `!map` link first | No external identity provider needed to start; Microsoft, Discord or Dex plug in later |
| Visibility | Every logged-in player sees everything | Small trusted server |
| Terrain freshness | ~15 min | Builds appear within minutes; the pause per cycle stays short |
| Renderer | uNmINeD behind a renderer interface | Proven on FWB; our own renderer can replace it later without touching consumers |
| Live data | Stable Script API pack writing to BDS stdout | No experiments, so achievements stay intact |
| Snapshot | Console bridge streams it out | The world PVC is block storage mounted only by the server pod; the bridge is already in that pod |

## Probe evidence

Throwaway probes run on 2026-09-28. Nothing touched the live server.

**World copy** (backup `fwb-20260927T040108Z`, quiesced, 755 MB compressed,
736 MB on disk, read through a read-only helper pod since deleted):

- 128,032 overworld chunks spanning x −6240..11263, z −21712..7407 (sparse,
  not a solid rectangle); 18,376 nether; 5,556 end.
- LevelDB tag 57 (`HardcodedSpawners`) is present in vanilla BDS: 202
  overworld and 298 nether keys, every record `4 + 25·n` bytes (int32 count,
  then 6 int32 bbox and 1 type byte). Boxes are clipped per chunk, so one
  structure spans many records. Types seen: 1 nether fortress, 2 witch hut,
  3 ocean monument, 5 pillager outpost. Villages, strongholds and ancient
  cities are not in tag 57.
- Tag 49 (block entities): 13,149 chests, 8,447 spawners, 3,074 barrels,
  21 shulker boxes, 38 ender chests, 6 beacons.
- `level.dat` carries `RandomSeed`; `experiments` is
  `{experiments_ever_used: 0, saved_with_toggled_experiments: 0}`.

**uNmINeD CLI** (0.20.10-dev, Linux):

- Overworld full render: 152,195 chunks, 0 failures, 58.4 s, 1.39 GB peak
  memory, 69 MB of WebP tiles. Unchanged re-render: 1.1 s, every tile skipped
  via its per-tile hash files. Nether needs `--topY=100` or the bedrock roof
  hides everything.
- The default JPEG output crashes in `GetJpegEncoder` after writing 1,728
  blank tiles, and `/usr/bin/time` reports exit status 0. Automation must force
  `--imageformat=webp` and verify tiles are non-empty.
- License: closed source, free, non-commercial, no redistribution or
  modification. So it is downloaded at runtime, pinned by version and
  checksum, and never baked into a published image.

**Script API** (local BDS 1.26.51.1 in Docker, fresh world):

- A pack depending on stable `@minecraft/server` `2.10.0` runs with no
  experiments; `level.dat` still reports `experiments_ever_used=0`.
- Output reaches stdout only with `content-log-console-output-enabled=true`
  (itzg env `CONTENT_LOG_CONSOLE_OUTPUT_ENABLED=true`). Line format:
  `[<ts> INFO] [Scripting] <message>`, each followed by a blank line.
- No truncation at 32 KB, but a line over ~4 KB makes the *next* line start
  with a NUL byte. Records stay under 4 KB and the consumer strips `\x00`.
- Stable and working: `world.getAllPlayers()`, `dimension.getEntities()`,
  `entity.location`, `typeId`, `id`, `nameTag`, and the health component.
  `getEntities()` sees only loaded chunks, which is where players are.

## Map architecture

```
┌──────────────── minecraft-fwb pod ────────────────┐
│ BDS + mcmap pack ── stdout "[Scripting] MCMAP …"  │
│ console bridge sidecar                            │
│   GET  /events    SSE of stdout (exists)          │
│   GET  /snapshot  hold → manifest/files → resume  │
└────────────────────────┬──────────────────────────┘
                         │
┌────────────────────────▼──────────────────────────┐
│ mcmap (gameops/minecraft/mcmap)                    │
│   live     tail /events → latest positions → SSE   │
│   mirror   every 15 min, fetch changed files only  │
│   render   uNmINeD → WebP tiles on a PVC           │
│   extract  tag 57, block entities, level.dat       │
│   auth     Session ← Provider[]                    │
│   http     Leaflet UI, /tiles, /api, /live         │
└────────────────────────┬──────────────────────────┘
                         │ Postgres: players, waypoints, map sessions
                  HTTPRoute + TLS, session-gated
```

The snapshot is incremental because LevelDB `.ldb` table files never change
once written; only `MANIFEST-*`, `CURRENT` and the `.log` do. During
`save hold`, the bridge returns the `save query` file list with byte lengths;
mcmap requests only files it does not already have at that length, the bridge
serves them truncated to those lengths, then `save resume` runs on every path,
including errors and client disconnects. A cycle moves megabytes, not the
whole 736 MB, and the pause stays short.

## Roadmap

Each sub-project gets its own spec, plan and PRs, in this order:

0. **gameops monorepo** — Part 2 below.
1. **Map foundation** — snapshot endpoint, mirror, renderer interface with
   uNmINeD, tile serving, Leaflet UI for all three dimensions, grid,
   coordinates, shareable URL position.
2. **Login** — session model, provider interface, `!map` one-time link via
   the agent.
3. **Live layer** — script pack, NUL-safe log parsing, SSE fan-out, live
   players and filterable mobs.
4. **Data markers** — waypoints, beds and bases, named mobs, containers.
5. **Structures** — real from tag 57, predicted from the seed beyond explored
   chunks, marked as predicted.
6. **Chunkbase extras** — biomes, slime chunks, spawn, search, player trails.

1 and 2 are required before any player can use the map. 3 to 6 are
independent layers on top.

# Part 2 — gameops monorepo migration

## Goals

- One repo, `jdwillmsen/gameops`, holding the agent, console bridge and AFK
  bot with every commit of their history preserved and `git blame` intact.
- Each component still versions and releases on its own, automatically on
  merge, and publishes to the same image names on ghcr.io and docker.io.
- A PR runs CI only for the components it touches, behind one required check.
- No change to the chart or to anything running in the cluster.

## Non-goals

- Moving the Helm chart.
- Renaming images.
- Pulling in non-Minecraft projects.
- Any map code. That starts in sub-project 1, in the new repo.

## Layout

After the migration and restructure:

```
gameops/
  go.mod                    module github.com/jdwillmsen/gameops
  internal/                 game-agnostic code with more than one user
  minecraft/
    agent/                  was minecraft-server-agent
      cmd/ internal/ Dockerfile README.md docs/ eval/ scripts/
    bridge/                 was mc-console-bridge
    afkbot/                 was minecraft-afk-bot
  tools/release/            per-component semantic-release glue
  .github/workflows/
```

Code moves to a shared folder only when a second component uses it: the
census LevelDB reader moves to `minecraft/internal/leveldb` when mcmap needs
it, and presence goes to the top-level `internal/` when the restructure folds
`presenceapi` in. Go's `internal/` visibility rule enforces the boundary:
`minecraft/agent/internal` stays private to the agent.

## History merge

Each old repo is rewritten in a scratch clone with `git filter-repo`:

```
git filter-repo --to-subdirectory-filter minecraft/agent --tag-rename '':'agent-'
```

and likewise `bridge-` and `afkbot-`. The three rewritten histories are merged
into an empty `gameops` with `git merge --allow-unrelated-histories`, in one
merge commit per repo. Tags are renamed, not dropped, so `agent-v0.23.1` points
at the rewritten commit that was `v0.23.1`, and semantic-release continues each
series from there.

**Signatures.** Rewriting commits drops their GPG/SSH signatures, so the
imported history cannot pass `verify-pr-signatures` or a signed-commits
ruleset. The import therefore goes in as the bootstrap push that creates
`main` on the empty repo, before rulesets and required checks are turned on.
That is the one direct push to `main` this repo ever gets. Every commit after
it arrives by PR, signed, under the normal rules. The original signed commits
stay verifiable in the archived repos.

## Single Go module

A separate PR after the history merge, so the merge itself is a pure move and
this one is a pure restructure:

- One root `go.mod`, `module github.com/jdwillmsen/gameops`, `go 1.27`. The
  three current `go.mod` files already agree on Go 1.27 and gophertunnel
  v1.62.0, so there is no version conflict to resolve.
- Import paths are rewritten mechanically, e.g.
  `github.com/jdwillmsen/minecraft-server-agent/internal/census` becomes
  `github.com/jdwillmsen/gameops/minecraft/agent/internal/census`.
- `presenceapi` stops being a separately tagged module: its `go.mod`, the
  `replace` directive and its tag series go away, and it becomes
  `internal/presence`.
- Dockerfiles build from the repo root with the component path as an argument,
  since the module root is now the repo root.

## Releases

Each component keeps semantic-release, run once per component with its own
config in `tools/release/<component>.json`:

- `tagFormat` is `agent-v${version}` (and `bridge-`, `afkbot-`).
- A small local plugin in `tools/release/` wraps the commit analyzer and
  release notes generator and drops every commit that did not touch the
  component. A commit counts for a component if it changes a file under the
  component's directory, or under a shared path: `go.mod`, `go.sum`, the
  top-level `internal/`, or `minecraft/internal/`.
- The workflow runs the components one at a time, not in parallel, so their
  tag pushes do not race.
- The tag, stripped of its component prefix, is the image version: tag
  `agent-v0.24.0` publishes `minecraft-server-agent:0.24.0`. Registry tags
  therefore look exactly as they do today, and the chart and its Renovate
  rules need no change.

`release.yml` becomes one reusable workflow taking the component, image name
and description as inputs. Everything it does today is preserved: tag checkout
for rebuilds, ghcr.io and docker.io dual publish, OCI labels and annotations,
the PolyForm Noncommercial license label, and no `latest` tag.

Trade-off accepted: a `go.mod`/`go.sum` change releases every component, even
one that does not use the bumped dependency. Working out the real
per-component dependency graph from `go list -deps` is possible but not worth
it until spurious releases actually cause a problem.

## CI

- `ci.yml` has a `changes` job that maps changed paths to components, using
  the same rules as the release plugin, then one build/test/lint job per
  affected component.
- A final `ci-ok` job depends on all of them and fails if any failed. It is
  the only required status check, so branch protection does not change as the
  set of jobs changes.
- `codeql.yml`, `security-scan.yml` and `verify-pr-signatures.yml` run once
  for the repo.
- The AFK bot's `protocol-check.yml` keeps its job, scoped to `minecraft/`.
- One `renovate.json` groups Go dependencies, so a gophertunnel bump lands in
  one PR for every component that uses it.

## Cutover

1. Merge any open PRs in the three old repos, or move them over later.
2. Create `jdwillmsen/gameops` (PolyForm Noncommercial, like the others) and
   push the bootstrap history as `main`.
3. Turn on repo settings: rebase-only merges, ruleset on `main` (PRs only,
   signed commits, `ci-ok` required), and the `DOCKERHUB_TOKEN` secret.
4. Land the restructure PR, then the CI and release PR.
5. First monorepo releases publish new versions of all three images. Check
   that the chart rolls to them with no values change beyond the version.
6. Add a README pointer to `gameops` in each old repo and archive it. Archive,
   never delete: tags, releases and signed history stay readable.
7. Replace the local clones in `~/projects` with `~/projects/gameops`.

## Testing

- Dry run of the whole migration in a scratch directory before anything
  touches GitHub. After the history merge and again after the restructure,
  `go build ./...` and `go test ./...` must pass for each component.
- `git log --follow` on a sample file from each old repo must reach that
  file's original first commit.
- `git tag -l 'agent-v*'` must show every old agent tag, and likewise for the
  bridge and bot.
- Release glue test: a fixture repo with commits touching one component, a
  shared path, and neither, checked with semantic-release's dry-run mode.
  Only the expected components get a release, at the expected versions.
- CI path mapping test: the `changes` job's mapping table has unit cases for
  each path class.

## Risks

- **Signature loss on imported history.** Mitigated by the bootstrap push
  before rulesets, as above, and by archiving the old repos.
- **Release glue bugs cause missed or extra releases.** Mitigated by the dry-run
  fixture tests, and by chart image tags being pinned: a surprise release
  changes nothing in the cluster until the chart moves.
- **Renovate in `jdw-deployments` watches registry tags, not git tags.** Image
  tags keep the same shape, so nothing changes there. This is worth a check
  after the first release.
