# Actor presence, plan 2: presence tables migration

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `V8__minecraft_presence.sql` to the jdwillmsen-schemas migrations so that `minecraft.presence_overrides` and `minecraft.presence_status` exist in `jdwillmsen_prd`, writable by the `app` role, before plan 3's agent code needs them.

**Architecture:** One new key in the `jdwillmsen-migrations` ConfigMap in `jdwlabs/platform`. The existing ArgoCD Sync-hook Job (`flyway-jdwillmsen-prd-v1`) applies it on the next sync of `jdwillmsen-jdwillmsen-schemas`; the Job manifest needs no change. The repo has no SQL test in CI, so the test is a local script that replays every migration in the ConfigMap through the prd Job's own Flyway image against a throwaway Postgres 16 container and then exercises the new tables as `app`. Execute on a branch in a fresh worktree off `origin/main` of `jdwlabs/platform` (for example `~/worktrees/platform/feat-minecraft-presence-schema`, branch `feat/<ticket>-minecraft-presence-schema`); do not work in `~/projects/jdwlabs/platform` itself. The PR is authored by `jdwlabs-agent-bot` through the contents API, approved by jdwillmsen, merged with `--rebase`.

**Tech Stack:** PostgreSQL 16 (CNPG image `ghcr.io/cloudnative-pg/postgresql:16`), Flyway 11.2 (`flyway/flyway:11.2@sha256:6b6ce825edb0e199c31864aff792961ab5b37345d5cb0f4ee62a2ee917d98336`), Kubernetes ConfigMap, ArgoCD Sync hook, Docker, Python 3 + PyYAML (for extracting the SQL), yamllint, kubeconform, GitHub REST API, `gh`.

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md` (section "Data", rollout step 2). Contract: `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md` (section "Database").

## Global Constraints

- Schema `minecraft`, migration file name exactly `V8__minecraft_presence.sql`.
- Table and column names, types, nullability, defaults and CHECK values exactly as the contract's "Database" section: `presence_overrides(actor_id TEXT PK, state TEXT CHECK IN ('present','parked'), until TIMESTAMPTZ NULL, wake_on JSONB NULL, reason TEXT NOT NULL, set_by TEXT NOT NULL, set_at TIMESTAMPTZ NOT NULL DEFAULT now(), version BIGINT NOT NULL)` and `presence_status(actor_id TEXT PK, connected BOOLEAN NOT NULL, observed_state TEXT CHECK IN ('present','parked'), last_seen TIMESTAMPTZ NOT NULL, process_version TEXT NOT NULL DEFAULT '')`.
- States are `present` and `parked`. Nothing else.
- Migrations run as `postgres`; the workload connects as `app` and must have SELECT, INSERT, UPDATE, DELETE on both tables.
- `CREATE TABLE IF NOT EXISTS`, as every earlier migration in the file does.
- Comments explain why, never what; match V3-V7's density. No ticket IDs in the SQL, comments, commit message or PR body.
- Conventional commits, scope `jdwillmsen-schemas` (matches `8379b39`, `397a0e2`, `348ad5f`).
- Never edit an already-applied migration (V1-V7): Flyway's checksum validation fails the Job and blocks the sync.
- Never print, echo or persist the GitHub App key, JWT or installation token.

## Review Focus

1. **Default privileges actually cover the new tables.** V2 set `ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA minecraft` and prd shows `app=arwd/postgres` for tables (checked 2026-09-23 with `\ddp minecraft`), so no GRANT is written. A reviewer expecting an explicit GRANT should find the reason in the SQL comment; the replay test's "app writes" step and the post-merge `has_table_privilege` check pin it.
2. **`until` as a column name.** `UNTIL` is a non-reserved keyword in Postgres; the replay test proves the DDL parses and that an INSERT naming the column works unquoted, which is what plan 3's store will write.
3. **An edit to V1-V7 slipping into the diff.** The replay applies all eight migrations from scratch, which would not catch a checksum change against prd; Task 1 therefore checks `git diff origin/main` touches only added lines.
4. **YAML block-scalar indentation.** A mis-indented line ends the `|` block early and silently truncates or breaks the ConfigMap; the replay extracts SQL through a YAML parser, so truncation shows as a missing table, and yamllint runs as in CI.
5. **Re-running the hook.** Every ArgoCD sync re-runs `repair migrate`; the replay runs migrate twice and expects the second to be a no-op.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml` | Modify: append after line 401 (end of `V7__minecraft_auth_tokens.sql`) | New `V8__minecraft_presence.sql` key |
| `tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/flyway-migrate-prd.yaml` | None | Sync-hook Job (lines 1-106) already mounts the whole ConfigMap at `/flyway/sql` (lines 93-102) and runs `repair migrate` (lines 84-87) on every sync with `hook-delete-policy: BeforeHookCreation` (line 8), so a new key is picked up without a Job change. V3-V7 each shipped as a ConfigMap-only diff. |
| `$SCRATCH/check-migrations.sh` (executor's scratchpad, not committed) | Create | Replay test: Flyway against throwaway Postgres, then app-role checks |

---

### Task 1: V8 migration with its replay test

**Files:**
- Create (not committed): `$SCRATCH/check-migrations.sh`
- Modify: `tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml` (append after line 401)

**Interfaces:**
- Consumes: V2's default privileges (`migrations-configmap.yaml:162-165`); the prd Job's Flyway image, user, baseline and schema settings (`flyway-migrate-prd.yaml:42,63-87`).
- Produces: tables `minecraft.presence_overrides` and `minecraft.presence_status` with exactly the contract's columns; consumed by plan 3's `internal/presence` `Store`.

Set `SCRATCH` to your session scratchpad directory (never the repo) and `WT` to the worktree root for every command below.

- [ ] **Step 1: Write the replay test**

Write `$SCRATCH/check-migrations.sh`:

```bash
#!/usr/bin/env bash
# Replays every migration in a jdwillmsen-migrations ConfigMap against a
# throwaway Postgres 16 with the prd Job's Flyway image and settings, then
# checks the presence tables as the app role.
# Usage: check-migrations.sh <migrations-configmap.yaml>
set -euo pipefail
cm=$1
flyway_image=flyway/flyway:11.2@sha256:6b6ce825edb0e199c31864aff792961ab5b37345d5cb0f4ee62a2ee917d98336
work=$(mktemp -d)
net=pgcheck-$$
pg=pgcheck-db-$$
cleanup() {
  docker rm -f "$pg" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

python3 - "$cm" "$work" <<'PY'
import pathlib, sys, yaml
cm = yaml.safe_load(open(sys.argv[1]))
for name, body in cm["data"].items():
    pathlib.Path(sys.argv[2], name).write_text(body)
PY
chmod -R a+rX "$work"

docker network create "$net" >/dev/null
docker run -d --name "$pg" --network "$net" \
  -e POSTGRES_PASSWORD=check -e POSTGRES_DB=jdwillmsen_prd postgres:16 >/dev/null
# TCP, not the socket: the image's init-time server listens on the socket
# only, so a socket probe can pass before the restart into the real server.
for _ in $(seq 60); do
  docker exec "$pg" pg_isready -h 127.0.0.1 -U postgres -d jdwillmsen_prd >/dev/null 2>&1 && break
  sleep 1
done
docker exec "$pg" psql -h 127.0.0.1 -U postgres -d jdwillmsen_prd -v ON_ERROR_STOP=1 -qc \
  "CREATE ROLE app LOGIN PASSWORD 'check'"

migrate() {
  docker run --rm --network "$net" -v "$work":/flyway/sql:ro "$flyway_image" \
    -url="jdbc:postgresql://$pg:5432/jdwillmsen_prd" -user=postgres -password=check \
    -baselineOnMigrate=true -baselineVersion=0 -defaultSchema=public -schemas=public,minecraft \
    -locations=filesystem:/flyway/sql repair migrate 2>&1 | grep -E 'Successfully applied|up to date|ERROR'
}
echo "== first migrate"; migrate
echo "== second migrate"; migrate

as_app() {
  docker exec -e PGPASSWORD=check "$pg" psql -h 127.0.0.1 -U app -d jdwillmsen_prd \
    -v ON_ERROR_STOP=1 -Atc "$1"
}
echo "== columns"
as_app "SELECT table_name, column_name, data_type, is_nullable, coalesce(column_default, '')
        FROM information_schema.columns
        WHERE table_schema = 'minecraft' AND table_name LIKE 'presence_%'
        ORDER BY table_name, ordinal_position"
echo "== app writes"
as_app "INSERT INTO minecraft.presence_overrides (actor_id, state, until, wake_on, reason, set_by, version)
        VALUES ('afk-bot-1', 'parked', now() + interval '1 hour', '{\"any_player_join\": true}', 'check', 'api:check', 1);
        UPDATE minecraft.presence_overrides SET version = version + 1 WHERE actor_id = 'afk-bot-1' AND version = 1;
        DELETE FROM minecraft.presence_overrides WHERE actor_id = 'afk-bot-1';
        INSERT INTO minecraft.presence_status (actor_id, connected, observed_state, last_seen)
        VALUES ('afk-bot-1', true, 'present', now())
        ON CONFLICT (actor_id) DO UPDATE SET connected = excluded.connected;
        SELECT 'ok'"
echo "== check constraints"
for stmt in \
  "INSERT INTO minecraft.presence_overrides (actor_id, state, reason, set_by, version) VALUES ('agent', 'away', 'x', 'x', 1)" \
  "INSERT INTO minecraft.presence_status (actor_id, connected, observed_state, last_seen) VALUES ('agent', true, 'away', now())"; do
  if as_app "$stmt" >/dev/null 2>&1; then echo "accepted: $stmt"; exit 1; fi
  echo "rejected"
done
```

Then `chmod +x "$SCRATCH/check-migrations.sh"`.

- [ ] **Step 2: Run it against the unchanged ConfigMap to verify it fails**

Run: `"$SCRATCH/check-migrations.sh" "$WT/tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml"; echo "exit=$?"`

Expected (execution times vary):

```
== first migrate
Successfully applied 7 migrations to schema "public", now at version v7 (execution time ...)
== second migrate
Schema "public" is up to date. No migration necessary.
== columns
== app writes
ERROR:  relation "minecraft.presence_overrides" does not exist
LINE 1: INSERT INTO minecraft.presence_overrides (actor_id, state, u...
                    ^
exit=1
```

- [ ] **Step 3: Append the V8 migration**

Append to the end of `migrations-configmap.yaml` (after line 401, keeping one blank line before the key, exactly as between V6 and V7):

```yaml

  V8__minecraft_presence.sql: |
    -- Runtime deviations from each actor's presence default.
    --
    -- The default lives in Helm values and git stays the source of truth for
    -- it, so this table holds only the exceptions: one row per actor at most,
    -- and no row means the default applies. Storing every actor's desired
    -- state instead would give git and the database two answers to the same
    -- question, and clearing an override would have to know what to restore.
    --
    -- actor_id is the registry slug (agent, afk-bot-1), not an XUID, and not a
    -- foreign key to minecraft.players: the agent's own account is not a
    -- tracked player, and an actor must be parkable before it has ever joined.
    --
    -- version exists only to reject a write based on a stale read. It is not
    -- a history -- a removed override takes its count with it, and the next
    -- one starts again at 1 -- because every change is already recorded in
    -- the audit log with its source and reason.
    CREATE TABLE IF NOT EXISTS minecraft.presence_overrides
    (
        actor_id  TEXT        NOT NULL PRIMARY KEY,
        state     TEXT        NOT NULL CHECK (state IN ('present', 'parked')),
        -- Null means until someone clears it, which is what the parked-long
        -- alert watches for.
        until     TIMESTAMPTZ,
        wake_on   JSONB,
        reason    TEXT        NOT NULL,
        set_by    TEXT        NOT NULL,
        set_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
        version   BIGINT      NOT NULL
    );

    -- What each actor last reported about itself.
    --
    -- A separate table because it has a different writer and lifetime than an
    -- override: the actor overwrites its own row on every poll, whether or not
    -- anyone has overridden it, and the row outlives every override. Comparing
    -- observed_state here with the effective state is how the alerts tell a
    -- deliberate park from an actor that fell over.
    --
    -- Both tables fall under V2's default privileges for app, as every table
    -- since V3 has.
    CREATE TABLE IF NOT EXISTS minecraft.presence_status
    (
        actor_id        TEXT        NOT NULL PRIMARY KEY,
        connected       BOOLEAN     NOT NULL,
        observed_state  TEXT        NOT NULL CHECK (observed_state IN ('present', 'parked')),
        last_seen       TIMESTAMPTZ NOT NULL,
        process_version TEXT        NOT NULL DEFAULT ''
    );
```

- [ ] **Step 4: Run the replay test to verify it passes**

Run: `"$SCRATCH/check-migrations.sh" "$WT/tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml"; echo "exit=$?"`

Expected:

```
== first migrate
Successfully applied 8 migrations to schema "public", now at version v8 (execution time ...)
== second migrate
Schema "public" is up to date. No migration necessary.
== columns
presence_overrides|actor_id|text|NO|
presence_overrides|state|text|NO|
presence_overrides|until|timestamp with time zone|YES|
presence_overrides|wake_on|jsonb|YES|
presence_overrides|reason|text|NO|
presence_overrides|set_by|text|NO|
presence_overrides|set_at|timestamp with time zone|NO|now()
presence_overrides|version|bigint|NO|
presence_status|actor_id|text|NO|
presence_status|connected|boolean|NO|
presence_status|observed_state|text|NO|
presence_status|last_seen|timestamp with time zone|NO|
presence_status|process_version|text|NO|''::text
== app writes
INSERT 0 1
UPDATE 1
DELETE 1
INSERT 0 1
ok
== check constraints
rejected
rejected
exit=0
```

- [ ] **Step 5: Run the repo's own CI gates for this path**

Run, from `$WT`:

```bash
yamllint -c .yamllint.yml tenants/jdwillmsen/services/jdwillmsen-schemas/ && echo yamllint-ok
kubeconform -ignore-missing-schemas -summary tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/*.yaml
python3 tools/check-orphaned-manifests.py && echo orphans-ok
git diff origin/main --numstat
```

Expected: `yamllint-ok`; `Summary: 2 resources found in 2 files - Valid: 2, Invalid: 0, Errors: 0, Skipped: 0`; `orphans-ok`; and exactly one numstat line whose deleted-lines column is `0`:

```
50	0	tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml
```

(The added count is the block from Step 3 including its leading blank line; any non-zero second column means an applied migration was touched — revert it.)

- [ ] **Step 6: Commit locally**

The local commit records the reviewed change; Task 2 publishes the same file content through the contents API, which is what GitHub signs.

```bash
git -C "$WT" add tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml
git -C "$WT" commit -F - <<'MSG'
feat(jdwillmsen-schemas): add actor presence tables

Actors (the server agent and the AFK bots) can be parked and restored at
runtime. presence_overrides holds only the deviations from each actor's
Helm default, one row per actor, so clearing an override always falls
back to what git says. version is a compare-and-set token for
concurrent edits, not a history. presence_status is what each actor last
reported, which the alerts compare against the desired state so a
deliberate park no longer pages.

Both tables fall under V2's default privileges for the app role, as V3
through V7 did.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
MSG
```

Do not override `user.email` (the repo's signing identity is already configured). Do not push this commit.

---

### Task 2: Ship as a jdwlabs-agent-bot pull request

**Files:**
- Create (not committed): `$SCRATCH/pr-body.md`, `$SCRATCH/commit-msg.txt`
- Remote only: branch `feat/<ticket>-minecraft-presence-schema` on `jdwlabs/platform`

**Interfaces:**
- Consumes: the committed file from Task 1; Kubernetes secret `ai-sre/ai-sre-relay` keys `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY`.
- Produces: a merged PR on `jdwlabs/platform` main containing `V8__minecraft_presence.sql`; the merge commit SHA used by Task 3.

Why a bot: the `Baseline` ruleset requires one approval on every PR and GitHub forbids self-approval, so a jdwillmsen-authored PR here cannot be approved. The path is not in CODEOWNERS, so the Change Class gate does not apply, but Baseline still does. A locally signed commit under the bot is rejected by `Verify PR Signatures` (`reason: no_user`), so the commit is made through the contents API, which GitHub signs.

- [ ] **Step 1: Write the commit message and PR body**

```bash
git -C "$WT" log -1 --format=%B > "$SCRATCH/commit-msg.txt"
cat > "$SCRATCH/pr-body.md" <<'EOF'
## What

Adds `V8__minecraft_presence.sql` to the jdwillmsen-migrations ConfigMap:
`minecraft.presence_overrides` (runtime deviations from each actor's Helm
default, one row per actor, `version` for optimistic concurrency) and
`minecraft.presence_status` (what each actor last reported).

## Why

Step 2 of the actor presence rollout. The agent's presence store (next step)
reads and writes these tables; nothing uses them until it ships, so this is
safe to merge alone.

## Notes for review

- No GRANT: V2's default privileges give `app` `arwd` on tables postgres
  creates in `minecraft` (prd `\ddp minecraft` shows `app=arwd/postgres`),
  the same reason V3-V7 carry none.
- The Flyway Job is unchanged: it mounts the whole ConfigMap and runs as a
  Sync hook with BeforeHookCreation, so the new key applies on the next sync.
- Only added lines; V1-V7 are untouched, so Flyway checksum validation holds.

## Evidence

Local replay of all eight migrations through the prd Job's Flyway image
(`flyway/flyway:11.2@sha256:6b6c...`) against `postgres:16`, then as `app`:
insert/update/delete on both tables succeed, `state='away'` and
`observed_state='away'` are rejected, and a second `migrate` is a no-op.
yamllint, kubeconform and the orphaned-manifest check pass.

## Post-merge

ArgoCD syncs `jdwillmsen-jdwillmsen-schemas`; verify `flyway_schema_history`
shows version 8 and `\d minecraft.presence_overrides` on the CNPG primary.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
```

Paste the actual Step 4 and Step 5 output of Task 1 into the Evidence section before continuing.

- [ ] **Step 2: Mint the installation token, create the branch and commit through the API, open the PR**

Run this as one script (save it as `$SCRATCH/ship.sh`, run with `bash "$SCRATCH/ship.sh"`). It never echoes a credential; everything sensitive lives in `/dev/shm` and is shredded on exit.

```bash
#!/usr/bin/env bash
set -euo pipefail
umask 077
repo=jdwlabs/platform
branch=feat/<ticket>-minecraft-presence-schema
path=tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml
sec=$(mktemp -d /dev/shm/ship.XXXXXX)
trap 'shred -u "$sec"/* 2>/dev/null; rmdir "$sec"' EXIT

secret() { kubectl -n ai-sre get secret ai-sre-relay -o go-template="{{index .data \"$1\"}}" | base64 -d; }
secret GITHUB_APP_PRIVATE_KEY > "$sec/key.pem"
app_id=$(secret GITHUB_APP_ID)
inst_id=$(secret GITHUB_APP_INSTALLATION_ID)

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
now=$(date +%s)
hdr=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
pl=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now - 60)) $((now + 540)) "$app_id" | b64url)
sig=$(printf '%s.%s' "$hdr" "$pl" | openssl dgst -sha256 -sign "$sec/key.pem" -binary | b64url)
token=$(curl -fsS -X POST -H "Authorization: Bearer $hdr.$pl.$sig" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/app/installations/$inst_id/access_tokens" | jq -r .token)
export GH_TOKEN=$token

base_sha=$(gh api "repos/$repo/git/ref/heads/main" --jq .object.sha)
gh api -X POST "repos/$repo/git/refs" -f ref="refs/heads/$branch" -f sha="$base_sha" --silent
blob_sha=$(gh api "repos/$repo/contents/$path?ref=$branch" --jq .sha)
jq -n --rawfile message "$SCRATCH/commit-msg.txt" \
      --arg content "$(base64 -w0 "$WT/$path")" \
      --arg branch "$branch" --arg sha "$blob_sha" \
      '{message: $message, content: $content, branch: $branch, sha: $sha}' > "$sec/put.json"
gh api -X PUT "repos/$repo/contents/$path" --input "$sec/put.json" \
  --jq '.commit | "\(.sha) verified=\(.verification.verified)"'
gh pr create --repo "$repo" --base main --head "$branch" \
  --title "feat(jdwillmsen-schemas): add actor presence tables" \
  --body-file "$SCRATCH/pr-body.md"
```

Expected output: one line `<40-hex sha> verified=true`, then the PR URL `https://github.com/jdwlabs/platform/pull/<n>`. `SCRATCH` and `WT` must be exported before running. If the branch already exists (a retry), replace the `POST git/refs` line with `gh api -X PATCH "repos/$repo/git/refs/heads/$branch" -f sha="$base_sha" -F force=true --silent`; that auto-closes an open PR on the branch, so follow it with `gh pr reopen <n> --repo jdwlabs/platform` (under the bot token) instead of `gh pr create`.

- [ ] **Step 3: Wait for checks and confirm the PR shape**

Run (as jdwillmsen's own `gh`, no `GH_TOKEN`):

```bash
gh pr view <n> --repo jdwlabs/platform --json author,files,commits \
  --jq '{author: .author.login, files: [.files[].path], commits: [.commits[].messageHeadline]}'
gh pr checks <n> --repo jdwlabs/platform --watch
```

Expected: `author` is `app/jdwlabs-agent-bot`, `files` is the single ConfigMap path, one commit `feat(jdwillmsen-schemas): add actor presence tables`; every check passes, including `co-author-check`, `Verify PR Signatures`, `yamllint` and `kubeconform`. Also confirm no open code-scanning alert on the PR:

```bash
gh api "repos/jdwlabs/platform/code-scanning/alerts?ref=refs/pull/<n>/head&state=open" --jq length
```

Expected: `0`.

- [ ] **Step 4: Human approval, then rebase merge**

Stop and ask jdwillmsen to review the diff line by line and approve PR `<n>` (the bot cannot approve its own PR; there is no agent-side route around this). After `gh pr view <n> --repo jdwlabs/platform --json reviewDecision --jq .reviewDecision` prints `APPROVED` and all checks are green:

```bash
gh pr merge <n> --repo jdwlabs/platform --rebase --delete-branch
merged_sha=$(gh pr view <n> --repo jdwlabs/platform --json mergeCommit --jq .mergeCommit.oid)
echo "$merged_sha"
```

Expected: a 40-hex SHA. Never `--squash` (disabled in this repo) and never `--admin`.

---

### Task 3: Post-merge verification in prd (read-only)

**Files:** none.

**Interfaces:**
- Consumes: `merged_sha` from Task 2 Step 4.
- Produces: evidence that the tables exist in `jdwillmsen_prd` and `app` can write them; unblocks plan 3.

- [ ] **Step 1: Confirm ArgoCD synced the merge**

```bash
kubectl -n argocd get application jdwillmsen-jdwillmsen-schemas \
  -o jsonpath='{.status.sync.revisions[0]} {.status.sync.status} {.status.health.status} {.status.operationState.phase}{"\n"}'
```

Expected: `<merged_sha> Synced Healthy Succeeded`. Auto-sync is on (`automated: {prune: true, selfHeal: true}`), so this normally lands within the ArgoCD poll interval; if the revision is still the old one after 5 minutes, check `kubectl -n argocd get application jdwillmsen-jdwillmsen-schemas -o jsonpath='{.status.conditions}'` rather than forcing a sync.

- [ ] **Step 2: Confirm Flyway applied V8**

```bash
pg=$(kubectl -n database get pods -l cnpg.io/cluster=platform-postgresql-cluster-prd,cnpg.io/instanceRole=primary -o jsonpath='{.items[0].metadata.name}')
kubectl -n database exec "$pg" -c postgres -- psql -d jdwillmsen_prd -Atc \
  "SELECT version, description, success FROM public.flyway_schema_history ORDER BY installed_rank DESC LIMIT 2"
```

Expected:

```
8|minecraft presence|t
7|minecraft auth tokens|t
```

(The Job itself is deleted 300s after it finishes, `ttlSecondsAfterFinished: 300`, so the history table is the durable evidence. If the row is missing or `f`, read `kubectl -n database logs job/flyway-jdwillmsen-prd-v1` while it still exists.)

- [ ] **Step 3: Inspect the tables and the app role's privileges**

```bash
kubectl -n database exec "$pg" -c postgres -- psql -d jdwillmsen_prd -c '\d minecraft.presence_overrides' -c '\d minecraft.presence_status'
kubectl -n database exec "$pg" -c postgres -- psql -d jdwillmsen_prd -Atc \
  "SELECT t, has_table_privilege('app', 'minecraft.' || t, 'SELECT,INSERT,UPDATE,DELETE')
   FROM unnest(ARRAY['presence_overrides', 'presence_status']) AS t"
```

Expected: `\d` shows the eight and five columns from Task 1 Step 4 with `"presence_overrides_pkey" PRIMARY KEY, btree (actor_id)`, `"presence_status_pkey" PRIMARY KEY, btree (actor_id)` and the two `CHECK` constraints listing `'present'` and `'parked'`; the privilege query prints

```
presence_overrides|t
presence_status|t
```

Nothing in this task writes to prd.

- [ ] **Step 4: Tidy up**

```bash
git -C ~/projects/jdwlabs/platform worktree remove "$WT"
git -C ~/projects/jdwlabs/platform branch -D feat/<ticket>-minecraft-presence-schema
git -C ~/projects/jdwlabs/platform fetch origin && git -C ~/projects/jdwlabs/platform diff origin/main --stat -- tenants/jdwillmsen/services/jdwillmsen-schemas/
```

The local branch holds the unpushed local commit from Task 1, which the API commit superseded, so `-D` is expected. Record the PR URL, merged SHA and Step 2/3 output on the Jira ticket.

---

## Self-review

- Spec coverage: "Data" (two tables in `minecraft`, via jdwillmsen-schemas) is Task 1; rollout step 2 ("safe to ship alone") is Tasks 2-3. The audit-log half of "Data" belongs to plan 3 (it uses the existing `command_audit`-style logging in the agent) and is out of scope here.
- Placeholders: `<n>` and `<merged_sha>` are runtime values produced by earlier steps, not unfilled content.
- Consistency: table, column and file names match the contract verbatim; branch name identical in Tasks 2 and 3.
- Review Focus: items 1, 2, 4, 5 are pinned by the replay test (Task 1 Steps 2/4) and Task 3 Step 3; item 3 by Task 1 Step 5's numstat check.

## Deviations from spec/contract

1. **No change to `flyway-migrate-prd.yaml`.** The dispatch named it as a file to touch. The Job mounts the entire ConfigMap and re-runs as an ArgoCD Sync hook with `BeforeHookCreation` on every sync; V3-V7 (`348ad5f`, `a29f79e`, `397a0e2`, `8379b39`) each shipped as ConfigMap-only diffs with the Job name still `flyway-jdwillmsen-prd-v1`.
2. **No explicit GRANT to `app`.** The dispatch asked for grants "as prior migrations do"; none after V2 do. V2 set default privileges for tables postgres creates in `minecraft` (confirmed in prd: `app=arwd/postgres`), and V3-V7 rely on that and say so. The migration's comment states it; Task 3 verifies it.
3. **DDL formatting only.** The contract's DDL is reformatted to the file's style (opening paren on its own line, `('present', 'parked')` spacing) and drops the explicit `NULL` on `until`/`wake_on` (columns are nullable by default, and V1-V7 never write `NULL`). Names, types, nullability, defaults and CHECK values are unchanged.
4. **No SQL test in CI.** `jdwlabs/platform` validates YAML (yamllint, kubeconform, orphaned-manifest check) but never executes migrations, so the test is a local Docker replay script kept in the executor's scratchpad rather than added to the repo; adding a CI job would be a `.github/` change (CODEOWNERS-gated) outside this phase.
