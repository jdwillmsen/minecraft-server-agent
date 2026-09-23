# Actor presence, phase 6: chart and CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Render the actor list, presence tokens and presence variables for the agent, both AFK bots and the console bridge from one Helm values list; give `tools/mc` a `presence ls|park|unpark` client; document parking; then switch it on in production and prove it live.

**Architecture:** Work in the `jdw-deployments` repo (`github.com/jdwillmsen/jdw-deployments`), on a branch in a fresh worktree off `origin/main` (for example `feat/<ticket>-actor-presence-chart`, made with `gwta` or `EnterWorktree` at execution time; this plan does not create it). The feature ships in **two PRs**. PR A (Tasks 1-7) adds `global.actors`, the presence ExternalSecret, the presence consumers gated off behind `global.presence.enabled: false`, the CLI and the runbook, and renders byte-identical to `origin/main` apart from one new, inert ExternalSecret, so nothing restarts. Between the two PRs a human puts the tokens into Vault (Task 8). PR B (Task 9) bumps the agent, bridge and bot images to the releases from plans 3-5, adds the bridge's `BRIDGE_KICKABLE`, and sets `global.presence.enabled: true`. That sync restarts the server once. Task 10 verifies it live.

**Tech Stack:** Helm 3 (`helm lint`, `helm template`), Go templates with Sprig, External Secrets Operator v2.10 (`external-secrets.io/v1`, `target.template` engine v2) against the `vault` ClusterSecretStore, bash with `kubectl`, `curl` and `jq`, python3 + PyYAML for the rendered-chart test suites, shellcheck, and `tools/ci-local`, which mirrors `.github/workflows/ci.yml`.

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md` in minecraft-server-agent (sections "Domain model / Actor", "CLI", "Operations / Runbook", "Rollout" step 6). Cross-repo names come from `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md`, which is authoritative.

## Global Constraints

- Actor ids `agent`, `afk-bot-1`, `afk-bot-2`, matching `^[a-z0-9][a-z0-9-]{0,62}$`. Kinds `agent`, `afk-bot`. Group `bots`. `all` is implicit and never listed. States `present`, `parked`, nothing else.
- Env var names are verbatim from the contract: `PRESENCE_ACTORS` (JSON `[{"id","gamertag","kind","groups":[...],"default_state"}]`), `PRESENCE_TOKENS` (JSON `[{"name","token","scopes":[...],"actor":"<id, optional>"}]`), `PRESENCE_SELF_ID`, `PRESENCE_URL`, `PRESENCE_TOKEN`, `PRESENCE_ACTOR_ID`, `PRESENCE_DEFAULT`, `BRIDGE_KICKABLE` (comma-separated gamertags).
- `PRESENCE_TOKENS` entries: one per bot, named after its actor (`afk-bot-1`, `afk-bot-2`), scopes `presence:read` + `presence:report`, `"actor"` bound to that id. One operator token named `tools-mc`, scopes `presence:read` + `presence:write`, no actor. The CLI is a plain API client, so its writes are recorded as `set_by: api:tools-mc` (the contract has no `cli:` prefix).
- `BRIDGE_KICKABLE` lists gamertags as they are: comma-separated, unquoted, and none may contain `,`, `"`, `\`, a leading `@` or surrounding spaces, because the bridge refuses to start on those. The agent quotes gamertags that contain spaces when it sends `kick`.
- API routes are the contract's: `GET /v1/actors`, `PUT|DELETE /v1/actors/{id}/presence`, `PUT /v1/groups/{group}/presence`, auth `Authorization: Bearer <token>`. Durations are Go syntax (`30m`, `2h`). `reason` is required from the CLI.
- Existing Deployment and PVC names stay byte-identical: `<release>-afk-bot`, `<release>-afk-bot-2`, `<release>-server-agent`. The token caches are filed under them.
- No agent-run command mints, prints or exchanges a credential (`~/AGENTS.md`). Token values are generated and written to Vault only in a human's own terminal (Task 8). The CLI reads the operator token into a variable and never prints it or passes it on a command line.
- Image tags are set to the version each upstream plan released, read at execution time from `gh release list -R jdwillmsen/<repo> -L 1`. Never a guessed number, never a placeholder.
- The CLI follows AXI: TOON on stdout, errors on stdout as `error:` plus `hint:` (the existing `tools/mc` shape), exit 0 success including no-ops, 1 error, 2 usage error, no prompts, unknown flags rejected by name.
- Comments explain why, never what. No ticket IDs in code, comments, docs or commit messages. Conventional commits, each ending with the `Co-Authored-By` line the session's attribution reminder gives.
- `argocd/prd/config.yaml` syncs with `selfHeal: true`, so every change reaches production through a merged PR, never `kubectl scale` or `kubectl edit`.

## Review Focus

1. **The agent parks itself and the bots can no longer reach it.** Today `/readyz` answers 503 for a live agent without a Bedrock session (`internal/httpapi/http.go:80-92` in the agent repo), and the only agent Service (`agent-metrics.yaml`) carries ready pods only. A parked agent with one replica would leave the bots with no endpoint, still acting on "parked" after an unpark. Task 3 adds a dedicated Service with `publishNotReadyAddresses: true` and pins it in the test; Task 10 step 6 parks the agent and checks the Service still has an endpoint.
2. **Editing an actor's default fires a false "server restarting" countdown.** `global.*` is merged into the subchart's values, which `deployAnnounce.serverSpecHash` hashes, so any edit to `global.actors` would move that hash even though the StatefulSet does not change. Task 3 narrows the hash to what reaches the pod, and its test flips a default and asserts the server hash does not move.
3. **A gamertag the bridge cannot take.** A comma, quote, backslash or leading `@` would either split one name into two or stop the bridge from starting at all, and that bridge is the server pod's console path. A gamertag shared by two actors would make one park kick the other. Task 1 rejects all of these at render time.
4. **An operator token printed to a transcript.** `tools/mc presence` handles the token. Task 4's test asserts the token never appears in stdout and never appears in curl's argv, since argv is readable from `ps`.
5. **A partial group unpark.** There is no group DELETE route, so `unpark bots` removes each member's override one at a time. Task 5's test fails the second DELETE and asserts that the output names what was already unparked and that the exit code is 1.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `charts/minecraft-fwb/values.yaml` | modify | `global.actors` and `global.presence` (Tasks 1-2); bots' documentation-only `gamertag` fields replaced by a pointer (Task 1); bridge `BRIDGE_KICKABLE` and image tags (Task 9) |
| `charts/minecraft-fwb/templates/_actors.tpl` | create | Actor validation, `PRESENCE_ACTORS` JSON, kick list, lookups; presence secret/Service names |
| `charts/minecraft-fwb/templates/actors-validate.yaml` | create | Renders nothing. It exists so a bad actor list fails every render |
| `charts/minecraft-fwb/templates/presence-externalsecret.yaml` | create | Vault → `<release>-presence` Secret: `presence_tokens` JSON plus one key per bot, plus the operator token |
| `charts/minecraft-fwb/templates/presence-service.yaml` | create | `<release>-server-agent-presence`, publishing not-ready agent pods |
| `charts/minecraft-fwb/templates/agent-deployment.yaml` | modify | `PRESENCE_ACTORS`, `PRESENCE_SELF_ID`, `PRESENCE_TOKENS` after the `ANNOUNCE_API_TOKEN` block (lines 130-136) |
| `charts/minecraft-fwb/templates/bot-deployment.yaml`, `bot2-deployment.yaml` | modify | `PRESENCE_URL`, `PRESENCE_ACTOR_ID`, `PRESENCE_DEFAULT`, `PRESENCE_TOKEN` after `MC_VIEW_DISTANCE` |
| `charts/minecraft-fwb/templates/_helpers.tpl` | modify | `deployAnnounce.serverSpecHash` / `agentSpecHash` (lines 90-123) see only what reaches each workload |
| `tools/tests/test-actors.sh` | create | Rendered-chart suite: validation, names, secret, consumers, digests, kick list |
| `tools/mc` | modify | `presence ls|park|unpark` |
| `tools/tests/test-mc-presence.sh` | create | CLI suite against fake `kubectl` and `curl` |
| `README.md` | modify | New "Parking actors" runbook; "Relocating a bot" points at it |

`.github/workflows/ci.yml` needs no edit: the `tools-tests` job discovers `tools/tests/test-*.sh` and shellchecks `tools/mc tools/ci-local tools/tests/*.sh`, so both new suites are picked up automatically.

Common render command used below (the release name and namespace are the production ones, so rendered names can be compared literally):

```bash
render() {
  helm template jdwillmsen-minecraft-fwb-prd charts/minecraft-fwb -n jdwillmsen-prd \
    -f charts/minecraft-fwb/values.yaml -f charts/minecraft-fwb/values-prd.yaml \
    -f charts/minecraft-fwb/values-console-bridge.yaml "$@"
}
```

---
### Task 1: One actor list, validated at render time

**Files:**
- Create: `charts/minecraft-fwb/templates/_actors.tpl`
- Create: `charts/minecraft-fwb/templates/actors-validate.yaml`
- Modify: `charts/minecraft-fwb/values.yaml:7-35` (the `global:` block; `actors` goes after `consoleBridge`), `:742-743` (`bot.gamertag`), `:848-857` (`bot2.gamertag`)
- Test: `tools/tests/test-actors.sh`

**Interfaces:**
- Consumes: nothing from earlier tasks. Existing helpers `bot.name`, `bot2.name`, `agent.name` (`_helpers.tpl:33-39,69-71`).
- Produces (named templates, all in `_actors.tpl`):
  - `actors.validate` (context `.`): renders `""` or calls `fail`.
  - `actors.json` (context `.`): the `PRESENCE_ACTORS` string, keys `id, gamertag, kind, groups, default_state`.
  - `actors.kickable` (context `.`): `"JDWServerAgent,LightBlaz3,Dotablaze7321"`, in list order.
  - `actors.agent` (context `.`): the agent's entry as JSON; use `| fromJson`.
  - `actors.forValuesKey` (context `dict "root" $ "key" "bot"`): that bot's entry as JSON; use `| fromJson`.
  - `actors.botKeys`: JSON list `["bot","bot2"]`.
  - Values: `global.actors[]` with `id`, `kind`, `gamertag`, `defaultState`, `groups`, and `valuesKey` for an `afk-bot`.
  - `tools/tests/test-actors.sh` with helpers `render`, `expect_refused <label> <needle> <override-yaml>`, `fail`, and `$work/default.yaml` (the default render), which later tasks append to.

Why `global:`: the console-bridge sidecar is a string under `minecraft-bedrock.sidecarContainers` (`values.yaml:253`), `tpl`-rendered in the vendored subchart's scope, and only `.Values.global.*` crosses into it (`values.yaml:1-6`, `280-284`). The bridge needs the gamertags, so the list lives there. Named templates are shared across parent and subchart, so the sidecar can `include "actors.kickable" .` (Task 9). This was checked by rendering.

Why not generate the bot Deployments from the list: `README.md` ("The AFK bot(s)") explains why `bot` and `bot2` are separate blocks. A generated list would rename `bot`'s unsuffixed resources and orphan its signed-in token cache. Instead each `afk-bot` actor names its values block through `valuesKey`, and the render fails if an enabled bot block has no actor. That gives the spec's "a bot cannot exist without being registered" as a render-time check.

- [ ] **Step 1: Write the failing test**

Create `tools/tests/test-actors.sh`:

```bash
#!/usr/bin/env bash
# Pins what the chart derives from global.actors.
#
# One list feeds three processes that never talk about it to each other: the
# agent's registry, each bot's own identity and the bridge's kick allowlist. A
# drift between them is silent -- a bot the agent does not know is one nobody
# can park, and a gamertag missing from the bridge's list is a park that never
# unloads its chunks. So the three are read back from the rendered objects and
# compared, rather than trusted to the helper that built them.
set -euo pipefail

here="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$here/charts/minecraft-fwb"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $1"; exit 1; }

render() {
  helm template jdwillmsen-minecraft-fwb-prd "$chart" -n jdwillmsen-prd \
    -f "$chart/values.yaml" -f "$chart/values-prd.yaml" \
    -f "$chart/values-console-bridge.yaml" "$@"
}

# Renders with an override file and expects the render to fail naming $2.
expect_refused() {
  local label="$1" needle="$2" overrides="$3" out
  printf '%s\n' "$overrides" > "$work/override.yaml"
  if out="$(render -f "$work/override.yaml" 2>&1)"; then
    fail "$label: the render succeeded"
  fi
  grep -qF -- "$needle" <<<"$out" || fail "$label: refused, but not for the right reason: $out"
  echo "  ok: refuses $label"
}

# --- the shipped list renders ---------------------------------------------
render > "$work/default.yaml" || fail "the chart does not render with its own values"
echo "  ok: the shipped actor list renders"

# --- names the bots' token caches and Deployments are filed under ----------
# The Deployment and PVC names are what ArgoCD, the token caches and the
# README's kubectl lines all key on. Renaming one orphans a signed-in cache.
python3 - "$work/default.yaml" <<'PY'
import sys, yaml
names = {(d["kind"], d["metadata"]["name"]) for d in yaml.safe_load_all(open(sys.argv[1])) if d}
for want in [
    ("Deployment", "jdwillmsen-minecraft-fwb-prd-afk-bot"),
    ("Deployment", "jdwillmsen-minecraft-fwb-prd-afk-bot-2"),
    ("Deployment", "jdwillmsen-minecraft-fwb-prd-server-agent"),
    ("PersistentVolumeClaim", "jdwillmsen-minecraft-fwb-prd-afk-bot"),
    ("PersistentVolumeClaim", "jdwillmsen-minecraft-fwb-prd-afk-bot-2"),
]:
    assert want in names, f"{want} is no longer rendered; renaming it orphans live state"
PY
echo "  ok: Deployment and PVC names are unchanged"

# --- validation -------------------------------------------------------------
expect_refused "a malformed id" "must match" '
global:
  actors:
    - {id: Agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a duplicate id" "is listed twice" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a gamertag with a comma" "gamertag must be set, with no comma" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: "JDW,ServerAgent", defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a gamertag with a quote" "gamertag must be set, with no comma" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: "JDW\"Agent", defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a gamertag with a leading @" "gamertag must be set, with no comma" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: "@JDWServerAgent", defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a gamertag shared by two actors" "belongs to another actor" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: lightblaz3, defaultState: present, groups: [bots]}'

expect_refused "an unknown default state" "must be present or parked" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: away, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "the implicit group listed" "which every actor is already in" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: [all]}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a group named like an actor" "is also an actor id" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [agent]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "no agent" "exactly one actor of kind agent" '
global:
  actors:
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "a bot naming no values block" "valuesKey must be one of" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot3, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "two actors on one bot" "is already another actor" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}'

expect_refused "an enabled bot nobody registered" "bot2.enabled is true but no global.actors entry" '
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: present, groups: [bots]}'

echo "PASS"
```

```bash
chmod +x tools/tests/test-actors.sh
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tools/tests/test-actors.sh`
Expected: FAIL. The first two checks pass, then

```
  ok: the shipped actor list renders
  ok: Deployment and PVC names are unchanged
FAIL: a malformed id: the render succeeded
```

Nothing reads `global.actors` yet, so every invalid list renders.

- [ ] **Step 3: Add the actor list to values**

The gamertags below are the ones this file already records: `bot.gamertag` (line 743), `bot2.gamertag` (line 857), and the agent's `JDWServerAgent` (comments at lines 795 and 950). Move the bots' documentation-only copies first, while the line numbers still match. Replace lines 742-743 of `bot`:

```yaml
  # See bot2.gamertag for why this is recorded. Documentation only.
  gamertag: LightBlaz3
```

with:

```yaml
  # The gamertag this account plays as is afk-bot-1's in global.actors, which
  # the agent parks by and the bridge kicks by.
```

Replace lines 848-857 of `bot2` (the comment starting `# The gamertag this account actually appears as in game.` through `gamertag: Dotablaze7321`) with:

