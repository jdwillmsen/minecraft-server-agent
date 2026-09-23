# Actor presence, phase 7: presence-aware alerts. Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `JdwillmsenMinecraftAgentDisconnected` and `JdwillmsenMinecraftAfkBotsDegraded` so that they page only on an actor that should be present (`mc_presence_desired == 1`) and is not (`mc_presence_observed == 0`) for their existing windows (10m and 30m). Add a `JdwillmsenMinecraftActorParkedLong` warning on `mc_presence_override_age_seconds > 86400`.

**Architecture:** All changes are in the `jdwillmsen-tenant-alerts` PrometheusRule and its promtool unit tests in `jdwlabs/platform`. Each rewritten alert keeps the window inside the expression. It averages a per-actor *shortfall* series (`desired * (1 - observed)`, which is 1 for every minute an actor was wanted and absent) over the window through a 1m subquery, requires the current desired state to be 1, and uses a `for:` of half the window. A clean outage pages exactly at the old window, a flapping session still pages, and parking resolves the alert on the next evaluation. Each alert keeps one fallback branch for what the presence series cannot see. For the agent that is `mc_agent_connected`, used only while no presence series exists. For the bots it is the kube-state-metrics Deployment signal. Execute on a new branch in a fresh worktree off `origin/main` of `jdwlabs/platform`; Task 1 creates it. The PR is authored as `jdwlabs-agent-bot` so that jdwillmsen can approve it.

**Tech Stack:** Prometheus rule YAML (PrometheusRule CRD), PromQL, promtool 3.5.0 (`promtool check rules` / `promtool test rules`, pinned in `.github/workflows/validate.yml:108-122`), `tests/prometheus-rules/run.sh`, yamllint (CI config in `.github/workflows/validate.yml:26-48`), GitHub GraphQL `createCommitOnBranch` and the `gh` CLI.

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md` in minecraft-server-agent (sections "Operations > Alerts" and "Rollout" step 7). The cross-repo names come from `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md` ("Metrics (agent, leader only)").

## Global Constraints

- Metric names and the label are fixed by the contract: `mc_presence_desired{actor}` (1 present, 0 parked), `mc_presence_observed{actor}` (1 connected, 0 otherwise), and `mc_presence_override_age_seconds{actor}` (age of an override with no `until`; absent otherwise). All three are exported by the agent leader only.
- Actor ids are `agent`, `afk-bot-1` and `afk-bot-2`. The agent's id is `agent` (`PRESENCE_SELF_ID` default).
- Alert names are exactly `JdwillmsenMinecraftAfkBotsDegraded`, `JdwillmsenMinecraftAgentDisconnected` and `JdwillmsenMinecraftActorParkedLong`.
- Existing windows are kept: agent 10 minutes (critical), bots 30 minutes (warning).
- `JdwillmsenMinecraftActorParkedLong` is severity `warning` and fires when any override without `until` is older than 24 hours (86400 s).
- "Deliberate parking never pages anyone; an actor that should be present and is not still does."
- Namespace `jdwillmsen-prd`. Every rule carries the labels `severity`, `tenant: jdwillmsen` and `namespace: jdwillmsen-prd`, and `runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"`, matching the neighbouring rules.
- Windows go inside the expression, not in a long `for:`. A single contrary sample must not reset the clock (memory: *Prometheus for: needs an unbroken window*).
- No ticket IDs in code, comments, docs or commit messages. Conventional commits scoped `jdwillmsen-alerts`, with the trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Comments explain why, never what. Match the density of the surrounding rule file, which is high: every rule has a rationale block.
- Ship as `jdwlabs-agent-bot`. Never print, echo or persist a minted token or key. Merge with `--rebase` only.

## Review Focus

- **A bot pod dies without reporting.** Its `mc_presence_observed` may keep reading its last report (1). The contract does not say the agent ages reports out. Expected: the page still arrives. The Deployment branch catches it, pinned by Task 3's "a dead bot pod whose last report said connected still pages".
- **Leader handover mid-outage.** The exporting pod's name changes. Expected: one continuous alert, not a resolve and a re-fire. Both branches aggregate `max by (namespace, actor)`, pinned by Task 2's "a leader handover mid-outage keeps one alert firing".
- **Park expires and the agent cannot log back in** (the abuse-hold shape). Expected: a page ten minutes after it was wanted back, not ten minutes plus the park. Pinned by Task 2's "agent that fails to rejoin after its park ends pages ten minutes later".
- **Operator parks an agent that is already paging** to acknowledge it. Expected: the page resolves at once, not after the average drains. Pinned by Task 2's "parking a disconnected agent resolves the page at once".
- **`replicas: 0` in git for maintenance.** The old `sum(...) < 2` paged on it after 30 minutes, which is the spec's own complaint. Expected: silent. Pinned by Task 3's "a bot Deployment scaled to zero in git never pages".

---

## File Structure

All paths are relative to the `jdwlabs/platform` worktree root. Line numbers are as of `origin/main` at `1ba9a63`.

| File | Change | Responsibility |
|---|---|---|
| `tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml` | Modify `:580-606` (AFK bot rule and its comment), `:608-645` (agent rule and its comment). Insert after `:675` (end of `JdwillmsenMinecraftAgentConnectionSignalAbsent`). | The live rules ArgoCD applies to `jdwillmsen-prd`. |
| `tests/prometheus-rules/rules-agent-connectivity_test.yaml` | Replace whole file (136 lines today). | promtool tests for the agent rule and its `absent()` companion. |
| `tests/prometheus-rules/rules-actor-presence_test.yaml` | Create. | promtool tests for the bot rule and `JdwillmsenMinecraftActorParkedLong`. It is kept apart from the agent file because the agent file is about session connectivity and its absent() companion. `run.sh` picks up every `*_test.yaml` (`tests/prometheus-rules/run.sh`, last three lines). |
| `docs/OPERATIONS.md` | Insert three rows after `:443` (the `JdwillmsenMinecraftWorldArchiveShrank` row) in "5. Troubleshooting symptom → fix". | The page every rule's `runbook_url` points at. |

`tests/prometheus-rules/run.sh` extracts every `PrometheusRule` under `tenants/` into `rules/<metadata.name>.yaml`, so these tests load `rules/jdwillmsen-tenant-alerts.yaml`. It then runs `promtool check rules` and `promtool test rules`. No routing change is needed. `tests/alertmanager-routing/routing-matrix.yaml` routes on `tenant`/`severity`, and the new alert uses the same pair as its neighbours.

---

### Task 1: Worktree, production prerequisite check, green baseline

**Files:**
- None modified.

**Interfaces:**
- Consumes: plan 6 deployed to `jdwillmsen-prd` (the chart sets `PRESENCE_ACTORS`/`PRESENCE_TOKENS`, and the agent image exports the presence metrics).
- Produces: a worktree at `~/worktrees/platform/feat/<ticket>-actor-presence-alerts` on branch `feat/<ticket>-actor-presence-alerts` at `origin/main`, and the confirmed live label set every later task's tests are shaped after: `mc_presence_*{namespace="jdwillmsen-prd", actor="<id>", pod=..., job=..., service=...}` and bot Deployment names matching `jdwillmsen-minecraft-fwb-prd-afk-bot(-.+)?`.

- [ ] **Step 1: Create the worktree off fresh origin/main**

```bash
cd /home/dev-admin/projects/jdwlabs/platform
git fetch origin
git worktree add ~/worktrees/platform/feat/<ticket>-actor-presence-alerts -b feat/<ticket>-actor-presence-alerts origin/main
cd ~/worktrees/platform/feat/<ticket>-actor-presence-alerts
git branch --show-current
```

Expected: `feat/<ticket>-actor-presence-alerts`. If an agent session is driving this, use `EnterWorktree` instead. Branch from `origin/main` either way. Nothing in this phase depends on an open PR.

- [ ] **Step 2: Confirm plan 6 is live and the presence series are scraped (read-only)**

```bash
P=/api/v1/namespaces/monitoring/services/platform-kube-prometheus-s-prometheus:9090/proxy/api/v1/query
q() { kubectl get --raw "$P?query=$(python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$1")" | jq -c '.data.result[] | [.metric.actor // .metric.deployment, .metric.pod, .value[1]]'; }
q 'mc_presence_desired{namespace="jdwillmsen-prd"}'
q 'mc_presence_observed{namespace="jdwillmsen-prd"}'
q 'mc_presence_override_age_seconds{namespace="jdwillmsen-prd"}'
q 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd", deployment=~"jdwillmsen-minecraft-fwb-prd-afk-bot(-.+)?"}'
```

