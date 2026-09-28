# gameops Monorepo Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move `minecraft-server-agent`, `mc-console-bridge` and `minecraft-afk-bot` into a new `jdwillmsen/gameops` repo with full history. Each component keeps releasing automatically on merge under its current image name. CI and releases are driven by per-component `component.yaml` manifests.

**Architecture:**
- `git filter-repo` rewrites each old repo under `minecraft/<component>/` and renames its tags with a component prefix. The three histories are merged into one bootstrap `main`.
- One follow-up PR turns the repo into a single root Go module and adds a small Go tool, `tools/components`, which reads the manifests and answers "which components does this change affect".
- CI builds its job matrix from that tool. A second PR adds the release glue: a semantic-release plugin that asks the same tool which commits belong to each component.

**Tech Stack:** Go 1.27 (toolchain go1.27.1), `go.yaml.in/yaml/v3`, git-filter-repo, GitHub Actions, semantic-release 25 (Node 24), Docker Buildx, ghcr.io and docker.io.

**Spec:** `docs/superpowers/specs/2026-09-28-gameops-monorepo-design.md` (Part 2; Part 1 for context)

## Global Constraints

- Repo: `github.com/jdwillmsen/gameops`, public, license PolyForm Noncommercial 1.0.0 (copy `LICENSE.md` verbatim from any old repo; all three are identical).
- Go module path: `github.com/jdwillmsen/gameops`; `go 1.27`; `toolchain go1.27.1`.
- Component paths: `minecraft/agent`, `minecraft/bridge`, `minecraft/afkbot`.
- Tag prefixes: `agent-`, `bridge-`, `afkbot-`. Git tags look like `agent-v0.24.0`.
- Image names are unchanged: `minecraft-server-agent`, `mc-console-bridge`, `minecraft-afk-bot`. Image tags are the bare version (`0.24.0`), never `latest`.
- Images publish to `ghcr.io/jdwillmsen/<image>` first, then mirror to `docker.io/${{ vars.DOCKERHUB_USERNAME }}/<image>`, and only `linux/amd64`.
- Merges are rebase-only. Every commit after the bootstrap push arrives by PR, signed.
- The bootstrap push is the only direct push to `main` this repo ever gets. It happens before any ruleset exists.
- No Jira ticket IDs in any repo content (code, comments, docs, commit bodies). Branch names and PR titles carry them.
- Credentials (`DOCKERHUB_TOKEN`, `RELEASE_PR_TOKEN`) are set by the human in their own terminal, never through an agent shell.
- Comments explain *why*, never *what*, and match the density of the old repos' workflows.
- Branches: `chore/JDWLABS-657-<slug>`, worktrees under `~/worktrees/gameops/<branch>`.

## Review Focus

1. **A PR that touches no component** (for example only `README.md`) → the `component` matrix is empty and skipped, and `ci-ok` must still pass rather than block the merge.
2. **Component directories whose names are prefixes of each other** (`minecraft/agent/` vs a future `minecraft/agent2/`) → a change in one must not mark the other affected. Directories always carry a trailing slash.
3. **A push whose `before` SHA is all zeros or missing** (first push, or a force push) → CI treats every component as affected rather than none.
4. **Component B's semantic-release fails after component A already tagged** → A's image must still publish. A tag with no image is the exact hazard the old workflow comments warn about.
5. **A manifest description containing a newline** → it would corrupt `$GITHUB_OUTPUT` in the release workflow, so validation must reject it.

---

## File map (end state)

```
gameops/
  go.mod, go.sum                        root module (Task 3)
  LICENSE.md, README.md                 Task 3 / Task 6
  .gitignore, .dockerignore             Task 3
  .releaserc.json                       Task 7
  renovate.json                         Task 6
  internal/presenceapi/                 was minecraft-server-agent/presenceapi (Task 3)
  minecraft/agent/                      history-imported (Task 1), component.yaml (Task 5)
  minecraft/bridge/                     "
  minecraft/afkbot/                     "
  tools/components/                     manifest tool (Task 4), component.yaml (Task 5)
    main.go  manifest.go  affected.go  tag.go  git.go
    manifest_test.go  affected_test.go  tag_test.go  main_test.go
  tools/release/                        release glue (Task 7)
    component-filter.mjs  package.json  package-lock.json
    dry-run-test.sh  component.yaml
  .github/workflows/
    ci.yml codeql.yml security-scan.yml verify-pr-signatures.yml protocol-check.yml   (Task 6)
    semantic-release.yml release.yml                                                   (Task 7)
```

---

### Task 0: Land this spec and plan in the agent repo first

The docs branch should be in the agent's history before the import, so it moves into `gameops` with everything else.

**Files:** none new (branch `docs/JDWLABS-657-gameops-monorepo-design` already holds the spec and this plan).

- [ ] **Step 1: Push the branch and open the PR**

```bash
cd ~/worktrees/minecraft-server-agent/docs/JDWLABS-657-gameops-monorepo-design
git push -u origin docs/JDWLABS-657-gameops-monorepo-design
gh pr create -R jdwillmsen/minecraft-server-agent --base main \
  --title "docs(JDWLABS-657): design and plan the gameops monorepo" \
  --body "Design spec and implementation plan for moving this repo, mc-console-bridge and minecraft-afk-bot into jdwillmsen/gameops, plus the umbrella design for the live world map.

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 2: Human gate.** The human reviews and approves the PR. Wait for all checks to go green, then rebase-merge. Refresh local main with `git -C ~/projects/minecraft-server-agent pull --ff-only`.

---

### Task 1: Build the bootstrap history locally

Nothing touches GitHub in this task. The output is a local repo at `~/scratch/gameops` whose `main` holds the three rewritten histories.

**Files:**
- Create: `~/scratch/gameops/` (local repo), `~/scratch/gameops-src/{agent,bridge,afkbot}/` (throwaway mirrors)

- [ ] **Step 1: Freeze the old repos.** Close the two open Renovate PRs on the agent. Renovate will recreate them against `gameops`.

```bash
gh pr close 72 -R jdwillmsen/minecraft-server-agent -c "Superseded: this repo is moving into jdwillmsen/gameops; Renovate will reopen there."
gh pr close 74 -R jdwillmsen/minecraft-server-agent -c "Superseded: this repo is moving into jdwillmsen/gameops; Renovate will reopen there."
for r in minecraft-server-agent mc-console-bridge minecraft-afk-bot; do gh pr list -R jdwillmsen/$r; done
```

Expected: the final loop prints nothing. If a human PR is open, stop and ask whether to merge it first.

- [ ] **Step 2: Install git-filter-repo**

```bash
pipx install git-filter-repo
git filter-repo --version
```

Expected: a version string such as `2.47.0`. Any version works.

- [ ] **Step 3: Fresh mirrors, rewritten under their new paths with prefixed tags**

```bash
set -euo pipefail
rm -rf ~/scratch/gameops-src && mkdir -p ~/scratch/gameops-src
declare -A SRC=([agent]=minecraft-server-agent [bridge]=mc-console-bridge [afkbot]=minecraft-afk-bot)
for c in agent bridge afkbot; do
  git clone -q "git@github.com:jdwillmsen/${SRC[$c]}.git" ~/scratch/gameops-src/$c
  git -C ~/scratch/gameops-src/$c filter-repo --force \
    --to-subdirectory-filter "minecraft/$c" \
    --tag-rename "":"$c-"
done
git -C ~/scratch/gameops-src/agent tag -l | head -3
```

Expected: tags like `agent-v0.1.0`, `agent-v0.10.0`, and so on.

- [ ] **Step 4: Merge the three histories into one `main`**

```bash
set -euo pipefail
rm -rf ~/scratch/gameops && git init -q -b main ~/scratch/gameops && cd ~/scratch/gameops
git commit -q --allow-empty -m "chore: start gameops monorepo" \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
declare -A SRC=([agent]=minecraft-server-agent [bridge]=mc-console-bridge [afkbot]=minecraft-afk-bot)
for c in agent bridge afkbot; do
  git fetch -q --tags ~/scratch/gameops-src/$c main:import/$c
  git merge -q --allow-unrelated-histories --no-edit import/$c \
    -m "chore: import ${SRC[$c]} history into minecraft/$c" \
    -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
  git branch -q -D import/$c
done
git log --oneline -4
```

Expected: the top 4 commits are the three import merges and the empty start commit. The merges and the start commit are signed by your git config; the imported commits are not (see the spec, "Signatures").

- [ ] **Step 5: Verify history, blame and tags survived**

```bash
cd ~/scratch/gameops
git log --follow --format='%ad %s' --date=short -- minecraft/agent/cmd/agent/main.go | tail -1
git log --follow --format='%ad %s' --date=short -- minecraft/bridge/main.go | tail -1
git log --follow --format='%ad %s' --date=short -- minecraft/afkbot/cmd/bot/main.go | tail -1
for c in agent bridge afkbot; do printf '%s %s\n' "$c" "$(git tag -l "$c-v*" | wc -l)"; done
for r in minecraft-server-agent mc-console-bridge minecraft-afk-bot; do git -C ~/projects/$r tag -l 'v*' | wc -l; done
git describe --tags --abbrev=0 --match 'agent-v*'
```

Expected:
- Each `--follow` line shows that file's original first commit, which you can cross-check with the same command in the old clone.
- The three tag counts match the old repos' `v*` counts, pairwise.
- `git describe` prints the newest agent tag (`agent-v0.23.1` or later).

If a count differs, stop: a tag was lost in the rename.

- [ ] **Step 6: Each component still builds and tests where it now sits.** Each still has its own `go.mod` at this point.

```bash
cd ~/scratch/gameops
for c in agent bridge afkbot; do (cd minecraft/$c && go build ./... && go test -race ./...) || exit 1; done
(cd minecraft/agent/presenceapi && go test -race ./...)
```

Expected: all pass. This is the bootstrap state that gets pushed. Task 3's restructure is verified on its own PR before it reaches `main`.

---

### Task 2: Create the GitHub repo and push the bootstrap

**Human gate before Step 1:** this creates a public repo and does the one-time direct push to `main`. Confirm with the human before running it.

- [ ] **Step 1: Create the empty repo and match the old repos' merge settings**

```bash
gh repo create jdwillmsen/gameops --public \
  --description "Game server tooling: Minecraft Bedrock agent, console bridge, bots and world map"