```yaml
  # The gamertag this account plays as is afk-bot-2's in global.actors. It was
  # recorded here first, as documentation only, because nothing else mapped a
  # Deployment to a player -- `username` is only the token-cache key and the
  # gamertag sits inside an encoded JWT. The actor list is that map now, and
  # the agent and the bridge both read it.
```

Nothing reads `bot.gamertag` or `bot2.gamertag`. Confirm with `grep -rn gamertag charts/minecraft-fwb/templates tools`, which should print nothing before this task.

Then, in `charts/minecraft-fwb/values.yaml`, insert after `global.consoleBridge.secret.create: true` (line 35) and its blank line, still inside `global:`, before `minecraft-bedrock:` (line 37):

```yaml
  # Every account that puts a player into the world. One list, read by three
  # processes: the agent's registry (PRESENCE_ACTORS), each bot's own identity
  # (PRESENCE_ACTOR_ID, PRESENCE_DEFAULT) and the bridge's kick allowlist
  # (BRIDGE_KICKABLE). Under `global:` because the bridge sidecar is
  # tpl-rendered in the vendored subchart's scope, which reaches nothing else.
  #
  # defaultState is the baseline git owns. Parking at runtime is an override
  # on top of it with an owner, a reason and usually an expiry -- see README,
  # "Parking actors". Changing it here is for a change of intent that should
  # outlive any override.
  #
  # valuesKey ties an afk-bot to the values block its Deployment renders from.
  # The render fails if an enabled bot block has no entry here, so a bot
  # cannot run unregistered.
  actors:
    - id: agent
      kind: agent
      gamertag: JDWServerAgent
      defaultState: present
      groups: []
    - id: afk-bot-1
      kind: afk-bot
      valuesKey: bot
      gamertag: LightBlaz3
      defaultState: present
      groups: [bots]
    - id: afk-bot-2
      kind: afk-bot
      valuesKey: bot2
      gamertag: Dotablaze7321
      defaultState: present
      groups: [bots]
```

- [ ] **Step 4: Write the helpers and the validating template**

Create `charts/minecraft-fwb/templates/_actors.tpl`:

```gotemplate
{{/*
The bot values blocks an afk-bot actor may name. Each has its own Deployment
template, so a third bot adds its key here in the same change that copies
bot2-deployment.yaml.
*/}}
{{- define "actors.botKeys" -}}
{{- list "bot" "bot2" | toJson -}}
{{- end -}}

{{/*
Fails the render on any actor list the agent, the bots or the bridge would
read differently. Rendered from its own template so a bad list stops every
sync, not only the ones that happen to touch the agent.
*/}}
{{- define "actors.validate" -}}
{{- $botKeys := include "actors.botKeys" . | fromJsonArray -}}
{{- $ids := dict -}}
{{- $gamertags := dict -}}
{{- $usedKeys := dict -}}
{{- $agents := 0 -}}
{{- range $i, $a := .Values.global.actors -}}
{{- $where := printf "global.actors[%d]" $i -}}
{{- if not (regexMatch "^[a-z0-9][a-z0-9-]{0,62}$" (toString $a.id)) -}}
{{- fail (printf "%s.id %q must match ^[a-z0-9][a-z0-9-]{0,62}$" $where (toString $a.id)) -}}
{{- end -}}
{{- if hasKey $ids $a.id -}}
{{- fail (printf "%s.id %q is listed twice" $where $a.id) -}}
{{- end -}}
{{- $_ := set $ids $a.id true -}}
{{- $tag := toString $a.gamertag -}}
{{- if or (not $a.gamertag) (regexMatch "[,\"\\\\]|^@" $tag) (ne $tag (trim $tag)) -}}
{{- fail (printf "%s.gamertag must be set, with no comma, quote, backslash, leading @ or surrounding space: the bridge reads BRIDGE_KICKABLE as bare comma-separated names and refuses to start on those" $where) -}}
{{- end -}}
{{- if hasKey $gamertags (lower $a.gamertag) -}}
{{- fail (printf "%s.gamertag %q belongs to another actor" $where $a.gamertag) -}}
{{- end -}}
{{- $_ := set $gamertags (lower $a.gamertag) true -}}
{{- if not (has $a.defaultState (list "present" "parked")) -}}
{{- fail (printf "%s.defaultState must be present or parked, got %q" $where (toString $a.defaultState)) -}}
{{- end -}}
{{- range $a.groups -}}
{{- if eq . "all" -}}
{{- fail (printf "%s.groups lists \"all\", which every actor is already in" $where) -}}
{{- end -}}
{{- if not (regexMatch "^[a-z0-9][a-z0-9-]{0,62}$" (toString .)) -}}
{{- fail (printf "%s.groups entry %q must match ^[a-z0-9][a-z0-9-]{0,62}$" $where (toString .)) -}}
{{- end -}}
{{- if hasKey $ids . -}}
{{- fail (printf "%s.groups entry %q is also an actor id, so a target naming it is ambiguous" $where .) -}}
{{- end -}}
{{- end -}}
{{- if eq $a.kind "agent" -}}
{{- $agents = add1 $agents -}}
{{- if $a.valuesKey -}}
{{- fail (printf "%s is the agent and takes no valuesKey" $where) -}}
{{- end -}}
{{- else if eq $a.kind "afk-bot" -}}
{{- if not (has $a.valuesKey $botKeys) -}}
{{- fail (printf "%s.valuesKey must be one of %s, got %q" $where (join ", " $botKeys) (toString $a.valuesKey)) -}}
{{- end -}}
{{- if hasKey $usedKeys $a.valuesKey -}}
{{- fail (printf "%s.valuesKey %q is already another actor's" $where $a.valuesKey) -}}
{{- end -}}
{{- $_ := set $usedKeys $a.valuesKey true -}}
{{- else -}}
{{- fail (printf "%s.kind must be agent or afk-bot, got %q" $where (toString $a.kind)) -}}
{{- end -}}
{{- end -}}
{{- if ne $agents 1 -}}
{{- fail (printf "global.actors must list exactly one actor of kind agent, found %d" $agents) -}}
{{- end -}}
{{- range $botKeys -}}
{{- if and (index $.Values . "enabled") (not (hasKey $usedKeys .)) -}}
{{- fail (printf "%s.enabled is true but no global.actors entry has valuesKey %q: an unregistered bot is one the agent can neither park nor count" . .) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
PRESENCE_ACTORS for the agent, in the contract's snake_case field names. The
values list is camelCase like the rest of this file; valuesKey is chart
plumbing and stays out.
*/}}
{{- define "actors.json" -}}
{{- $out := list -}}
{{- range .Values.global.actors -}}
{{- $out = append $out (dict "id" .id "gamertag" .gamertag "kind" .kind "groups" (default list .groups) "default_state" .defaultState) -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/*
BRIDGE_KICKABLE. Every actor, the agent included: a parked agent whose
session the server holds open is kicked like any bot.
*/}}
{{- define "actors.kickable" -}}
{{- $tags := list -}}
{{- range .Values.global.actors -}}
{{- $tags = append $tags .gamertag -}}
{{- end -}}
{{- join "," $tags -}}
{{- end -}}

{{/*
The agent's own actor entry, as JSON. Validates first: templates render in
name order, so a consumer can reach a bad list before actors-validate.yaml
does, and would otherwise fail with a nil error instead of the real one.
*/}}
{{- define "actors.agent" -}}
{{- include "actors.validate" . -}}
{{- range .Values.global.actors -}}
{{- if eq .kind "agent" -}}{{- toJson . -}}{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The actor entry for a bot values block, as JSON; call with
(dict "root" $ "key" "bot"). Validates first, for the reason above.
*/}}
{{- define "actors.forValuesKey" -}}
{{- include "actors.validate" .root -}}
{{- $key := .key -}}
{{- range .root.Values.global.actors -}}
{{- if eq (toString .valuesKey) $key -}}{{- toJson . -}}{{- end -}}
{{- end -}}
{{- end -}}
```

Create `charts/minecraft-fwb/templates/actors-validate.yaml`:

```gotemplate
{{- include "actors.validate" . -}}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `bash tools/tests/test-actors.sh && shellcheck -s bash tools/tests/test-actors.sh`
Expected:

```
  ok: the shipped actor list renders
  ok: Deployment and PVC names are unchanged
  ok: refuses a malformed id
  ok: refuses a duplicate id
  ok: refuses a gamertag with a comma
  ok: refuses a gamertag with a quote
  ok: refuses a gamertag with a leading @
  ok: refuses a gamertag shared by two actors
  ok: refuses an unknown default state
  ok: refuses the implicit group listed
  ok: refuses a group named like an actor
  ok: refuses no agent
  ok: refuses a bot naming no values block
  ok: refuses two actors on one bot
  ok: refuses an enabled bot nobody registered
PASS
```

shellcheck prints nothing.

- [ ] **Step 6: Commit**

```bash
git add charts/minecraft-fwb/values.yaml charts/minecraft-fwb/templates/_actors.tpl   charts/minecraft-fwb/templates/actors-validate.yaml tools/tests/test-actors.sh
git commit -m "feat(minecraft-fwb): declare the world's actors in one validated list"
```

---

### Task 2: The presence secret, rendered before anything reads it

**Files:**
- Create: `charts/minecraft-fwb/templates/presence-externalsecret.yaml`
- Modify: `charts/minecraft-fwb/templates/_actors.tpl` (append)
- Modify: `charts/minecraft-fwb/values.yaml` (`global.presence`, after `global.actors`)
- Test: `tools/tests/test-actors.sh` (append)

**Interfaces:**
- Consumes: `global.actors` and the `render`/`fail`/`$work/default.yaml` test scaffolding from Task 1.
- Produces:
  - `presence.tokenKey` (context: an actor id or token name string) renders `presence_token_<id with - as _>`, for example `presence_token_afk_bot_1` and `presence_token_tools_mc`.
  - `presence.secret.name` (context `.`) renders `<release>-presence`.
  - Values `global.presence.enabled` (bool, `false`), `global.presence.secret.create` (bool, `true`), `global.presence.operatorToken.name` (`tools-mc`).
  - Kubernetes Secret `<release>-presence` with keys `presence_tokens` (the `PRESENCE_TOKENS` JSON), `presence_token_afk_bot_1`, `presence_token_afk_bot_2` and `presence_token_tools_mc`.
  - Vault properties in the `minecraft-fwb` document (`kv/minecraft-fwb`): `presence_token_afk_bot_1`, `presence_token_afk_bot_2`, `presence_token_tools_mc`. Task 8 creates them.

How `ANNOUNCE_API_TOKEN` is sourced, and why this does not copy it exactly: `agent-deployment.yaml:130-136` reads `agent.announceApi.existingSecret`, which is a Secret a human creates by hand outside the chart. It is `""` in `values.yaml:1303-1305`, so that API is off in production today. Presence reuses the consumption side of that pattern: a `secretKeyRef` added only when the feature is on. It sources the Secret the way `console-secret-externalsecret.yaml` does instead: an ExternalSecret from the `vault` ClusterSecretStore, document `minecraft-fwb`, rendered ahead of any consumer, with `or create enabled` gating. There are two reasons. First, a bot's `PRESENCE_TOKEN` must be exactly the string the agent's `PRESENCE_TOKENS` binds to that bot. One Vault property rendered into both keys through ESO's `target.template` cannot drift, but two hand-typed Secret values can. Second, names, scopes and bindings then come from `global.actors`, so adding a bot never means hand-editing JSON inside a Secret. ESO v2.10.0 is what the cluster runs (`kubectl get deploy -n external-secrets`), and `engineVersion: v2` templates are supported there.

Tokens must be JSON-safe, because ESO substitutes them into a JSON string. Task 8 generates them with `openssl rand -hex 32`.

- [ ] **Step 1: Write the failing test**

In `tools/tests/test-actors.sh`, insert immediately before the final `echo "PASS"`:

```bash
# --- the presence secret -----------------------------------------------------
# ESO renders target.template with the Vault properties as `.<key>`. Doing the
# same substitution here proves the three things that matter: the agent's JSON
# parses, every bot's own key holds exactly the token the agent binds to it,
# and the operator token cannot report as a bot.
python3 - "$work/default.yaml" <<'PY'
import json, re, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
es = next((d for d in docs if d["kind"] == "ExternalSecret"
           and d["metadata"]["name"] == "jdwillmsen-minecraft-fwb-prd-presence"), None)
assert es, "the presence ExternalSecret is not rendered while presence is off; it must sync before anything reads it"
props = {r["secretKey"]: r["remoteRef"] for r in es["spec"]["data"]}
assert set(props) == {"presence_token_afk_bot_1", "presence_token_afk_bot_2", "presence_token_tools_mc"}, sorted(props)
for key, ref in props.items():
    assert ref["key"] == "minecraft-fwb" and ref["property"] == key, (key, ref)
fake = {k: "tok-" + k for k in props}
rendered = {k: re.sub(r"\{\{ \.(\w+) \}\}", lambda m: fake[m.group(1)], v)
            for k, v in es["spec"]["target"]["template"]["data"].items()}
tokens = {t["name"]: t for t in json.loads(rendered["presence_tokens"])}
assert set(tokens) == {"afk-bot-1", "afk-bot-2", "tools-mc"}, sorted(tokens)
for bot in ("afk-bot-1", "afk-bot-2"):
    key = "presence_token_" + bot.replace("-", "_")
    assert rendered[key] == tokens[bot]["token"], f"{bot}'s own key and the agent's list disagree"
    assert tokens[bot]["actor"] == bot and sorted(tokens[bot]["scopes"]) == ["presence:read", "presence:report"], tokens[bot]
assert "actor" not in tokens["tools-mc"] and sorted(tokens["tools-mc"]["scopes"]) == ["presence:read", "presence:write"], tokens["tools-mc"]
PY
echo "  ok: the presence secret binds each bot's token to its own actor"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tools/tests/test-actors.sh`
Expected: FAIL after the Task 1 checks, with

```
AssertionError: the presence ExternalSecret is not rendered while presence is off; it must sync before anything reads it
```

- [ ] **Step 3: Add `global.presence` to values**

In `charts/minecraft-fwb/values.yaml`, directly after the last `global.actors` entry (`groups: [bots]` of `afk-bot-2`) and a blank line, still inside `global:`:

```yaml
  # Runtime parking of the actors above: the agent's /v1 presence API, each
  # bot's reconciler and the bridge's kick allowlist. Under `global:` for the
  # same reason as `actors`, so this one switch reaches the bridge sidecar too.
  presence:
    # Off, the agent mounts no /v1 routes, the bots run exactly as before and
    # the bridge refuses every kick. It needs images that read the variables
    # and a synced secret -- see README, "Parking actors".
    enabled: false

    secret:
      # Renders the ExternalSecret on its own, ahead of anything reading it,
      # the order the console-bridge secret follows: a missing Vault property
      # then fails this one object, visibly, instead of a workload.
      create: true

    # The token `tools/mc presence` authenticates with, scoped read and write.
    # Its name is what the audit trail records for a CLI write, as
    # api:tools-mc. Bot tokens are derived from `actors`, one per afk-bot,
    # named and bound after that actor and scoped read and report.
    operatorToken:
      name: tools-mc
```

- [ ] **Step 4: Add the helpers and the ExternalSecret**

Append to `charts/minecraft-fwb/templates/_actors.tpl`:

```gotemplate
{{/* The presence Secret key holding one token; call with an actor id or token name. */}}
{{- define "presence.tokenKey" -}}
presence_token_{{ . | replace "-" "_" }}
{{- end -}}