Expected:
- `mc_presence_desired` and `mc_presence_observed` each return exactly three rows, `agent`, `afk-bot-1` and `afk-bot-2`, all from the same single agent pod, with values `"0"` or `"1"`.
- `mc_presence_override_age_seconds` returns zero rows, or one row per actor that currently has an override without `until`.
- The Deployment query returns the two bot Deployments (today `jdwillmsen-minecraft-fwb-prd-afk-bot` and `jdwillmsen-minecraft-fwb-prd-afk-bot-2`) and nothing else.

**Stop and report** instead of continuing in any of these cases:
- The presence queries return nothing. Plan 6 is not live, and shipping now would leave the agent rule on its fallback and the bot rule on its Deployment branch only.
- The label is not literally `actor`, for example `exported_actor`.
- The Deployment query misses a bot Deployment. Every expression below would need its selector changed first.

- [ ] **Step 3: Confirm the metric Help strings match the contract's semantics**

```bash
kubectl -n jdwillmsen-prd exec deploy/jdwillmsen-minecraft-fwb-prd-server-agent -- wget -qO- http://127.0.0.1:9090/metrics | grep -E '^# HELP mc_presence_'
```

Expected: HELP lines for `mc_presence_desired` (1 present, 0 parked), `mc_presence_observed` (1 connected, 0 otherwise) and `mc_presence_override_age_seconds`. If `wget` is not in the image, use the Prometheus metadata API instead: `kubectl get --raw '/api/v1/namespaces/monitoring/services/platform-kube-prometheus-s-prometheus:9090/proxy/api/v1/metadata?metric=mc_presence_observed' | jq .`. If observed reads 1 for *parked*, or desired uses any encoding other than 1/0, stop: every expression below assumes the contract's encoding.

- [ ] **Step 4: Run the rule gate on the untouched tree**

```bash
cd ~/worktrees/platform/feat/<ticket>-actor-presence-alerts
promtool --version | head -1
tests/prometheus-rules/run.sh; echo "exit $?"
```

Expected: `promtool, version 3.5.0 ...` (the version CI pins). Output ends with `extracted 19 rule set(s)`, `Checking rules/jdwillmsen-tenant-alerts.yaml` / `SUCCESS: 15 rules found`, a run of `SUCCESS` lines and `exit 0`. If promtool is missing or a different version, install 3.5.0 as CI does (`.github/workflows/validate.yml:117-122`, checksum `e811827af26d822afb09a4f28314f61b618b12cff5369835a67f674d8b46f39a`).

No commit in this task.

---

### Task 2: Agent alert pages only while the agent should be present

**Files:**
- Modify: `tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml:608-645`
- Modify: `docs/OPERATIONS.md` (insert after `:443`)
- Test: `tests/prometheus-rules/rules-agent-connectivity_test.yaml` (whole file)

**Interfaces:**
- Consumes: the Task 1 worktree. `mc_presence_desired`/`mc_presence_observed` with `actor="agent"`. `mc_agent_connected` (existing, `minecraft-server-agent/internal/httpapi/metrics.go`).
- Produces: `JdwillmsenMinecraftAgentDisconnected` with two label shapes. The presence branch gives `{severity="critical", tenant="jdwillmsen", namespace="jdwillmsen-prd", actor="agent"}`. The fallback gives the same labels without `actor`. The annotation text is the exact `description` below, which the tests assert.

- [ ] **Step 1: Write the failing tests**

Replace the whole of `tests/prometheus-rules/rules-agent-connectivity_test.yaml` with:

```yaml
# Unit tests for JdwillmsenMinecraftAgentDisconnected and its absent()
# companion JdwillmsenMinecraftAgentConnectionSignalAbsent.
#
# mc_agent_connected has existed since the chat agent's first release and
# nothing alerted on it until a real session outage went unwatched for forty
# minutes and was only noticed by a player in the world. The central
# assertion below is not "does it fire" alone, but "does it fire only on a
# session that stays down" — the agent's own retry loop clears an ordinary
# server restart or protocol bump in well under ten minutes on its own, and a
# rule that cannot sit through one of those is a rule that pages on nothing.
#
# The agent can now be parked on purpose, so the rule reads the leader's
# presence series first and mc_agent_connected only while those are missing.
# The first cases below feed mc_agent_connected alone and pin that fallback to
# exactly the behaviour the rule had before presence existed; the presence
# cases after them pin what parking changes.
#
# Rule files are extracted from the PrometheusRule manifests under tenants/ by
# run.sh, since promtool reads bare rule groups and not the CRD wrapper.
rule_files:
  - rules/jdwillmsen-tenant-alerts.yaml
evaluation_interval: 1m
tests:
  # Connected for an hour, then down and staying down, with no presence series
  # at all — no leader has exported them yet, or presence is switched off.
  # Past ten minutes this is exactly the shape of the incident that prompted
  # the rule: a retry loop that is not succeeding, not one still working.
  - interval: 1m
    name: agent down past the retry-loop window pages critical when no presence series exist
    input_series:
      - series: 'mc_agent_connected{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics"}'
        values: '1+0x60 0+0x600'
    alert_rule_test:
      # Five minutes into the drop — under the ten-minute window, and also
      # under the couple of minutes the agent's own retry loop normally takes.
      # This is the boundary that keeps an ordinary reconnect from paging.
      - eval_time: 65m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []
      # Eleven minutes down: the retry loop itself is now the thing that is
      # broken.
      - eval_time: 71m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
      # Still firing hours later — the live incident this rule exists for ran
      # forty minutes with nobody paged at all.
      - eval_time: 3h
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # The ordinary case this rule must stay quiet for: a five-minute drop — the
  # shape of a server restart or a protocol bump — that clears on its own
  # before the window is ever satisfied. Firing here would make the rule
  # indistinguishable from routine churn, which is the whole reason it is not
  # a shorter window.
  - interval: 1m
    name: a short disconnect that recovers on its own never pages
    input_series:
      - series: 'mc_agent_connected{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics"}'
        values: '1+0x60 0+0x5 1+0x600'
    alert_rule_test:
      - eval_time: 64m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []
      - eval_time: 90m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []

  # The gap `== 0` cannot see: the agent Deployment vanishes outright (deleted,
  # crashlooping, or a ServiceMonitor that stopped matching) rather than
  # reporting a stuck zero. Every series it exported goes with it, presence
  # included, so JdwillmsenMinecraftAgentDisconnected has nothing to read on
  # either branch and stays silent. Only the absent() companion can see this,
  # and this is the guarantee for "no metrics from the agent at all": it pages
  # after fifteen minutes, exactly as before presence existed.
  - interval: 1m
    name: workload vanishing entirely pages the absent rule, not the disconnected one
    input_series:
      - series: 'mc_agent_connected{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics"}'
        values: '1+0x30'
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x30'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x30'
    alert_rule_test:
      # Well past the last sample, the staleness window, and the rule's own
      # 15m `for:`.
      - eval_time: 3h
        alertname: JdwillmsenMinecraftAgentConnectionSignalAbsent
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
            exp_annotations:
              summary: "No connection signal exists for the Minecraft chat agent"
              description: >-
                mc_agent_connected has not been scraped from jdwillmsen-prd for fifteen minutes. The agent may be
                connected fine, but nothing can now tell you if it is not — JdwillmsenMinecraftAgentDisconnected reads
                this series and is silent while it is missing. Check the jdwillmsen-minecraft-fwb-prd-server-agent
                Deployment, its pod, and its ServiceMonitor in jdwillmsen-prd. If the agent is being retired
                deliberately, delete these rules in the same change.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
      - eval_time: 3h
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []

  # Wanted in the world and not there: the presence branch pages on the same
  # ten-minute window the fallback does, labelled with the actor rather than
  # a pod, so it survives a leader handover as one alert.
  - interval: 1m
    name: agent desired present but absent pages critical on the presence branch
    input_series:
      - series: 'mc_agent_connected{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics"}'
        values: '1+0x60 0+0x600'
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x660'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x60 0+0x600'
    alert_rule_test:
      - eval_time: 65m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []
      - eval_time: 71m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: agent
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # The case the rewrite exists for: the agent leaves the world on purpose and
  # its session gauge reads 0 for as long as it is parked. Under the old rule
  # this paged critical ten minutes later.
  - interval: 1m
    name: agent parked while mc_agent_connected is 0 never pages
    input_series:
      - series: 'mc_agent_connected{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics"}'
        values: '1+0x60 0+0x600'
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x59 0+0x601'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x60 0+0x600'
    alert_rule_test:
      - eval_time: 71m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []
      - eval_time: 10h
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []

  # Parking an agent that is already paging is how an operator acknowledges
  # it: the current desired state must read 1 as well, so the alert resolves
  # on the next evaluation instead of draining out of a ten-minute average.
  - interval: 1m
    name: parking a disconnected agent resolves the page at once
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x80 0+0x60'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x60 0+0x80'
    alert_rule_test:
      - eval_time: 75m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: agent
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
      - eval_time: 81m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []

  # An agent whose park expires and which then cannot log back in — the
  # abuse-hold shape again — pages on the same ten minutes counted from the
  # moment it was wanted back, not from when it left.
  - interval: 1m
    name: agent that fails to rejoin after its park ends pages ten minutes later
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '0+0x60 1+0x60'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '0+0x120'
    alert_rule_test:
      - eval_time: 69m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts: []
      - eval_time: 71m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: agent
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # A session that connects for a minute and drops again, over and over. A
  # `for:` on an instantaneous `== 0` restarts its clock on every one of those
  # minutes and never fires; the window inside the expression keeps counting.
  - interval: 1m
    name: a session that keeps dropping straight after connecting still pages
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x90'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x60 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1'
    alert_rule_test:
      - eval_time: 80m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: agent
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # A leader handover renames the pod exporting the presence series. The
  # alert must carry on as the same alert across it, not resolve and re-fire
  # under the new pod's name.
  - interval: 1m
    name: a leader handover mid-outage keeps one alert firing
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x75'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x60 0+0x15'
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-7c9d0e1f86-fghij",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '_x76 1+0x30'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",service="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",pod="jdwillmsen-minecraft-fwb-prd-server-agent-7c9d0e1f86-fghij",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '_x76 0+0x30'
    alert_rule_test:
      - eval_time: 90m
        alertname: JdwillmsenMinecraftAgentDisconnected
        exp_alerts:
          - exp_labels:
              severity: critical
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: agent
            exp_annotations:
              summary: "Minecraft chat agent has lost its Bedrock session"
              description: >-
                The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
                agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
                agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
                so this long out means its own retry loop has been failing, not merely retrying. The known cause is
                Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
                cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
                a browser and complete the security challenge, and the agent's existing retry loop will log it back in
                without anything further.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
tests/prometheus-rules/run.sh; echo "exit $?"
```