gh api -X PATCH repos/jdwillmsen/gameops \
  -F allow_rebase_merge=true -F allow_squash_merge=false -F allow_merge_commit=false \
  -F delete_branch_on_merge=true
```

- [ ] **Step 2: Bootstrap push (the one direct push)**

```bash
cd ~/scratch/gameops
git remote add origin git@github.com:jdwillmsen/gameops.git
git push -u origin main
git push origin --tags
gh api repos/jdwillmsen/gameops/tags --paginate -q '.[].name' | wc -l
```

Expected: the tag count equals the sum of the three counts from Task 1 Step 5.

- [ ] **Step 3: Ruleset on `main`, same shape as the old `protect-main`**

The required checks listed here don't exist yet. PR A (Tasks 3 to 6) adds the workflows that report them, and they run on PR A itself.

```bash
gh api -X POST repos/jdwillmsen/gameops/rulesets --input - <<'JSON'
{
  "name": "protect-main",
  "target": "branch",
  "enforcement": "active",
  "conditions": {"ref_name": {"include": ["~DEFAULT_BRANCH"], "exclude": []}},
  "bypass_actors": [],
  "rules": [
    {"type": "deletion"},
    {"type": "non_fast_forward"},
    {"type": "required_linear_history"},
    {"type": "pull_request", "parameters": {
      "allowed_merge_methods": ["rebase"],
      "dismiss_stale_reviews_on_push": true,
      "require_code_owner_review": false,
      "require_extra_approval_for_unattributed_changes": true,
      "require_last_push_approval": false,
      "required_approving_review_count": 0,
      "required_review_thread_resolution": true,
      "required_reviewers": []}},
    {"type": "required_status_checks", "parameters": {
      "strict_required_status_checks_policy": false,
      "do_not_enforce_on_create": false,
      "required_status_checks": [
        {"context": "analyze"},
        {"context": "ci-ok"},
        {"context": "scan / binaries"},
        {"context": "scan / gitleaks"},
        {"context": "scan / scan"},
        {"context": "signatures / signatures"}]}}
  ]
}
JSON
gh variable set DOCKERHUB_USERNAME -R jdwillmsen/gameops -b jdwillmsen
```

- [ ] **Step 4: Human-only step. Secrets, set in the human's own terminal, not this session.** Hand the human these exact commands:

```bash
gh secret set DOCKERHUB_TOKEN -R jdwillmsen/gameops     # Docker Hub PAT with Read, Write, Delete
gh secret set RELEASE_PR_TOKEN -R jdwillmsen/gameops    # same kind of token minecraft-afk-bot uses for protocol-check
```

Verify from the agent session (this prints names only): `gh secret list -R jdwillmsen/gameops` shows both.

- [ ] **Step 5: Local clone for the rest of the work**

```bash
git clone -q git@github.com:jdwillmsen/gameops.git ~/projects/gameops
git -C ~/projects/gameops worktree add -q -b chore/JDWLABS-657-monorepo-foundation \
  ~/worktrees/gameops/chore/JDWLABS-657-monorepo-foundation origin/main
```

Tasks 3 to 6 are commits on this branch, which becomes **PR A**.

---

### Task 3: Single root Go module (PR A, commit 1)

A pure restructure: no behaviour change. It's a `build:` commit because it changes how every image is built, and Task 7 makes `build:` release a patch.

**Files:**
- Create: `go.mod`, `go.sum` (root), `.gitignore`, `.dockerignore`, `LICENSE.md` (root)
- Move: `minecraft/agent/presenceapi/*` → `internal/presenceapi/`
- Delete: `minecraft/*/go.mod`, `minecraft/*/go.sum`, `minecraft/agent/presenceapi/go.mod`, `minecraft/*/.github/`, `minecraft/*/.releaserc.json`, `minecraft/*/renovate.json`, `minecraft/*/.gitignore`, `minecraft/*/.dockerignore`, `minecraft/*/LICENSE.md`
- Modify: every `*.go` importing an old module path; `minecraft/{agent,bridge,afkbot}/Dockerfile`

Work in `~/worktrees/gameops/chore/JDWLABS-657-monorepo-foundation` for Tasks 3 to 6.

- [ ] **Step 1: Move shared files to the root and delete the per-repo copies**

```bash
set -euo pipefail
git mv minecraft/agent/LICENSE.md LICENSE.md
git rm -q minecraft/bridge/LICENSE.md minecraft/afkbot/LICENSE.md
git rm -rq minecraft/*/.github minecraft/*/.releaserc.json minecraft/*/renovate.json \
  minecraft/*/.gitignore minecraft/*/.dockerignore