{{- define "presence.secret.name" -}}
{{ .Release.Name }}-presence
{{- end -}}
```

Create `charts/minecraft-fwb/templates/presence-externalsecret.yaml`:

```gotemplate
{{- if or .Values.global.presence.secret.create .Values.global.presence.enabled }}
# PRESENCE_TOKENS for the agent and PRESENCE_TOKEN for each bot, from one
# Vault property per token.
#
# Templated rather than a stored JSON blob: a bot's token has to be the same
# string the agent's list carries for it, and one property read into both
# places cannot drift the way two hand-copied values would. Names, scopes and
# bindings come from global.actors, so registering a bot never means editing
# a secret by hand -- only adding its token property.
#
# `or` for the same reason as console-secret-externalsecret.yaml: the create
# flag can hide this object only while nothing reads it.
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: {{ include "presence.secret.name" . }}
  namespace: {{ .Release.Namespace }}
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault
    kind: ClusterSecretStore
  target:
    name: {{ include "presence.secret.name" . }}
    creationPolicy: Owner
    template:
      engineVersion: v2
      data:
        {{- $tokens := list }}
        {{- range .Values.global.actors }}
        {{- if eq .kind "afk-bot" }}
        {{- $key := include "presence.tokenKey" .id }}
        {{- $tokens = append $tokens (dict "name" .id "token" (printf "{{ .%s }}" $key) "scopes" (list "presence:read" "presence:report") "actor" .id) }}
        {{ $key }}: {{ printf "{{ .%s }}" $key | quote }}
        {{- end }}
        {{- end }}
        {{- $operatorKey := include "presence.tokenKey" .Values.global.presence.operatorToken.name }}
        {{- $tokens = append $tokens (dict "name" .Values.global.presence.operatorToken.name "token" (printf "{{ .%s }}" $operatorKey) "scopes" (list "presence:read" "presence:write")) }}
        {{ $operatorKey }}: {{ printf "{{ .%s }}" $operatorKey | quote }}
        presence_tokens: {{ $tokens | toJson | quote }}
  data:
    {{- range .Values.global.actors }}
    {{- if eq .kind "afk-bot" }}
    - secretKey: {{ include "presence.tokenKey" .id }}
      remoteRef:
        conversionStrategy: Default
        decodingStrategy: None
        metadataPolicy: None
        key: minecraft-fwb
        property: {{ include "presence.tokenKey" .id }}
    {{- end }}
    {{- end }}
    - secretKey: {{ include "presence.tokenKey" .Values.global.presence.operatorToken.name }}
      remoteRef:
        conversionStrategy: Default
        decodingStrategy: None
        metadataPolicy: None
        key: minecraft-fwb
        property: {{ include "presence.tokenKey" .Values.global.presence.operatorToken.name }}
{{- end }}
```

`printf "{{ .%s }}"` emits a literal ESO placeholder. Helm does not evaluate a string it prints, so it reaches ESO unchanged. `toJson | quote` produces one YAML string that holds the JSON, and the test decodes it the way the agent will.

- [ ] **Step 5: Run the test to verify it passes**

Run: `bash tools/tests/test-actors.sh && shellcheck -s bash tools/tests/test-actors.sh`
Expected: every Task 1 line, then

```
  ok: the presence secret binds each bot's token to its own actor
PASS
```

- [ ] **Step 6: Lint**

Run: `helm lint charts/minecraft-fwb`
Expected: `1 chart(s) linted, 0 chart(s) failed` (the existing `[INFO] Chart.yaml: icon is recommended` line may also appear).

- [ ] **Step 7: Commit**

```bash
git add charts/minecraft-fwb/values.yaml charts/minecraft-fwb/templates/_actors.tpl   charts/minecraft-fwb/templates/presence-externalsecret.yaml tools/tests/test-actors.sh
git commit -m "feat(minecraft-fwb): render the presence tokens from Vault ahead of any reader"
```

---

### Task 3: Presence consumers, switched off, and digests that stay put

**Files:**
- Create: `charts/minecraft-fwb/templates/presence-service.yaml`
- Modify: `charts/minecraft-fwb/templates/_actors.tpl` (append `presence.service.name`)
- Modify: `charts/minecraft-fwb/templates/agent-deployment.yaml:136` (after the `ANNOUNCE_API_TOKEN` block's `{{- end }}`)
- Modify: `charts/minecraft-fwb/templates/bot-deployment.yaml:62` (after `MC_VIEW_DISTANCE`)
- Modify: `charts/minecraft-fwb/templates/bot2-deployment.yaml:66` (after `MC_VIEW_DISTANCE`)
- Modify: `charts/minecraft-fwb/templates/_helpers.tpl:90-92` (`deployAnnounce.serverSpecHash`), `:118-123` (`deployAnnounce.agentSpecHash`)
- Test: `tools/tests/test-actors.sh` (append)

**Interfaces:**
- Consumes: `actors.json`, `actors.agent`, `actors.forValuesKey`, `actors.kickable` (Task 1); `presence.tokenKey`, `presence.secret.name`, `global.presence.enabled` (Task 2); `agent.name` (`_helpers.tpl:69-71`); `agent.metrics.port` (`values.yaml:1230`, `9090`, the agent's `HTTP_ADDR`).
- Produces:
  - `presence.service.name` renders `<release>-server-agent-presence`.
  - Service `<release>-server-agent-presence` on port 9090 with `publishNotReadyAddresses: true`. The bots use it, and `tools/mc presence` (Task 4) port-forwards to it.
  - Env on the agent: `PRESENCE_ACTORS`, `PRESENCE_SELF_ID`, `PRESENCE_TOKENS`.
  - Env on each bot: `PRESENCE_URL=http://<release>-server-agent-presence.<ns>.svc.cluster.local:9090`, `PRESENCE_ACTOR_ID`, `PRESENCE_DEFAULT`, `PRESENCE_TOKEN`.
  - `PRESENCE_POLL_MS` is left unset, so the bot's contract default of `10000` applies. This follows the `LEADER_*` precedent in `values.yaml:1113-1114`.

The dedicated Service is needed because of `agent-deployment.yaml:140-155` and the agent's `internal/httpapi/http.go:80-92`: `/readyz` answers 503 for the live agent whenever it has no Bedrock session, and a parked agent has none by design. `<release>-server-agent-metrics` (`agent-metrics.yaml`) therefore loses its only endpoint the moment the agent parks itself. The bots would then keep acting on their last answer ("parked") after an unpark. Every agent process serves `/v1` from Postgres (spec, "Reads and writes go straight to Postgres, so the standby replica serves the API as well as the leader"), so publishing not-ready pods is safe.

- [ ] **Step 1: Write the failing test**

In `tools/tests/test-actors.sh`, insert immediately before the final `echo "PASS"`:

```bash
# --- consumers, off and on ---------------------------------------------------
# Off must read exactly as before this feature: no variable a bot or the agent
# would act on, no Service, and no kick allowlist. Both states are set
# explicitly, so the suite means the same whichever one values.yaml ships.
render --set global.presence.enabled=false > "$work/presence-off.yaml"
python3 - "$work/presence-off.yaml" <<'PY'
import sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if not d or d["kind"] not in ("Deployment", "StatefulSet", "Service"):
        continue
    assert not d["metadata"]["name"].endswith("-server-agent-presence"), "the presence Service renders while presence is off"
    if d["kind"] == "Service":
        continue
    for c in d["spec"]["template"]["spec"]["containers"]:
        names = {e["name"] for e in c.get("env") or []}
        stray = {n for n in names if n.startswith("PRESENCE_") or n == "BRIDGE_KICKABLE"}
        assert not stray, f"{d['metadata']['name']}/{c['name']} carries {sorted(stray)} while presence is off"
PY
echo "  ok: presence off renders no consumer"

render --set global.presence.enabled=true > "$work/on.yaml" || fail "the chart does not render with presence on"
python3 - "$work/on.yaml" <<'PY'
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
def env_of(kind, name, container):
    d = next(d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name)
    c = next(c for c in d["spec"]["template"]["spec"]["containers"] if c["name"] == container)
    return {e["name"]: e for e in c.get("env") or []}

agent = env_of("Deployment", "jdwillmsen-minecraft-fwb-prd-server-agent", "agent")
actors = json.loads(agent["PRESENCE_ACTORS"]["value"])
assert actors == [
    {"id": "agent", "gamertag": "JDWServerAgent", "kind": "agent", "groups": [], "default_state": "present"},
    {"id": "afk-bot-1", "gamertag": "LightBlaz3", "kind": "afk-bot", "groups": ["bots"], "default_state": "present"},
    {"id": "afk-bot-2", "gamertag": "Dotablaze7321", "kind": "afk-bot", "groups": ["bots"], "default_state": "present"},
], actors
assert agent["PRESENCE_SELF_ID"]["value"] == "agent"
ref = agent["PRESENCE_TOKENS"]["valueFrom"]["secretKeyRef"]
assert ref == {"name": "jdwillmsen-minecraft-fwb-prd-presence", "key": "presence_tokens"}, ref

url = "http://jdwillmsen-minecraft-fwb-prd-server-agent-presence.jdwillmsen-prd.svc.cluster.local:9090"
for name, container, actor in (("jdwillmsen-minecraft-fwb-prd-afk-bot", "bot", "afk-bot-1"),
                               ("jdwillmsen-minecraft-fwb-prd-afk-bot-2", "bot2", "afk-bot-2")):
    env = env_of("Deployment", name, container)
    assert env["PRESENCE_URL"]["value"] == url, env["PRESENCE_URL"]
    assert env["PRESENCE_ACTOR_ID"]["value"] == actor
    assert env["PRESENCE_DEFAULT"]["value"] == "present"
    ref = env["PRESENCE_TOKEN"]["valueFrom"]["secretKeyRef"]
    assert ref == {"name": "jdwillmsen-minecraft-fwb-prd-presence",
                   "key": "presence_token_" + actor.replace("-", "_")}, ref

svc = next(d for d in docs if d["kind"] == "Service"
           and d["metadata"]["name"] == "jdwillmsen-minecraft-fwb-prd-server-agent-presence")
assert svc["spec"]["publishNotReadyAddresses"] is True, "a parked agent is not ready; the bots must still reach it"
assert svc["spec"]["selector"] == {"app": "jdwillmsen-minecraft-fwb-prd-server-agent"}
assert svc["spec"]["ports"][0]["port"] == 9090 and svc["spec"]["ports"][0]["targetPort"] == "metrics"
PY
echo "  ok: presence on wires the agent, both bots and the presence Service"

# --- the deploy-announce digests ----------------------------------------------
# global.actors sits inside the subchart's values, so the server digest would
# see every edit to it. Only the rendered kick list reaches the StatefulSet: a
# default flipped here restarts a bot, not the server, and must not buy
# players a restart countdown.
digests() {
  python3 -c '
import sys, yaml
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d and d["kind"] == "ConfigMap" and d["metadata"]["name"].endswith("-server-spec-hash"):
        print(d["data"]["hash"], d["data"]["agentHash"])
' "$1"
}
cat > "$work/flip.yaml" <<'YAML'
global:
  actors:
    - {id: agent, kind: agent, gamertag: JDWServerAgent, defaultState: present, groups: []}
    - {id: afk-bot-1, kind: afk-bot, valuesKey: bot, gamertag: LightBlaz3, defaultState: parked, groups: [bots]}
    - {id: afk-bot-2, kind: afk-bot, valuesKey: bot2, gamertag: Dotablaze7321, defaultState: present, groups: [bots]}
YAML
render -f "$work/flip.yaml" --set global.presence.enabled=false > "$work/flip-off.yaml"
render -f "$work/flip.yaml" --set global.presence.enabled=true > "$work/flip-on.yaml"
read -r off_server off_agent < <(digests "$work/presence-off.yaml")
read -r on_server on_agent < <(digests "$work/on.yaml")
read -r flip_off_server _ < <(digests "$work/flip-off.yaml")
read -r flip_on_server flip_on_agent < <(digests "$work/flip-on.yaml")
[ "$off_server" = "$flip_off_server" ] || fail "a bot's default moved the server digest while presence is off"
[ "$on_server" = "$flip_on_server" ] || fail "a bot's default moved the server digest though no gamertag changed"
[ "$on_agent" != "$flip_on_agent" ] || fail "a changed actor list must move the agent digest: the agent restarts for it"
[ "$off_agent" != "$on_agent" ] || fail "turning presence on must move the agent digest"
echo "  ok: only what reaches a workload moves its digest"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tools/tests/test-actors.sh`
Expected: `ok: presence off renders no consumer` passes (nothing reads presence yet), then FAIL with a `KeyError: 'PRESENCE_ACTORS'` traceback from the "presence on" check.

- [ ] **Step 3: Wire the agent**

In `charts/minecraft-fwb/templates/agent-deployment.yaml`, after line 136 (the `{{- end }}` closing `with .Values.agent.announceApi.existingSecret`) and before `ports:`:

```gotemplate
            {{- if .Values.global.presence.enabled }}
            {{- $self := include "actors.agent" . | fromJson }}
            # The registry rides the same switch as the tokens: an agent that
            # knew its actors but mounted no /v1 routes would park and kick on
            # its own policy with no API to undo it.
            - name: PRESENCE_ACTORS
              value: {{ include "actors.json" . | quote }}
            - name: PRESENCE_SELF_ID
              value: {{ $self.id | quote }}
            - name: PRESENCE_TOKENS
              valueFrom:
                secretKeyRef:
                  name: {{ include "presence.secret.name" . }}
                  key: presence_tokens
            {{- end }}
```

The comment sits inside the `if`, so a render with presence off is byte-identical to today's. Task 7 checks this.

- [ ] **Step 4: Wire both bots**

In `charts/minecraft-fwb/templates/bot-deployment.yaml`, after line 62 (`value: {{ .Values.bot.viewDistance | quote }}`):

```gotemplate
            {{- if .Values.global.presence.enabled }}
            {{- $actor := include "actors.forValuesKey" (dict "root" . "key" "bot") | fromJson }}
            # PRESENCE_DEFAULT is what the bot acts on until the agent first
            # answers; after that it keeps the last answer through an agent
            # outage, so a restarting agent never makes the bots flap.
            - name: PRESENCE_URL
              value: "http://{{ include "presence.service.name" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.agent.metrics.port }}"
            - name: PRESENCE_ACTOR_ID
              value: {{ $actor.id | quote }}
            - name: PRESENCE_DEFAULT
              value: {{ $actor.defaultState | quote }}
            - name: PRESENCE_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "presence.secret.name" . }}
                  key: {{ include "presence.tokenKey" $actor.id }}
            {{- end }}
```

In `charts/minecraft-fwb/templates/bot2-deployment.yaml`, after line 66 (`value: {{ .Values.bot2.viewDistance | quote }}`):

```gotemplate
            {{- if .Values.global.presence.enabled }}
            {{- $actor := include "actors.forValuesKey" (dict "root" . "key" "bot2") | fromJson }}
            # PRESENCE_DEFAULT is what the bot acts on until the agent first
            # answers; after that it keeps the last answer through an agent
            # outage, so a restarting agent never makes the bots flap.
            - name: PRESENCE_URL
              value: "http://{{ include "presence.service.name" . }}.{{ .Release.Namespace }}.svc.cluster.local:{{ .Values.agent.metrics.port }}"
            - name: PRESENCE_ACTOR_ID
              value: {{ $actor.id | quote }}
            - name: PRESENCE_DEFAULT
              value: {{ $actor.defaultState | quote }}
            - name: PRESENCE_TOKEN
              valueFrom:
                secretKeyRef:
                  name: {{ include "presence.secret.name" . }}
                  key: {{ include "presence.tokenKey" $actor.id }}
            {{- end }}
```

- [ ] **Step 5: Add the presence Service**

Append to `charts/minecraft-fwb/templates/_actors.tpl`:

```gotemplate
{{- define "presence.service.name" -}}
{{ include "agent.name" . }}-presence
{{- end -}}
```

Create `charts/minecraft-fwb/templates/presence-service.yaml`:

```gotemplate
{{- if and .Values.global.presence.enabled .Values.agent.enabled .Values.global.consoleBridge.enabled }}
# What the bots poll for their desired state. Not the metrics Service: that one
# carries only ready pods, and /readyz follows the live agent's Bedrock session
# -- which a parked agent has closed on purpose. Through it, parking the agent
# would leave the bots with no endpoint, still acting on "parked" after an
# unpark. Every agent process answers /v1 from Postgres whether or not it is in
# the world, so this one publishes them all.
apiVersion: v1
kind: Service
metadata:
  name: {{ include "presence.service.name" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    app: {{ include "agent.name" . }}
spec:
  publishNotReadyAddresses: true
  selector:
    app: {{ include "agent.name" . }}
  ports:
    - name: http
      port: {{ .Values.agent.metrics.port }}
      targetPort: metrics
{{- end }}
```