Expected: `exit 1`. promtool reports `FAILED:` for `rules-agent-connectivity_test.yaml` with these cases (names and times):
- `agent down past the retry-loop window pages critical when no presence series exist` at `1h11m` and `3h`. The labels no longer carry `pod`/`job`/`service`, and the description changed.
- `agent desired present but absent pages critical on the presence branch` at `1h11m`.
- `agent parked while mc_agent_connected is 0 never pages` at `1h11m` and `10h`.
- `parking a disconnected agent resolves the page at once` at `1h15m`.
- `agent that fails to rejoin after its park ends pages ten minutes later` at `1h11m`.
- `a session that keeps dropping straight after connecting still pages` at `1h20m`.
- `a leader handover mid-outage keeps one alert firing` at `1h30m`.

`a short disconnect that recovers on its own never pages` and `workload vanishing entirely pages the absent rule, not the disconnected one` pass before and after. They are regression guards.

- [ ] **Step 3: Rewrite the rule**

Run from the worktree root. The script replaces the comment block and rule from the line `        # jdwillmsen-minecraft-fwb-prd-server-agent has published mc_agent_connected` through that rule's `runbook_url:` line.

```bash
python3 - <<'PY'
import pathlib

BLOCK = r'''        # jdwillmsen-minecraft-fwb-prd-server-agent has published mc_agent_connected
        # since its first release and nothing has ever read it. Its Xbox Live
        # session dropped at 2026-09-09 02:48:09Z and it could not log back in
        # until 03:27:53Z — thirty-nine minutes; nothing paged, and the only
        # reason anyone knew was a player standing in the world who saw it leave.
        #
        # The agent already retries a dropped session on its own, and does so
        # fast — an ordinary server restart or a protocol bump recovers inside a
        # couple of minutes without anyone noticing, which is why this cannot be
        # a short window. Ten minutes down is past that: it is no longer the
        # agent recovering, it is the retry loop itself failing over and over.
        # The cause worth naming outright is Xbox Live parking the account in an
        # abuse-mode hold: what is confirmed is that the account was flagged
        # after a dozen headless logins over the preceding afternoon, not that
        # attempt volume is what tripped it — either way, clearing the hold is
        # not something the agent's own retry loop can do, and describing this
        # as a client bug sends whoever is paged looking in the wrong place.
        # Only a human signing into that Microsoft account in a browser and
        # clearing the security challenge fixes it; once that happens the
        # agent's existing retry loop does the rest unattended.
        #
        # The agent can now be parked on purpose, out of the world while it
        # keeps monitoring, so a missing session is only a fault while its
        # desired state is present. The window lives in the expression, not in
        # `for:`: the shortfall series is 1 for each minute the agent was
        # wanted and absent, so a session that connects for a moment and drops
        # again still accumulates towards the threshold instead of resetting a
        # `for:` clock, and minutes it spent parked count as nothing owed. Over
        # half of ten minutes plus a five-minute hold is ten minutes for a
        # clean drop, the same window as before. Parking it clears the alert
        # immediately, because the current desired state must also read 1.
        #
        # The presence series are exported by the leader only. While none
        # exists (no leader has taken the lock yet, or presence is switched
        # off), the rule falls back to mc_agent_connected so that case pages
        # exactly as it did before presence existed. Both branches drop
        # pod and instance through max by, so a leader handover does not
        # resolve and re-fire the alert under a new pod name.
        - alert: JdwillmsenMinecraftAgentDisconnected
          expr: |
            (
              avg_over_time(
                (
                  max by (namespace, actor) (mc_presence_desired{namespace="jdwillmsen-prd", actor="agent"})
                  * on (namespace, actor)
                  (1 - max by (namespace, actor) (mc_presence_observed{namespace="jdwillmsen-prd", actor="agent"}))
                )[10m:1m]
              ) > 0.5
              and on (namespace, actor)
              max by (namespace, actor) (mc_presence_desired{namespace="jdwillmsen-prd", actor="agent"}) == 1
            )
            or
            (
              avg_over_time((1 - max by (namespace) (mc_agent_connected{namespace="jdwillmsen-prd"}))[10m:1m]) > 0.5
              unless on (namespace)
              max by (namespace) (mc_presence_desired{namespace="jdwillmsen-prd", actor="agent"})
            )
          for: 5m
          labels:
            severity: critical
            tenant: jdwillmsen
            namespace: jdwillmsen-prd
          annotations:
            summary: "Minecraft chat agent has lost its Bedrock session"
            description: >-
              The chat agent should be in the world and has been out of it for most of the last ten minutes. A parked
              agent never raises this; check `tools/mc presence ls` first if you did not expect it to be present. The
              agent reconnects on its own inside a couple of minutes for an ordinary server restart or protocol bump,
              so this long out means its own retry loop has been failing, not merely retrying. The known cause is
              Xbox Live placing the account in an abuse-mode hold after repeated login attempts, which the agent
              cannot clear itself: sign into the Microsoft account behind jdwillmsen-minecraft-fwb-prd-server-agent in
              a browser and complete the security challenge, and the agent's existing retry loop will log it back in
              without anything further.
            runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
'''

p = pathlib.Path("tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml")
lines = p.read_text().split("\n")
start = lines.index("        # jdwillmsen-minecraft-fwb-prd-server-agent has published mc_agent_connected")
alert = lines.index("        - alert: JdwillmsenMinecraftAgentDisconnected", start)
end = next(i for i in range(alert, len(lines)) if lines[i].lstrip().startswith("runbook_url:"))
lines[start:end + 1] = BLOCK.rstrip("\n").split("\n")
p.write_text("\n".join(lines))
PY
```