mkdir -p internal
git mv minecraft/agent/presenceapi internal/presenceapi
git rm -q internal/presenceapi/go.mod
```

- [ ] **Step 2: Root `.gitignore` and `.dockerignore`**

`.gitignore`:

```
/bin/
/out/
*.exe
node_modules
```

(`node_modules` without a trailing slash also matches the symlink the release dry-run test creates.)

`.dockerignore`, the old per-repo list re-rooted. The old `*.md` matched only each repo's root, so the equivalent is the root plus each component's root, which keeps any Markdown a package embeds.

```
.git
.github
node_modules
*.md
minecraft/*/*.md
```

- [ ] **Step 3: Root `go.mod` from the union of the three**

Each module is pinned at the highest version any of the three required, which is what minimal version selection would pick across them, so no component moves to a version none of them had tested.

```bash
set -euo pipefail
cat > go.mod <<'EOF'
module github.com/jdwillmsen/gameops

go 1.27

toolchain go1.27.1
EOF
for c in agent bridge afkbot; do
  go mod edit -json minecraft/$c/go.mod | jq -r '.Require[]? | "\(.Path) \(.Version)"'
done | grep -v 'jdwillmsen/minecraft-server-agent/presenceapi' | sort -V | awk '{v[$1]=$2} END {for (m in v) print m"@"v[m]}' \
  | sort > /tmp/gameops-requires
xargs -a /tmp/gameops-requires -I{} go mod edit -require={} go.mod
git rm -q minecraft/*/go.mod minecraft/*/go.sum
```

Step 5's `go mod tidy` then marks indirect dependencies and writes `go.sum`.

- [ ] **Step 4: Rewrite import paths.** Do the most specific path first, because the agent's module path is a prefix of the old presenceapi path.

```bash
set -euo pipefail
rewrite() { grep -rlF --include='*.go' "$1" . | xargs -r sed -i "s#$1#$2#g"; }
rewrite 'github.com/jdwillmsen/minecraft-server-agent/presenceapi' 'github.com/jdwillmsen/gameops/internal/presenceapi'
rewrite 'github.com/jdwillmsen/minecraft-server-agent' 'github.com/jdwillmsen/gameops/minecraft/agent'
rewrite 'github.com/jdwillmsen/mc-console-bridge' 'github.com/jdwillmsen/gameops/minecraft/bridge'
rewrite 'github.com/jdwillmsen/minecraft-afk-bot' 'github.com/jdwillmsen/gameops/minecraft/afkbot'
grep -rn --include='*.go' -E 'jdwillmsen/(minecraft-server-agent|mc-console-bridge|minecraft-afk-bot)' . || echo "no old import paths left"
```

Expected: `no old import paths left`.

- [ ] **Step 5: Tidy, then build, vet and test everything**

```bash
go mod tidy
gofmt -l . ; CGO_ENABLED=0 go build ./... && go vet ./... && go test -race ./...
```

Expected: `gofmt -l` prints nothing, and build, vet and test all pass.

If a test fails on a path (for example a test that reads `../../go.mod` or runs `go run ./cmd/...`), fix that test to use the new relative location. Don't skip it. Also check the scripts:

```bash
grep -rnE '\./cmd/|go run \.|presenceapi' minecraft/*/scripts minecraft/*/eval 2>/dev/null
```

Update any hit to its `./minecraft/<c>/cmd/...` path.

- [ ] **Step 6: Dockerfiles build from the repo root**

`minecraft/agent/Dockerfile`: keep every existing comment that still applies and the two digest-pinned base images unchanged. Replace the build stage down to the last `go build`:

```dockerfile
FROM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
WORKDIR /src

# Built from the repo root: the whole monorepo is one Go module, so the
# module files live there, not beside this Dockerfile.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agent ./minecraft/agent/cmd/agent
# One codebase, one image: census runs as a scheduled job beside the
# server rather than inside the agent process, so its binary rides along
# here instead of getting a Dockerfile of its own.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/census ./minecraft/agent/cmd/census
# Same reasoning as census: the probe speaks the same Bedrock handshake this
# module already implements, so it rides here rather than growing a second
# image to pin, pull and keep in step with the protocol code it shares.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/joinprobe ./minecraft/agent/cmd/joinprobe
```

Leave the runtime stage as it is.

`minecraft/bridge/Dockerfile` build stage:

```dockerfile
FROM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mc-console-bridge ./minecraft/bridge
```

`minecraft/afkbot/Dockerfile` build stage (the copied-packages comment moves with the file; keep it):

```dockerfile
FROM golang:1.27-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
WORKDIR /src

COPY go.mod go.sum ./
# The packages shared with the agent are copies under internal/, not
# imports; why is in minecraft/afkbot/docs/decisions.md.
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/bot ./minecraft/afkbot/cmd/bot
```

- [ ] **Step 7: Build each image from the root**

```bash
for c in agent bridge afkbot; do docker build -q -f minecraft/$c/Dockerfile -t gameops-$c:test . || exit 1; done
docker run --rm gameops-bridge:test --help 2>&1 | head -3
```

Expected: three image IDs, and the bridge prints its usage or its config error, not "exec format" or "not found".

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -q -m "build: make gameops one Go module" -m "The three imported repos had their own go.mod, CI and release config. They
are one module now, so a change to shared code builds and tests every
component that uses it in the same commit. The presenceapi contract, a
separately tagged module until now, becomes an ordinary package, and
each Dockerfile builds from the repo root where the module files live." \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `tools/components`, the manifest tool (PR A, commit 2)

TDD. This is the only new logic in PR A; CI and releases both depend on it.

**Files:**
- Create: `tools/components/manifest.go`, `affected.go`, `tag.go`, `git.go`, `main.go`
- Test: `tools/components/manifest_test.go`, `affected_test.go`, `tag_test.go`, `main_test.go`

**Interfaces (produced, used by Tasks 5 to 7):**
- `type Manifest struct { Name, Kind, Language string; Depends []string; Tasks Tasks; Release *Release; Dir string }`. `Dir` is repo-relative with a trailing `/`.
- `type Tasks struct { Build, Test, Lint string }`
- `type Release struct { Tag string; Artifacts []string; Image *Image }`
- `type Image struct { Name, Description, ShortDescription string }` (YAML key `short_description`)
- `func Load(root string) ([]Manifest, error)`, sorted by `Name`
- `func Validate(root string, ms []Manifest) []error`
- `type Mode int` with `ModeCI`, `ModeRelease`
- `func Affected(ms []Manifest, paths []string, mode Mode) []Manifest`
- `func ParseReleaseTag(tag string) (prefix, version string, err error)`
- CLI, run from anywhere inside the repo:
  - `go run ./tools/components validate` prints `N components valid`.
  - `go run ./tools/components affected -base <sha> -head <sha>` prints a JSON array of `{"name","dir","language","build","test","lint","image"}`. An empty or all-zero `-base` means every component.
  - `go run ./tools/components commits -component <name>` reads SHAs on stdin and prints the SHAs whose files affect that component (release mode), one per line.
  - `go run ./tools/components releasable` prints `<name> <tag>` per component that has a `release` block.
  - `go run ./tools/components get -release-tag <tag>` prints `key=value` lines: `name`, `dir`, `version`, `test`, `image`, `description`, `short_description`.

- [ ] **Step 1: Write the failing manifest tests**

`tools/components/manifest_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const agentYAML = `name: agent
kind: service
language: go
depends: [internal/]
tasks:
  build: go build ./minecraft/agent/...
  test: go test ./minecraft/agent/...
release:
  tag: agent
  artifacts: [image]
  image:
    name: minecraft-server-agent
    description: Chat agent
    short_description: Chat agent
`

func TestLoadFindsManifestsAndSetsDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "internal/.keep", "")
	writeFile(t, root, "minecraft/agent/component.yaml", agentYAML)
	writeFile(t, root, "minecraft/agent/Dockerfile", "FROM scratch\n")
	writeFile(t, root, "node_modules/x/component.yaml", "name: ignored\n")

	ms, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Name != "agent" || ms[0].Dir != "minecraft/agent/" {
		t.Fatalf("got %+v", ms)
	}
	if ms[0].Release.Image.ShortDescription != "Chat agent" {
		t.Fatalf("short_description not decoded: %+v", ms[0].Release.Image)
	}
	if errs := Validate(root, ms); len(errs) != 0 {
		t.Fatalf("valid manifest rejected: %v", errs)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a/component.yaml", "name: a\nkind: cli\nlanguage: go\ntaks: {}\n")
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "taks") {
		t.Fatalf("want unknown-field error naming taks, got %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	root := t.TempDir()
	ms := []Manifest{
		{Name: "Bad_Name", Kind: "server", Language: "cobol", Dir: "a/",
			Depends: []string{"nope", "missing/"}},
		{Name: "lib", Kind: "library", Language: "go", Dir: "b/",
			Tasks:   Tasks{Build: "x", Test: "y"},
			Release: &Release{Tag: "lib", Artifacts: []string{"image", "tarball"}}},
		{Name: "lib", Kind: "service", Language: "go", Dir: "c/",
			Tasks: Tasks{Build: "x", Test: "y"},
			Release: &Release{Tag: "lib", Image: &Image{Name: "c", Description: "two\nlines"}}},
	}
	errs := Validate(root, ms)
	joined := ""
	for _, e := range errs {
		joined += e.Error() + "\n"
	}
	for _, want := range []string{
		`a/component.yaml: name "Bad_Name"`,
		`a/component.yaml: kind "server"`,
		`a/component.yaml: language "cobol"`,
		`a/component.yaml: tasks.build is required`,
		`a/component.yaml: tasks.test is required`,
		`a/component.yaml: depends "nope" must end with /`,
		`a/component.yaml: depends "missing/" does not exist`,
		`b/component.yaml: a library has no release`,
		`b/component.yaml: release.artifacts "tarball"`,
		`b/component.yaml: release.artifacts has image but release.image is missing`,
		`c/component.yaml: name "lib" already used by b/`,
		`c/component.yaml: release.tag "lib" already used by b/`,
		`c/component.yaml: release.image is set but release.artifacts has no image`,
		`c/component.yaml: release.image descriptions must be one line`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing error %q in:\n%s", want, joined)
		}
	}
}

func TestValidateRequiresDockerfileForImage(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "internal/.keep", "")
	writeFile(t, root, "minecraft/agent/component.yaml", agentYAML)
	ms, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	errs := Validate(root, ms)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "minecraft/agent/Dockerfile does not exist") {
		t.Fatalf("got %v", errs)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./tools/components/`
Expected: FAIL to compile (`undefined: Load`, `undefined: Manifest`).

- [ ] **Step 3: Implement `manifest.go`**

```go
// Command components reads the component.yaml manifests that describe every
// buildable thing in this repo. CI and releases both ask it which components a
// change affects, so the two can never disagree about it.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const manifestFile = "component.yaml"

type Manifest struct {
	Name     string   `yaml:"name"`
	Kind     string   `yaml:"kind"`
	Language string   `yaml:"language"`
	Depends  []string `yaml:"depends"`
	Tasks    Tasks    `yaml:"tasks"`
	Release  *Release `yaml:"release"`
	// Dir is repo-relative with a trailing slash, so a prefix match on
	// minecraft/agent/ can never also match a sibling minecraft/agent2/.
	Dir string `yaml:"-"`
}

type Tasks struct {
	Build string `yaml:"build"`
	Test  string `yaml:"test"`
	Lint  string `yaml:"lint"`
}

type Release struct {
	Tag       string   `yaml:"tag"`
	Artifacts []string `yaml:"artifacts"`
	Image     *Image   `yaml:"image"`
}

type Image struct {
	Name             string `yaml:"name"`
	Description      string `yaml:"description"`
	ShortDescription string `yaml:"short_description"`
}

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	kinds       = set("service", "frontend", "library", "pack", "job", "cli")
	languages   = set("go", "node", "typescript")
	artifacts   = set("image", "github-asset")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func keys(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

func Load(root string) ([]Manifest, error) {
	var ms []Manifest
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != manifestFile {
			return nil
		}
		m, err := parse(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		m.Dir = filepath.ToSlash(rel) + "/"
		ms = append(ms, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	return ms, nil
}

func parse(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	// A misspelt key would otherwise decode to nothing and silently drop a
	// task or a release.
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Validate returns every problem at once, so one CI run shows everything a
// new component got wrong.
func Validate(root string, ms []Manifest) []error {
	var errs []error
	bad := func(m Manifest, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s%s: %s", m.Dir, manifestFile, fmt.Sprintf(format, args...)))
	}
	names := map[string]string{}
	tags := map[string]string{}
	for _, m := range ms {
		if !namePattern.MatchString(m.Name) {
			bad(m, "name %q must match %s", m.Name, namePattern)
		}
		if prev, ok := names[m.Name]; ok {
			bad(m, "name %q already used by %s", m.Name, prev)
		} else {
			names[m.Name] = m.Dir
		}
		if !kinds[m.Kind] {
			bad(m, "kind %q is not one of %s", m.Kind, keys(kinds))
		}
		if !languages[m.Language] {
			bad(m, "language %q is not one of %s", m.Language, keys(languages))
		}
		if m.Tasks.Build == "" {
			bad(m, "tasks.build is required")
		}
		if m.Tasks.Test == "" {
			bad(m, "tasks.test is required")
		}
		for _, d := range m.Depends {
			if !strings.HasSuffix(d, "/") {
				bad(m, "depends %q must end with / (directories only)", d)
				continue
			}
			if _, err := os.Stat(filepath.Join(root, d)); err != nil {
				bad(m, "depends %q does not exist", d)
			}
		}
		if m.Release != nil {
			validateRelease(root, m, tags, bad)
		}
	}
	return errs
}

func validateRelease(root string, m Manifest, tags map[string]string, bad func(Manifest, string, ...any)) {
	r := m.Release
	if m.Kind == "library" {
		bad(m, "a library has no release; drop the release block")
	}
	if !namePattern.MatchString(r.Tag) {
		bad(m, "release.tag %q must match %s", r.Tag, namePattern)
	}
	if prev, ok := tags[r.Tag]; ok {
		bad(m, "release.tag %q already used by %s", r.Tag, prev)
	} else {
		tags[r.Tag] = m.Dir
	}
	wantsImage := false
	for _, a := range r.Artifacts {
		if !artifacts[a] {
			bad(m, "release.artifacts %q is not one of %s", a, keys(artifacts))
		}
		wantsImage = wantsImage || a == "image"
	}
	switch {
	case wantsImage && r.Image == nil:
		bad(m, "release.artifacts has image but release.image is missing")
	case !wantsImage && r.Image != nil:
		bad(m, "release.image is set but release.artifacts has no image")
	}
	if r.Image == nil {
		return
	}
	// These values are written to $GITHUB_OUTPUT as key=value lines, where a
	// newline would start a bogus key.
	if strings.ContainsAny(r.Image.Name+r.Image.Description+r.Image.ShortDescription, "\r\n") {
		bad(m, "release.image descriptions must be one line")
	}
	if !namePattern.MatchString(r.Image.Name) {
		bad(m, "release.image.name %q must match %s", r.Image.Name, namePattern)
	}
	if r.Image.Description == "" {
		bad(m, "release.image.description is required")
	}
	if wantsImage {
		if _, err := os.Stat(filepath.Join(root, m.Dir, "Dockerfile")); errors.Is(err, fs.ErrNotExist) {
			bad(m, "release.artifacts has image but %sDockerfile does not exist", m.Dir)
		}
	}
}
```

- [ ] **Step 4: Run the manifest tests**

Run: `go test ./tools/components/ -run 'Load|Validate'`
Expected: PASS. The remaining files come next; `go vet` complains about nothing yet.

- [ ] **Step 5: Write the failing `affected` and tag tests**

`tools/components/affected_test.go`:

```go
package main

import (
	"reflect"
	"testing"
)

func names(ms []Manifest) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func TestAffected(t *testing.T) {
	ms := []Manifest{
		{Name: "agent", Language: "go", Dir: "minecraft/agent/", Depends: []string{"internal/"}},
		{Name: "agent2", Language: "go", Dir: "minecraft/agent2/"},
		{Name: "web", Language: "typescript", Dir: "minecraft/web/", Depends: []string{"packages/ui/"}},
	}
	cases := []struct {
		name  string
		paths []string
		mode  Mode
		want  []string
	}{
		{"own dir", []string{"minecraft/agent/main.go"}, ModeCI, []string{"agent"}},
		{"sibling prefix does not leak", []string{"minecraft/agent2/x.go"}, ModeCI, []string{"agent2"}},
		{"depends", []string{"internal/presenceapi/a.go"}, ModeRelease, []string{"agent"}},
		{"go toolchain hits only go", []string{"go.sum"}, ModeRelease, []string{"agent", "agent2"}},
		{"ts toolchain hits only ts", []string{"pnpm-lock.yaml"}, ModeRelease, []string{"web"}},
		{"root docs hit nothing", []string{"README.md"}, ModeCI, []string{}},
		{"tools rerun everything in CI", []string{"tools/components/main.go"}, ModeCI, []string{"agent", "agent2", "web"}},
		{"workflows rerun everything in CI", []string{".github/workflows/ci.yml"}, ModeCI, []string{"agent", "agent2", "web"}},
		{"tools release nothing", []string{"tools/components/main.go", ".github/workflows/ci.yml"}, ModeRelease, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := names(Affected(ms, c.paths, c.mode))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
```

`tools/components/tag_test.go`:

```go
package main

import "testing"

func TestParseReleaseTag(t *testing.T) {
	good := map[string][2]string{
		"agent-v0.24.0":        {"agent", "0.24.0"},
		"mcmap-web-v1.2.3":     {"mcmap-web", "1.2.3"},
		"afkbot-v1.0.0-rc.1":   {"afkbot", "1.0.0-rc.1"},
		"foo-v2-v3.0.0":        {"foo-v2", "3.0.0"},
	}
	for tag, want := range good {
		p, v, err := ParseReleaseTag(tag)
		if err != nil || p != want[0] || v != want[1] {
			t.Errorf("%s: got %q %q %v, want %q %q", tag, p, v, err, want[0], want[1])
		}
	}
	for _, tag := range []string{"v0.24.0", "agent-0.24.0", "agent-v0.24", "agent-vlatest", "-v1.0.0"} {
		if _, _, err := ParseReleaseTag(tag); err == nil {
			t.Errorf("%s: want error", tag)
		}
	}
}
```

- [ ] **Step 6: Run them and watch them fail**

Run: `go test ./tools/components/`
Expected: FAIL to compile (`undefined: Affected`, `undefined: ParseReleaseTag`).

- [ ] **Step 7: Implement `affected.go` and `tag.go`**

`tools/components/affected.go`:

```go
package main

import "strings"

type Mode int

const (
	// ModeCI decides what to build and test.
	ModeCI Mode = iota
	// ModeRelease decides what a commit ships.
	ModeRelease
)

// Root files whose change can alter every component in that language: a
// bumped dependency or workspace entry reaches all of them.
var toolchainFiles = map[string][]string{
	"go":         {"go.mod", "go.sum"},
	"typescript": {"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"},
}

// These change how every component is checked, so CI reruns them all, but
// they change no shipped code, so they never cause a release.
var ciOnlyEverything = []string{"tools/", ".github/"}

func Affected(ms []Manifest, paths []string, mode Mode) []Manifest {
	out := []Manifest{}
	for _, m := range ms {
		if affects(m, paths, mode) {
			out = append(out, m)
		}
	}
	return out
}

func affects(m Manifest, paths []string, mode Mode) bool {
	prefixes := append([]string{m.Dir}, m.Depends...)
	if mode == ModeCI {
		prefixes = append(prefixes, ciOnlyEverything...)
	}
	for _, p := range paths {
		for _, pre := range prefixes {
			if strings.HasPrefix(p, pre) {
				return true
			}
		}
		for _, f := range toolchainFiles[m.Language] {
			if p == f {
				return true
			}
		}
	}
	return false
}
```

`tools/components/tag.go`:

```go
package main

import (
	"fmt"
	"regexp"
)

// Greedy on the prefix so a component tag that itself contains "-v<digit>"
// still splits at the last one.
var releaseTag = regexp.MustCompile(`^([a-z][a-z0-9-]*)-v([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$`)

func ParseReleaseTag(tag string) (prefix, version string, err error) {
	m := releaseTag.FindStringSubmatch(tag)
	if m == nil {
		return "", "", fmt.Errorf("release tag %q is not <component>-v<semver>", tag)
	}
	return m[1], m[2], nil
}
```

- [ ] **Step 8: Run all unit tests**

Run: `go test ./tools/components/`
Expected: PASS.

- [ ] **Step 9: Write the failing CLI test against a real git repo**

`tools/components/main_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture builds a repo with two components and returns its root and the
// SHAs of: the base commit, a bridge-only commit, a shared-code commit and a
// docs-only commit.
func fixture(t *testing.T) (root string, base, bridge, shared, docs string) {
	root = t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	writeFile(t, root, "internal/.keep", "")
	writeFile(t, root, "minecraft/agent/component.yaml", agentYAML)
	writeFile(t, root, "minecraft/agent/Dockerfile", "FROM scratch\n")
	writeFile(t, root, "minecraft/bridge/component.yaml",
		"name: bridge\nkind: service\nlanguage: go\ntasks: {build: b, test: t}\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "base")
	base = git(t, root, "rev-parse", "HEAD")
	commit := func(rel, msg string) string {
		writeFile(t, root, rel, msg)
		git(t, root, "add", "-A")
		git(t, root, "commit", "-qm", msg)
		return git(t, root, "rev-parse", "HEAD")
	}
	bridge = commit("minecraft/bridge/main.go", "bridge")
	shared = commit("internal/x.go", "shared")
	docs = commit("README.md", "docs")
	return
}

func runCLI(t *testing.T, root, stdin string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(root, args, strings.NewReader(stdin), &out); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	return out.String()
}

func TestCLICommitsKeepsOnlyComponentCommits(t *testing.T) {
	root, base, bridge, shared, docs := fixture(t)
	got := runCLI(t, root, strings.Join([]string{base, bridge, shared, docs}, "\n"),
		"commits", "-component", "agent")
	want := base + "\n" + shared + "\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCLIAffectedMatrix(t *testing.T) {
	root, base, _, _, docs := fixture(t)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(runCLI(t, root, "", "affected", "-base", base, "-head", docs)), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0]["name"] != "agent" || rows[0]["image"] != true || rows[1]["image"] != false {
		t.Fatalf("got %v", rows)
	}
	for _, zero := range []string{"", "0000000000000000000000000000000000000000"} {
		if err := json.Unmarshal([]byte(runCLI(t, root, "", "affected", "-base", zero, "-head", docs)), &rows); err != nil || len(rows) != 2 {
			t.Fatalf("base %q: want every component, got %v %v", zero, rows, err)
		}
	}
	if got := runCLI(t, root, "", "affected", "-base", docs+"~1", "-head", docs); got != "[]\n" {
		t.Fatalf("docs-only change: got %q, want []", got)
	}
}

func TestCLIGetAndReleasable(t *testing.T) {
	root, _, _, _, _ := fixture(t)
	if got := runCLI(t, root, "", "releasable"); got != "agent agent\n" {
		t.Fatalf("releasable: got %q", got)
	}
	got := runCLI(t, root, "", "get", "-release-tag", "agent-v1.2.3")
	for _, want := range []string{"name=agent\n", "dir=minecraft/agent\n", "version=1.2.3\n",
		"image=minecraft-server-agent\n", "description=Chat agent\n", "test=go test ./minecraft/agent/...\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("get output missing %q:\n%s", want, got)
		}
	}
	var out bytes.Buffer
	if err := run(root, []string{"get", "-release-tag", "nobody-v1.0.0"}, nil, &out); err == nil {
		t.Fatal("unknown tag prefix: want error")
	}
}
```

- [ ] **Step 10: Run and watch it fail**

Run: `go test ./tools/components/ -run CLI`
Expected: FAIL to compile (`undefined: run`).

- [ ] **Step 11: Implement `git.go` and `main.go`**

`tools/components/git.go`:

```go
package main

import (
	"fmt"
	"os/exec"
	"strings"
)

func gitLines(root string, args ...string) ([]string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func changedFiles(root, base, head string) ([]string, error) {
	return gitLines(root, "diff", "--name-only", base+"..."+head)
}

// -m lists a merge commit's changes against each parent, and --root lets the
// first commit report its files instead of nothing.
func commitFiles(root, sha string) ([]string, error) {
	return gitLines(root, "diff-tree", "--no-commit-id", "--name-only", "-r", "-m", "--root", sha)
}

func repoRoot() (string, error) {
	lines, err := gitLines(".", "rev-parse", "--show-toplevel")
	if err != nil || len(lines) != 1 {
		return "", fmt.Errorf("not inside a git repository: %v", err)
	}
	return lines[0], nil
}
```

`tools/components/main.go`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const usage = `usage: components <command> [flags]
  validate                                 check every component.yaml
  affected -base <sha> -head <sha>         JSON matrix of components CI must run
  commits -component <name>                filter stdin SHAs to that component's
  releasable                               "<name> <tag>" per releasable component
  get -release-tag <component>-v<semver>   key=value facts for the release workflow`

func main() {
	root, err := repoRoot()
	if err == nil {
		err = run(root, os.Args[1:], os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "components:", err)
		os.Exit(1)
	}
}

func run(root string, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	ms, err := Load(root)
	if err != nil {
		return err
	}
	if errs := Validate(root, ms); len(errs) > 0 {
		return errors.Join(errs...)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	base := fs.String("base", "", "base commit")
	head := fs.String("head", "HEAD", "head commit")
	component := fs.String("component", "", "component name")
	releaseTag := fs.String("release-tag", "", "full release tag")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "validate":
		_, err = fmt.Fprintf(stdout, "%d components valid\n", len(ms))
		return err
	case "affected":
		return affectedCmd(root, ms, *base, *head, stdout)
	case "commits":
		return commitsCmd(root, ms, *component, stdin, stdout)
	case "releasable":
		for _, m := range ms {
			if m.Release != nil {
				fmt.Fprintf(stdout, "%s %s\n", m.Name, m.Release.Tag)
			}
		}
		return nil
	case "get":
		return getCmd(ms, *releaseTag, stdout)
	}
	return fmt.Errorf("unknown command %q\n%s", args[0], usage)
}

type matrixRow struct {
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	Language string `json:"language"`
	Build    string `json:"build"`
	Test     string `json:"test"`
	Lint     string `json:"lint"`
	Image    bool   `json:"image"`
}

func affectedCmd(root string, ms []Manifest, base, head string, stdout io.Writer) error {
	selected := ms
	// A push with no usable before-SHA (a new branch, a force push) has no
	// diff to trust, so everything runs rather than nothing.
	if strings.Trim(base, "0") != "" {
		paths, err := changedFiles(root, base, head)
		if err != nil {
			return err
		}
		selected = Affected(ms, paths, ModeCI)
	}
	rows := []matrixRow{}
	for _, m := range selected {
		img := m.Release != nil && m.Release.Image != nil
		rows = append(rows, matrixRow{m.Name, strings.TrimSuffix(m.Dir, "/"), m.Language,
			m.Tasks.Build, m.Tasks.Test, m.Tasks.Lint, img})
	}
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", b)
	return err
}

func find(ms []Manifest, match func(Manifest) bool) (Manifest, bool) {
	for _, m := range ms {
		if match(m) {
			return m, true
		}
	}
	return Manifest{}, false
}

func commitsCmd(root string, ms []Manifest, name string, stdin io.Reader, stdout io.Writer) error {
	m, ok := find(ms, func(m Manifest) bool { return m.Name == name })
	if !ok {
		return fmt.Errorf("no component named %q", name)
	}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		sha := strings.TrimSpace(sc.Text())
		if sha == "" {
			continue
		}
		files, err := commitFiles(root, sha)
		if err != nil {
			return err
		}
		if affects(m, files, ModeRelease) {
			fmt.Fprintln(stdout, sha)
		}
	}
	return sc.Err()
}

func getCmd(ms []Manifest, tag string, stdout io.Writer) error {
	prefix, version, err := ParseReleaseTag(tag)
	if err != nil {
		return err
	}
	m, ok := find(ms, func(m Manifest) bool { return m.Release != nil && m.Release.Tag == prefix })
	if !ok {
		return fmt.Errorf("no component releases under tag prefix %q", prefix)
	}
	kv := [][2]string{
		{"name", m.Name}, {"dir", strings.TrimSuffix(m.Dir, "/")}, {"version", version}, {"test", m.Tasks.Test},
	}
	if img := m.Release.Image; img != nil {
		kv = append(kv, [2]string{"image", img.Name}, [2]string{"description", img.Description},
			[2]string{"short_description", img.ShortDescription})
	}
	for _, p := range kv {
		fmt.Fprintf(stdout, "%s=%s\n", p[0], p[1])
	}
	return nil
}
```

- [ ] **Step 12: Run all tests, then vet and gofmt**

Run: `go test -race ./tools/components/ && go vet ./tools/components/ && gofmt -l tools/`
Expected: PASS, and `gofmt -l` prints nothing.

- [ ] **Step 13: Commit**

```bash
git add tools/components
git commit -q -m "feat(tools): add the component manifest tool" -m "CI and releases both need to know which components a change touches.
tools/components answers that from each component's component.yaml, so
the two can never disagree, and a new component or language needs a
manifest rather than workflow edits." \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: Manifests for every component (PR A, commit 3)

**Files:**
- Create: `minecraft/agent/component.yaml`, `minecraft/bridge/component.yaml`, `minecraft/afkbot/component.yaml`, `tools/components/component.yaml`

The descriptions below are copied verbatim from each old `release.yml` (`org.opencontainers.image.description` and the Docker Hub `short-description:`), read on 2026-09-28.

- [ ] **Step 1: Write the manifests**

`minecraft/agent/component.yaml`:

```yaml
name: agent
kind: service
language: go
depends:
  - internal/presenceapi/
tasks:
  build: CGO_ENABLED=0 go build ./minecraft/agent/...
  test: go test -race ./minecraft/agent/...
  lint: test -z "$(gofmt -l minecraft/agent)" && go vet ./minecraft/agent/...
release:
  tag: agent
  artifacts: [image]
  image:
    name: minecraft-server-agent
    description: "Minecraft Bedrock server chat agent: tool-calling LLM assistant, welcomes, stats, knowledge lookup"
    short_description: Minecraft Bedrock chat agent - tool-calling LLM assistant, welcomes, stats
```

`minecraft/bridge/component.yaml`:

```yaml
name: bridge
kind: service
language: go
tasks:
  build: CGO_ENABLED=0 go build ./minecraft/bridge/...
  test: go test -race ./minecraft/bridge/...
  lint: test -z "$(gofmt -l minecraft/bridge)" && go vet ./minecraft/bridge/...
release:
  tag: bridge
  artifacts: [image]
  image:
    name: mc-console-bridge
    description: Sidecar exposing the Bedrock server console over HTTP behind a command allowlist
    short_description: Sidecar exposing the Bedrock server console over HTTP behind a command allowlist
```

`minecraft/afkbot/component.yaml`:

```yaml
name: afkbot
kind: service
language: go
tasks:
  build: CGO_ENABLED=0 go build ./minecraft/afkbot/...
  test: go test -race ./minecraft/afkbot/...
  lint: test -z "$(gofmt -l minecraft/afkbot)" && go vet ./minecraft/afkbot/...
release:
  tag: afkbot
  artifacts: [image]
  image:
    name: minecraft-afk-bot
    description: Headless Minecraft Bedrock client that holds a player slot so mob farms keep ticking
    short_description: Headless Minecraft Bedrock client that holds a player slot so mob farms keep ticking
```

`tools/components/component.yaml`:

```yaml
name: components
kind: cli
language: go
tasks:
  build: go build ./tools/components
  test: go test -race ./tools/components
  lint: test -z "$(gofmt -l tools/components)" && go vet ./tools/components
```

- [ ] **Step 2: Validate, and check that the tasks run**

```bash
go run ./tools/components validate
go run ./tools/components releasable
for c in agent bridge afkbot; do bash -c "$(sed -n 's/^  lint: //p' minecraft/$c/component.yaml)" || exit 1; done
```

Expected: `4 components valid`, then `afkbot afkbot`, `agent agent`, `bridge bridge`, and all three lint commands exit 0.

- [ ] **Step 3: Commit**

```bash
git add minecraft/*/component.yaml tools/components/component.yaml
git commit -q -m "build: describe each component in a component.yaml" \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: CI, repo-wide workflows, Renovate and README (PR A, commits 4 and 5), then open PR A