- [ ] **Step 6: Run the test; the digest check still fails**

Run: `bash tools/tests/test-actors.sh`
Expected: `ok: presence on wires the agent, both bots and the presence Service`, then

```
FAIL: a bot's default moved the server digest while presence is off
```

This is the false countdown described in Review Focus 2.

- [ ] **Step 7: Narrow both digests**

In `charts/minecraft-fwb/templates/_helpers.tpl`, replace lines 90-92:

```gotemplate
{{- define "deployAnnounce.serverSpecHash" -}}
{{ index .Values "minecraft-bedrock" | toYaml | sha256sum | trunc 16 }}
{{- end -}}
```

with:

```gotemplate
{{- define "deployAnnounce.serverSpecHash" -}}
{{- /*
global.* is merged into the subchart's values, so global.actors and
global.presence would move this digest on every edit, though the StatefulSet
reads only the kick list derived from them: a bot's default flipped here would
count down a server restart that never comes. They are swapped for exactly
what the sidecar renders, and with presence off the digest is the one it was
before either existed.
*/ -}}
{{- $values := deepCopy (index .Values "minecraft-bedrock") -}}
{{- $global := omit $values.global "actors" "presence" -}}
{{- if .Values.global.presence.enabled -}}
{{- $_ := set $global "bridgeKickable" (include "actors.kickable" .) -}}
{{- end -}}
```

and replace lines 118-123:

```gotemplate
{{- define "deployAnnounce.agentSpecHash" -}}
{{- if and .Values.agent.enabled .Values.global.consoleBridge.enabled
          (gt (int .Values.agent.replicas) 0) -}}
{{ .Values.agent | toYaml | sha256sum | trunc 16 }}
{{- end -}}
{{- end -}}
```

with:

```gotemplate
{{- define "deployAnnounce.agentSpecHash" -}}
{{- if and .Values.agent.enabled .Values.global.consoleBridge.enabled
          (gt (int .Values.agent.replicas) 0) -}}
{{- /*
The actor list reaches the agent as PRESENCE_ACTORS, so it is part of what
restarts the agent once presence is on -- and nothing while it is off, which
keeps this digest unchanged until then.
*/ -}}
{{- $values := .Values.agent -}}
{{- if .Values.global.presence.enabled -}}
{{- $values = dict "agent" .Values.agent "actors" (include "actors.json" .) -}}
{{- end -}}
{{ $values | toYaml | sha256sum | trunc 16 }}
{{- end -}}
{{- end -}}
```

`deepCopy` matters here. `omit` returns a new map, but `set $values "global"` writes into the map it is given, and without the copy that map is the live `.Values["minecraft-bedrock"]` every later template reads.

- [ ] **Step 8: Run the tests to verify they pass**

Run: `bash tools/tests/test-actors.sh && shellcheck -s bash tools/tests/test-actors.sh && helm lint charts/minecraft-fwb`
Expected: all earlier lines, then

```
  ok: presence off renders no consumer
  ok: presence on wires the agent, both bots and the presence Service
  ok: only what reaches a workload moves its digest
PASS
```

and `1 chart(s) linted, 0 chart(s) failed`.

- [ ] **Step 9: Confirm the digests match production's current ones**

```bash
main="$(mktemp -d)"
git archive origin/main charts/minecraft-fwb | tar -x -C "$main"
diff <(render | grep -E '^  (hash|agentHash):') \
     <(helm template jdwillmsen-minecraft-fwb-prd "$main/charts/minecraft-fwb" -n jdwillmsen-prd \
         -f "$main/charts/minecraft-fwb/values.yaml" -f "$main/charts/minecraft-fwb/values-prd.yaml" \
         -f "$main/charts/minecraft-fwb/values-console-bridge.yaml" | grep -E '^  (hash|agentHash):') \
  && echo "digests unchanged"
rm -rf "$main"
```

Expected: `digests unchanged`. The next sync's PreSync hook then sees no server or agent change and sends no countdown. Task 7 repeats this comparison for the whole render.

- [ ] **Step 10: Commit**

```bash
git add charts/minecraft-fwb/templates tools/tests/test-actors.sh
git commit -m "feat(minecraft-fwb): wire presence into the agent and bots behind one switch"
```

---

### Task 4: `tools/mc presence ls`

**Files:**
- Modify: `tools/mc:180` (insert the presence block before `usage() {`), `:188` (usage line), `:201` (dispatch)
- Test: `tools/tests/test-mc-presence.sh`

**Interfaces:**
- Consumes: the `<release>-server-agent-presence` Service on 9090 (Task 3) and Secret `<release>-presence` key `presence_token_tools_mc` (Task 2). The API's `GET /v1/actors` returns `[]ActorView` (contract: `id, gamertag, kind, groups` plus the embedded `Presence` fields `actor_id, effective, default, override{state, until, wake_on, reason, set_by, set_at, version}` and `status{connected, observed_state, last_seen, process_version}`). Existing `tools/mc` globals `NAMESPACE`, `SELF` and `err()` (`tools/mc:10-20`).
- Produces (bash functions in `tools/mc`, used by Task 5):
  - `usage_err MSG [HINT]` prints `error:`/`hint:` and exits 2.
  - `presence_need`, `presence_connect` (sets `PRESENCE_BASE`), `presence_token` (sets `PRESENCE_TOKEN`).
  - `api METHOD PATH [JSON]` sets `API_STATUS` (`000` if nothing answered) and `API_BODY`.
  - `api_fail` translates `API_STATUS` into `err`, exit 1.
  - `presence_actors` sets `ACTORS` (the JSON array).
  - `TOON_JQ` is a jq prelude defining `tv`, which quotes a TOON scalar only where the format requires it.
  - `presence_ls`, `presence_usage`, `cmd_presence`.
  - Environment: `MC_RELEASE` (default `jdwillmsen-minecraft-fwb-prd`), `MC_PRESENCE_PORT` (9090), `MC_PRESENCE_TOKEN`, `MC_PRESENCE_URL`.

Output follows the AXI skill (`~/.claude/skills/axi/SKILL.md`): TOON tables with the total count, a definitive list of groups, and `help[]` next steps. Errors stay in this tool's existing `error:`/`hint:` shape (`tools/mc:14-20`) so the whole tool reads one way. The default schema has five fields, not AXI's three or four. `source` (who set an override) and `until` are what an operator decides on, and `connected` is the only place the observed status shows.

The API is reached by `kubectl port-forward` to the presence Service for the life of one command, the way `README.md` already reaches the join-probe port. `kubectl get --raw` through the API server's service proxy cannot carry an `Authorization` header to the backend, because the API server consumes that header itself. Port-forwarding to a Service picks a backing pod whether or not it is ready, which matches the Service's own `publishNotReadyAddresses`.

- [ ] **Step 1: Write the failing test**

Create `tools/tests/test-mc-presence.sh`:

```bash
#!/usr/bin/env bash
# Exercises `tools/mc presence` against fake kubectl and curl, so it runs in CI
# with no cluster and no agent.
#
# The fakes answer from fixture files named after the request, and record
# every request and every argv, so what was sent -- and what was not, like the
# token on a command line -- is asserted rather than assumed.
set -euo pipefail

here="$(cd "$(dirname "$0")/../.." && pwd)"
mc="$here/tools/mc"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $1"; exit 1; }

TOKEN="gggggggggggggggg"
mkdir -p "$work/bin" "$work/fx"

cat > "$work/bin/kubectl" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$KUBECTL_LOG"
case "$1" in
  get) [ -n "${FAKE_NO_SECRET:-}" ] || printf '%s' "$(printf '%s' "$FAKE_TOKEN" | base64)" ;;
  port-forward)
    [ -n "${FAKE_NO_SERVICE:-}" ] && { echo 'Error from server (NotFound): services not found'; exit 1; }
    echo "Forwarding from 127.0.0.1:40123 -> 9090"
    exec sleep 30 ;;
esac
SHIM

# Answers from $FIXTURES/<METHOD><path with / as _>.json, status from a
# sibling .code file (default 200). Records the Authorization header it was
# handed on stdin, so the test can prove the token arrived that way.
cat > "$work/bin/curl" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$ARGV_LOG"
method=GET body="" url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    --data-binary) body="$2"; shift 2 ;;
    -H|-w|--max-time) shift 2 ;;
    http*) url="$1"; shift ;;
    *) shift ;;
  esac
done
header="$(cat)"
path="${url#http://*/}"
printf '%s /%s %s\n' "$method" "$path" "$body" >> "$CAPTURE"
printf '%s\n' "$header" >> "$HEADERS"
slug="$method$(printf '/%s' "$path" | tr '/' '_')"
code=200
[ -f "$FIXTURES/$slug.code" ] && code="$(cat "$FIXTURES/$slug.code")"
[ -f "$FIXTURES/$slug.json" ] && cat "$FIXTURES/$slug.json"
printf '\n%s' "$code"
SHIM
chmod +x "$work/bin/kubectl" "$work/bin/curl"

# afk-bot-1 parked from the CLI with an expiry; afk-bot-2 and the agent at
# their defaults. The agent reports no status, so `connected` must say null
# rather than false.
cat > "$work/fx/GET_v1_actors.json" <<'JSON'
[{"id":"agent","gamertag":"JDWServerAgent","kind":"agent","groups":[],"actor_id":"agent","effective":"present","default":"present"},
 {"id":"afk-bot-1","gamertag":"LightBlaz3","kind":"afk-bot","groups":["bots"],"actor_id":"afk-bot-1","effective":"parked","default":"present",
  "override":{"state":"parked","until":"2026-09-23T12:00:00Z","reason":"farm rebuild","set_by":"api:tools-mc","set_at":"2026-09-23T10:00:00Z","version":3},
  "status":{"connected":false,"observed_state":"parked","last_seen":"2026-09-23T10:00:05Z","process_version":"1.2.0"}},
 {"id":"afk-bot-2","gamertag":"Dotablaze7321","kind":"afk-bot","groups":["bots"],"actor_id":"afk-bot-2","effective":"present","default":"present",
  "status":{"connected":true,"observed_state":"present","last_seen":"2026-09-23T10:00:07Z","process_version":"1.2.0"}}]
JSON

run() {
  : > "$work/capture"; : > "$work/argv"; : > "$work/headers"; : > "$work/kubectl"
  env PATH="$work/bin:$PATH" MC_NAMESPACE=test-ns FIXTURES="$work/fx" FAKE_TOKEN="$TOKEN" \
      CAPTURE="$work/capture" ARGV_LOG="$work/argv" HEADERS="$work/headers" KUBECTL_LOG="$work/kubectl" \
      "$@"
}

# --- ls ----------------------------------------------------------------------
set +e
out="$(run bash "$mc" presence ls)"; rc=$?
set -e
[ "$rc" = 0 ] || fail "ls exited $rc: $out"
grep -qx 'count: 3' <<<"$out" || fail "ls must state the total: $out"
grep -qx 'actors\[3\]{id,effective,source,until,connected}:' <<<"$out" || fail "ls must print a TOON table header: $out"
grep -qx '  agent,present,default,null,null' <<<"$out" || fail "an actor at its default reads as source default, no status as null: $out"
grep -qx '  afk-bot-1,parked,"api:tools-mc","2026-09-23T12:00:00Z",false' <<<"$out" || fail "values with a colon are quoted and false stays false: $out"
grep -qx '  afk-bot-2,present,default,null,true' <<<"$out" || fail "a connected bot reads true: $out"
grep -qx 'groups\[2\]: bots,all' <<<"$out" || fail "ls must list the groups a target can name: $out"
grep -q '^help\[2\]:' <<<"$out" || fail "ls must offer next steps: $out"
echo "  ok: ls prints actors as TOON with counts, groups and next steps"

grep -q 'port-forward -n test-ns svc/jdwillmsen-minecraft-fwb-prd-server-agent-presence :9090' "$work/kubectl" \
  || fail "ls must reach the API through the presence Service: $(cat "$work/kubectl")"
grep -q 'http://127.0.0.1:40123/v1/actors' "$work/argv" || fail "ls must call the forwarded port: $(cat "$work/argv")"
echo "  ok: ls reaches the presence Service through a port-forward"

# --- the token -----------------------------------------------------------------
grep -qx "Authorization: Bearer $TOKEN" "$work/headers" || fail "the token must reach curl as the Authorization header"
grep -qF "$TOKEN" "$work/argv" && fail "the token appeared in curl's argv, which ps shows to every user"
grep -qF "$TOKEN" <<<"$out" && fail "the token appeared in the output"
echo "  ok: the token reaches curl on stdin and is never printed"

out="$(run env MC_PRESENCE_URL=http://agent.test MC_PRESENCE_TOKEN=from-env bash "$mc" presence ls)"
grep -qx "Authorization: Bearer from-env" "$work/headers" || fail "MC_PRESENCE_TOKEN must override the cluster secret"
[ ! -s "$work/kubectl" ] || fail "with URL and token given, nothing may touch the cluster: $(cat "$work/kubectl")"
echo "  ok: MC_PRESENCE_URL and MC_PRESENCE_TOKEN bypass the cluster"

# --- errors ----------------------------------------------------------------------
set +e
out="$(run env FAKE_NO_SERVICE=1 bash "$mc" presence ls)"; rc=$?
set -e
[ "$rc" = 1 ] || fail "an unreachable API must exit 1, got $rc"
grep -q '^error: could not reach the presence API' <<<"$out" || fail "an unreachable API must say so: $out"
grep -q '^hint: presence may be off' <<<"$out" || fail "an unreachable API must suggest why: $out"
grep -q 'Error from server' <<<"$out" && fail "kubectl's own error text leaked: $out"
echo "  ok: a missing presence Service is reported, not leaked"

echo 401 > "$work/fx/GET_v1_actors.code"
set +e
out="$(run bash "$mc" presence ls)"; rc=$?
set -e
rm "$work/fx/GET_v1_actors.code"
[ "$rc" = 1 ] || fail "a 401 must exit 1, got $rc"
grep -q '^error: the presence API rejected the operator token' <<<"$out" || fail "a 401 must be translated: $out"
echo "  ok: a rejected token is translated"

set +e
out="$(run bash "$mc" presence ls --bogus)"; rc=$?
set -e
[ "$rc" = 2 ] || fail "an unknown flag must exit 2, got $rc"
grep -q '^hint: valid flags for presence ls: --help' <<<"$out" || fail "an unknown flag must list the valid ones: $out"
[ ! -s "$work/kubectl" ] || fail "a usage error must be caught before touching the cluster"
echo "  ok: unknown flags exit 2 before any call"

out="$(run bash "$mc" presence --help)"
grep -q 'presence park <target> --reason' <<<"$out" || fail "presence --help must document park: $out"
echo "  ok: presence --help documents every subcommand"

echo "PASS"
```

```bash
chmod +x tools/tests/test-mc-presence.sh
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tools/tests/test-mc-presence.sh`
Expected:

```
FAIL: ls exited 1: error: unknown command: presence
hint: mc --help
```

- [ ] **Step 3: Implement `presence ls`**

In `tools/mc`, insert before `usage() {` (line 180):