Why the expression has this shape:
- `max by (namespace, actor)` drops `pod`, so a leader handover is one alert.
- The `[10m:1m]` subquery average of the shortfall crosses `0.5` five minutes into a clean drop. `for: 5m` then fires it at ten minutes, the old window. Probed with promtool: silent at 70m and firing at 71m for a drop at 61m.
- `and ... desired == 1` makes a park resolve the alert immediately.
- The fallback `unless on (namespace) max by (namespace) (mc_presence_desired{actor="agent"})` is live only while no presence series exists at all.

- [ ] **Step 4: Add the runbook row**

```bash
python3 - <<'PY'
import pathlib

ROW = r'''| `JdwillmsenMinecraftAgentDisconnected` (critical) | The chat agent's desired presence is `present` and it has been out of the world for most of the last ten minutes. Run `tools/mc presence ls` first: if the agent is meant to be out, park it (`tools/mc presence park agent --for 2h --reason <text>`) and the page resolves on the next evaluation. Otherwise the known cause is Xbox Live holding the account in abuse mode: sign into the agent's Microsoft account in a browser and clear the security challenge, and the agent's own retry loop logs it back in. A parked agent never raises this. While the leader exports no presence series (no leader yet, or presence switched off) the rule falls back to `mc_agent_connected` and pages exactly as it did before parking existed; with no agent metrics at all it is silent and `JdwillmsenMinecraftAgentConnectionSignalAbsent` pages instead. |'''

p = pathlib.Path("docs/OPERATIONS.md")
lines = p.read_text().split("\n")
anchor = next(i for i, l in enumerate(lines) if l.startswith("| `JdwillmsenMinecraftWorldArchiveShrank` (critical) |"))
lines.insert(anchor + 1, ROW)
p.write_text("\n".join(lines))
PY
```

- [ ] **Step 5: Run the gate and yamllint to verify they pass**

```bash
tests/prometheus-rules/run.sh; echo "exit $?"
yamllint -d "{extends: default, rules: {line-length: {max: 300}, truthy: {allowed-values: ['true', 'false']}, comments: {min-spaces-from-content: 1}, document-start: disable}}" tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml; echo "lint $?"
```

Expected: `Checking rules/jdwillmsen-tenant-alerts.yaml` / `SUCCESS: 15 rules found`, no `FAILED`, `exit 0`, `lint 0`. The yamllint config is copied from `.github/workflows/validate.yml:37-48`. If yamllint is missing, run `pip install --user yamllint`.

- [ ] **Step 6: Commit**

```bash
git add tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml tests/prometheus-rules/rules-agent-connectivity_test.yaml docs/OPERATIONS.md
git commit -m "feat(jdwillmsen-alerts): page on the chat agent only while it should be present" -m "The agent can now be parked out of the world while it keeps monitoring, and its session gauge reads 0 for as long as it is. JdwillmsenMinecraftAgentDisconnected now reads the leader's presence series and pages on minutes the agent was wanted and absent, averaged over the same ten-minute window, so a park never pages and a session that flaps still does. mc_agent_connected stays as the fallback while no presence series exists." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

Do not pass `-c user.email`. The repo's signing identity is already configured (memory: *jdwlabs/platform PR mechanics*).

---

### Task 3: AFK bot alert pages on a bot wanted in the world, keeps a Deployment branch

**Files:**
- Modify: `tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml:580-606`
- Modify: `docs/OPERATIONS.md` (insert after the row Task 2 added)
- Create: `tests/prometheus-rules/rules-actor-presence_test.yaml`

**Interfaces:**
- Consumes: `mc_presence_desired`/`mc_presence_observed` for every `actor!="agent"`. `kube_deployment_status_replicas_available` and `kube_deployment_spec_replicas` from kube-state-metrics, with `deployment=~"jdwillmsen-minecraft-fwb-prd-afk-bot(-.+)?"` (confirmed in Task 1 Step 2).
- Produces: `JdwillmsenMinecraftAfkBotsDegraded` with labels `{severity="warning", tenant="jdwillmsen", namespace="jdwillmsen-prd"}` plus either `actor="<bot id>"` (presence branch) or `deployment="<name>"` (Deployment branch). The description template switches on `$labels.actor`. Task 4 appends to the test file created here.

- [ ] **Step 1: Write the failing tests**

Create `tests/prometheus-rules/rules-actor-presence_test.yaml`:

```yaml
# Unit tests for JdwillmsenMinecraftAfkBotsDegraded.
#
# Parking takes an AFK bot out of the world at runtime without touching its
# Deployment, so "the bot is not in the world" stopped being a fault on its
# own. The bot rule now pages on a bot wanted in the world and not there, from
# the agent's presence series, and keeps a Deployment branch for what those
# series cannot report: a bot pod that will not run, or every presence series
# gone with the agent.
#
# Rule files are extracted from the PrometheusRule manifests under tenants/ by
# run.sh, since promtool reads bare rule groups and not the CRD wrapper.
rule_files:
  - rules/jdwillmsen-tenant-alerts.yaml