**Files:**
- Create: `.github/workflows/ci.yml`, `codeql.yml`, `security-scan.yml`, `verify-pr-signatures.yml`, `protocol-check.yml`; `renovate.json`; `README.md`

- [ ] **Step 1: Copy the repo-wide workflows unchanged**

`security-scan.yml` and `verify-pr-signatures.yml` are byte-identical in all three old repos, so copy them from the agent. `codeql.yml` differs only in comments across the repos; take the agent's.

```bash
mkdir -p .github/workflows
for w in security-scan verify-pr-signatures codeql; do
  git show origin/main:minecraft/agent/.github/workflows/$w.yml > .github/workflows/$w.yml
done
```

(`origin/main` is still the bootstrap import at this point, with the per-repo `.github/` directories Task 3 deleted on this branch.)

In `codeql.yml`, replace the autobuild comment with this one. Autobuild at the root now builds the whole module:

```yaml
      # One module at the repo root, so autobuild reaches every Go component.
      # A TypeScript component adds javascript-typescript to languages above.
```

- [ ] **Step 2: `protocol-check.yml`**

Copy the AFK bot's file and change exactly these lines:
- `go run ./cmd/protocolcheck \` becomes `go run ./minecraft/afkbot/cmd/protocolcheck \`.
- In the PR body heredoc, `this repo speaks` becomes `the bots speak`. The agent now shares the bump.
- `Dispatch \`release.yml\` once merged` becomes `Merging releases every Go component that uses gophertunnel`. A `go.sum` change releases them via semantic-release, so there's no dispatch step any more.