```bash
# --- presence ----------------------------------------------------------------
#
# A thin client over the agent's /v1 presence API. The API is not published
# outside the cluster, so each call port-forwards to the presence Service for
# its own lifetime.

RELEASE="${MC_RELEASE:-jdwillmsen-minecraft-fwb-prd}"
PRESENCE_PORT="${MC_PRESENCE_PORT:-9090}"
# Matches global.presence.operatorToken.name in the chart, via the
# presence.tokenKey helper.
PRESENCE_TOKEN_KEY="presence_token_tools_mc"

# Usage errors exit 2 rather than err's 1, so a caller can tell "you asked
# wrongly" from "what you asked for could not be done".
usage_err() {
  echo "error: $1"
  [ $# -gt 1 ] && echo "hint: $2"
  exit 2
}

presence_need() {
  command -v jq >/dev/null 2>&1 || err "jq is required for presence" "install jq, then re-run"
  command -v curl >/dev/null 2>&1 || err "curl is required for presence" "install curl, then re-run"
}

# MC_PRESENCE_URL skips the port-forward, for a caller that already has a
# route in (and for the tests, which have no cluster).
presence_connect() {
  if [ -n "${MC_PRESENCE_URL:-}" ]; then
    PRESENCE_BASE="$MC_PRESENCE_URL"
    return 0
  fi
  PF_LOG="$(mktemp)"
  kubectl port-forward -n "$NAMESPACE" "svc/${RELEASE}-server-agent-presence" ":$PRESENCE_PORT" \
    >"$PF_LOG" 2>&1 &
  PF_PID=$!
  trap 'kill "$PF_PID" 2>/dev/null; rm -f "$PF_LOG"' EXIT
  local port="" _
  for _ in $(seq 1 50); do
    port="$(grep -oE '127\.0\.0\.1:[0-9]+' "$PF_LOG" | head -1 | cut -d: -f2)"
    [ -n "$port" ] && break
    kill -0 "$PF_PID" 2>/dev/null || break
    sleep 0.1
  done
  [ -n "$port" ] || err "could not reach the presence API" \
    "presence may be off (global.presence.enabled in charts/minecraft-fwb/values.yaml), or: kubectl get svc -n $NAMESPACE ${RELEASE}-server-agent-presence"
  PRESENCE_BASE="http://127.0.0.1:$port"
}

# The token stays in this variable. It is never echoed, and it reaches curl on
# stdin rather than in argv, which any user on the machine can read from ps.
presence_token() {
  if [ -n "${MC_PRESENCE_TOKEN:-}" ]; then
    PRESENCE_TOKEN="$MC_PRESENCE_TOKEN"
    return 0
  fi
  PRESENCE_TOKEN="$(kubectl get secret -n "$NAMESPACE" "${RELEASE}-presence" \
    -o jsonpath="{.data.$PRESENCE_TOKEN_KEY}" 2>/dev/null | base64 -d 2>/dev/null)"
  [ -n "$PRESENCE_TOKEN" ] || err "no operator token in secret ${RELEASE}-presence" \
    "check it has synced: kubectl get externalsecret -n $NAMESPACE ${RELEASE}-presence"
}

# api METHOD PATH [JSON]; sets API_STATUS (000 when nothing answered) and
# API_BODY.
api() {
  local out
  local -a args=(-sS -X "$1" --max-time 10 -H @- -w '\n%{http_code}')
  [ $# -gt 2 ] && args+=(-H 'Content-Type: application/json' --data-binary "$3")
  out="$(curl "${args[@]}" "$PRESENCE_BASE$2" 2>/dev/null <<<"Authorization: Bearer $PRESENCE_TOKEN")" || true
  API_STATUS="${out##*$'\n'}"
  API_BODY="${out%$'\n'*}"
  [[ "$API_STATUS" =~ ^[0-9]{3}$ ]] || { API_STATUS=000; API_BODY=""; }
}

# Translates a failed call into the CLI's own error. The API's message is ours
# and safe to show; the raw body never is printed.
api_fail() {
  local msg
  msg="$(jq -r '.message // empty' <<<"$API_BODY" 2>/dev/null)"
  case "$API_STATUS" in
    000) err "the presence API did not answer" "the agent may be restarting: $SELF status" ;;
    401) err "the presence API rejected the operator token" \
           "the secret may have rotated since this ran; re-run, or: kubectl get externalsecret -n $NAMESPACE ${RELEASE}-presence" ;;
    403) err "the operator token may not do this${msg:+: $msg}" "check its scopes in global.presence of the chart" ;;
    404) err "${msg:-no such actor or group}" "$SELF presence ls" ;;
    503) err "the presence store is unavailable${msg:+: $msg}" \
           "the bots keep their last state meanwhile; retry: $SELF presence ls" ;;
    *)   err "the presence API answered $API_STATUS${msg:+: $msg}" "$SELF presence ls" ;;
  esac
}

# TOON scalar: quoted only where the format requires it.
TOON_JQ='def tv: if . == null then "null"
  elif (type == "boolean" or type == "number") then tostring
  elif test("^$|^[\\s-]|\\s$|[,:\"\\\\\\[\\]{}#]|^(true|false|null)$|^-?[0-9]") then tojson
  else . end;'

presence_actors() {
  api GET /v1/actors
  [ "$API_STATUS" = 200 ] || api_fail
  ACTORS="$API_BODY"
}

presence_ls() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -h|--help) presence_usage; return 0 ;;
      *) usage_err "unknown argument for presence ls: $1" "valid flags for presence ls: --help" ;;
    esac
  done
  presence_need; presence_connect; presence_token; presence_actors
  jq -r "$TOON_JQ"'
    "count: \(length)",
    "actors[\(length)]{id,effective,source,until,connected}:",
    (.[] | "  " + ([.id, .effective,
                    (if .override then .override.set_by else "default" end),
                    (.override.until // null),
                    (if .status then .status.connected else null end)] | map(tv) | join(","))),
    (([.[].groups[]?] | unique) + ["all"]) as $g
    | "groups[\($g | length)]: " + ($g | map(tv) | join(",")),
    "help[2]:",
    "  Run `'"$SELF"' presence park <id|group|all> --reason \"<text>\" [--for 2h]` to park",
    "  Run `'"$SELF"' presence unpark <id|group|all>` to return to the default"
  ' <<<"$ACTORS"
}

presence_usage() {
  cat <<USAGE
$SELF presence — park and restore the accounts that hold the world's chunks

  $SELF presence [ls]                                   every actor and its state
  $SELF presence park <target> --reason <text> [--for <duration>]
  $SELF presence unpark <target>                        back to the default in git

  <target>    an actor id, a group, or all
  --for       Go duration (30m, 2h); without it the park lasts until unparked
  --reason    required; recorded with the override

examples:
  $SELF presence park bots --for 2h --reason "farm rebuild"
  $SELF presence unpark bots

environment:
  MC_RELEASE          release name (default: $RELEASE)
  MC_PRESENCE_TOKEN   operator token, instead of reading the cluster secret
  MC_PRESENCE_URL     API base URL, instead of a port-forward
USAGE
}

cmd_presence() {
  case "${1:-}" in
    ""|ls)  shift || true; presence_ls "$@" ;;
    -h|--help|help) presence_usage ;;
    *) usage_err "unknown presence command: $1" "$SELF presence --help" ;;
  esac
}
```

In `usage()`, after line 188 (`  $SELF run <command> [args...]    any console command`), add:

```
  $SELF presence [ls|park|unpark]  park or restore the bots and the agent
```

In the dispatch `case` at the bottom, after line 201 (`  run)        shift; cmd_run "$@" ;;`), add:

```bash
  presence)   shift; cmd_presence "$@" ;;
```

`presence_usage` already documents `park` and `unpark`. Task 5 lands them in the same PR, before anything is released.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bash tools/tests/test-mc-presence.sh && bash tools/tests/test-mc.sh && shellcheck -s bash tools/mc tools/tests/test-mc-presence.sh`
Expected:

```
  ok: ls prints actors as TOON with counts, groups and next steps
  ok: ls reaches the presence Service through a port-forward
  ok: the token reaches curl on stdin and is never printed
  ok: MC_PRESENCE_URL and MC_PRESENCE_TOKEN bypass the cluster
  ok: a missing presence Service is reported, not leaked
  ok: a rejected token is translated
  ok: unknown flags exit 2 before any call
  ok: presence --help documents every subcommand
PASS
```