evaluation_interval: 1m
tests:
  # Parked on purpose: the bot leaves the world a minute after its desired
  # state flips, and its pod keeps running. Under the old rule a runtime
  # switch did not exist; this is what it has to stay quiet for.
  - interval: 1m
    name: a parked bot never pages
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '1+0x59 0+0x300'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '1+0x60 0+0x300'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x360'
    alert_rule_test:
      - eval_time: 95m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts: []
      - eval_time: 6h
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts: []

  # Wanted and absent, with the pod running — the bot cannot join. Pages on
  # the rule's thirty-minute window for that bot only; the agent being out of
  # the world at the same time is its own rule's business, not this one's.
  - interval: 1m
    name: a bot desired present but absent pages after thirty minutes
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '1+0x200'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '1+0x60 0+0x140'
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-2"}'
        values: '1+0x200'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-2"}'
        values: '1+0x200'
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x200'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '1+0x60 0+0x140'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x200'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x200'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x360'
    alert_rule_test:
      # Twenty-nine minutes out: inside the window a server restart and the
      # bot's own backoff are allowed.
      - eval_time: 89m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts: []
      - eval_time: 92m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts:
          - exp_labels:
              severity: warning
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: afk-bot-1
            exp_annotations:
              summary: "An AFK bot that should be in the world is not"
              description: >-
                AFK bot afk-bot-1 should be in the world and has been out of it for most
                of the last thirty minutes. A parked bot never raises this; check `tools/mc presence ls`
                if you did not expect it to be present. The mob farm stops ticking around a missing bot and chat capture
                pauses with bot 1. Short sessions that end in reconnect events after protocol_spoofed log lines mean a
                protocol-schema change the spoof cannot bridge; a pod that will not start is a Deployment problem. Check
                the bot pods and their logs in jdwillmsen-prd.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # The shape the old Deployment-level rule was documented as blind to: the
  # pod runs, the bot joins, and the session ends a minute later, over and
  # over. Every short session would reset a `for:` on `== 0`.
  - interval: 1m
    name: a bot that keeps dropping straight after joining still pages
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-2"}'
        values: '1+0x120'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-2"}'
        values: '1+0x60 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1 0 0 0 1'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x120'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x120'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x360'
    alert_rule_test:
      - eval_time: 115m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts:
          - exp_labels:
              severity: warning
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: afk-bot-2
            exp_annotations:
              summary: "An AFK bot that should be in the world is not"
              description: >-
                AFK bot afk-bot-2 should be in the world and has been out of it for most
                of the last thirty minutes. A parked bot never raises this; check `tools/mc presence ls`
                if you did not expect it to be present. The mob farm stops ticking around a missing bot and chat capture
                pauses with bot 1. Short sessions that end in reconnect events after protocol_spoofed log lines mean a
                protocol-schema change the spoof cannot bridge; a pod that will not start is a Deployment problem. Check
                the bot pods and their logs in jdwillmsen-prd.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # The agent is down, so no presence series exist at all, and a bot pod will
  # not start. The Deployment branch is the only thing that can still see
  # it, and it pages on the same thirty minutes the old rule did.
  - interval: 1m
    name: with no presence series a bot pod that will not run still pages
    input_series:
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x60 0+0x140'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x200'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x200'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x200'
    alert_rule_test:
      - eval_time: 89m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts: []
      - eval_time: 92m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts:
          - exp_labels:
              severity: warning
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              deployment: jdwillmsen-minecraft-fwb-prd-afk-bot
            exp_annotations:
              summary: "An AFK bot that should be in the world is not"
              description: >-
                Deployment jdwillmsen-minecraft-fwb-prd-afk-bot has had no available replica for
                most of the last thirty minutes. A parked bot never raises this; check `tools/mc presence ls`
                if you did not expect it to be present. The mob farm stops ticking around a missing bot and chat capture
                pauses with bot 1. Short sessions that end in reconnect events after protocol_spoofed log lines mean a
                protocol-schema change the spoof cannot bridge; a pod that will not start is a Deployment problem. Check
                the bot pods and their logs in jdwillmsen-prd.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # A bot pod that dies reports nothing more, so its observed state can go on
  # reading the last thing it said — connected. The Deployment branch does
  # not depend on the bot telling anyone, and pages regardless.
  - interval: 1m
    name: a dead bot pod whose last report said connected still pages
    input_series:
      - series: 'mc_presence_desired{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '1+0x200'
      - series: 'mc_presence_observed{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '1+0x200'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x60 0+0x140'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x200'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x360'
    alert_rule_test:
      - eval_time: 92m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts:
          - exp_labels:
              severity: warning
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              deployment: jdwillmsen-minecraft-fwb-prd-afk-bot
            exp_annotations:
              summary: "An AFK bot that should be in the world is not"
              description: >-
                Deployment jdwillmsen-minecraft-fwb-prd-afk-bot has had no available replica for
                most of the last thirty minutes. A parked bot never raises this; check `tools/mc presence ls`
                if you did not expect it to be present. The mob farm stops ticking around a missing bot and chat capture
                pauses with bot 1. Short sessions that end in reconnect events after protocol_spoofed log lines mean a
                protocol-schema change the spoof cannot bridge; a pod that will not start is a Deployment problem. Check
                the bot pods and their logs in jdwillmsen-prd.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # replicas: 0 in git is the documented maintenance switch. The old
  # sum() < 2 paged on it after thirty minutes; asking for no replica is not
  # a replica going missing.
  - interval: 1m
    name: a bot Deployment scaled to zero in git never pages
    input_series:
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x60 0+0x300'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot-2",job="kube-state-metrics"}'
        values: '1+0x59 0+0x301'
      - series: 'kube_deployment_status_replicas_available{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x360'
      - series: 'kube_deployment_spec_replicas{namespace="jdwillmsen-prd",deployment="jdwillmsen-minecraft-fwb-prd-afk-bot",job="kube-state-metrics"}'
        values: '1+0x360'
    alert_rule_test:
      - eval_time: 95m
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts: []
      - eval_time: 6h
        alertname: JdwillmsenMinecraftAfkBotsDegraded
        exp_alerts: []
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
tests/prometheus-rules/run.sh; echo "exit $?"
```

Expected: `exit 1`, with `FAILED:` in `rules-actor-presence_test.yaml` for:
- `a bot desired present but absent pages after thirty minutes` at `1h32m`.
- `a bot that keeps dropping straight after joining still pages` at `1h55m`.
- `with no presence series a bot pod that will not run still pages` at `1h32m`. The old rule fires, but without a `deployment` label.
- `a dead bot pod whose last report said connected still pages` at `1h32m`, for the same reason.
- `a bot Deployment scaled to zero in git never pages` at `1h35m` and `6h`. The old `sum() < 2` pages on it.

`a parked bot never pages` passes before and after. Parking never touches the Deployment, so the old rule was already quiet there, and the test guards that the presence branch keeps it so.

- [ ] **Step 3: Rewrite the rule**

The script replaces the comment block starting `        # sum() rather than count(... > 0): with both bots down the count form` through the `JdwillmsenMinecraftAfkBotsDegraded` rule's `runbook_url:` line.

```bash
python3 - <<'PY'
import pathlib

BLOCK = r'''        # Two branches, each a question the other cannot answer.
        #
        # The presence branch is the one that knows intent: per actor, it
        # accumulates the minutes a bot was wanted in the world and was not
        # there, so a bot parked on purpose never raises it and a bot that
        # joins for a moment and drops again (the shape a protocol-schema
        # change the 0.2.0 spoof cannot bridge leaves behind) still does,
        # instead of resetting a `for:` clock on every short session. Over half
        # of thirty minutes plus a fifteen-minute hold is thirty minutes for a
        # clean drop, the same window as before: a server restart disconnects
        # both bots by design and their own backoff has them back well inside
        # it. actor!="agent" rather than a list of bot ids, so a bot added to
        # the chart's actor list is covered without touching this rule.
        #
        # The Deployment branch stays because the observed state of a bot is
        # only what that bot last reported to the agent: a pod that will not
        # run reports nothing, and while the agent is down there are no
        # presence series at all. kube-state-metrics sees a pod that will not
        # run regardless. Parking never changes replica counts, so this branch
        # cannot be tripped by parking either, and a Deployment scaled to zero
        # in git for maintenance is excluded by spec replicas rather than
        # paging after thirty minutes the way the old sum() < 2 did.
        #
        # Warning, not critical: the bots keep the mob farm ticking and the
        # chat log flowing, but nothing player-facing breaks while they are
        # down.
        - alert: JdwillmsenMinecraftAfkBotsDegraded
          expr: |
            (
              avg_over_time(
                (
                  max by (namespace, actor) (mc_presence_desired{namespace="jdwillmsen-prd", actor!="agent"})
                  * on (namespace, actor)
                  (1 - max by (namespace, actor) (mc_presence_observed{namespace="jdwillmsen-prd", actor!="agent"}))
                )[30m:1m]
              ) > 0.5
              and on (namespace, actor)
              max by (namespace, actor) (mc_presence_desired{namespace="jdwillmsen-prd", actor!="agent"}) == 1
            )
            or
            (
              avg_over_time(
                (1 - clamp_max(max by (namespace, deployment) (kube_deployment_status_replicas_available{namespace="jdwillmsen-prd", deployment=~"jdwillmsen-minecraft-fwb-prd-afk-bot(-.+)?"}), 1))[30m:1m]
              ) > 0.5
              and on (namespace, deployment)
              max by (namespace, deployment) (kube_deployment_spec_replicas{namespace="jdwillmsen-prd", deployment=~"jdwillmsen-minecraft-fwb-prd-afk-bot(-.+)?"}) > 0
            )
          for: 15m
          labels:
            severity: warning
            tenant: jdwillmsen
            namespace: jdwillmsen-prd
          annotations:
            summary: "An AFK bot that should be in the world is not"
            description: >-
              {{ if $labels.actor }}AFK bot {{ $labels.actor }} should be in the world and has been out of it for most
              of the last thirty minutes{{ else }}Deployment {{ $labels.deployment }} has had no available replica for
              most of the last thirty minutes{{ end }}. A parked bot never raises this; check `tools/mc presence ls`
              if you did not expect it to be present. The mob farm stops ticking around a missing bot and chat capture
              pauses with bot 1. Short sessions that end in reconnect events after protocol_spoofed log lines mean a
              protocol-schema change the spoof cannot bridge; a pod that will not start is a Deployment problem. Check
              the bot pods and their logs in jdwillmsen-prd.
            runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
'''

p = pathlib.Path("tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml")
lines = p.read_text().split("\n")
start = lines.index("        # sum() rather than count(... > 0): with both bots down the count form")
alert = lines.index("        - alert: JdwillmsenMinecraftAfkBotsDegraded", start)
end = next(i for i in range(alert, len(lines)) if lines[i].lstrip().startswith("runbook_url:"))
lines[start:end + 1] = BLOCK.rstrip("\n").split("\n")
p.write_text("\n".join(lines))
PY
```

Why the replica fallback is kept (the brief allowed it only if needed):
- `mc_presence_observed` for a bot is only what that bot last POSTed to `/v1/actors/{id}/status`. The contract does not require the agent to turn a silent bot into 0, so a dead pod can read "connected" indefinitely.
- While the agent is down, no presence series exist at all.