```bash
git show origin/main:minecraft/afkbot/.github/workflows/protocol-check.yml > .github/workflows/protocol-check.yml
```

- [ ] **Step 3: `ci.yml`**

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read

jobs:
  # Which components this change touches, decided by the same tool that
  # decides what a release ships.
  plan:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.plan.outputs.matrix }}
    steps:
      - uses: actions/checkout@v7.0.1
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
          cache: true
      - id: plan
        env:
          # PRs diff against their base; pushes against the previous tip. An
          # empty or all-zero base (manual run, new branch) runs everything.
          BASE: ${{ github.event.pull_request.base.sha || github.event.before }}
        run: |
          set -euo pipefail
          go run ./tools/components validate
          matrix="$(go run ./tools/components affected -base "${BASE:-}" -head HEAD)"
          echo "$matrix" | jq .
          echo "matrix=$matrix" >> "$GITHUB_OUTPUT"

  component:
    needs: plan
    if: needs.plan.outputs.matrix != '[]'
    strategy:
      fail-fast: false
      matrix:
        include: ${{ fromJSON(needs.plan.outputs.matrix) }}
    name: ${{ matrix.name }}
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7.0.1
      # Every component gets Go: the release glue and repo tooling shell out
      # to tools/components even when the component itself is not Go.
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
          cache: true
      - if: matrix.language == 'node' || matrix.language == 'typescript'
        uses: actions/setup-node@v7.0.0
        with:
          node-version: 24
      - name: lint
        if: matrix.lint != ''
        run: ${{ matrix.lint }}
      - name: test
        run: ${{ matrix.test }}
      - name: build
        run: ${{ matrix.build }}
      - if: matrix.image
        uses: docker/setup-buildx-action@v4
      - name: image (no push)
        if: matrix.image
        uses: docker/build-push-action@v7
        with:
          context: .
          file: ${{ matrix.dir }}/Dockerfile
          push: false

  # The one required check. Its name never changes, however many component
  # jobs a change fans out to; a change touching no component skips them and
  # still passes here.
  ci-ok:
    if: always()
    needs: [plan, component]
    runs-on: ubuntu-latest
    steps:
      - env:
          PLAN: ${{ needs.plan.result }}
          COMPONENTS: ${{ needs.component.result }}
        run: |
          echo "plan=$PLAN components=$COMPONENTS"
          [[ "$PLAN" == success ]] && [[ "$COMPONENTS" == success || "$COMPONENTS" == skipped ]]