`test-mc.sh` still ends in `PASS`, and shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
git add tools/mc tools/tests/test-mc-presence.sh
git commit -m "feat(tools): list actor presence from tools/mc"
```

---

### Task 5: `tools/mc presence park` and `unpark`

**Files:**
- Modify: `tools/mc` (insert before `cmd_presence() {`; replace `cmd_presence`)
- Test: `tools/tests/test-mc-presence.sh` (append)

**Interfaces:**
- Consumes: from Task 4, `usage_err`, `presence_need`, `presence_connect`, `presence_token`, `presence_actors`/`ACTORS`, `api`/`API_STATUS`/`API_BODY`, `api_fail`, `TOON_JQ`, `presence_usage`. Contract bodies: `SetRequest` `{state, until?, duration?, wake_on?, reason, version}`. `PUT /v1/actors/{id}/presence` returns `Presence` or 409 `Error{code, message, current}`. `PUT /v1/groups/{group}/presence` returns `[]Presence`, with `version` ignored. `DELETE /v1/actors/{id}/presence` returns `Presence`.
- Produces: `presence_resolve TARGET` (sets `TARGET_KIND` to `actor|group` and `TARGET_IDS[]`), `presence_park`, `presence_unpark`, and the final `cmd_presence`.

Behaviour choices, each pinned by a test below:
- A single actor is written against the `override.version` that `GET /v1/actors` returned (0 when there is no override), and a 409 is shown, not retried. Someone else's change wins until the operator looks.
- A group or `all` goes through the group route in one transaction.
- `unpark` of a group deletes each member's override, because there is no group DELETE. Members without an override are skipped, so repeating an unpark is a no-op with exit 0.
- `--reason` is required, `--for` must be a Go duration, and the target must match the id regex. All of these fail with exit 2 before any cluster call.
- The CLI adds no `wake_on` or default expiry. The chat rule "an agent park always has a way back" is the agent's policy (plan 3), and the CLI stays thin. Task 6's runbook tells operators to give `--for` when parking the agent or `all`.

- [ ] **Step 1: Write the failing test**

In `tools/tests/test-mc-presence.sh`, insert immediately before the final `echo "PASS"`:

```bash
# --- park --------------------------------------------------------------------
cat > "$work/fx/PUT_v1_groups_bots_presence.json" <<'JSON'
[{"actor_id":"afk-bot-1","effective":"parked","default":"present","override":{"state":"parked","until":"2026-09-23T12:00:00Z","reason":"farm rebuild","set_by":"api:tools-mc","set_at":"2026-09-23T10:00:00Z","version":4}},
 {"actor_id":"afk-bot-2","effective":"parked","default":"present","override":{"state":"parked","until":"2026-09-23T12:00:00Z","reason":"farm rebuild","set_by":"api:tools-mc","set_at":"2026-09-23T10:00:00Z","version":1}}]
JSON
out="$(run bash "$mc" presence park bots --for 2h --reason "farm rebuild")"
grep -qx 'PUT /v1/groups/bots/presence {"state":"parked","reason":"farm rebuild","version":0,"duration":"2h"}' "$work/capture" \
  || fail "a group park must PUT the group route with the duration: $(cat "$work/capture")"
grep -qx 'parked\[2\]{id,effective,until}:' <<<"$out" || fail "park must list what it parked: $out"
grep -qx '  afk-bot-2,parked,"2026-09-23T12:00:00Z"' <<<"$out" || fail "park must show each expiry: $out"
grep -q 'presence unpark bots' <<<"$out" || fail "park must say how to undo it: $out"
echo "  ok: parking a group sends one group write with the duration"

cat > "$work/fx/PUT_v1_actors_afk-bot-1_presence.json" <<'JSON'
{"actor_id":"afk-bot-1","effective":"parked","default":"present","override":{"state":"parked","reason":"moving it","set_by":"api:tools-mc","set_at":"2026-09-23T10:00:00Z","version":4}}
JSON
out="$(run bash "$mc" presence park afk-bot-1 --reason "moving it")"
grep -qx 'PUT /v1/actors/afk-bot-1/presence {"state":"parked","reason":"moving it","version":3}' "$work/capture" \
  || fail "an actor park must carry the version it read and no duration: $(cat "$work/capture")"
grep -qx '  afk-bot-1,parked,null' <<<"$out" || fail "a park without --for has no expiry: $out"
echo "  ok: parking one actor writes against the version it read"

echo 409 > "$work/fx/PUT_v1_actors_afk-bot-1_presence.code"
cat > "$work/fx/PUT_v1_actors_afk-bot-1_presence.json" <<'JSON'
{"code":"conflict","message":"version mismatch","current":{"actor_id":"afk-bot-1","effective":"present","default":"present","override":{"state":"present","reason":"back now","set_by":"chat:Steve","set_at":"2026-09-23T10:01:00Z","version":4}}}
JSON
set +e
out="$(run bash "$mc" presence park afk-bot-1 --reason "moving it")"; rc=$?
set -e
rm "$work/fx/PUT_v1_actors_afk-bot-1_presence.code"
[ "$rc" = 1 ] || fail "a conflict must exit 1, got $rc"
grep -qx 'current: afk-bot-1,present,"chat:Steve"' <<<"$out" || fail "a conflict must show who changed it: $out"
[ "$(grep -c '^PUT' "$work/capture")" = 1 ] || fail "a conflict must not be retried over someone else's change"
echo "  ok: a conflicting edit is shown, not overwritten"

for args in "park bots --for 2h" "park bots --reason x --for soon" "park --reason x" "park Bots --reason x" \
            "park bots --reason x --force" "park bots extra --reason x"; do
  set +e
  # shellcheck disable=SC2086  # word splitting is the point: each case is an argv
  out="$(run bash "$mc" presence $args)"; rc=$?
  set -e
  [ "$rc" = 2 ] || fail "'presence $args' must be a usage error (exit 2), got $rc: $out"
  grep -q '^error:' <<<"$out" || fail "'presence $args' must say what is wrong: $out"
  grep -q '^hint:' <<<"$out" || fail "'presence $args' must say what to run instead: $out"
done
[ ! -s "$work/kubectl" ] || fail "usage errors must be caught before touching the cluster"
echo "  ok: a missing reason, bad duration, bad target or unknown flag exits 2 before any call"

set +e
out="$(run bash "$mc" presence park bost --reason x)"; rc=$?
set -e
[ "$rc" = 1 ] || fail "an unknown target must exit 1, got $rc"
grep -q '^hint: targets are agent, afk-bot-1, afk-bot-2, bots, all' <<<"$out" || fail "an unknown target must list the real ones: $out"
grep -q '^PUT' "$work/capture" && fail "an unknown target must write nothing"
echo "  ok: an unknown target is refused before anything is written"

# --- unpark ------------------------------------------------------------------
cat > "$work/fx/DELETE_v1_actors_afk-bot-1_presence.json" <<'JSON'
{"actor_id":"afk-bot-1","effective":"present","default":"present"}
JSON
out="$(run bash "$mc" presence unpark bots)"
[ "$(grep -c '^DELETE' "$work/capture")" = 1 ] || fail "only actors with an override are unparked: $(cat "$work/capture")"
grep -qx 'DELETE /v1/actors/afk-bot-1/presence ' "$work/capture" || fail "unpark must DELETE the override: $(cat "$work/capture")"
grep -qx 'unparked\[1\]{id,effective}:' <<<"$out" || fail "unpark must print a TOON table: $out"
grep -qx '  afk-bot-1,present' <<<"$out" || fail "unpark must show the restored state: $out"
grep -qx 'unchanged\[1\]: afk-bot-2' <<<"$out" || fail "unpark must name what was already at its default: $out"
echo "  ok: unparking a group removes each member's override"

set +e
out="$(run bash "$mc" presence unpark afk-bot-2)"; rc=$?
set -e
[ "$rc" = 0 ] || fail "unparking an actor at its default is a no-op, not an error (rc=$rc)"
grep -q 'no-op' <<<"$out" || fail "a no-op must say so: $out"
grep -q '^DELETE' "$work/capture" && fail "a no-op must write nothing"
echo "  ok: unparking what is not parked is an explicit no-op"

# Both bots parked, the second DELETE fails: what already happened is stated.
jq '.[2].override = .[1].override' "$work/fx/GET_v1_actors.json" > "$work/fx/both.json"
mv "$work/fx/both.json" "$work/fx/GET_v1_actors.json"
echo 503 > "$work/fx/DELETE_v1_actors_afk-bot-2_presence.code"
echo '{"code":"unavailable","message":"store unavailable"}' > "$work/fx/DELETE_v1_actors_afk-bot-2_presence.json"
set +e
out="$(run bash "$mc" presence unpark bots)"; rc=$?
set -e
[ "$rc" = 1 ] || fail "a failed member must fail the command, got $rc"
grep -qx 'unparked_before_failure: afk-bot-1' <<<"$out" || fail "a partial unpark must name what it already did: $out"
grep -q '^error: the presence store is unavailable' <<<"$out" || fail "the failure must be translated: $out"
echo "  ok: a partial group unpark reports what it did before failing"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tools/tests/test-mc-presence.sh; echo "exit=$?"`
Expected: the eight Task 4 lines, then the suite stops with `exit=2`. `presence park` is still an unknown presence command (a usage error, exit 2), and `set -e` ends the script at that command substitution.

- [ ] **Step 3: Implement park and unpark**

In `tools/mc`, insert immediately before `cmd_presence() {`:

```bash
ID_RE='^[a-z0-9][a-z0-9-]{0,62}$'

# Resolves a target against the live actor list, so a typo is refused before
# anything is written. Sets TARGET_KIND (actor|group) and TARGET_IDS.
presence_resolve() {
  local target="$1"
  if jq -e --arg t "$target" 'any(.[]; .id == $t)' <<<"$ACTORS" >/dev/null; then
    TARGET_KIND=actor
  elif [ "$target" = all ] || jq -e --arg t "$target" 'any(.[]; (.groups // []) | index($t))' <<<"$ACTORS" >/dev/null; then
    TARGET_KIND=group
  else
    err "unknown target: $target" \
      "targets are $(jq -r '[.[].id] + ([.[].groups[]?] | unique) + ["all"] | join(", ")' <<<"$ACTORS")"
  fi
  mapfile -t TARGET_IDS < <(jq -r --arg t "$target" --arg k "$TARGET_KIND" '
    .[] | select($k == "group" and ($t == "all" or ((.groups // []) | index($t)))
                 or ($k == "actor" and .id == $t)) | .id' <<<"$ACTORS")
}

presence_park() {
  local target="" duration="" reason=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -h|--help) presence_usage; return 0 ;;
      --for) [ $# -gt 1 ] || usage_err "--for needs a duration" "$SELF presence park bots --for 2h --reason \"<text>\""
             duration="$2"; shift 2 ;;
      --reason) [ $# -gt 1 ] || usage_err "--reason needs text" "$SELF presence park bots --reason \"<text>\""
                reason="$2"; shift 2 ;;
      -*) usage_err "unknown flag for presence park: $1" "valid flags for presence park: --for, --reason, --help" ;;
      *) [ -z "$target" ] || usage_err "presence park takes one target, got a second: $1" "$SELF presence park <id|group|all> --reason \"<text>\""
         target="$1"; shift ;;
    esac
  done
  [ -n "$target" ] || usage_err "presence park needs a target" "$SELF presence park <id|group|all> --reason \"<text>\" [--for 2h]"
  [[ "$target" =~ $ID_RE ]] || usage_err "not an actor id, group or all: $target" "$SELF presence ls"
  [ -n "${reason// /}" ] || usage_err "--reason is required" "$SELF presence park $target --reason \"<why>\""
  if [ -n "$duration" ] && ! [[ "$duration" =~ ^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$ ]]; then
    usage_err "not a Go duration: $duration" "use units like 30m, 2h or 1h30m"
  fi

  presence_need; presence_connect; presence_token; presence_actors
  presence_resolve "$target"

  local body result
  if [ "$TARGET_KIND" = actor ]; then
    # The version read is the one written against: a 409 means someone else
    # changed this actor in between, and their change is shown, not overwritten.
    local version
    version="$(jq -r --arg t "$target" '.[] | select(.id == $t) | .override.version // 0' <<<"$ACTORS")"
    body="$(jq -nc --arg r "$reason" --arg d "$duration" --argjson v "$version" \
      '{state: "parked", reason: $r, version: $v} + (if $d == "" then {} else {duration: $d} end)')"
    api PUT "/v1/actors/$target/presence" "$body"
    if [ "$API_STATUS" = 409 ]; then
      echo "error: $target was changed by someone else while this ran"
      jq -r "$TOON_JQ"'.current | "current: " + ([.actor_id, .effective, (.override.set_by // "default")] | map(tv) | join(","))' \
        <<<"$API_BODY" 2>/dev/null
      echo "hint: $SELF presence ls, then re-run if it is still wanted"
      exit 1
    fi
    [ "$API_STATUS" = 200 ] || api_fail
    result="[$API_BODY]"
  else
    body="$(jq -nc --arg r "$reason" --arg d "$duration" \
      '{state: "parked", reason: $r, version: 0} + (if $d == "" then {} else {duration: $d} end)')"
    api PUT "/v1/groups/$target/presence" "$body"
    [ "$API_STATUS" = 200 ] || api_fail
    result="$API_BODY"
  fi
  jq -r "$TOON_JQ"'
    "parked[\(length)]{id,effective,until}:",
    (.[] | "  " + ([.actor_id, .effective, (.override.until // null)] | map(tv) | join(","))),
    "help[1]:",
    "  Run `'"$SELF"' presence unpark '"$target"'` to return to the default"
  ' <<<"$result"
}

presence_unpark() {
  local target=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -h|--help) presence_usage; return 0 ;;
      -*) usage_err "unknown flag for presence unpark: $1" "valid flags for presence unpark: --help" ;;
      *) [ -z "$target" ] || usage_err "presence unpark takes one target, got a second: $1" "$SELF presence unpark <id|group|all>"
         target="$1"; shift ;;
    esac
  done
  [ -n "$target" ] || usage_err "presence unpark needs a target" "$SELF presence unpark <id|group|all>"
  [[ "$target" =~ $ID_RE ]] || usage_err "not an actor id, group or all: $target" "$SELF presence ls"

  presence_need; presence_connect; presence_token; presence_actors
  presence_resolve "$target"

  # There is no group DELETE, so a group is removed one member at a time. An
  # actor with no override is already at its default and is left alone, which
  # makes a repeat of this command a no-op rather than an error.
  local id rows=() unchanged=()
  for id in "${TARGET_IDS[@]}"; do
    if ! jq -e --arg id "$id" '.[] | select(.id == $id) | .override' <<<"$ACTORS" >/dev/null; then
      unchanged+=("$id")
      continue
    fi
    api DELETE "/v1/actors/$id/presence"
    if [ "$API_STATUS" != 200 ]; then
      [ "${#rows[@]}" -eq 0 ] || printf 'unparked_before_failure: %s\n' "$(printf '%s\n' "${rows[@]}" | cut -d, -f1 | paste -sd, -)"
      api_fail
    fi
    rows+=("$(jq -r "$TOON_JQ"'[.actor_id, .effective] | map(tv) | join(",")' <<<"$API_BODY")")
  done
  if [ "${#rows[@]}" -eq 0 ]; then
    echo "unparked: 0 of ${#TARGET_IDS[@]} had an override, nothing to do (no-op)"
    return 0
  fi
  echo "unparked[${#rows[@]}]{id,effective}:"
  printf '  %s\n' "${rows[@]}"
  [ "${#unchanged[@]}" -eq 0 ] || echo "unchanged[${#unchanged[@]}]: $(IFS=,; echo "${unchanged[*]}")"
}
```

Replace the whole `cmd_presence` function with:

```bash
cmd_presence() {
  case "${1:-}" in
    ""|ls)  shift || true; presence_ls "$@" ;;
    park)   shift; presence_park "$@" ;;
    unpark) shift; presence_unpark "$@" ;;
    -h|--help|help) presence_usage ;;
    *) usage_err "unknown presence command: $1" "$SELF presence --help" ;;
  esac
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bash tools/tests/test-mc-presence.sh && bash tools/tests/test-mc.sh && shellcheck -s bash tools/mc tools/tests/*.sh`
Expected: the eight Task 4 lines, then

```
  ok: parking a group sends one group write with the duration
  ok: parking one actor writes against the version it read
  ok: a conflicting edit is shown, not overwritten
  ok: a missing reason, bad duration, bad target or unknown flag exits 2 before any call
  ok: an unknown target is refused before anything is written
  ok: unparking a group removes each member's override
  ok: unparking what is not parked is an explicit no-op
  ok: a partial group unpark reports what it did before failing
PASS
```

`test-mc.sh` ends in `PASS`, and shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
git add tools/mc tools/tests/test-mc-presence.sh
git commit -m "feat(tools): park and unpark actors from tools/mc"
```

---

### Task 6: The "Parking actors" runbook

**Files:**
- Modify: `README.md` (insert `### Parking actors` before `### Reading the mob census`; add a warning to "Relocating a bot" step 1, after line 150)
- Test: `tools/tests/test-mc-presence.sh` (append)

**Interfaces:**
- Consumes: the CLI grammar from Tasks 4-5, the Secret/Service/value names from Tasks 2-3, and the chat commands and timings from the contract.
- Produces: the runbook the spec's "Operations / Runbook" asks for. `replicas: 0` stays documented for relocation and maintenance.

The spec says this section replaces the `replicas: 0` advice "for gameplay use". Relocating a bot is not gameplay use, and parking would break it: the agent's loop kicks a parked actor's gamertag about 20 seconds after the park, and that gamertag is also the one the human signs in as to move the character. "Relocating a bot" therefore keeps `replicas: 0` and gains a warning.

- [ ] **Step 1: Write the failing test**

In `tools/tests/test-mc-presence.sh`, insert immediately before the final `echo "PASS"`:

```bash
# --- the runbook's own commands --------------------------------------------------
# README's "Parking actors" is where an operator copies these from, so each one
# must still parse. Anything but a usage error (exit 2) is fine here: the fakes
# decide the outcome, the grammar is what is under test.
mapfile -t documented < <(grep -oE '^tools/mc presence[^#]*' "$here/README.md" | sed 's/ *$//')
[ "${#documented[@]}" -ge 3 ] || fail "README shows fewer than three tools/mc presence commands"
for line in "${documented[@]}"; do
  mapfile -t argv < <(python3 -c 'import shlex, sys; print("\n".join(shlex.split(sys.argv[1])[1:]))' "$line")
  set +e
  out="$(run bash "$mc" "${argv[@]}")"; rc=$?
  set -e
  [ "$rc" != 2 ] || fail "README documents a command the CLI rejects: $line -> $out"
done
echo "  ok: every tools/mc presence command in the README parses"
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bash tools/tests/test-mc-presence.sh`
Expected: every Task 4-5 line, then `FAIL: README shows fewer than three tools/mc presence commands`.

- [ ] **Step 3: Write the runbook**

In `README.md`, insert immediately before the line `### Reading the mob census` (it follows "Why the console's origin check is off"):

````markdown
### Parking actors

The agent and both AFK bots are *actors*: accounts that put a player into the
world and keep the chunks around it loaded and ticking. Parking an actor
disconnects it, and that frees those chunks for Bedrock's global limits. It
does not move the actor. Lowering a bot's `viewDistance` does not do this,
because ticking follows the server's simulation distance around every player.

The actors are listed once, in `global.actors` in `values.yaml`. Each entry's
`defaultState` is the baseline git owns. A park is an *override* on top of
it, stored in Postgres with who set it, why, and usually when it expires. It
survives pod restarts, rollouts and leader handovers. Removing the override
always returns the actor to what git says.

Three ways in, all equivalent:

| From | Read | Park | Restore |
|---|---|---|---|
| In-game chat | `!presence` | `!park <target> [duration]` (operator) | `!unpark <target>` |
| `tools/mc` | `tools/mc presence` | `tools/mc presence park <target> --reason "<why>" [--for 2h]` | `tools/mc presence unpark <target>` |
| HTTP | `GET /v1/actors` | `PUT /v1/actors/{id}/presence`, `PUT /v1/groups/{group}/presence` | `DELETE /v1/actors/{id}/presence` |

`<target>` is an actor id (`agent`, `afk-bot-1`, `afk-bot-2`), a group
(`bots`) or `all`. Durations use Go syntax: `30m`, `2h`, `1h30m`.

```bash
tools/mc presence                                              # who is where, and why
tools/mc presence park bots --for 2h --reason "TPS recovery"  # both bots out for two hours
tools/mc presence unpark bots                                  # back to the git default now
```

`tools/mc presence` port-forwards to `<release>-server-agent-presence` and
authenticates with the `presence_token_tools_mc` key of the `<release>-presence`
Secret. It never prints the token. `MC_PRESENCE_TOKEN` and `MC_PRESENCE_URL`
override both, for use from outside the cluster's kubeconfig.

**What to expect.** Bots poll every 10 seconds, and about 20 seconds after an
actor is parked the agent kicks its gamertag if the server still lists it,
because Bedrock holds a session open after the client leaves. So a parked
actor is gone from `tools/mc players` within about 30 seconds. After an unpark
or an expiry it is back once its next poll lands and its login completes,
usually within 30 seconds. A deliberately parked actor does not page anyone.
An actor that should be present and is not still does.

**Parking the agent.** It leaves the world but keeps monitoring: TPS, the
scheduler, the server watcher and the policy loop all keep running. What stops
is chat. While the agent is parked nobody can use `!` commands or `@server`,
including `!unpark`. A chat park of the agent (`@server leave`, or `!park all`)
therefore always carries a way back: one hour unless given a duration, and it
wakes when any player joins. A park from `tools/mc` or the API carries only
what you give it. Give `--for` when you park `agent` or `all`, or be ready to
`tools/mc presence unpark agent` yourself.

**If the agent is down,** the bots keep acting on the last answer they got,
and a bot that never got one uses its `defaultState`. An agent outage never
makes the bots flap. Expiries and wakes wait for an agent to hold the leader
lock again.

**Not for relocating a bot.** While an actor is parked the agent kicks its
gamertag from the server, which would kick *you* if you are signed in as that
account to move it. Relocation keeps its own procedure, "Relocating a bot"
above, through `replicas: 0`.

**Changing a default** (an actor that should normally be away) is a PR to
`defaultState` in `global.actors`. That restarts the actor's own pod and the
agent, but not the server.

**Adding a bot** means its values block and Deployment template (see "The AFK
bot(s)"), its key in `actors.botKeys` in `templates/_actors.tpl`, an entry in
`global.actors`, and a token. The render refuses an enabled bot with no actor
entry. The token is a new `presence_token_<id with - as _>` property in the
`kv/minecraft-fwb` Vault document, added the same way as the others (below).

**Tokens.** Three properties in `kv/minecraft-fwb`: `presence_token_afk_bot_1`,
`presence_token_afk_bot_2` and `presence_token_tools_mc`. The `<release>-presence`
ExternalSecret assembles them into the agent's `PRESENCE_TOKENS` and each
bot's own `PRESENCE_TOKEN`, so a bot's copy can never differ from the agent's.
Create or rotate them yourself, in your own terminal, never through an agent's
shell (`AGENTS.md`). Use `patch`, not `put`: `put` replaces the whole document,
and with it the console bridge's secrets.

```bash
vault kv patch kv/minecraft-fwb \
  presence_token_afk_bot_1="$(openssl rand -hex 32)" \
  presence_token_afk_bot_2="$(openssl rand -hex 32)" \
  presence_token_tools_mc="$(openssl rand -hex 32)"
kubectl -n jdwillmsen-prd annotate externalsecret jdwillmsen-minecraft-fwb-prd-presence \
  force-sync="$(date +%s)" --overwrite
kubectl -n jdwillmsen-prd get externalsecret jdwillmsen-minecraft-fwb-prd-presence   # SecretSynced / True
```

The tokens are hex because ESO substitutes them into a JSON string. After a
rotation, delete the agent and bot pods so they read the new values. Env from
a Secret is fixed at pod start. Use `kubectl delete pod`, not `kubectl rollout
restart`: selfHeal reverts the restart annotation.

**The switch** is `global.presence.enabled`. Off, the agent mounts no `/v1`
routes, the bots run exactly as they did before, and the bridge refuses every
`kick`. It needs agent, bridge and bot images that read the variables.
Turning it on or off restarts the server pod once, because the bridge sidecar's
kick list changes. The deploy-announce hook counts that down like any other
server restart.
````

In "Relocating a bot", after step 1's last line (`   login for no reason.`, line 150), add:

```markdown

   Do not park the bot for this instead (see "Parking actors"): the agent
   kicks a parked actor's gamertag from the server, and that would be you.
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash tools/tests/test-mc-presence.sh && shellcheck -s bash tools/tests/test-mc-presence.sh`
Expected: all earlier lines, then

```
  ok: every tools/mc presence command in the README parses
PASS
```

- [ ] **Step 5: Commit**

```bash
git add README.md tools/tests/test-mc-presence.sh
git commit -m "docs(minecraft-fwb): add the parking actors runbook"
```

---

### Task 7: Prove PR A changes nothing live, run every gate, open the PR

**Files:** none changed. This task produces evidence for the PR description.

**Interfaces:**
- Consumes: Tasks 1-6 committed on the branch.
- Produces: PR A, whose description carries the render comparison below.

- [ ] **Step 1: Compare the full production render against `origin/main`**

```bash
git fetch origin main
work="$(mktemp -d)"
git archive origin/main charts/minecraft-fwb | tar -x -C "$work"
r() {
  helm template jdwillmsen-minecraft-fwb-prd "$1" -n jdwillmsen-prd \
    -f "$1/values.yaml" -f "$1/values-prd.yaml" -f "$1/values-console-bridge.yaml"
}
r "$work/charts/minecraft-fwb" > "$work/before.yaml"
r charts/minecraft-fwb > "$work/after.yaml"
python3 - "$work/before.yaml" "$work/after.yaml" <<'PY'
import sys, yaml
def objs(path):
    return {(d["kind"], d["metadata"]["name"]): d for d in yaml.safe_load_all(open(path)) if d}
before, after = objs(sys.argv[1]), objs(sys.argv[2])
print("added:", sorted(set(after) - set(before)))
print("removed:", sorted(set(before) - set(after)))
print("changed:", sorted(k for k in before.keys() & after.keys() if before[k] != after[k]))
PY
echo "lines removed or changed in the text render: $(diff "$work/before.yaml" "$work/after.yaml" | grep -c '^<')"
rm -rf "$work"
```

Expected, exactly:

```
added: [('ExternalSecret', 'jdwillmsen-minecraft-fwb-prd-presence')]
removed: []
changed: []
lines removed or changed in the text render: 0
```

This shows that no Deployment, PVC, StatefulSet or digest changed (the digests live in the `-server-spec-hash` ConfigMap and the deploy-announce Job's env). `<release>-afk-bot`, `<release>-afk-bot-2` and `<release>-server-agent` and their PVCs render byte-identical, the PreSync hook announces nothing, and no pod restarts. If `changed:` lists anything, stop and find which template moved before going further.

- [ ] **Step 2: Run the local mirror of CI**

Run: `tools/ci-local`
Expected: the final line reads `all 10 gates passed`. The `smoke-test` gate boots the Bedrock image with Docker, because `values.yaml` changed against `origin/main`. `gitleaks` and `trivy` also need Docker. If any gate prints `SKIP`, install the missing tool and rerun; a skipped gate is not a pass.

- [ ] **Step 3: Push and open PR A**

Use the repo's ship path (`/no-mistakes`, or `git push -u origin HEAD` then `gh pr create`). Title: `feat(minecraft-fwb): actor list, presence plumbing and tools/mc presence (off)`. The body must include:
- the Step 1 output verbatim, and the statement that nothing restarts on sync;
- that `global.presence.enabled` is `false`, and what turns it on (Tasks 8-9);
- that the new ExternalSecret will read `SecretSyncedError` until Task 8's Vault properties exist, and that this is harmless because nothing reads it.

Every CI check must be green and every review thread resolved before merge. Merges are rebase-only. After merging, run `git pull --ff-only` on the local `main`.

---

### Task 8: Put the presence tokens into Vault (human terminal)

**Files:** none. This task creates credentials, so **the agent must not run the Vault command**, through its own shell or through Claude Code's `!` mode (`~/AGENTS.md`, "Credential-minting commands").

**Interfaces:**
- Consumes: the `<release>-presence` ExternalSecret from PR A (it can be merged before or after this task).
- Produces: Vault properties `presence_token_afk_bot_1`, `presence_token_afk_bot_2` and `presence_token_tools_mc` in `kv/minecraft-fwb`, and a Secret `jdwillmsen-minecraft-fwb-prd-presence` that is `SecretSynced`.

- [ ] **Step 1: Hand the human the command**

Give the human this block to run in a terminal outside the agent session (a separate SSH login or a tmux pane that no agent reads):

```bash
vault kv patch kv/minecraft-fwb \
  presence_token_afk_bot_1="$(openssl rand -hex 32)" \
  presence_token_afk_bot_2="$(openssl rand -hex 32)" \
  presence_token_tools_mc="$(openssl rand -hex 32)"
```

`patch`, not `put`. `put` replaces the whole document, and `console_websocket_password` and `console_bridge_token` live in it. Losing them takes the console path down. Hex output is JSON-safe, which the ExternalSecret's template needs. Wait for the human to confirm they ran it.

- [ ] **Step 2: Once PR A is synced, force a refresh and confirm the sync**

```bash
kubectl -n jdwillmsen-prd annotate externalsecret jdwillmsen-minecraft-fwb-prd-presence \
  force-sync="$(date +%s)" --overwrite
kubectl -n jdwillmsen-prd get externalsecret jdwillmsen-minecraft-fwb-prd-presence
```

Expected: `STATUS` `SecretSynced`, `READY` `True`. `SecretSyncedError` means a property is missing or misspelled. Go back to the human, and do not continue on weaker evidence.

- [ ] **Step 3: Check the Secret's shape without printing a value**

```bash
kubectl -n jdwillmsen-prd get secret jdwillmsen-minecraft-fwb-prd-presence -o json \
  | jq -r '.data | keys | join(",")'
kubectl -n jdwillmsen-prd get secret jdwillmsen-minecraft-fwb-prd-presence -o json | jq -e '
  (.data | map_values(@base64d)) as $d
  | ($d.presence_tokens | fromjson) as $t
  | ($t | length) == 3
    and ([$t[] | .token | test("^[0-9a-f]{64}$")] | all)
    and ($t[] | select(.name == "afk-bot-1") | .token) == $d.presence_token_afk_bot_1
    and ($t[] | select(.name == "afk-bot-2") | .token) == $d.presence_token_afk_bot_2
    and ($t[] | select(.name == "tools-mc") | .token) == $d.presence_token_tools_mc' >/dev/null \
  && echo "presence secret consistent"
```

Expected:

```
presence_token_afk_bot_1,presence_token_afk_bot_2,presence_token_tools_mc,presence_tokens
presence secret consistent
```

The second command decodes inside `jq` and prints only the verdict. No token reaches the terminal.

---

### Task 9: Turn presence on with the released images (PR B)

**Files:**
- Modify: `charts/minecraft-fwb/values.yaml`: the console-bridge sidecar env (after `HTTP_ADDR` `":8766"`), the bridge image literal, `bot.image.tag`, `bot2.image.tag`, `agent.image.tag` and the comments above them, and `global.presence.enabled`
- Test: `tools/tests/test-actors.sh` (append)

Work on a **new** branch in a fresh worktree off `origin/main` after PR A has merged, for example `feat/<ticket>-actor-presence-enable`. Line numbers moved in PR A, so every edit below is located by its content.

**Interfaces:**
- Consumes: everything PR A shipped; Task 8's synced Secret; the images released by plan 3 (minecraft-server-agent), plan 4 (mc-console-bridge) and plan 5 (minecraft-afk-bot); the V8 tables from plan 2.
- Produces: production running presence. `BRIDGE_KICKABLE` is set on the `console-bridge` sidecar. `global.presence.enabled: true`.

- [ ] **Step 1: Check every prerequisite, and stop on the first miss**

```bash
kubectl -n jdwillmsen-prd get externalsecret jdwillmsen-minecraft-fwb-prd-presence \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
primary="$(kubectl get pod -n database -o name \
  -l cnpg.io/cluster=platform-postgresql-cluster-prd,cnpg.io/instanceRole=primary)"
kubectl exec -n database "$primary" -c postgres -- psql -d jdwillmsen_prd -Atc \
  "select to_regclass('minecraft.presence_overrides') is not null
      and to_regclass('minecraft.presence_status') is not null"
```

Expected: `True`, then `t`. `f` means plan 2's migration has not run. The agent would then answer every presence call with 503, so do not continue.

- [ ] **Step 2: Read the released versions; never guess them**

The version for each image is the one released by its plan: plan 3 for the agent, plan 4 for the bridge, plan 5 for the bot. Read it from `gh release list -R jdwillmsen/<repo> -L 1`. The bridge and bot repos have published tags without GitHub releases so far (`gh release list` prints nothing for them today), and the agent repo also carries `presenceapi/v*` module tags. So fall back to the newest plain `vX.Y.Z` tag, and prove below that it carries the feature:

```bash
latest_version() {
  local repo="$1" tag
  tag="$(gh release list -R "jdwillmsen/$repo" -L 1 --json tagName --jq '.[0].tagName // empty' 2>/dev/null)"
  if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    tag="$(git ls-remote --tags --refs --sort=-v:refname "https://github.com/jdwillmsen/$repo" 'v*' \
      | sed 's|.*refs/tags/||' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)"
  fi
  printf '%s\n' "${tag#v}"
}
AGENT_VERSION="$(latest_version minecraft-server-agent)"
BRIDGE_VERSION="$(latest_version mc-console-bridge)"
BOT_VERSION="$(latest_version minecraft-afk-bot)"
echo "agent=$AGENT_VERSION bridge=$BRIDGE_VERSION bot=$BOT_VERSION"

carries() {  # repo version needle: counts the needle in that tag's source
  gh api "repos/jdwillmsen/$1/tarball/v$2" | tar -xzO 2>/dev/null | grep -ac -- "$3"
}
carries minecraft-server-agent "$AGENT_VERSION" PRESENCE_TOKENS
carries mc-console-bridge "$BRIDGE_VERSION" BRIDGE_KICKABLE
carries minecraft-afk-bot "$BOT_VERSION" PRESENCE_URL

for image in minecraft-server-agent:$AGENT_VERSION mc-console-bridge:$BRIDGE_VERSION minecraft-afk-bot:$BOT_VERSION; do
  docker manifest inspect "ghcr.io/jdwillmsen/$image" >/dev/null && echo "published: $image"
done
```

Expected: three versions newer than the ones in `values.yaml` today (agent `0.20.0`, bridge `0.2.0`, bot `1.1.0`). Each `carries` count must be at least `1`, and there must be three `published:` lines. A count of `0` means that plan has not released yet. Stop and wait for it; do not pick an older or newer tag by hand.

- [ ] **Step 3: Write the failing test**

In `tools/tests/test-actors.sh`, insert immediately before the final `echo "PASS"`:

```bash
python3 - "$work/on.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
sts = next(d for d in docs if d["kind"] == "StatefulSet")
bridge = next(c for c in sts["spec"]["template"]["spec"]["containers"] if c["name"] == "console-bridge")
env = {e["name"]: e for e in bridge["env"]}
assert env["BRIDGE_KICKABLE"]["value"] == "JDWServerAgent,LightBlaz3,Dotablaze7321", env.get("BRIDGE_KICKABLE")
PY
echo "  ok: the bridge may kick every actor and nobody else"
```

Run: `bash tools/tests/test-actors.sh`
Expected: FAIL with a `KeyError: 'BRIDGE_KICKABLE'` traceback after `ok: only what reaches a workload moves its digest`.

- [ ] **Step 4: Give the bridge its kick list**

In `charts/minecraft-fwb/values.yaml`, inside `minecraft-bedrock.sidecarContainers`, in the `console-bridge` container's `env`, directly after:

```yaml
        - name: HTTP_ADDR
          value: ":8766"
```

insert:

```gotemplate
        {{- if .Values.global.presence.enabled }}
        # The only gamertags `kick` is accepted for, so a leaked bridge token
        # cannot remove a real player. Built from global.actors, the list the
        # agent parks from.
        - name: BRIDGE_KICKABLE
          value: {{ include "actors.kickable" . | quote }}
        {{- end }}
```

This string is `tpl`-rendered in the subchart. `.Values.global.presence.enabled` and `include "actors.kickable" .` both resolve there, because `global` is merged into the subchart's values and named templates are shared across charts (checked by rendering). The comment sits inside the `if`, so a render with presence off carries no trace of it. `actors.kickable` joins the gamertags unquoted, and Task 1's validation guarantees none contains a character the bridge rejects.

Run: `bash tools/tests/test-actors.sh`
Expected: the earlier lines, then `  ok: the bridge may kick every actor and nobody else` and `PASS`.

- [ ] **Step 5: Commit**

```bash
git add charts/minecraft-fwb/values.yaml tools/tests/test-actors.sh
git commit -m "feat(minecraft-fwb): let the console bridge kick actors and only actors"
```

- [ ] **Step 6: Bump the three images to the versions from Step 2**

In the same shell as Step 2 (the variables must be set; `echo "$AGENT_VERSION $BRIDGE_VERSION $BOT_VERSION"` must print three versions):

```bash
python3 - "$AGENT_VERSION" "$BOT_VERSION" "$BRIDGE_VERSION" <<'PY'
import re, sys
agent, bot, bridge = sys.argv[1:4]
path = "charts/minecraft-fwb/values.yaml"
s = open(path).read()
def bump(s, block, version):
    start = s.index(f"\n{block}:\n")
    m = re.compile(r'^    tag: "[^"]+"$', re.M).search(s, start)
    return s[:m.start()] + f'    tag: "{version}"' + s[m.end():]
for block, version in (("bot", bot), ("bot2", bot), ("agent", agent)):
    s = bump(s, block, version)
s, n = re.subn(r'ghcr\.io/jdwillmsen/mc-console-bridge:[^"]+"',
               f'ghcr.io/jdwillmsen/mc-console-bridge:{bridge}"', s)
assert n == 1, f"expected one bridge image literal, found {n}"
open(path, "w").write(s)
PY
yq '.agent.image.tag, .bot.image.tag, .bot2.image.tag' charts/minecraft-fwb/values.yaml
grep -o 'mc-console-bridge:[0-9.]*' charts/minecraft-fwb/values.yaml
```

Expected: the agent, bot and bot versions from Step 2, then `mc-console-bridge:` followed by the bridge version. The script edits only the first `    tag:` inside each named block, which is that block's `image.tag`.

Then record why each tag moved. The comment above a tag is the only place anyone reads what a restart will change (see the agent's `image` comments). Directly above the agent's `    tag:` line, after the paragraph ending `census run instead of a hand-pulled world and a scratch scanner.`, add:

```yaml
    #
    # The presence release. Monitoring -- TPS sampling, the scheduler, the
    # server watcher -- follows the leader lock instead of the in-world
    # session, so the agent can leave the world and keep watching it. It adds
    # the /v1 presence API, the loop that expires, wakes and kicks parked
    # actors, !presence, !park, !unpark and @server leave, and the
    # mc_presence_* metrics. All of it reads global.actors and the presence
    # tokens, and was switched on in the same change as this bump. Needs the
    # V8 presence tables, checked present first.
```

Directly above `bot.image.tag`, after the line `    # the device-code login by hand. See README, "Relocating a bot".`, add:

```yaml
    #
    # The presence release adds the reconciler: with PRESENCE_URL set, the bot
    # asks the agent every 10 seconds whether it should be in the world, and
    # closes its session and stops reconnecting while parked. With it unset
    # the bot behaves exactly as before.
```

Directly above `bot2.image.tag`, after the line `    # until the farm says the Go one works.`, add:

```yaml
    #
    # The presence release, same as bot above and for the same reason.
```

Directly above the console-bridge `image:` line inside `sidecarContainers`, after `      # below) and is a literal for that reason.`, add:

```yaml
      #
      # The presence release accepts `kick` for the gamertags in
      # BRIDGE_KICKABLE below and refuses it for everyone else.
```

- [ ] **Step 7: Switch presence on**

In `global.presence`, replace:

```yaml
    # and a synced secret -- see README, "Parking actors".
    enabled: false
```

with:

```yaml
    # and a synced secret -- see README, "Parking actors".
    #
    # On once the agent, bridge and bot releases that read these variables
    # were published and the presence ExternalSecret was observed SecretSynced
    # -- the order every secret in this chart follows.
    enabled: true
```

- [ ] **Step 8: Run every suite and prove the blast radius**

```bash
bash tools/tests/test-actors.sh && bash tools/tests/test-mc-presence.sh && helm lint charts/minecraft-fwb
```

Expected: both suites end in `PASS`. The actors suite sets presence on and off explicitly, so it holds with either shipped value. Lint prints `1 chart(s) linted, 0 chart(s) failed`.

Then rerun Task 7 Step 1's comparison script unchanged, against `origin/main`, which is now PR A. Expected:

```
added: [('Service', 'jdwillmsen-minecraft-fwb-prd-server-agent-presence')]
removed: []
changed: [('ConfigMap', 'jdwillmsen-minecraft-fwb-prd-server-spec-hash'), ('CronJob', 'jdwillmsen-minecraft-fwb-prd-census'), ('Deployment', 'jdwillmsen-minecraft-fwb-prd-afk-bot'), ('Deployment', 'jdwillmsen-minecraft-fwb-prd-afk-bot-2'), ('Deployment', 'jdwillmsen-minecraft-fwb-prd-join-probe'), ('Deployment', 'jdwillmsen-minecraft-fwb-prd-server-agent'), ('Job', 'jdwillmsen-minecraft-fwb-prd-deploy-announce'), ('StatefulSet', 'jdwillmsen-minecraft-fwb-prd-minecraft-bedrock')]
```

The last line reads `lines removed or changed in the text render:` with a non-zero count. That is expected here. The census CronJob and the join probe change only because they run the agent image. The Deployment and PVC names are the same set as before, so the token caches are kept. Finally run `tools/ci-local` and expect `all 10 gates passed`.

- [ ] **Step 9: Commit and open PR B**

```bash
git add charts/minecraft-fwb/values.yaml
git commit -m "feat(minecraft-fwb): turn actor presence on with the releases that read it"
```

Open the PR through the repo's ship path. The description must include the Step 2 output (the three versions, the `carries` counts and the `published:` lines), the Step 8 comparison, and this warning: **merging restarts the server pod once.** The console-bridge sidecar's image and env change, so the StatefulSet's pod template changes, and the deploy-announce PreSync hook counts that down for players. The agent and both bots restart too. Merge at a time of low player activity, and keep `docs/incidents/2026-08-31-bedrock-crash-on-player-join.md` in mind, as for any server restart. Merge only after review with all checks green. Rebase-only.

---

### Task 10: Live verification after the ArgoCD sync

**Files:** none in the repo. The watch script lives in the session scratchpad (`$WATCH` below). Evidence goes into PR B's thread and the ticket (Jira is the source of truth for this work).

**Interfaces:**
- Consumes: PR B merged and synced; `tools/mc presence` and `tools/mc players` from the merged `main`; the agent's `mc_presence_*` metrics (contract).
- Produces: measured timings for park-to-gone and expiry-to-back, run against production.

Run every command from the jdw-deployments checkout on `main` (after `git pull --ff-only`), with `kubectl` pointed at the production cluster. The polling script runs detached (`run_in_background`), and its output is read when it exits. Foreground sleeps are not used.

- [ ] **Step 1: Confirm the sync landed**

```bash
kubectl -n argocd get application jdwillmsen-minecraft-fwb-prd \
  -o jsonpath='{.status.sync.status} {.status.sync.revision}{"\n"}'
kubectl -n argocd get application jdwillmsen-minecraft-fwb-prd -o json | jq -r '
  .status.resources[] | select(.health.status != null and .health.status != "Healthy")
  | "\(.kind)/\(.name) \(.health.status)"'
for d in afk-bot afk-bot-2 server-agent; do
  kubectl -n jdwillmsen-prd get deploy "jdwillmsen-minecraft-fwb-prd-$d" \
    -o jsonpath='{.metadata.name} {.spec.template.spec.containers[0].image} {.status.readyReplicas}{"\n"}'
done
kubectl -n jdwillmsen-prd get pod jdwillmsen-minecraft-fwb-prd-minecraft-bedrock-0 \
  -o jsonpath='{range .spec.containers[*]}{.name} {.image}{"\n"}{end}'
```

Expected: `Synced` at PR B's merge commit. No unhealthy resource belongs to this change. The Application read `Degraded` before this work because of an unrelated failed `version-check` Job run, so judge by resource and not by the rolled-up status. The three Deployments show the Task 9 versions with `1` ready replica. The server pod lists `console-bridge ghcr.io/jdwillmsen/mc-console-bridge:<bridge version>`.

- [ ] **Step 2: Confirm the bots kept their sign-in**

```bash
for d in afk-bot afk-bot-2; do
  echo "$d $(kubectl -n jdwillmsen-prd logs deploy/jdwillmsen-minecraft-fwb-prd-$d --since=30m | grep -ac 'Authenticate at')"
done
```

Expected: `afk-bot 0` and `afk-bot-2 0`. A non-zero count means a pod is waiting on a device-code login ("The AFK bot(s)" in README). That is a human step and blocks everything below.

- [ ] **Step 3: Baseline**

```bash
tools/mc presence
tools/mc players
```

Expected: `count: 3`. The rows read `agent,present,default,null,true`, `afk-bot-1,present,default,null,true` and `afk-bot-2,present,default,null,true`. `connected` is `true` for all three: the bots report through the API, and the agent writes its own status. `groups[2]: bots,all`. `tools/mc players` lists `JDWServerAgent`, `LightBlaz3` and `Dotablaze7321`.

- [ ] **Step 4: Write the watch script**

Save this as `presence-watch.sh` in the session's scratchpad directory, not in the repo. Every command below that runs it starts with `WATCH=` set to that file's absolute path, because shell variables do not persist between tool calls:

```bash
#!/usr/bin/env bash
# Times how long until a set of gamertags leaves (or rejoins) the server list.
#   presence-watch.sh gone|back <deadline-epoch> <gamertag>...
# Prints one line per poll and a verdict; exits 0 when the condition held
# before the deadline, 1 otherwise.
set -uo pipefail
mode="$1" deadline="$2"; shift 2
start="$(date +%s)"
while :; do
  online="$(tools/mc players)"
  now="$(date +%s)"
  hits=0
  for tag in "$@"; do grep -qxF -- "  - $tag" <<<"$online" && hits=$((hits + 1)); done
  echo "t+$((now - start))s present=$hits/$#"
  if { [ "$mode" = gone ] && [ "$hits" -eq 0 ]; } || { [ "$mode" = back ] && [ "$hits" -eq "$#" ]; }; then
    echo "verdict: $mode at $(date -u +%H:%M:%SZ), $((now - start))s after the watch began"
    exit 0
  fi
  [ "$now" -lt "$deadline" ] || { echo "verdict: not $mode by the deadline"; exit 1; }
  sleep 3
done
```

- [ ] **Step 5: Park both bots for five minutes, and time their exit**

```bash
tools/mc presence park bots --for 5m --reason "presence rollout verification"
bash "$WATCH" gone "$(( $(date +%s) + 60 ))" LightBlaz3 Dotablaze7321
```

Run the second line in the background, right after the first. Expected: the park prints `parked[2]{id,effective,until}:` with two `parked` rows whose `until` is about five minutes out. Write that `until` down. The watch ends with `verdict: gone at ..., Ns after the watch began`, and **N must be 30 or less** (bot poll 10s, plus the kick grace 20s for a session the server holds open). Then:

```bash
tools/mc presence
kubectl -n jdwillmsen-prd port-forward svc/jdwillmsen-minecraft-fwb-prd-server-agent-metrics :9090 &
```

Read the local port from the `Forwarding from 127.0.0.1:<port>` line, then run `curl -s localhost:<port>/metrics | grep -E '^mc_presence_(desired|observed)'` and stop the port-forward.

Expected: both bot rows read `parked,"api:tools-mc","<until>",false`. `mc_presence_desired{actor="afk-bot-1"} 0` and `{actor="afk-bot-2"} 0`, and the matching `mc_presence_observed` are `0`. The agent's two series are `1`.

If the leader is the other replica, the scrape answers without `mc_presence_*` (the series are leader-only). In that case port-forward to the pod whose `mc_agent_leader` is `1`.

- [ ] **Step 6: Let the park expire, and time their return**

```bash
until_epoch="$(date -d "<until from step 5>" +%s)"
bash "$WATCH" back "$(( until_epoch + 60 ))" LightBlaz3 Dotablaze7321
```

Run it in the background. Expected: `verdict: back at HH:MM:SSZ`, and **that time is no more than 30 seconds after `until`** (policy tick 10s, bot poll 10s, then its login). Then `tools/mc presence` shows both bots `present,default,null,true`, and `mc_presence_desired`/`observed` are back to `1`.

- [ ] **Step 7: Park the agent, and prove the bots still follow the API**

This checks Review Focus 1 live. While the agent is parked it is not ready, so only `publishNotReadyAddresses` keeps the bots' route open.

```bash
tools/mc presence park agent --for 10m --reason "presence rollout verification: agent"
bash "$WATCH" gone "$(( $(date +%s) + 60 ))" JDWServerAgent
kubectl -n jdwillmsen-prd get endpoints jdwillmsen-minecraft-fwb-prd-server-agent-presence \
  -o jsonpath='ready={.subsets[*].addresses[*].ip} notReady={.subsets[*].notReadyAddresses[*].ip}{"\n"}'
```

Expected: `JDWServerAgent` is gone within 30 seconds. The endpoints line lists the agent pod's IP, most likely under `notReady=`. An empty line means the bots are cut off. Stop and fix the Service before going on.

With the agent still parked, move one bot through a full park and unpark:

```bash
tools/mc presence park afk-bot-2 --reason "presence rollout verification: bot while agent parked"
bash "$WATCH" gone "$(( $(date +%s) + 60 ))" Dotablaze7321
tools/mc presence unpark afk-bot-2
bash "$WATCH" back "$(( $(date +%s) + 60 ))" Dotablaze7321
```

Expected: both verdicts within 30 seconds. The bot read both changes through the Service while its only backing pod was not ready.

Finally, bring the agent back by hand rather than waiting for the timer. This exercises `unpark` on the agent:

```bash
tools/mc presence unpark agent
bash "$WATCH" back "$(( $(date +%s) + 90 ))" JDWServerAgent
```

Expected: `unparked[1]{id,effective}:` then `  agent,present`, and the agent is back in the player list within the deadline, announcing its return in chat. Its login is a real Xbox sign-in, so allow up to the 90 seconds given.

- [ ] **Step 8: Confirm nothing paged**

```bash
kubectl -n monitoring port-forward svc/platform-kube-prometheus-s-prometheus :9090 &
```

Then run `curl -s 'localhost:<port>/api/v1/query' --data-urlencode 'query=ALERTS{alertname=~"JdwillmsenMinecraft.*",alertstate="firing"}' | jq -r '.data.result[].metric.alertname'` and stop the port-forward.

Expected: no alert that started during steps 5-7. The existing alert windows (30 minutes for the bots) are longer than these parks. Plan 7 rewrites the alerts to use `mc_presence_desired`.

- [ ] **Step 9: Record the evidence**

Post, as a comment on PR B and on the ticket: the Step 1 image lines, the three watch verdicts with their `until` times, the Step 5 metric lines and the Step 7 endpoints line. Then transition the ticket per the team's flow. If any timing missed its 30-second bound, record the measured value, open a follow-up in the owning repo (the agent for kicks and expiry, the bot for polling), and leave presence on. A slow return is no worse than today's `replicas` flip.

## Deviations from spec/contract

1. **The actor list is `global.actors`, not an ordinary top-level values list.** The console-bridge sidecar that needs the gamertags is a `tpl`-rendered string inside the vendored `minecraft-bedrock` subchart (`values.yaml:1-6`, `253`, `280-284`), and only `.Values.global.*` reaches that scope. It is still one list. The consequence is that `global.*` feeds the server's deploy-announce digest, which Task 3 narrows so that editing an actor does not announce a server restart.
2. **Bot Deployments are not generated from the list** (spec, "Domain model": "The chart renders both the bot Deployments and the agent's actor registry from it"). README ("The AFK bot(s)") records why `bot`/`bot2` are separate blocks: generating them would rename `<release>-afk-bot` and orphan its signed-in token cache, and this phase must keep every Deployment and PVC name byte-identical. Each `afk-bot` actor instead names its block through `valuesKey`, and the render fails if an enabled bot block has no actor. The spec's invariant, that no bot exists unregistered and no actor exists without a bot, holds at render time.
3. **`PRESENCE_URL` targets a new Service, `http://<release>-server-agent-presence.<ns>.svc.cluster.local:9090`,** not the contract's example `http://<release>-server-agent:8080`. No Service of that name exists. The agent's `HTTP_ADDR` is `:9090` (`agent.metrics.port`), and the only agent Service (`<release>-server-agent-metrics`) carries ready pods only. The agent's `/readyz` answers 503 for a live agent without a Bedrock session (`internal/httpapi/http.go:80-92`), so the bots would lose the API exactly when the agent parks itself. The new Service sets `publishNotReadyAddresses: true`. The contract calls its URL an example, so no cross-repo name changes.
4. **Tokens come from a Vault ExternalSecret, not a hand-made `existingSecret` like `ANNOUNCE_API_TOKEN`.** `ANNOUNCE_API_TOKEN` reads a human-created Secret named by `agent.announceApi.existingSecret`, which is `""` (off) in production. Presence keeps that pattern's consumption side (a `secretKeyRef` added only when the feature is on) and sources the Secret the way the console-bridge secret is sourced: a `vault` ClusterSecretStore ExternalSecret on `kv/minecraft-fwb`, rendered before anything reads it. ESO's `target.template` builds `PRESENCE_TOKENS` and each bot's `PRESENCE_TOKEN` from the same Vault property, so the two cannot drift. Values are still created only in a human's terminal (Task 8).
5. **Operator token `tools-mc`, and `set_by` recorded as `api:tools-mc`.** The spec's "Interfaces / Data" lists a `cli:<token-name>` prefix. The coordinator's contract update drops it: the CLI is an ordinary API client. Bot tokens are named after their actor ids.
6. **Rollout step 6 ships as two PRs with a human step between them,** not one. PR A (inert, byte-identical except one ExternalSecret) lands the secret before any consumer, following the house rule from `docs/incidents/2026-09-06-fwb-console-bridge-secret.md`. PR B switches presence on together with the image bumps. The bridge's `BRIDGE_KICKABLE` also waits for PR B: editing `sidecarContainers` moves the server digest, and PR A would otherwise announce a server restart that does not happen.
7. **Image versions for the bridge and the bot cannot come from `gh release list` alone.** `mc-console-bridge` and `minecraft-afk-bot` publish `vX.Y.Z` tags without GitHub releases (`gh release list` prints nothing for either today), and the agent repo carries `presenceapi/v*` module tags. Task 9 reads `gh release list -R jdwillmsen/<repo> -L 1` first, falls back to the newest plain `vX.Y.Z` tag, and requires proof that the tag's source contains the plan's variable and that the image is published.
8. **The CLI exits 2 on usage errors,** where the spec's "CLI" section says "exit codes 0 and 1". The user's global rule makes every agent-facing CLI follow AXI, which reserves 2 for usage errors so a caller can tell "asked wrongly" from "could not be done". Runtime failures exit 1 and no-ops exit 0, as the spec says.
9. **A park from the CLI carries no default expiry or `wake_on`, even for the agent.** The spec gives chat parks of the agent a one-hour timer and wake-on-join. The CLI stays a thin client over the API, so that default belongs to the agent (plan 3) if it applies to API writes at all. The runbook tells operators to pass `--for` when they park `agent` or `all`.
10. **"Relocating a bot" keeps `replicas: 0`.** The spec replaces that advice "for gameplay use", and relocation is not gameplay use. Parking would also break it: the agent kicks a parked actor's gamertag, which is the account the human signs in as. The runbook says so.