kube-state-metrics answers both cases independently of the agent. The fallback is still parking-safe because parking never changes replica counts (spec non-goal: "Changing replica counts or any Kubernetes object at runtime"). `kube_deployment_spec_replicas > 0` also stops `replicas: 0` in git from paging. Timing, probed with promtool: silent at 90m and firing at 91m for a drop at 61m, which is the old 30-minute window.

- [ ] **Step 4: Add the runbook row**

```bash
python3 - <<'PY'
import pathlib

ROW = r'''| `JdwillmsenMinecraftAfkBotsDegraded` (warning) | Two shapes, told apart by the label. With `actor`: that bot should be present and has been out of the world for most of the last thirty minutes while the agent can see it. `protocol_spoofed` log lines followed by short sessions mean a protocol-schema change the spoof cannot bridge. With `deployment`: that bot's pod has had no available replica for most of thirty minutes, which is visible even while the agent is down. A parked bot raises neither, and neither does a Deployment set to `replicas: 0` in git. Park bots for gameplay and keep `replicas: 0` for maintenance. |'''

p = pathlib.Path("docs/OPERATIONS.md")
lines = p.read_text().split("\n")
anchor = next(i for i, l in enumerate(lines) if l.startswith("| `JdwillmsenMinecraftAgentDisconnected` (critical) |"))
lines.insert(anchor + 1, ROW)
p.write_text("\n".join(lines))
PY
```

- [ ] **Step 5: Run the gate and yamllint to verify they pass**

```bash
tests/prometheus-rules/run.sh; echo "exit $?"
yamllint -d "{extends: default, rules: {line-length: {max: 300}, truthy: {allowed-values: ['true', 'false']}, comments: {min-spaces-from-content: 1}, document-start: disable}}" tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml; echo "lint $?"
```

Expected: `SUCCESS: 15 rules found` for `jdwillmsen-tenant-alerts.yaml`, no `FAILED`, `exit 0`, `lint 0`.

- [ ] **Step 6: Commit**

```bash
git add tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml tests/prometheus-rules/rules-actor-presence_test.yaml docs/OPERATIONS.md
git commit -m "feat(jdwillmsen-alerts): page on an AFK bot only while it should be present" -m "Parking takes a bot out of the world without touching its Deployment. JdwillmsenMinecraftAfkBotsDegraded now pages per actor on minutes a bot was wanted and absent over the same thirty-minute window, which also catches the short-session reconnect loop the Deployment-level rule was blind to. The Deployment branch stays for a pod that will not run, which a bot cannot report and which stays visible while the agent is down, and it now ignores a Deployment scaled to zero in git." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: JdwillmsenMinecraftActorParkedLong

**Files:**
- Modify: `tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml` (insert after the `JdwillmsenMinecraftAgentConnectionSignalAbsent` rule, `:675` on `origin/main`)
- Modify: `docs/OPERATIONS.md` (insert after the row Task 3 added)
- Modify: `tests/prometheus-rules/rules-actor-presence_test.yaml` (header and append)

**Interfaces:**
- Consumes: `mc_presence_override_age_seconds{actor}`, and the test file from Task 3.
- Produces: `JdwillmsenMinecraftActorParkedLong` with labels `{severity="warning", tenant="jdwillmsen", namespace="jdwillmsen-prd", actor="<id>"}`.

- [ ] **Step 1: Write the failing tests**

Change the header's first line from `# Unit tests for JdwillmsenMinecraftAfkBotsDegraded.` to two lines, and append the new cases to the end of `tests/prometheus-rules/rules-actor-presence_test.yaml`:

```bash
python3 - <<'PY'
import pathlib

CASES = r'''  # An override with no `until` crosses a day: warn, and name the actor.
  - interval: 1m
    name: an override older than a day warns
    input_series:
      - series: 'mc_presence_override_age_seconds{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '86000+60x30'
    alert_rule_test:
      # Past 86400 at 7m, so still inside the five-minute hold at 11m.
      - eval_time: 11m
        alertname: JdwillmsenMinecraftActorParkedLong
        exp_alerts: []
      - eval_time: 12m
        alertname: JdwillmsenMinecraftActorParkedLong
        exp_alerts:
          - exp_labels:
              severity: warning
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: afk-bot-1
            exp_annotations:
              summary: "An actor has held a runtime override with no expiry for over a day"
              description: >-
                afk-bot-1 has had a presence override with no `until` for 1d 0h 5m 20s.
                Nothing clears it on its own. If it is still wanted, make it the actor's default in the minecraft-fwb
                chart values; otherwise clear it with `tools/mc presence unpark afk-bot-1`.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"

  # Clearing the override removes the gauge, and the warning with it. An
  # override with an `until` never has the series in the first place.
  - interval: 1m
    name: clearing a day-old override resolves the warning
    input_series:
      - series: 'mc_presence_override_age_seconds{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="afk-bot-1"}'
        values: '90000+60x20 _x40'
    alert_rule_test:
      - eval_time: 15m
        alertname: JdwillmsenMinecraftActorParkedLong
        exp_alerts:
          - exp_labels:
              severity: warning
              tenant: jdwillmsen
              namespace: jdwillmsen-prd
              actor: afk-bot-1
            exp_annotations:
              summary: "An actor has held a runtime override with no expiry for over a day"
              description: >-
                afk-bot-1 has had a presence override with no `until` for 1d 1h 15m 0s.
                Nothing clears it on its own. If it is still wanted, make it the actor's default in the minecraft-fwb
                chart values; otherwise clear it with `tools/mc presence unpark afk-bot-1`.
              runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
      - eval_time: 40m
        alertname: JdwillmsenMinecraftActorParkedLong
        exp_alerts: []

  # An override younger than a day is ordinary use.
  - interval: 1m
    name: an override younger than a day stays quiet
    input_series:
      - series: 'mc_presence_override_age_seconds{namespace="jdwillmsen-prd",pod="jdwillmsen-minecraft-fwb-prd-server-agent-6b8f9c9d75-abcde",job="jdwillmsen-minecraft-fwb-prd-server-agent-metrics",actor="agent"}'
        values: '3600+60x60'
    alert_rule_test:
      - eval_time: 60m
        alertname: JdwillmsenMinecraftActorParkedLong
        exp_alerts: []
'''

p = pathlib.Path("tests/prometheus-rules/rules-actor-presence_test.yaml")
s = p.read_text()
old = "# Unit tests for JdwillmsenMinecraftAfkBotsDegraded.\n"
assert s.startswith(old)
s = "# Unit tests for JdwillmsenMinecraftAfkBotsDegraded and\n# JdwillmsenMinecraftActorParkedLong.\n" + s[len(old):]
p.write_text(s.rstrip("\n") + "\n\n" + CASES)
PY
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
tests/prometheus-rules/run.sh; echo "exit $?"
```

Expected: `exit 1`, with `FAILED:` for `an override older than a day warns` at `12m` and `clearing a day-old override resolves the warning` at `15m`. The rule does not exist yet. `an override younger than a day stays quiet` passes.

- [ ] **Step 3: Add the rule**

```bash
python3 - <<'PY'
import pathlib

BLOCK = r'''
        # A park with no expiry is the one runtime override nothing brings back
        # on its own: no timer, and a bot's park carries no wake trigger unless
        # someone set one. Git stays the baseline only while overrides stay
        # deviations, so one that outlives a day is either forgotten or has
        # become the new baseline and belongs in the chart's actor defaults.
        # The gauge exists only for overrides without `until`, which is every
        # case this rule is about. It only ever grows, so `for:` here is just
        # margin for a leader handover, not a smoothing window.
        - alert: JdwillmsenMinecraftActorParkedLong
          expr: max by (namespace, actor) (mc_presence_override_age_seconds{namespace="jdwillmsen-prd"}) > 86400
          for: 5m
          labels:
            severity: warning
            tenant: jdwillmsen
            namespace: jdwillmsen-prd
          annotations:
            summary: "An actor has held a runtime override with no expiry for over a day"
            description: >-
              {{ $labels.actor }} has had a presence override with no `until` for {{ $value | humanizeDuration }}.
              Nothing clears it on its own. If it is still wanted, make it the actor's default in the minecraft-fwb
              chart values; otherwise clear it with `tools/mc presence unpark {{ $labels.actor }}`.
            runbook_url: "https://github.com/jdwlabs/platform/blob/main/docs/OPERATIONS.md#5-troubleshooting-symptom--fix"
'''

p = pathlib.Path("tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml")
lines = p.read_text().split("\n")
alert = lines.index("        - alert: JdwillmsenMinecraftAgentConnectionSignalAbsent")
end = next(i for i in range(alert, len(lines)) if lines[i].lstrip().startswith("runbook_url:"))
lines[end + 1:end + 1] = BLOCK.rstrip("\n").split("\n")
p.write_text("\n".join(lines))
PY
```