```

- [ ] **Step 4: `renovate.json`, merging the three old configs**

The agent and bridge only extended the presets. The AFK bot's gophertunnel and golang rules now protect every Go component.

```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "description": [
    "Runs as the Renovate GitHub App, whose commits GitHub signs, which verify-pr-signatures requires.",
    "The shared jdwlabs presets batch non-majors weekly and automerge them once required checks pass. The two rules below keep a human on the updates where a green build does not prove the bots can still reach the server."
  ],
  "extends": ["github>jdwlabs/.github", "github>jdwlabs/.github:automerge"],
  "packageRules": [
    {
      "description": [
        "gophertunnel carries the Bedrock packet schema the agent and the bots speak; an upgrade changes which server versions they can reach at all.",
        "Reviewed alone, at any time rather than in the weekly batch, and never automerged: protocol-check opens this bump when production moves past what the library speaks."
      ],
      "matchPackageNames": ["github.com/sandertv/gophertunnel"],
      "groupName": null,
      "schedule": ["at any time"],
      "automerge": false
    },
    {
      "description": "The golang build image must not run ahead of the go directive in go.mod, so a major or minor bump is raised together with go.mod, by a human.",
      "matchDatasources": ["docker"],
      "matchPackageNames": ["golang"],
      "matchUpdateTypes": ["major", "minor"],
      "groupName": null,
      "automerge": false
    }
  ],
  "platformAutomerge": true
}
```

- [ ] **Step 5: Root `README.md`**

````markdown
# gameops

Tooling around the game servers: services, bots, frontends and the packs they
load. One repo so a change that crosses components lands in one reviewed PR.

## Layout

| Path | What |
|---|---|
| `minecraft/agent/` | Bedrock server chat agent, census, join probe |
| `minecraft/bridge/` | Console bridge sidecar |
| `minecraft/afkbot/` | AFK bots |
| `internal/` | Go code shared across games |
| `minecraft/internal/` | Go code shared across Minecraft components (created when first needed) |
| `tools/components/` | Reads `component.yaml`; CI and releases use it |

Each component's own README covers what it does and how to run it.

## Adding a component

1. Create its directory under the game it belongs to.
2. Add a `component.yaml` (copy a neighbour's). `kind`, `language`, `tasks`
   and, if it ships, `release` are the whole contract; CI and releases read
   nothing else.
3. `go run ./tools/components validate`.

CI runs a component's `lint`, `test` and `build` when its directory, a
`depends` path or its language's root toolchain files change. A merge to
`main` releases it (tag `<release.tag>-v<semver>`) when a commit touching it
is a `feat`, `fix`, `perf`, `build` or `chore(deps)`.

## Go

One module, `github.com/jdwillmsen/gameops`, at the root. Build a component
from the root: `go build ./minecraft/agent/...`.

## License

[PolyForm Noncommercial 1.0.0](LICENSE.md)
````

Each component's `README.md` and `README.docker.md` may link to its old repo. Update those links:

```bash
grep -rln -E 'github.com/jdwillmsen/(minecraft-server-agent|mc-console-bridge|minecraft-afk-bot)' minecraft/*/README*.md \
  | xargs -r sed -i -E \
    -e 's#github.com/jdwillmsen/minecraft-server-agent#github.com/jdwillmsen/gameops/tree/main/minecraft/agent#g' \
    -e 's#github.com/jdwillmsen/mc-console-bridge#github.com/jdwillmsen/gameops/tree/main/minecraft/bridge#g' \
    -e 's#github.com/jdwillmsen/minecraft-afk-bot#github.com/jdwillmsen/gameops/tree/main/minecraft/afkbot#g'
git diff --stat -- 'minecraft/*/README*.md'
```

Read the diff. A link that was a `go install` path or a clone URL, not a web link, has to be fixed by hand to what makes sense now.

- [ ] **Step 6: Local check of the CI plan step**

```bash
go run ./tools/components affected -base origin/main -head HEAD | jq -r '.[].name'
```

Expected: `afkbot agent bridge components` in some order. Every component is touched on this branch.

- [ ] **Step 7: Commit twice**

```bash
git add .github renovate.json
git commit -q -m "ci: run checks per affected component" -m "One CI workflow plans a matrix from the component manifests and ends in a
single ci-ok check, so branch protection stays fixed while the set of
jobs changes per PR. CodeQL, the security scan and signature checks run
once for the repo instead of once per imported repo." \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git add README.md minecraft/*/README*.md
git commit -q -m "docs: describe the gameops layout and how to add a component" \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 8: Push and open PR A**

```bash
git push -u origin chore/JDWLABS-657-monorepo-foundation
gh pr create -R jdwillmsen/gameops --base main \
  --title "chore(JDWLABS-657): monorepo foundation - one module, component manifests, CI" \
  --body "$(cat <<'EOF'
Turns the imported history into one working repo.

- **build: make gameops one Go module**: the three go.mod files merge into one at the root, imports are rewritten, presenceapi becomes internal/presenceapi, and Dockerfiles build from the root. No behaviour change.
- **feat(tools): add the component manifest tool**: tools/components, which CI and (next PR) releases use to decide what a change affects.
- **build: component.yaml per component**
- **ci: run checks per affected component**: a matrix plus the single `ci-ok` required check. CodeQL, the security scan and signature checks are copied from the old repos.
- **docs**: root README; component README links point here.

Verified locally: `go build/vet/test -race ./...` green, all three images build from the root, `tools/components validate` passes.

Releases are not wired yet: nothing on main tags or publishes until the release PR lands.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 9: Babysit to green.** Required checks: `analyze`, `ci-ok`, `scan / *`, `signatures / signatures`. Every component job must show in the run. Fix anything red with new commits on the branch.
- [ ] **Step 10: Human gate.** The human reviews PR A line by line, then rebase-merges it. Refresh with `git -C ~/projects/gameops pull --ff-only`.

---

### Task 7: Release glue (PR B)

**Files:**
- Create: `tools/release/component-filter.mjs`, `tools/release/package.json`, `tools/release/package-lock.json`, `tools/release/dry-run-test.sh`, `tools/release/component.yaml`, `.releaserc.json`, `.github/workflows/semantic-release.yml`, `.github/workflows/release.yml`

Branch: `chore/JDWLABS-657-release-glue` from `origin/main`, in `~/worktrees/gameops/chore/JDWLABS-657-release-glue`.

**Interfaces:**
- Consumes: `go run ./tools/components commits -component <name>` (stdin SHAs, stdout kept SHAs), `releasable`, and `get -release-tag <tag>`, all from Task 4.
- Produces: tags `<tag>-v<semver>`, GitHub releases, and images `<image>:<semver>`.

- [ ] **Step 1: Pinned toolchain.** Before, the old repos pinned it through `npx -p` arguments. It now needs to be a real dependency tree the local plugin can import from.

```bash
mkdir -p tools/release && cd tools/release
cat > package.json <<'EOF'
{
  "private": true,
  "description": "Pinned semantic-release toolchain for per-component releases; see component-filter.mjs.",
  "type": "module"
}
EOF
npm install --save-exact --no-fund --no-audit \
  semantic-release@25.0.9 \
  conventional-changelog-conventionalcommits@9.3.1 \
  @semantic-release/exec@7.1.0 \
  @semantic-release/commit-analyzer@13 \
  @semantic-release/release-notes-generator@14
cd ../..
```

Expected: `package.json` gains exact versions, and `package-lock.json` is created. `node_modules/` is git-ignored by the root `.gitignore`.

- [ ] **Step 2: The plugin, `tools/release/component-filter.mjs`**

```js
// Each component runs semantic-release on its own, but semantic-release reads
// every commit on main. This wraps the conventional-commits analyzer and
// notes generator so each run only sees commits that affect its component,
// as decided by tools/components -- the same code CI uses.
import { execFileSync } from 'node:child_process';
import * as analyzer from '@semantic-release/commit-analyzer';
import * as notes from '@semantic-release/release-notes-generator';

function componentCommits(context) {
  const component = context.env.RELEASE_COMPONENT;
  if (!component) {
    throw new Error('RELEASE_COMPONENT is not set; run through the release workflow');
  }
  const out = execFileSync(
    'go',
    ['run', './tools/components', 'commits', '-component', component],
    { cwd: context.cwd, input: context.commits.map((c) => c.hash).join('\n'), encoding: 'utf8' },
  );
  const kept = new Set(out.split('\n').filter(Boolean));
  context.logger.log('%s: %d of %d commits affect it', component, kept.size, context.commits.length);
  return { ...context, commits: context.commits.filter((c) => kept.has(c.hash)) };
}

export const analyzeCommits = (config, context) =>
  analyzer.analyzeCommits(config, componentCommits(context));

export const generateNotes = (config, context) =>
  notes.generateNotes(config, componentCommits(context));
```

- [ ] **Step 3: `.releaserc.json` at the root.** The preset options are global, so they reach the local plugin both in real runs and in the dry-run test, where `--plugins` replaces the plugin list. `build` releases a patch because a `build:` commit changes a shipped image.

```json
{
  "branches": ["main"],
  "preset": "conventionalcommits",
  "releaseRules": [
    { "type": "chore", "scope": "deps", "release": "patch" },
    { "type": "build", "release": "patch" }
  ],
  "presetConfig": {
    "types": [
      { "type": "feat", "section": "Features" },
      { "type": "fix", "section": "Bug Fixes" },
      { "type": "perf", "section": "Performance Improvements" },
      { "type": "revert", "section": "Reverts" },
      { "type": "build", "section": "Build System" },
      { "type": "chore", "scope": "deps", "section": "Dependencies" },
      { "type": "docs", "section": "Documentation", "hidden": true },
      { "type": "style", "section": "Styles", "hidden": true },
      { "type": "chore", "section": "Miscellaneous Chores", "hidden": true },
      { "type": "refactor", "section": "Code Refactoring", "hidden": true },
      { "type": "test", "section": "Tests", "hidden": true },
      { "type": "ci", "section": "Continuous Integration", "hidden": true }
    ]
  },
  "plugins": [
    "./tools/release/component-filter.mjs",
    ["@semantic-release/github", { "successComment": false, "failComment": false, "releasedLabels": false }],
    ["@semantic-release/exec", {
      "successCmd": "printf '%s %s\\n' \"$RELEASE_COMPONENT\" '${nextRelease.gitTag}' >> \"$RELEASED_FILE\""
    }]
  ]
}
```

- [ ] **Step 4: The failing dry-run test, `tools/release/dry-run-test.sh`**

```bash
#!/usr/bin/env bash
# Dry-runs the release glue against a scratch history built from this tree, so
# a change to it is tested without tagging anything real.
set -euo pipefail
repo="$(git rev-parse --show-toplevel)"
sr="$repo/tools/release/node_modules/.bin/semantic-release"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git init -q --bare -b main "$work/remote.git"
git clone -q "$work/remote.git" "$work/clone" 2>/dev/null
cd "$work/clone"
export GIT_CONFIG_GLOBAL=/dev/null GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid \
  GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
# The working tree, not HEAD, so an uncommitted change to the glue is what
# gets tested.
(cd "$repo" && git ls-files -z -co --exclude-standard | tar --null -T - -c) | tar -x
ln -s "$repo/tools/release/node_modules" tools/release/node_modules
git add -A && git commit -qm "chore: fixture root"
for c in agent bridge afkbot; do git tag "$c-v1.0.0"; done

change() { echo "$2" >> "$1"; git add -A; git commit -qm "$2"; }
change minecraft/agent/README.md "feat: agent feature"
change minecraft/bridge/README.md "fix: bridge fix"
change README.md "feat: root-only change releases nothing"
change go.sum "chore(deps): bump a shared dependency"
git push -q origin main --tags

fail=0
expect() {
  local component=$1 want=$2 out
  out="$(RELEASE_COMPONENT=$component "$sr" --dry-run --no-ci --branches main \
    --repository-url "$work/remote.git" --tag-format "$component-v\${version}" \
    --plugins ./tools/release/component-filter.mjs 2>&1)" || { echo "$out"; fail=1; return; }
  if grep -q "The next release version is $want" <<<"$out"; then
    echo "ok   $component -> $want"
  else
    echo "FAIL $component: want $want"; grep -E "next release|no relevant|commits affect" <<<"$out"; fail=1
  fi
}
# agent: feat (own dir) + deps -> minor. bridge: fix + deps -> patch.
# afkbot: only deps -> patch; the root-only feat must not reach it.
expect agent 1.1.0
expect bridge 1.0.1
expect afkbot 1.0.1
exit $fail
```

```bash
chmod +x tools/release/dry-run-test.sh
git add tools/release/package.json tools/release/package-lock.json tools/release/dry-run-test.sh
```

- [ ] **Step 5: Watch it fail before the plugin and config exist**

Move the two files from Steps 2 and 3 aside and run the test:

```bash
mv tools/release/component-filter.mjs .releaserc.json /tmp/
tools/release/dry-run-test.sh; echo "exit=$?"
mv /tmp/component-filter.mjs tools/release/ && mv /tmp/.releaserc.json .
```

Expected: a non-zero `exit=`, with semantic-release unable to load `./tools/release/component-filter.mjs`.

- [ ] **Step 6: Run it for real**

Run: `tools/release/dry-run-test.sh`
Expected:
```
ok   agent -> 1.1.0
ok   bridge -> 1.0.1
ok   afkbot -> 1.0.1
```

If `afkbot` shows `1.1.0`, the filter is not applied: check the `commits affect it` log line. If every component shows `1.1.0`, the plugin is loaded but `commits` is returning everything.

- [ ] **Step 7: `tools/release/component.yaml`,** so CI runs this test whenever the glue changes

```yaml
name: release-glue
kind: cli
language: node
tasks:
  build: npm ci --prefix tools/release --no-fund --no-audit
  test: npm ci --prefix tools/release --no-fund --no-audit && tools/release/dry-run-test.sh