`for: 5m` here is margin for a leader handover, not smoothing. The gauge only grows while the override exists, so it has no jitter to average away. The rule covers any override without `until`, which is what the spec says ("any override without `until`"). The name says "Parked" because every such override is a park in practice: all three actors default to `present`, and an agent parked from chat always has an `until`.

- [ ] **Step 4: Add the runbook row**

```bash
python3 - <<'PY'
import pathlib

ROW = r'''| `JdwillmsenMinecraftActorParkedLong` (warning) | An actor has held a presence override with no `until` for over 24 hours, and nothing clears one of those on its own. If the override is still wanted, make it that actor's default in the minecraft-fwb chart's actor list so git says so. Otherwise clear it with `tools/mc presence unpark <actor>`. |'''

p = pathlib.Path("docs/OPERATIONS.md")
lines = p.read_text().split("\n")
anchor = next(i for i, l in enumerate(lines) if l.startswith("| `JdwillmsenMinecraftAfkBotsDegraded` (warning) |"))
lines.insert(anchor + 1, ROW)
p.write_text("\n".join(lines))
PY
```

- [ ] **Step 5: Run the gate and yamllint to verify they pass**

```bash
tests/prometheus-rules/run.sh; echo "exit $?"
yamllint -d "{extends: default, rules: {line-length: {max: 300}, truthy: {allowed-values: ['true', 'false']}, comments: {min-spaces-from-content: 1}, document-start: disable}}" tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml; echo "lint $?"
```

Expected: `Checking rules/jdwillmsen-tenant-alerts.yaml` / `SUCCESS: 16 rules found`, no `FAILED`, `exit 0`, `lint 0`.

- [ ] **Step 6: Commit**

```bash
git add tenants/jdwillmsen/services/jdwillmsen-alerts/postInstall/prometheusrule.yaml tests/prometheus-rules/rules-actor-presence_test.yaml docs/OPERATIONS.md
git commit -m "feat(jdwillmsen-alerts): warn when a presence override outlives a day" -m "An override with no expiry is the one runtime deviation nothing clears on its own. After a day it is either forgotten or has become the baseline and belongs in the chart's actor defaults, so JdwillmsenMinecraftActorParkedLong warns and names the actor." -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Verify, ship as jdwlabs-agent-bot, merge, confirm live

**Files:**
- None modified. The ship script lives in the executor's scratchpad, never in the repo.

**Interfaces:**
- Consumes: the three commits from Tasks 2-4 on `feat/<ticket>-actor-presence-alerts`.
- Produces: a merged PR in `jdwlabs/platform`, and the three rules loaded by the prd Prometheus.

- [ ] **Step 1: Rebase on the latest main and run the full gate**

```bash
cd ~/worktrees/platform/feat/<ticket>-actor-presence-alerts
git fetch origin && git rebase origin/main
tests/prometheus-rules/run.sh; echo "exit $?"
yamllint -d "{extends: default, rules: {line-length: {max: 300}, truthy: {allowed-values: ['true', 'false']}, comments: {min-spaces-from-content: 1}, document-start: disable}}" tenants/; echo "lint $?"
git log --oneline origin/main..HEAD
git diff origin/main --stat
```

Expected: `exit 0`, `lint 0`, exactly three commits (Task 2, 3 and 4 subjects), and four files changed: `prometheusrule.yaml`, the two test files and `docs/OPERATIONS.md`. Grep the diff for ticket keys before shipping: `git diff origin/main | grep -nE 'JDWLABS-[0-9]+'` must print nothing.

- [ ] **Step 2: Replay the commits onto a bot-authored branch**

The method follows memory *bot-authored PR method*, with one refinement. GraphQL `createCommitOnBranch` replaces the per-file contents API, so each of the three commits lands whole instead of one commit per file. On `main`, a commit that changes a rule without its test would be a red commit (rebase merges keep every commit). GitHub signs `createCommitOnBranch` commits made with an App installation token. Step 3 verifies that, and the contents API is the fallback.

Write this to `$SCRATCH/ship-presence-alerts.sh` (the session scratchpad, outside the repo) and run it. It never echoes the key, JWT or token.

```bash
#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO=jdwlabs/platform
BRANCH=feat/<ticket>-actor-presence-alerts
WT=~/worktrees/platform/$BRANCH

T=$(mktemp -d /dev/shm/bot.XXXXXX)
trap 'find "$T" -type f -exec shred -u {} +; rm -rf "$T"' EXIT

sec() { kubectl -n ai-sre get secret ai-sre-relay -o go-template="{{index .data \"$1\"}}" | base64 -d; }
APP_ID=$(sec GITHUB_APP_ID)
INST_ID=$(sec GITHUB_APP_INSTALLATION_ID)
sec GITHUB_APP_PRIVATE_KEY > "$T/key.pem"

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
now=$(date +%s)
hdr=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
pl=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' $((now - 60)) $((now + 540)) "$APP_ID" | b64url)
sig=$(printf '%s.%s' "$hdr" "$pl" | openssl dgst -sha256 -sign "$T/key.pem" | b64url)
GH_TOKEN=$(curl -fsS -X POST \
  -H "Authorization: Bearer $hdr.$pl.$sig" -H "Accept: application/vnd.github+json" \
  "https://api.github.com/app/installations/$INST_ID/access_tokens" | jq -r .token)
export GH_TOKEN

base=$(git -C "$WT" merge-base HEAD origin/main)
gh api -X POST "repos/$REPO/git/refs" -f ref="refs/heads/$BRANCH" -f sha="$base" > /dev/null

head=$base
for c in $(git -C "$WT" rev-list --reverse "$base..HEAD"); do
  python3 - "$WT" "$c" "$REPO" "$BRANCH" "$head" "$T/req.json" <<'PY'
import base64, json, subprocess, sys

wt, commit, repo, branch, head, out = sys.argv[1:]
git = lambda *a: subprocess.check_output(["git", "-C", wt, *a])
msg = git("log", "-1", "--format=%B", commit).decode().strip()
headline, _, body = msg.partition("\n")
adds, dels = [], []
for line in git("diff-tree", "--no-commit-id", "--name-status", "-r", commit).decode().splitlines():
    status, path = line.split("\t", 1)
    if status == "D":
        dels.append({"path": path})
    else:
        adds.append({"path": path, "contents": base64.b64encode(git("show", f"{commit}:{path}")).decode()})
json.dump({
    "query": "mutation($input: CreateCommitOnBranchInput!) { createCommitOnBranch(input: $input) { commit { oid } } }",
    "variables": {"input": {
        "branch": {"repositoryNameWithOwner": repo, "branchName": branch},
        "message": {"headline": headline, "body": body.strip()},
        "expectedHeadOid": head,
        "fileChanges": {"additions": adds, "deletions": dels},
    }},
}, open(out, "w"))
PY
  head=$(gh api graphql --input "$T/req.json" --jq '.data.createCommitOnBranch.commit.oid')
  echo "pushed $(git -C "$WT" log -1 --format=%s "$c") as $head"
done

gh api "repos/$REPO/compare/$base...$BRANCH" --jq '.commits[] | [.sha[0:12], .author.login, .commit.verification.verified] | @tsv'
# Identical tree SHAs mean identical content, whatever the commit metadata.
[ "$(gh api "repos/$REPO/commits/$BRANCH" --jq .commit.tree.sha)" = "$(git -C "$WT" rev-parse 'HEAD^{tree}')" ] \
  && echo "remote tree matches local" || { echo "remote tree differs from local" >&2; exit 1; }