```

Run `go run ./tools/components validate`. Expected: `5 components valid`.

- [ ] **Step 8: `.github/workflows/semantic-release.yml`**

Keep the old file's trigger, `if:` guard, concurrency block and checkout comments verbatim; they still apply. Replace the job bodies with these:

```yaml
jobs:
  release:
    if: >-
      github.event.workflow_run.conclusion == 'success' &&
      github.event.workflow_run.event == 'push' &&
      github.event.workflow_run.head_branch == 'main' &&
      github.event.workflow_run.head_repository.full_name == github.repository
    runs-on: ubuntu-latest
    permissions:
      contents: write
    outputs:
      tags: ${{ steps.released.outputs.tags }}
    steps:
      - uses: actions/checkout@v7.0.1
        with:
          ref: ${{ github.event.workflow_run.head_sha }}
          fetch-depth: 0
          persist-credentials: false
      - uses: actions/setup-go@v7
        with:
          go-version-file: go.mod
          cache: true
      - uses: actions/setup-node@v7.0.0
        with:
          node-version: 24
      - run: npm ci --prefix tools/release --no-fund --no-audit

      # One run per component, in series, so their tag pushes never race. The
      # exec plugin appends each tag it pushes to RELEASED_FILE as it goes, so
      # a later component failing cannot hide an earlier one's tag.
      - id: release
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          RELEASED_FILE: ${{ runner.temp }}/released
        run: |
          set -euo pipefail
          : > "$RELEASED_FILE"
          go run ./tools/components releasable | while read -r name tag; do
            RELEASE_COMPONENT="$name" tools/release/node_modules/.bin/semantic-release \
              --tag-format "${tag}-v\${version}"
          done

      # always(): an already-pushed tag must reach the publish job even if a
      # later component's run failed, or the tag names an image that was never
      # built.
      - id: released
        if: always()
        env:
          RELEASED_FILE: ${{ runner.temp }}/released
        run: |
          tags="$(jq -Rsc 'split("\n") | map(select(length > 0) | split(" ")[1])' "$RELEASED_FILE" 2>/dev/null || echo '[]')"
          echo "tags=$tags" >> "$GITHUB_OUTPUT"
          echo "released: $tags"

  publish:
    needs: release
    if: always() && needs.release.outputs.tags != '' && needs.release.outputs.tags != '[]'
    strategy:
      fail-fast: false
      matrix:
        tag: ${{ fromJSON(needs.release.outputs.tags) }}
    permissions:
      contents: read
      packages: write
    uses: ./.github/workflows/release.yml
    with:
      tag: ${{ matrix.tag }}
    secrets:
      DOCKERHUB_TOKEN: ${{ secrets.DOCKERHUB_TOKEN }}
```

- [ ] **Step 9: `.github/workflows/release.yml`,** one reusable workflow for every image component

Start from the agent's old `release.yml` (`git -C ~/projects/minecraft-server-agent show origin/main:.github/workflows/release.yml`; the old clones stay until Task 9) and keep all of its comments that still apply. The changes:
1. The trigger tags become `['*-v[0-9]*']`. `workflow_dispatch` and `workflow_call` take `tag` described as `Full release tag, e.g. agent-v0.24.0`.
2. A new first job `resolve` checks out `refs/tags/${TAG}` (`TAG: ${{ inputs.tag || github.ref_name }}`), runs `go run ./tools/components get -release-tag "$TAG" >> "$GITHUB_OUTPUT"`, and exposes `name`, `dir`, `version`, `test`, `image`, `description` and `short_description` as job outputs. If the output has no `image=` line, the job fails with `::error::<tag> releases no image`.
3. A `verify` job (`needs: resolve`) checks out the tag, sets up Go and runs `${{ needs.resolve.outputs.test }}`. This carries over the bridge's re-run-CI-before-publish guarantee.
4. `publish` (`needs: [resolve, verify]`):
   - `IMAGE_METADATA` takes title, description and documentation from `needs.resolve.outputs`. Title is the `image` output. Documentation is `https://github.com/${{ github.repository }}/tree/main/${{ needs.resolve.outputs.dir }}#readme`.
   - `images:` becomes `ghcr.io/${{ github.repository_owner }}/${{ needs.resolve.outputs.image }}`, and the tag value is `needs.resolve.outputs.version`.
   - Checkout uses `ref: refs/tags/${{ inputs.tag || github.ref_name }}`.
   - `build-push-action` gains `file: ${{ needs.resolve.outputs.dir }}/Dockerfile` with `context: .`.
   - The old `version` step's semver regex goes away; `ParseReleaseTag` does that now.
5. `mirror-dockerhub`:
   - It uses the same outputs for the image name and SOURCE/TARGET.
   - `readme-filepath` becomes `./${{ needs.resolve.outputs.dir }}/README.docker.md`, and `short-description` is `${{ needs.resolve.outputs.short_description }}`.
   - Its checkout ref uses the full tag.

Check it:

```bash
docker run --rm -v "$PWD:/repo" -w /repo rhysd/actionlint:latest -color
```

Expected: no findings. (`actionlint` via Docker needs no install.)

- [ ] **Step 10: Commit, push and open PR B**

```bash
git add tools/release .releaserc.json .github/workflows/semantic-release.yml .github/workflows/release.yml
git commit -q -m "ci: release each component on its own" -m "semantic-release runs once per component with a component-prefixed tag,
and a local plugin limits each run to the commits that touch that
component, via the same tools/components logic CI uses. build: commits
now release a patch, because they change a shipped image. One reusable
release workflow publishes any component's image from its manifest." \
  -m "Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
git push -u origin chore/JDWLABS-657-release-glue
gh pr create -R jdwillmsen/gameops --base main \
  --title "ci(JDWLABS-657): per-component semantic-release and image publishing" \
  --body "$(cat <<'EOF'
Adds releases to the monorepo.

- semantic-release runs once per releasable component (`tools/components releasable`), in series, with tag format `<tag>-v${version}`.
- `tools/release/component-filter.mjs` narrows each run to commits that affect that component, using `tools/components commits`.
- `build:` now releases a patch (a Dockerfile/build change changes the image).
- `release.yml` is one reusable workflow; the manifest supplies the image name, descriptions and Dockerfile path. Dual publish, labels, provenance/SBOM and no-`latest` are unchanged.
- A later component failing can't strand an earlier one's tag: the tags are collected with `always()`.

`tools/release/dry-run-test.sh` (run by CI via the `release-glue` component): agent 1.1.0, bridge 1.0.1, afkbot 1.0.1 on a fixture where a root-only `feat` must not reach afkbot.

**On merge** this releases all three components: PR A's `build:` commit counts as a patch for each. That first release is the end-to-end check.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

- [ ] **Step 11: Babysit to green; human review; rebase-merge.**

---

### Task 8: First releases and chart check

- [ ] **Step 1: Watch the release after PR B merges**

```bash
gh run list -R jdwillmsen/gameops --workflow semantic-release.yml -L 1
gh run watch -R jdwillmsen/gameops "$(gh run list -R jdwillmsen/gameops --workflow semantic-release.yml -L 1 --json databaseId -q '.[0].databaseId')"
git -C ~/projects/gameops fetch -q --tags && git -C ~/projects/gameops tag --sort=-creatordate | head -3
```

Expected: three new tags, one each for `agent-v…`, `bridge-v…` and `afkbot-v…`, each a patch (or more) above the imported latest.

- [ ] **Step 2: Images exist in both registries with the unchanged names**

```bash
cd ~/projects/gameops
for t in $(git tag --sort=-creatordate | head -3); do
  facts="$(go run ./tools/components get -release-tag "$t")"
  img="$(sed -n 's/^image=//p' <<<"$facts")"; ver="$(sed -n 's/^version=//p' <<<"$facts")"
  echo "$img:$ver"
  docker buildx imagetools inspect "ghcr.io/jdwillmsen/$img:$ver" --format '{{.Manifest.Digest}}'
  docker buildx imagetools inspect "docker.io/jdwillmsen/$img:$ver" --format '{{.Manifest.Digest}}'
done
```

Expected: for each image, two identical digests (ghcr.io, then docker.io).

- [ ] **Step 3: The chart rolls with only a version change**

In `~/projects/jdwlabs/jdw-deployments`, use a worktree on branch `chore/JDWLABS-657-gameops-images`. Bump the three image tags in `charts/minecraft-fwb/values.yaml` to the new versions, and nothing else. The AFK bot image is `enabled:false` in that repo's Renovate config and only moves by hand, so doing this by hand for all three is consistent.

```bash
helm template charts/minecraft-fwb | grep -E 'image: .*(minecraft-server-agent|mc-console-bridge|minecraft-afk-bot)'
```

Expected: the three images at the new tags, with repository names unchanged. Open the PR, and deploy it through the normal Argo CD flow after human approval. Afterwards confirm the pods are Ready and the agent answers in chat (`@server` ping) before moving on.

---

### Task 9: Archive the old repos and swap local clones

**Human gate:** archiving is visible to anyone who follows these repos. Confirm with the human before Step 2.

- [ ] **Step 1: Pointer READMEs.** For each old repo, open a PR that prepends this to its `README.md`:

```markdown
> **Moved.** This project now lives in
> [jdwillmsen/gameops](https://github.com/jdwillmsen/gameops/tree/main/minecraft/<c>),
> history included. This repository is archived and read-only.
```

Use `<c>` = `agent`, `bridge` or `afkbot`. Branch `docs/JDWLABS-657-moved-to-gameops`. It merges through each repo's existing checks. It's a `docs:` commit, so it doesn't release.

- [ ] **Step 2: Archive**

```bash
for r in minecraft-server-agent mc-console-bridge minecraft-afk-bot; do
  gh repo archive jdwillmsen/$r --yes
  gh repo view jdwillmsen/$r --json isArchived -q .isArchived
done
```

Expected: `true` three times.

- [ ] **Step 3: Local clones.** Leave them for the human to delete. Deleting them from here would destroy any uncommitted work in their worktrees. List what exists so the human can decide:

```bash
for r in minecraft-server-agent mc-console-bridge minecraft-afk-bot; do
  git -C ~/projects/$r worktree list
  git -C ~/projects/$r status --short | head -5
done
```

- [ ] **Step 4: Close out.** Record in the epic, with command output:
  - the `git log --follow` evidence from Task 1 Step 5;
  - the three release tags and digest pairs from Task 8;
  - the chart PR;
  - the `isArchived` output.

  Then transition it to Done.

---

## Out of scope, to ticket at close-out

- The AFK bot's `internal/{mcauth,liveness,logging,mcproto,skin}` are copies of `minecraft/agent/pkg/…`. In one module they can become imports. That's a behaviour-affecting cleanup; it needs its own ticket and its own review.
- `no-mistakes` pipeline setup for `gameops`, if you want it as the ship path there.