gh pr create --repo "$REPO" --base main --head "$BRANCH" \
  --title "feat(jdwillmsen-alerts): presence-aware Minecraft actor alerts" \
  --body-file "$WT/../pr-body.md"
```

Before running it, write the PR body to `~/worktrees/platform/feat/pr-body.md` (outside the worktree, so it is never committed):

```markdown
Rewrites the two Minecraft actor alerts so that deliberate parking never pages, and adds a warning for a runtime override that outlives a day.

- `JdwillmsenMinecraftAgentDisconnected` (critical, 10m): pages on minutes the agent was wanted in the world (`mc_presence_desired == 1`) and absent (`mc_presence_observed == 0`), averaged inside the expression. A clean drop pages at ten minutes as before, a flapping session still pages, and parking resolves it immediately. It falls back to `mc_agent_connected` only while no presence series exists.
- `JdwillmsenMinecraftAfkBotsDegraded` (warning, 30m): the same shape per bot actor. A Deployment branch (kube-state-metrics) stays for a pod that will not run, which a bot cannot report itself and which stays visible while the agent is down. It no longer pages on `replicas: 0` in git.
- `JdwillmsenMinecraftActorParkedLong` (warning): an override with no `until` older than 24h.
- With no agent metrics at all, both rewritten rules stay silent on their presence branches. `JdwillmsenMinecraftAgentConnectionSignalAbsent` pages after 15m (unchanged), and the bot Deployment branch still watches the bot pods.

Evidence: `tests/prometheus-rules/run.sh` passes locally with promtool 3.5.0, and every case above is a promtool unit test. The presence metrics were confirmed scraped in prd before this change.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

Expected script output: three `pushed ... as <oid>` lines, then three rows of `<sha>	jdwlabs-agent-bot[bot]	true`, then `remote tree matches local`, then the PR URL.

- [ ] **Step 3: If any commit shows `verified` false, fall back to the contents API**

Only if Step 2 printed `false`:
1. Delete the remote branch (`gh api -X DELETE repos/jdwlabs/platform/git/refs/heads/feat/<ticket>-actor-presence-alerts` under the bot token). No PR exists yet at that point, because the verification print comes before `gh pr create`. If one was created, run `gh pr reopen` after the redo, as the memory notes.
2. Redo the replay with one `PUT /repos/jdwlabs/platform/contents/<path>` per file, as in memory *bot-authored PR method* step 4. Order the calls so that each rule file change lands in the same call sequence as its test file.
3. Record in the PR description that the history is per-file because the multi-file path was unsigned.

- [ ] **Step 4: Watch CI to green**

```bash
gh pr checks --repo jdwlabs/platform feat/<ticket>-actor-presence-alerts --watch
```

Expected: every check passes, including `Validate / prometheus-rules`, `Validate / yamllint`, `Validate / alertmanager-routing` and `Verify PR Signatures`. Then check that code scanning has nothing open on the PR head: `gh api "repos/jdwlabs/platform/code-scanning/alerts?ref=refs/heads/feat/<ticket>-actor-presence-alerts&state=open" --jq length` must print `0`.

- [ ] **Step 5: Hand to jdwillmsen for approval (human step)**

Ask jdwillmsen to review and approve the PR as himself. The bot cannot approve its own PR, and every platform PR needs one approval. Do not use `--admin`.

- [ ] **Step 6: Merge with rebase and refresh main**

```bash
gh pr merge --repo jdwlabs/platform feat/<ticket>-actor-presence-alerts --rebase --delete-branch
cd /home/dev-admin/projects/jdwlabs/platform && git pull --ff-only
git diff HEAD origin/main --stat
```

Expected: the merge succeeds, `git pull --ff-only` fast-forwards, and the final diff prints nothing.

- [ ] **Step 7: Confirm the rules are live in prd (read-only)**

ArgoCD syncs the `jdwillmsen-alerts` service. Wait for the sync, then:

```bash
kubectl -n jdwillmsen-prd get prometheusrule jdwillmsen-tenant-alerts -o jsonpath='{.spec.groups[0].rules[*].alert}' | tr ' ' '\n' | grep -E 'AfkBotsDegraded|AgentDisconnected|ActorParkedLong'
R=/api/v1/namespaces/monitoring/services/platform-kube-prometheus-s-prometheus:9090/proxy/api/v1/rules
kubectl get --raw "$R?type=alert" | jq -r '.data.groups[].rules[] | select(.name | test("JdwillmsenMinecraft(AfkBotsDegraded|AgentDisconnected|ActorParkedLong)")) | [.name, .health, .state] | @tsv'
```

Expected: all three names from the CR, and three rows reading `ok` with state `inactive`. `firing` or `pending` is only acceptable if it matches the real state: check with `tools/mc presence ls`. If a row reads `err`, revert the PR at once. A broken expression is a rule that never fires.

- [ ] **Step 8: Record evidence on the ticket**

Comment on the Jira ticket named in the branch with the PR URL, the `run.sh` result, and the Step 7 output. Transition it per the board's flow (memory: *Jira is the source of truth*).

---

## Self-review

- **Spec coverage:** "Operations > Alerts" is covered: both rewrites are Tasks 2-3, `ActorParkedLong` is Task 4, and the rule tests under `tests/prometheus-rules/` are in Tasks 2-4. Rollout step 7's "ships after step 6, once the metrics exist in production" is Task 1 Steps 2-3. The runbook's "Parking actors" section belongs to plan 6 (the chart README). This plan only adds the `OPERATIONS.md` rows that the alerts' `runbook_url` points at.
- **Brief coverage:**
  - "parked => silent": `a parked bot never pages`, `agent parked while mc_agent_connected is 0 never pages`.
  - "desired but absent => fires": `agent desired present but absent ...`, `a bot desired present but absent ...`.
  - "agent parked while mc_agent_connected==0 => silent": its own test.
  - "parked >24h => warning": `an override older than a day warns`.
  - "metrics absent entirely (agent down)": `workload vanishing entirely ...` (Disconnected silent, SignalAbsent pages at 15m, unchanged) and `with no presence series a bot pod that will not run still pages`. The guarantee is stated in the PR body and the OPERATIONS row.
- **Placeholders:** none. Every block is the exact content, and every test was run against promtool 3.5.0 in a scratch copy of `origin/main`, including each intermediate state after Tasks 2, 3 and 4.
- **Consistency:**
  - Alert names, label sets and description text in the rules match the `exp_labels`/`exp_annotations` in the tests. The folded `>-` scalars compare after folding.
  - The anchors used by the Task 3 and Task 4 row inserts are exactly the rows Tasks 2 and 3 insert.
  - The rule count goes from 15 (Tasks 2, 3) to 16 (Task 4).

## Deviations from spec/contract

- **Replica-based fallback kept in `JdwillmsenMinecraftAfkBotsDegraded`.** The spec says the alert fires "only when desired is 1 and observed is 0". The bot's observed state is self-reported, and the contract does not define what the agent exports for a bot that stops reporting. With no fallback, a dead bot pod, or any bot while the agent is down, would never page. That is a regression from today's rule. The fallback cannot be tripped by parking because parking never changes replicas, and it now ignores `replicas: 0`.
- **`mc_agent_connected` fallback kept in `JdwillmsenMinecraftAgentDisconnected`, live only while no `mc_presence_desired{actor="agent"}` series exists.** The presence metrics are leader-only. Without the fallback, an agent pod with no leader (or presence switched off) would stop paging, which it does today.
- **Smoothed thresholds instead of a literal `== 0` held for the window.** "Observed 0 for the window" is implemented as "wanted and absent for over half the window, held for the other half". Clean outages page at exactly the old windows (probed: 10m and 30m). A flapping session also pages, per the memory rule that a `for:` over an instantaneous comparison never fires on a jittery signal.
- **`JdwillmsenMinecraftActorParkedLong` covers any override without `until`**, as the spec text says, not only parked ones. The contract's gauge carries no state label.
- **New test file `tests/prometheus-rules/rules-actor-presence_test.yaml`** for the bot and parked-long rules. `rules-agent-connectivity_test.yaml` keeps the agent rule only. `run.sh` globs `*_test.yaml`, so no harness change is needed.
- **Shipping uses GraphQL `createCommitOnBranch`** rather than the memory's per-file contents API, so that each commit stays whole on a rebase-only `main`. The contents API is the documented fallback (Task 5 Step 3).
