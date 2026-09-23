# Actor Presence 3: Presence Control Plane in the Server Agent — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the agent a runtime switch that parks or restores any actor (the agent itself, either AFK bot, a group, or all of them) within seconds, from chat or HTTP. The switch keeps state in Postgres, runs its policy loop on the leader only, kicks parked actors that stay on the server, and exports the metrics the alerts are rewritten against.

**Architecture:** There are two parts. `presenceapi/` is a separate Go module holding only the wire types, which the AFK bot imports. `internal/presence` holds `Registry` (actors from `PRESENCE_ACTORS`), pure `Policy`, `Store` (Postgres, versioned writes), `Service` (validation, audit and the one write path shared by the API, chat and loop), `Gate` and `JoinLog`, the leader-only `Loop`, the `/v1` `API` and the `ChatPlugin`. `cmd/agent` wires them into plan 1's lifecycle split. The presence `Gate` replaces `alwaysPresent{}` as the `sessionGate` that `runSessions` reads, and the loop starts next to `startLiveWork` in each leader turn. Execute on a new branch in a fresh worktree of `minecraft-server-agent` cut from `origin/main` **after plan 1 (`2026-09-23-actor-presence-01-lifecycle-split.md`) has merged**. This plan does not create the worktree. The `presence_overrides` and `presence_status` tables come from plan 2's `V8__minecraft_presence.sql`. The code handles a missing table as a deploy-ordering state (`pgerr.Unready`), so this plan may ship before that migration runs.

**Tech Stack:** Go 1.27, `net/http` pattern routing (`GET /v1/actors/{id}/presence`), pgx v5 (already a dependency), Prometheus client_golang (already a dependency). The only new module is the in-repo `presenceapi`, which uses the standard library only.

**Spec:** `docs/superpowers/specs/2026-09-23-actor-presence-design.md`. This plan implements "Agent: `internal/presence`", "Guaranteed unload", "Interfaces" (HTTP and chat), "Data", "Operations" (metrics only) and "Error handling", which is rollout step 3. **Contract:** `docs/superpowers/plans/2026-09-23-actor-presence-00-contract.md` is authoritative for every cross-repo name. **Builds on:** plan 1's `sessionGate`, `runSessions`, `sessionModes`, `alwaysPresent`, `bridgeRoster` and `(*adapters.BridgeClient).OnlinePlayers`.

## Global Constraints

- Go 1.27 as pinned in `go.mod`. `presenceapi/go.mod` is `module github.com/jdwillmsen/minecraft-server-agent/presenceapi`, `go 1.27`, with **no `require` lines at all**, so standard library only. The root module gets `require github.com/jdwillmsen/minecraft-server-agent/presenceapi v0.1.0` plus `replace github.com/jdwillmsen/minecraft-server-agent/presenceapi => ./presenceapi`. No other new dependency is allowed.
- Every name in the contract is used verbatim: the type and field names, JSON tags, routes, scopes (`presence:read`, `presence:write`, `presence:report`), env vars (`PRESENCE_ACTORS`, `PRESENCE_TOKENS`, `PRESENCE_SELF_ID`), metric names, error codes, the ETag format `"<version>-<effective>"`, actor ids, the regex `^[a-z0-9][a-z0-9-]{0,62}$`, and the `set_by` prefixes.
- Timings are fixed: policy tick `10s`, kick grace `20s`, and a chat park of the agent defaults to `until = now + 1h` with `wake_on = {"any_player_join": true}`. An explicit duration replaces only the timer.
- The `/v1` routes are mounted only when `PRESENCE_TOKENS` is non-empty (and therefore `PRESENCE_ACTORS` too). With both unset the agent behaves exactly as after plan 1: `alwaysPresent{}` stays the gate, and no loop, route or metric series appears.
- API reads and writes go straight to Postgres on any replica. Only the loop is leader-only. `mc_presence_*` gauges are exported by the leader only and withdrawn when its turn ends.
- The actor ids from the registry are the only values that reach the `actor` label. Nothing typed in chat reaches a label.
- Every override write, removal and wake produces one row in the existing `minecraft.command_audit` table through `audit.Store` (the `auditTrail` wrapper in production). An audit failure is logged and counted (`metrics.AuditWriteFailure`). It never fails the write.
- Log events are snake_case with `logging.Fields`. Tokens never appear in a log line, an error message or a test failure message.
- Comments explain why, never what, at the density of the file they sit in. No ticket IDs in code, comments, docs or commit messages. Conventional commits.
- Run `gofmt -w` on every touched file. Keep `go vet ./...`, `go test -race ./...` (root and `presenceapi/`) and `golangci-lint run` clean.
- Every commit message ends with a blank line and then:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  ```

## Review Focus

- **A park set just after a player joined, inside one tick window.** An operator types `!leave` right after a friend arrives. Expected: the join that happened *before* the override's `set_at` does not wake it, so the agent stays parked until a *later* join. Pinned in Task 4 (`join before the park is ignored`).
- **An operator replaces an override between the loop's read and its removal.** Expected: the loop deletes only the version it evaluated, so the operator's newer row survives and is re-evaluated next tick. Pinned in Task 5 (live `Remove` by version) and Task 9 (`TestLoopDoesNotRemoveANewerOverride`).
- **A bot that crashed and stopped reporting.** Its last row still says `connected: true`. Expected: `mc_presence_observed` drops to 0 once the report is older than 60s (six bot polls), so the degraded alert still fires for an actor that should be present. Pinned in Task 9 (`TestLoopTreatsAStaleReportAsNotConnected`).
- **A standby that wins the lock while its own override says parked.** Expected: it never joins the world, not even for the moment before the loop's first tick. The loop primes the gate synchronously before `runSessions` first reads it. Pinned in Task 9 (`TestPrimeDecidesTheGateBeforeRunStarts`) and Task 12 (wiring order).
- **A bot's token used against another actor or a group.** Expected: 403 `forbidden` for status reports, single-actor writes and group writes alike, with nothing written. Pinned in Task 10 (`TestBoundTokenCannotSpeakForAnotherActor`).
- **A gamertag whose case differs between `list` output and configuration.** Expected: the kick still fires, and an actor's own join never counts as a player join. Pinned in Task 4 (actor join, different case) and Task 9 (kick, different case).

---

## File Structure

| Path | Action | Responsibility |
|---|---|---|
| `presenceapi/go.mod` | Create | Separate module, standard library only. |
| `presenceapi/presenceapi.go` | Create | Contract types verbatim, plus `State.Valid` and the `Code*` constants for `Error.Code`. |
| `presenceapi/presenceapi_test.go` | Create | Round-trips every golden file byte-for-byte. |
| `presenceapi/testdata/*.json` | Create | The six golden files named in the contract. |
| `.github/workflows/ci.yml` | Modify (after the `go test` step, line 40) | Vet and test the nested module, which `./...` at the root skips. |
| `go.mod` | Modify | `require` and `replace` for `presenceapi`. |
| `Dockerfile` | Modify (lines 4-5) | Copy `presenceapi/go.mod` before `go mod download`, which otherwise cannot resolve the `replace`. |
| `internal/config/presence.go` | Create | `PresenceActor`, `PresenceToken`, `loadPresence` and their validation. |
| `internal/config/config.go` | Modify (struct lines 161-169, `Load` lines 275-316) | New fields, filled from `loadPresence`. |
| `internal/config/presence_test.go` | Create | Parsing and every refusal. |
| `internal/presence/registry.go` | Create | `Actor`, `Registry`: ids, groups, `all`, target resolution. |
| `internal/presence/policy.go` | Create | Pure `Evaluate`, `ChatPark`, `View`. |
| `internal/presence/store.go` | Create | `Store` interface, `Change`, `ErrConflict`, `ErrDisabled`, `Nop`. |
| `internal/presence/postgres.go` | Create | `Postgres` store over the shared pool. |
| `internal/presence/postgres_live_test.go` | Create | `livedb` tests against the real V8 tables. |
| `internal/presence/service.go` | Create | Validation, versioned writes, audit, change notification. |
| `internal/presence/fakes_test.go` | Create | In-memory `fakeStore`, `fakeAudit`, `testRegistry` shared by the package tests. |
| `internal/presence/gate.go` | Create | `Gate`: this process's own effective state, satisfying plan 1's `sessionGate`. |
| `internal/presence/joinlog.go` | Create | `JoinLog`: recent player arrivals from both session modes. |
| `internal/presence/loop.go` | Create | Leader-only `Loop`: expire, wake, gate, self status, metrics, kicks. |
| `internal/presence/loop_live_test.go` | Create | `livedb` leader-handover test. |
| `internal/presence/api.go` | Create | `/v1` handlers: tokens, scopes, ETag/304, 409, 403. |
| `internal/presence/chat.go` | Create | `ChatPlugin` (`!presence`, `!park`, `!unpark`, `!leave`), `LeaveArgs`. |
| `internal/metrics/presence.go` | Create | `mc_presence_*` series and their recorders. |
| `internal/adapters/kick.go` | Create | `(*BridgeClient).Kick`. |
| `internal/httpapi/presence.go` | Create | `MountPresence`. |
| `internal/httpapi/metrics.go` | Modify (`SetConnected`, lines 45-52) | `Connected()` read-back for the agent's own observed status. |
| `cmd/agent/presence.go` | Create | `presenceRuntime`: builds and leads the presence parts, and wraps plan 1's session modes. |
| `cmd/agent/bridgeroster.go` | Modify (plan 1's file, `apply`) | `onJoin` hook so joins seen from the bridge count for `wake_on`. |
| `cmd/agent/main.go` | Modify | Build the runtime, mount the API, register the plugin, route `@server leave`, swap the gate, lead the loop. |
| `README.md` | Modify | Env vars, metrics, a "Parking actors" section with routes and chat. |

Each `internal/presence` file above has a matching `_test.go`, created in the same task.

---
### Task 1: `presenceapi` contract module

**Files:**
- Create: `presenceapi/go.mod`, `presenceapi/presenceapi.go`, `presenceapi/presenceapi_test.go`
- Create: `presenceapi/testdata/presence_parked.json`, `presence_default.json`, `set_request.json`, `status.json`, `error_conflict.json`, `actors.json`
- Modify: `.github/workflows/ci.yml` (append a step after `go test`, line 39-40)

**Interfaces:**
- Consumes: nothing.
- Produces (package `presenceapi`, contract verbatim): `type State string`, `StatePresent`, `StateParked`, `func (s State) Valid() bool`, `WakeOn`, `Override`, `Presence`, `SetRequest`, `Status`, `ActorView`, `Error`. Also `const CodeNotFound, CodeConflict, CodeForbidden, CodeUnauthorized, CodeInvalid, CodeUnavailable` (the six `Error.Code` strings from the contract's comment).

- [ ] **Step 1: Write the golden files**

These are the canonical `json.MarshalIndent(v, "", "  ")` form plus a trailing newline. The round-trip test compares bytes, so write them exactly as shown.

`presenceapi/testdata/presence_parked.json`:
```json
{
  "actor_id": "afk-bot-1",
  "effective": "parked",
  "default": "present",
  "override": {
    "state": "parked",
    "until": "2026-09-23T20:00:00Z",
    "wake_on": {
      "any_player_join": true
    },
    "reason": "chunk budget for the build",
    "set_by": "api:tools-mc",
    "set_at": "2026-09-23T18:00:00Z",
    "version": 3
  }
}
```

`presenceapi/testdata/presence_default.json`:
```json
{
  "actor_id": "afk-bot-2",
  "effective": "present",
  "default": "present"
}
```

`presenceapi/testdata/set_request.json`:
```json
{
  "state": "parked",
  "duration": "2h",
  "wake_on": {
    "players": [
      "Jdwillmsen"
    ]
  },
  "reason": "chunk budget for the build",
  "version": 0
}
```

`presenceapi/testdata/status.json`:
```json
{
  "connected": true,
  "observed_state": "present",
  "last_seen": "2026-09-23T18:00:05Z",
  "process_version": "1.4.0"
}
```

`presenceapi/testdata/error_conflict.json`:
```json
{
  "code": "conflict",
  "message": "afk-bot-1 changed since version 2 was read",
  "current": {
    "actor_id": "afk-bot-1",
    "effective": "parked",
    "default": "present",
    "override": {
      "state": "parked",
      "reason": "chunk budget for the build",
      "set_by": "api:ops",
      "set_at": "2026-09-23T18:00:00Z",
      "version": 3
    }
  }
}
```

`presenceapi/testdata/actors.json`:
```json
[
  {
    "id": "agent",
    "gamertag": "JdwAgent",
    "kind": "agent",
    "groups": [],
    "actor_id": "agent",
    "effective": "present",
    "default": "present",
    "status": {
      "connected": true,
      "observed_state": "present",
      "last_seen": "2026-09-23T18:00:05Z",
      "process_version": "0.21.0"
    }
  },
  {
    "id": "afk-bot-1",
    "gamertag": "JdwAfk1",
    "kind": "afk-bot",
    "groups": [
      "bots"
    ],
    "actor_id": "afk-bot-1",
    "effective": "parked",
    "default": "present",
    "override": {
      "state": "parked",
      "reason": "chunk budget for the build",
      "set_by": "chat:Jdwillmsen",
      "set_at": "2026-09-23T18:00:00Z",
      "version": 1
    }
  }
]
```

- [ ] **Step 2: Write the failing test**

`presenceapi/presenceapi_test.go`:
```go
package presenceapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// roundTrip decodes a golden file strictly and encodes it again. Byte
// equality pins every JSON name and every omitempty: the AFK bot decodes
// these same files, so a tag changed here without changing the files fails
// in both repositories rather than in production.
func roundTrip[T any](t *testing.T, name string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var v T
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, raw) {
		t.Errorf("%s does not round-trip:\n--- got\n%s--- want\n%s", name, got, raw)
	}
	return v
}

func TestGoldenFilesRoundTrip(t *testing.T) {
	t.Run("presence_parked", func(t *testing.T) { roundTrip[Presence](t, "presence_parked.json") })
	t.Run("presence_default", func(t *testing.T) { roundTrip[Presence](t, "presence_default.json") })
	t.Run("set_request", func(t *testing.T) { roundTrip[SetRequest](t, "set_request.json") })
	t.Run("status", func(t *testing.T) { roundTrip[Status](t, "status.json") })
	t.Run("error_conflict", func(t *testing.T) { roundTrip[Error](t, "error_conflict.json") })
	t.Run("actors", func(t *testing.T) { roundTrip[[]ActorView](t, "actors.json") })
}

func TestParkedPresenceDecodesToItsMeaning(t *testing.T) {
	p := roundTrip[Presence](t, "presence_parked.json")
	if p.Effective != StateParked || p.Default != StatePresent {
		t.Errorf("effective/default = %q/%q, want parked/present", p.Effective, p.Default)
	}
	if p.Override == nil || p.Override.Until == nil || p.Override.WakeOn == nil {
		t.Fatalf("override = %+v, want until and wake_on set", p.Override)
	}
	if want := time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC); !p.Override.Until.Equal(want) {
		t.Errorf("until = %v, want %v", p.Override.Until, want)
	}
	if !p.Override.WakeOn.AnyPlayerJoin || p.Override.Version != 3 {
		t.Errorf("wake_on/version = %+v/%d, want any_player_join and 3", p.Override.WakeOn, p.Override.Version)
	}
}

// Version 0 is "I expect no override", so it must be sent rather than
// omitted: a request without it would be a different request.
func TestSetRequestAlwaysCarriesItsVersion(t *testing.T) {
	b, err := json.Marshal(SetRequest{State: StateParked, Reason: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"version":0`)) {
		t.Errorf("encoded %s, want an explicit version 0", b)
	}
}

func TestStateValid(t *testing.T) {
	for s, want := range map[State]bool{StatePresent: true, StateParked: true, "": false, "Parked": false, "gone": false} {
		if got := s.Valid(); got != want {
			t.Errorf("State(%q).Valid() = %v, want %v", s, got, want)
		}
	}
}
```

- [ ] **Step 3: Create the module and run the test to see it fail**

`presenceapi/go.mod`:
```
module github.com/jdwillmsen/minecraft-server-agent/presenceapi

go 1.27
```

Run: `cd presenceapi && go test ./...`
Expected: FAIL, build errors such as `undefined: Presence` and `undefined: SetRequest`.

- [ ] **Step 4: Write the types**

`presenceapi/presenceapi.go`:
```go
// Package presenceapi is the wire contract of the agent's presence API: the
// bodies its /v1 routes accept and return.
//
// A module of its own with nothing beyond the standard library, because the
// AFK bot imports it. Importing the agent's root module once raised twenty of
// the bot's module versions and made every agent release a dependency bump
// there; this module moves only when the contract does.
package presenceapi

import "time"

type State string

const (
	StatePresent State = "present"
	StateParked  State = "parked"
)

// Valid reports whether s is one of the two states the API accepts.
func (s State) Valid() bool { return s == StatePresent || s == StateParked }

type WakeOn struct {
	AnyPlayerJoin bool     `json:"any_player_join,omitempty"`
	Players       []string `json:"players,omitempty"`
}

// Override is a runtime deviation from an actor's default.
type Override struct {
	State   State      `json:"state"`
	Until   *time.Time `json:"until,omitempty"`
	WakeOn  *WakeOn    `json:"wake_on,omitempty"`
	Reason  string     `json:"reason"`
	SetBy   string     `json:"set_by"`
	SetAt   time.Time  `json:"set_at"`
	Version int64      `json:"version"`
}

// Presence is GET /v1/actors/{id}/presence.
type Presence struct {
	ActorID   string    `json:"actor_id"`
	Effective State     `json:"effective"`
	Default   State     `json:"default"`
	Override  *Override `json:"override,omitempty"`
}

// SetRequest is the body of PUT /v1/actors/{id}/presence and
// PUT /v1/groups/{group}/presence. Exactly one of Until and Duration may be
// set. Version is required for a single actor (0 = "expect no override"),
// ignored for a group.
type SetRequest struct {
	State    State      `json:"state"`
	Until    *time.Time `json:"until,omitempty"`
	Duration string     `json:"duration,omitempty"` // Go duration, e.g. "2h"
	WakeOn   *WakeOn    `json:"wake_on,omitempty"`
	Reason   string     `json:"reason"`
	Version  int64      `json:"version"`
}

// Status is the body of POST /v1/actors/{id}/status and part of ActorView.
type Status struct {
	Connected      bool      `json:"connected"`
	ObservedState  State     `json:"observed_state"`
	LastSeen       time.Time `json:"last_seen"`
	ProcessVersion string    `json:"process_version"`
}

// ActorView is one element of GET /v1/actors.
type ActorView struct {
	ID       string   `json:"id"`
	Gamertag string   `json:"gamertag"`
	Kind     string   `json:"kind"`
	Groups   []string `json:"groups"`
	Presence
	Status *Status `json:"status,omitempty"`
}

// Error is every non-2xx body.
type Error struct {
	Code    string    `json:"code"`
	Message string    `json:"message"`
	Current *Presence `json:"current,omitempty"` // set on 409
}

// The values Error.Code takes.
const (
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeForbidden    = "forbidden"
	CodeUnauthorized = "unauthorized"
	CodeInvalid      = "invalid"
	CodeUnavailable  = "unavailable"
)
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `cd presenceapi && gofmt -l . && go vet ./... && go test -race ./...`
Expected: `gofmt` and `go vet` print nothing; `ok  	github.com/jdwillmsen/minecraft-server-agent/presenceapi`.

- [ ] **Step 6: Test the nested module in CI**

The root's `go test -race ./...` stops at a nested `go.mod`, so the contract's own tests would otherwise never run in CI. In `.github/workflows/ci.yml`, after the `go test` step (lines 39-40), append:

```yaml
      # A module of its own, so the root's ./... never reaches it.
      - name: presenceapi
        working-directory: presenceapi
        run: |
          go vet ./...
          go test -race ./...
```

- [ ] **Step 7: Commit**

```bash
git add presenceapi .github/workflows/ci.yml
git commit -m "feat(presenceapi): add the presence wire contract as its own module" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 2: Parse `PRESENCE_ACTORS`, `PRESENCE_TOKENS` and `PRESENCE_SELF_ID`

**Files:**
- Create: `internal/config/presence.go`
- Modify: `internal/config/config.go` (add fields after `AnnounceAPIToken`, lines 161-165; call `loadPresence` after the bridge timeout, line 275-278; fill the fields in the `cfg` literal, lines 280-315)
- Test: `internal/config/presence_test.go`

**Interfaces:**
- Consumes: `stringDefault(name, def string) string` (`config.go:340-346`), and the test helpers `clearEnv(t)` and `setRequired(t)` (`config_test.go:8-31`).
- Produces:
  - `type PresenceActor struct { ID, Gamertag, Kind string; Groups []string; DefaultState string }` (JSON `id, gamertag, kind, groups, default_state`)
  - `type PresenceToken struct { Name, Token string; Scopes []string; Actor string }` (JSON `name, token, scopes, actor`)
  - `Config.PresenceActors []PresenceActor`, `Config.PresenceTokens []PresenceToken`, `Config.PresenceSelfID string` (default `"agent"`)

Validation happens here, at startup, so a bad actor list fails `Load` with a message. It never becomes a registry that answers 404 for an actor that should exist.

- [ ] **Step 1: Write the failing test**

`internal/config/presence_test.go`:
```go
package config

import (
	"strings"
	"testing"
)

const presenceActorsJSON = `[
 {"id":"agent","gamertag":"JdwAgent","kind":"agent","groups":[],"default_state":"present"},
 {"id":"afk-bot-1","gamertag":"JdwAfk1","kind":"afk-bot","groups":["bots"],"default_state":"present"},
 {"id":"afk-bot-2","gamertag":"JdwAfk2","kind":"afk-bot","groups":["bots"],"default_state":"parked"}
]`

const presenceTokensJSON = `[
 {"name":"ops","token":"aaaaaaaaaaaaaaaa-ops","scopes":["presence:read","presence:write"]},
 {"name":"afk-bot-1","token":"aaaaaaaaaaaaaaaa-b1","scopes":["presence:read","presence:report"],"actor":"afk-bot-1"}
]`

func loadWithPresence(t *testing.T, actors, tokens, self string) (Config, error) {
	t.Helper()
	clearEnv(t)
	setRequired(t)
	t.Setenv("PRESENCE_ACTORS", actors)
	t.Setenv("PRESENCE_TOKENS", tokens)
	t.Setenv("PRESENCE_SELF_ID", self)
	return Load()
}

// Unset is the supported default: no actors, no tokens, and the agent in the
// world exactly as before presence existed.
func TestPresenceIsOffWhenUnset(t *testing.T) {
	cfg, err := loadWithPresence(t, "", "", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PresenceActors) != 0 || len(cfg.PresenceTokens) != 0 {
		t.Errorf("actors/tokens = %v/%v, want none", cfg.PresenceActors, cfg.PresenceTokens)
	}
	if cfg.PresenceSelfID != "agent" {
		t.Errorf("PresenceSelfID = %q, want the default %q", cfg.PresenceSelfID, "agent")
	}
}

func TestPresenceParsesActorsAndTokens(t *testing.T) {
	cfg, err := loadWithPresence(t, presenceActorsJSON, presenceTokensJSON, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PresenceActors) != 3 {
		t.Fatalf("got %d actors, want 3", len(cfg.PresenceActors))
	}
	bot := cfg.PresenceActors[1]
	if bot.ID != "afk-bot-1" || bot.Gamertag != "JdwAfk1" || bot.Kind != "afk-bot" || bot.DefaultState != "present" || len(bot.Groups) != 1 || bot.Groups[0] != "bots" {
		t.Errorf("actor[1] = %+v", bot)
	}
	if len(cfg.PresenceTokens) != 2 || cfg.PresenceTokens[1].Actor != "afk-bot-1" {
		t.Errorf("tokens = %+v", cfg.PresenceTokens)
	}
}

func TestPresenceRefusesBadConfiguration(t *testing.T) {
	bot := func(fields string) string {
		return `[{"id":"agent","gamertag":"JdwAgent","kind":"agent","default_state":"present"},{` + fields + `}]`
	}
	cases := []struct {
		name, actors, tokens, self, wantInError string
	}{
		{"not JSON", `[{`, "", "", "PRESENCE_ACTORS"},
		{"unknown key", bot(`"id":"b","gamertag":"B","kind":"afk-bot","default-state":"present"`), "", "", "default-state"},
		{"trailing data", `[] []`, "", "", "PRESENCE_ACTORS"},
		{"bad id", bot(`"id":"Bot_1","gamertag":"B","kind":"afk-bot","default_state":"present"`), "", "", "Bot_1"},
		{"id all", bot(`"id":"all","gamertag":"B","kind":"afk-bot","default_state":"present"`), "", "", `"all"`},
		{"duplicate id", bot(`"id":"agent","gamertag":"B","kind":"afk-bot","default_state":"present"`), "", "", "twice"},
		{"blank gamertag", bot(`"id":"b","gamertag":" ","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"quoted gamertag", bot(`"id":"b","gamertag":"B\"x","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"gamertag clash by case", bot(`"id":"b","gamertag":"jdwagent","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"bad kind", bot(`"id":"b","gamertag":"B","kind":"bot","default_state":"present"`), "", "", "kind"},
		{"bad default", bot(`"id":"b","gamertag":"B","kind":"afk-bot","default_state":"away"`), "", "", "default_state"},
		{"group all", bot(`"id":"b","gamertag":"B","kind":"afk-bot","groups":["all"],"default_state":"present"`), "", "", "group"},
		{"group named like an actor", bot(`"id":"b","gamertag":"B","kind":"afk-bot","groups":["agent"],"default_state":"present"`), "", "", "group"},
		{"self missing", presenceActorsJSON, "", "server", "PRESENCE_SELF_ID"},
		{"self not an agent", presenceActorsJSON, "", "afk-bot-1", "PRESENCE_SELF_ID"},
		{"tokens without actors", "", presenceTokensJSON, "", "PRESENCE_ACTORS"},
		{"short token", presenceActorsJSON, `[{"name":"ops","token":"short","scopes":["presence:read"]}]`, "", "16"},
		{"unknown scope", presenceActorsJSON, `[{"name":"ops","token":"aaaaaaaaaaaaaaaa","scopes":["presence:admin"]}]`, "", "presence:admin"},
		{"no scopes", presenceActorsJSON, `[{"name":"ops","token":"aaaaaaaaaaaaaaaa","scopes":[]}]`, "", "scope"},
		{"unknown bound actor", presenceActorsJSON, `[{"name":"b","token":"aaaaaaaaaaaaaaaa","scopes":["presence:read"],"actor":"afk-bot-9"}]`, "", "afk-bot-9"},
		{"report without actor", presenceActorsJSON, `[{"name":"b","token":"aaaaaaaaaaaaaaaa","scopes":["presence:report"]}]`, "", "presence:report"},
		{"duplicate name", presenceActorsJSON, `[{"name":"a","token":"aaaaaaaaaaaaaaaa-1","scopes":["presence:read"]},{"name":"a","token":"aaaaaaaaaaaaaaaa-2","scopes":["presence:read"]}]`, "", "twice"},
		{"shared secret", presenceActorsJSON, `[{"name":"a","token":"aaaaaaaaaaaaaaaa","scopes":["presence:read"]},{"name":"b","token":"aaaaaaaaaaaaaaaa","scopes":["presence:read"]}]`, "", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithPresence(t, tc.actors, tc.tokens, tc.self)
			if err == nil {
				t.Fatal("Load accepted it")
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error %q does not mention %q", err, tc.wantInError)
			}
		})
	}
}

// A token is a secret: whatever is wrong with the configuration, the error
// that reaches the pod log must not carry one.
func TestPresenceErrorsNeverCarryATokenValue(t *testing.T) {
	const secret = "aaaaaaaaaaaaaaaa-leak"
	_, err := loadWithPresence(t, presenceActorsJSON, `[{"name":"b","token":"`+secret+`","scopes":["presence:nope"]}]`, "")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error = %v, want a refusal that does not include the token", err)
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/config/ -run Presence`
Expected: FAIL, build errors `cfg.PresenceActors undefined` and `cfg.PresenceSelfID undefined`.

- [ ] **Step 3: Implement the parsing**

`internal/config/presence.go`:
```go
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// PresenceActor is one entry of PRESENCE_ACTORS: an account that puts a
// player into the world.
type PresenceActor struct {
	ID           string   `json:"id"`
	Gamertag     string   `json:"gamertag"`
	Kind         string   `json:"kind"`
	Groups       []string `json:"groups"`
	DefaultState string   `json:"default_state"`
}

// PresenceToken is one entry of PRESENCE_TOKENS.
type PresenceToken struct {
	Name   string   `json:"name"`
	Token  string   `json:"token"`
	Scopes []string `json:"scopes"`
	Actor  string   `json:"actor"`
}

// presenceID is the shape of an actor id, a group and a token name. All
// three are printed into set_by, audit rows and metric labels.
var presenceID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// minPresenceToken is short enough for any generated secret and long enough
// that a placeholder like "changeme" is refused rather than deployed.
const minPresenceToken = 16

var (
	presenceKinds  = map[string]bool{"agent": true, "afk-bot": true}
	presenceStates = map[string]bool{"present": true, "parked": true}
	presenceScopes = map[string]bool{"presence:read": true, "presence:write": true, "presence:report": true}
)

func loadPresence() ([]PresenceActor, []PresenceToken, string, error) {
	self := stringDefault("PRESENCE_SELF_ID", "agent")
	var actors []PresenceActor
	if err := strictJSON("PRESENCE_ACTORS", &actors); err != nil {
		return nil, nil, "", err
	}
	var tokens []PresenceToken
	if err := strictJSON("PRESENCE_TOKENS", &tokens); err != nil {
		return nil, nil, "", err
	}
	if err := checkPresenceActors(actors, self); err != nil {
		return nil, nil, "", err
	}
	if err := checkPresenceTokens(tokens, actors); err != nil {
		return nil, nil, "", err
	}
	return actors, tokens, self, nil
}

// strictJSON decodes name's value into v, leaving v alone when it is unset.
// Unknown keys are refused: "default-state" would otherwise silently give an
// actor the zero default. The decoder's messages name offsets and keys, never
// values, so a malformed token list cannot print a secret.
func strictJSON(name string, v any) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("environment variable %s is not valid: %w", name, err)
	}
	if dec.More() {
		return fmt.Errorf("environment variable %s has data after its JSON value", name)
	}
	return nil
}

func checkPresenceActors(actors []PresenceActor, self string) error {
	if len(actors) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(actors))
	tags := make(map[string]bool, len(actors))
	for i, a := range actors {
		switch {
		case !presenceID.MatchString(a.ID):
			return fmt.Errorf("PRESENCE_ACTORS[%d]: id %q must match %s", i, a.ID, presenceID)
		case a.ID == "all":
			return fmt.Errorf(`PRESENCE_ACTORS[%d]: id "all" names every actor and cannot be one`, i)
		case ids[a.ID]:
			return fmt.Errorf("PRESENCE_ACTORS: id %q is listed twice", a.ID)
		}
		ids[a.ID] = true
		// A quote cannot be passed through the bridge's kick, and surrounding
		// space would never match the server's roster.
		if a.Gamertag == "" || strings.TrimSpace(a.Gamertag) != a.Gamertag || strings.Contains(a.Gamertag, `"`) {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): gamertag %q must be set, unpadded and without quotes", i, a.ID, a.Gamertag)
		}
		// Case-insensitive because the server's list and chat are.
		if tags[strings.ToLower(a.Gamertag)] {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): gamertag %q belongs to another actor", i, a.ID, a.Gamertag)
		}
		tags[strings.ToLower(a.Gamertag)] = true
		if !presenceKinds[a.Kind] {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): kind %q must be agent or afk-bot", i, a.ID, a.Kind)
		}
		if !presenceStates[a.DefaultState] {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): default_state %q must be present or parked", i, a.ID, a.DefaultState)
		}
	}
	for i, a := range actors {
		for _, g := range a.Groups {
			// A group spelled like an actor would make "!park agent" mean two
			// things; "all" is implicit and listing it would add nothing.
			if !presenceID.MatchString(g) || g == "all" || ids[g] {
				return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): group %q must match %s and be neither all nor an actor id", i, a.ID, g, presenceID)
			}
		}
	}
	for _, a := range actors {
		if a.ID == self {
			if a.Kind != "agent" {
				return fmt.Errorf("PRESENCE_SELF_ID %q is an %s, not the agent", self, a.Kind)
			}
			return nil
		}
	}
	return fmt.Errorf("PRESENCE_SELF_ID %q is not in PRESENCE_ACTORS", self)
}

func checkPresenceTokens(tokens []PresenceToken, actors []PresenceActor) error {
	if len(tokens) == 0 {
		return nil
	}
	if len(actors) == 0 {
		return errors.New("PRESENCE_TOKENS is set but PRESENCE_ACTORS is empty: there is nothing for a token to act on")
	}
	known := make(map[string]bool, len(actors))
	for _, a := range actors {
		known[a.ID] = true
	}
	names := make(map[string]bool, len(tokens))
	secrets := make(map[string]string, len(tokens))
	for i, tk := range tokens {
		switch {
		case !presenceID.MatchString(tk.Name):
			return fmt.Errorf("PRESENCE_TOKENS[%d]: name %q must match %s", i, tk.Name, presenceID)
		case names[tk.Name]:
			return fmt.Errorf("PRESENCE_TOKENS: name %q is listed twice", tk.Name)
		case len(strings.TrimSpace(tk.Token)) < minPresenceToken:
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): token must be at least %d characters", i, tk.Name, minPresenceToken)
		}
		names[tk.Name] = true
		if other, dup := secrets[tk.Token]; dup {
			return fmt.Errorf("PRESENCE_TOKENS: %s and %s share a secret, so neither could be told apart", other, tk.Name)
		}
		secrets[tk.Token] = tk.Name
		if len(tk.Scopes) == 0 {
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): at least one scope is required", i, tk.Name)
		}
		report := false
		for _, s := range tk.Scopes {
			if !presenceScopes[s] {
				return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): unknown scope %q", i, tk.Name, s)
			}
			report = report || s == "presence:report"
		}
		if tk.Actor != "" && !known[tk.Actor] {
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): actor %q is not in PRESENCE_ACTORS", i, tk.Name, tk.Actor)
		}
		// A status report is only ever about the reporter itself, so a token
		// that may report must say which actor it is.
		if report && tk.Actor == "" {
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): presence:report needs an actor", i, tk.Name)
		}
	}
	return nil
}
```

In `internal/config/config.go`, after the `AnnounceAPIToken` field (line 165), add:

```go

	// PresenceActors is every account that puts a player into the world, from
	// PRESENCE_ACTORS. Empty turns presence control off entirely: the agent is
	// always in the world, as before it existed.
	PresenceActors []PresenceActor
	// PresenceTokens authorise the /v1 presence routes. Empty leaves them
	// unmounted, on the same terms as AnnounceAPIToken.
	PresenceTokens []PresenceToken
	// PresenceSelfID is which of PresenceActors this process is.
	PresenceSelfID string
```

In `Load`, after the `bridgeTimeout` block (line 278), add:

```go
	presenceActors, presenceTokens, presenceSelf, err := loadPresence()
	if err != nil {
		return Config{}, err
	}
```

and in the `cfg` literal, after `AnnounceAPIToken:` (line 313), add:

```go
		PresenceActors:            presenceActors,
		PresenceTokens:            presenceTokens,
		PresenceSelfID:            presenceSelf,
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `gofmt -w internal/config && gofmt -l internal/config && go test -race ./internal/config/`
Expected: `gofmt -l` prints nothing, and the test run prints `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/config`.

- [ ] **Step 5: Commit**

```bash
git add internal/config/presence.go internal/config/presence_test.go internal/config/config.go
git commit -m "feat(config): parse and validate the presence actors and tokens" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 3: `Registry`, and the root module's use of `presenceapi`

**Files:**
- Create: `internal/presence/registry.go`
- Test: `internal/presence/registry_test.go`
- Modify: `go.mod` (require and replace), `Dockerfile` (lines 4-5)

**Interfaces:**
- Consumes: `presenceapi.State`, `presenceapi.StatePresent`, `presenceapi.StateParked` (Task 1).
- Produces (package `presence`):
  - `type Actor struct { ID, Gamertag, Kind string; Groups []string; Default presenceapi.State }`
  - `const KindAgent = "agent"`, `const KindAFKBot = "afk-bot"`, `const GroupAll = "all"`
  - `func NewRegistry(actors []Actor, selfID string) (*Registry, error)`
  - `(*Registry).Enabled() bool`, `.Actors() []Actor`, `.Actor(id string) (Actor, bool)`, `.SelfID() string`, `.Group(name string) ([]Actor, bool)`, `.Resolve(target string) ([]Actor, bool)`, `.IsActor(gamertag string) bool`, `.Targets() []string`
  - `func isActorName(actors []Actor, gamertag string) bool` (unexported, reused by `Evaluate` in Task 4)

- [ ] **Step 1: Write the failing test**

`internal/presence/registry_test.go`:
```go
package presence

import (
	"slices"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

func threeActors() []Actor {
	return []Actor{
		{ID: "agent", Gamertag: "JdwAgent", Kind: KindAgent, Default: presenceapi.StatePresent},
		{ID: "afk-bot-1", Gamertag: "JdwAfk1", Kind: KindAFKBot, Groups: []string{"bots"}, Default: presenceapi.StatePresent},
		{ID: "afk-bot-2", Gamertag: "JdwAfk2", Kind: KindAFKBot, Groups: []string{"bots"}, Default: presenceapi.StateParked},
	}
}

func ids(actors []Actor) []string {
	out := make([]string, 0, len(actors))
	for _, a := range actors {
		out = append(out, a.ID)
	}
	return out
}

func TestRegistryResolvesActorsGroupsAndAll(t *testing.T) {
	r, err := NewRegistry(threeActors(), "agent")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cases := map[string][]string{
		"afk-bot-1":   {"afk-bot-1"},
		" AFK-Bot-1 ": {"afk-bot-1"},
		"bots":        {"afk-bot-1", "afk-bot-2"},
		"all":         {"agent", "afk-bot-1", "afk-bot-2"},
		"ALL":         {"agent", "afk-bot-1", "afk-bot-2"},
	}
	for target, want := range cases {
		got, ok := r.Resolve(target)
		if !ok || !slices.Equal(ids(got), want) {
			t.Errorf("Resolve(%q) = %v, %v; want %v", target, ids(got), ok, want)
		}
	}
	if got, ok := r.Resolve("nobody"); ok {
		t.Errorf("Resolve(nobody) = %v, want not found", ids(got))
	}
}

// The API's group route takes the path segment as written, so it is
// case-sensitive where chat is not.
func TestRegistryGroupIsExact(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	if _, ok := r.Group("BOTS"); ok {
		t.Error("Group(BOTS) resolved; the route is case-sensitive")
	}
	if got, ok := r.Group("all"); !ok || len(got) != 3 {
		t.Errorf("Group(all) = %v, %v", ids(got), ok)
	}
}

func TestRegistryRecognisesActorsByGamertagInAnyCase(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	if !r.IsActor("jdwafk1") || r.IsActor("Steve") {
		t.Error("IsActor must match actors case-insensitively and nobody else")
	}
}

func TestRegistryGroupsAreNeverNil(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	a, _ := r.Actor("agent")
	if a.Groups == nil {
		t.Error("an actor with no groups has nil Groups; the API would encode null instead of []")
	}
}

func TestRegistryTargetsListsWhatChatAccepts(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	want := []string{"agent", "afk-bot-1", "afk-bot-2", "bots", "all"}
	if got := r.Targets(); !slices.Equal(got, want) {
		t.Errorf("Targets() = %v, want %v", got, want)
	}
}

func TestRegistryRefusesASelfThatIsNotTheAgent(t *testing.T) {
	if _, err := NewRegistry(threeActors(), "afk-bot-1"); err == nil {
		t.Error("a bot accepted as self")
	}
	if _, err := NewRegistry(threeActors(), "server"); err == nil {
		t.Error("an unknown self accepted")
	}
}

func TestEmptyRegistryIsDisabled(t *testing.T) {
	r, err := NewRegistry(nil, "agent")
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	if r.Enabled() {
		t.Error("an empty registry reports enabled")
	}
	if _, ok := r.Group(GroupAll); ok {
		t.Error("all resolved on an empty registry")
	}
}
```

- [ ] **Step 2: Wire the module and run the test to see it fail**

Run:
```bash
go mod edit -require=github.com/jdwillmsen/minecraft-server-agent/presenceapi@v0.1.0 \
  -replace=github.com/jdwillmsen/minecraft-server-agent/presenceapi=./presenceapi
go test ./internal/presence/
```
Expected: FAIL, build errors `undefined: Actor`, `undefined: NewRegistry`, `undefined: KindAgent`.

- [ ] **Step 3: Implement the registry**

`internal/presence/registry.go`:
```go
// Package presence decides which actors -- the accounts that put a player
// into the world -- should be in it, and keeps them there or out.
//
// Git is the baseline: every actor has a default from its Helm values. What
// this package stores is only ever a deviation from that default, with an
// owner, a reason and usually an expiry, and clearing one always falls back
// to what git says.
package presence

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	KindAgent  = "agent"
	KindAFKBot = "afk-bot"
	// GroupAll names every actor. Implicit: it is never listed in an actor's
	// groups, and configuration refuses it there.
	GroupAll = "all"
)

// Actor is an account that puts a player into the world.
type Actor struct {
	ID       string
	Gamertag string
	Kind     string
	Groups   []string
	Default  presenceapi.State
}

// Registry is the configured actor list. Fixed for the life of the process.
type Registry struct {
	actors []Actor
	byID   map[string]int
	groups map[string][]int
	selfID string
}

// NewRegistry builds the registry. Configuration has already validated the
// list; the checks here are the ones this package relies on, not a second
// copy of every rule.
func NewRegistry(actors []Actor, selfID string) (*Registry, error) {
	r := &Registry{byID: make(map[string]int), groups: make(map[string][]int), selfID: selfID}
	for i, a := range actors {
		if _, dup := r.byID[a.ID]; dup {
			return nil, fmt.Errorf("presence: actor %q is registered twice", a.ID)
		}
		if a.Groups == nil {
			a.Groups = []string{}
		}
		r.actors = append(r.actors, a)
		r.byID[a.ID] = i
		for _, g := range a.Groups {
			r.groups[g] = append(r.groups[g], i)
		}
	}
	if len(actors) > 0 {
		if self, ok := r.Actor(selfID); !ok || self.Kind != KindAgent {
			return nil, fmt.Errorf("presence: self %q is not a registered agent", selfID)
		}
	}
	return r, nil
}

// Enabled reports whether any actor is configured. With none, presence
// control is off and the agent is always in the world.
func (r *Registry) Enabled() bool { return len(r.actors) > 0 }

// Actors returns every actor in configuration order.
func (r *Registry) Actors() []Actor { return slices.Clone(r.actors) }

func (r *Registry) Actor(id string) (Actor, bool) {
	i, ok := r.byID[id]
	if !ok {
		return Actor{}, false
	}
	return r.actors[i], true
}

// SelfID is the actor this process is.
func (r *Registry) SelfID() string { return r.selfID }

// Group returns the members of a configured group, or every actor for all.
// Exact: it serves a URL path segment, not something typed in chat.
func (r *Registry) Group(name string) ([]Actor, bool) {
	if name == GroupAll {
		return r.Actors(), r.Enabled()
	}
	idx, ok := r.groups[name]
	if !ok {
		return nil, false
	}
	out := make([]Actor, 0, len(idx))
	for _, i := range idx {
		out = append(out, r.actors[i])
	}
	return out, true
}

// Resolve turns a chat target -- an actor id, a group or all -- into actors.
// Case-insensitive because it is typed in chat; configuration keeps ids and
// groups lower-case, so folding cannot make two targets collide.
func (r *Registry) Resolve(target string) ([]Actor, bool) {
	t := strings.ToLower(strings.TrimSpace(target))
	if a, ok := r.Actor(t); ok {
		return []Actor{a}, true
	}
	return r.Group(t)
}

// IsActor reports whether gamertag belongs to an actor. An actor arriving is
// never a player arriving, whatever case the server reports the name in.
func (r *Registry) IsActor(gamertag string) bool { return isActorName(r.actors, gamertag) }

func isActorName(actors []Actor, gamertag string) bool {
	for _, a := range actors {
		if strings.EqualFold(a.Gamertag, gamertag) {
			return true
		}
	}
	return false
}

// Targets lists what Resolve accepts, for a usage reply: actors in
// configuration order, then groups by name, then all.
func (r *Registry) Targets() []string {
	out := make([]string, 0, len(r.actors)+len(r.groups)+1)
	for _, a := range r.actors {
		out = append(out, a.ID)
	}
	groups := make([]string, 0, len(r.groups))
	for g := range r.groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return append(append(out, groups...), GroupAll)
}
```

- [ ] **Step 4: Copy the nested module into the image build**

`go mod download` resolves the `replace` target, so its `go.mod` has to be in the build context before the download step. In `Dockerfile`, replace lines 4-5:

```dockerfile
COPY go.mod go.sum ./
RUN go mod download
```

with:

```dockerfile
# The contract module is a local replace target, so go mod download needs
# its go.mod before the rest of the source arrives.
COPY go.mod go.sum ./
COPY presenceapi/go.mod presenceapi/
RUN go mod download
```

- [ ] **Step 5: Run the tests, tidy, and build the image**

Run:
```bash
gofmt -l internal/presence && go mod tidy && git diff --stat go.mod go.sum && go test -race ./internal/presence/
docker build -t presence-check .
```
Expected: `gofmt -l` prints nothing. `go.mod` shows the new require and replace lines, and `go.sum` is unchanged (a directory replace carries no checksum). The tests print `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/presence`. The image builds.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum Dockerfile internal/presence/registry.go internal/presence/registry_test.go
git commit -m "feat(presence): add the actor registry and use the contract module" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 4: Pure `Policy`

**Files:**
- Create: `internal/presence/policy.go`
- Test: `internal/presence/policy_test.go`

**Interfaces:**
- Consumes: `Actor`, `KindAgent`, `isActorName` (Task 3). `presenceapi.Override`, `WakeOn`, `Presence`, `State` (Task 1).
- Produces:
  - `type Join struct { Gamertag string; At time.Time }`
  - `type Cause string` with `CauseSet`, `CauseCleared`, `CauseExpired`, `CauseWoken`
  - `type Removal struct { ActorID string; Cause Cause; Override presenceapi.Override }`
  - `type Decision struct { Effective map[string]presenceapi.State; Remove []Removal }`
  - `func Evaluate(actors []Actor, overrides map[string]presenceapi.Override, joins []Join, now time.Time) Decision`
  - `const AgentParkWindow = time.Hour`
  - `func ChatPark(a Actor, d time.Duration, now time.Time) (until *time.Time, wake *presenceapi.WakeOn)`
  - `func View(a Actor, ov *presenceapi.Override) presenceapi.Presence`

`Evaluate` has no I/O and no clock of its own. Every rule is a table row. Future automatic policies become further inputs here that produce overrides with `set_by: policy:<name>`.

- [ ] **Step 1: Write the failing test**

`internal/presence/policy_test.go`:
```go
package presence

import (
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

var t0 = time.Date(2026, 9, 23, 18, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { t := t0.Add(d); return &t }

func parkedAt(set time.Time, until *time.Time, wake *presenceapi.WakeOn) presenceapi.Override {
	return presenceapi.Override{State: presenceapi.StateParked, Until: until, WakeOn: wake, Reason: "r", SetBy: "chat:Op", SetAt: set, Version: 4}
}

func TestEvaluate(t *testing.T) {
	anyJoin := &presenceapi.WakeOn{AnyPlayerJoin: true}
	named := &presenceapi.WakeOn{Players: []string{"Steve"}}
	now := t0.Add(10 * time.Minute)
	cases := []struct {
		name          string
		actor         string
		override      *presenceapi.Override
		joins         []Join
		wantEffective presenceapi.State
		wantCause     Cause // "" means nothing removed
	}{
		{name: "no override, present default", actor: "afk-bot-1", wantEffective: presenceapi.StatePresent},
		{name: "no override, parked default", actor: "afk-bot-2", wantEffective: presenceapi.StateParked},
		{name: "override without expiry holds", actor: "afk-bot-1", override: ptr(parkedAt(t0, nil, nil)), wantEffective: presenceapi.StateParked},
		{name: "override before its expiry holds", actor: "afk-bot-1", override: ptr(parkedAt(t0, at(time.Hour), nil)), wantEffective: presenceapi.StateParked},
		{name: "expiry reached exactly", actor: "afk-bot-1", override: ptr(parkedAt(t0, at(10*time.Minute), nil)), wantEffective: presenceapi.StatePresent, wantCause: CauseExpired},
		{name: "expired falls back to a parked default", actor: "afk-bot-2", override: &presenceapi.Override{State: presenceapi.StatePresent, Until: at(time.Minute), SetAt: t0, Version: 1}, wantEffective: presenceapi.StateParked, wantCause: CauseExpired},
		{name: "present override over parked default", actor: "afk-bot-2", override: &presenceapi.Override{State: presenceapi.StatePresent, SetAt: t0, Version: 1}, wantEffective: presenceapi.StatePresent},
		{name: "any join wakes", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"Steve", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent, wantCause: CauseWoken},
		{name: "an actor joining is not a player joining", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"jdwafk1", t0.Add(time.Minute)}}, wantEffective: presenceapi.StateParked},
		{name: "join before the park is ignored", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"Steve", t0.Add(-time.Second)}}, wantEffective: presenceapi.StateParked},
		{name: "join at the park instant counts", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"Steve", t0}}, wantEffective: presenceapi.StatePresent, wantCause: CauseWoken},
		{name: "named player wakes in any case", actor: "afk-bot-1", override: ptr(parkedAt(t0, nil, named)), joins: []Join{{"STEVE", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent, wantCause: CauseWoken},
		{name: "another player does not wake a named list", actor: "afk-bot-1", override: ptr(parkedAt(t0, nil, named)), joins: []Join{{"Alex", t0.Add(time.Minute)}}, wantEffective: presenceapi.StateParked},
		{name: "wake_on never removes a present override", actor: "afk-bot-2", override: &presenceapi.Override{State: presenceapi.StatePresent, WakeOn: anyJoin, SetAt: t0, Version: 1}, joins: []Join{{"Steve", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent},
		{name: "expiry wins over a wake in the same tick", actor: "agent", override: ptr(parkedAt(t0, at(5*time.Minute), anyJoin)), joins: []Join{{"Steve", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent, wantCause: CauseExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrides := map[string]presenceapi.Override{}
			if tc.override != nil {
				overrides[tc.actor] = *tc.override
			}
			d := Evaluate(threeActors(), overrides, tc.joins, now)
			if got := d.Effective[tc.actor]; got != tc.wantEffective {
				t.Errorf("effective = %q, want %q", got, tc.wantEffective)
			}
			switch {
			case tc.wantCause == "" && len(d.Remove) != 0:
				t.Errorf("removed %+v, want nothing removed", d.Remove)
			case tc.wantCause != "" && (len(d.Remove) != 1 || d.Remove[0].Cause != tc.wantCause || d.Remove[0].ActorID != tc.actor || d.Remove[0].Override.Version != tc.override.Version):
				t.Errorf("removed %+v, want %s of %s at version %d", d.Remove, tc.wantCause, tc.actor, tc.override.Version)
			}
			if len(d.Effective) != 3 {
				t.Errorf("effective covers %d actors, want every actor", len(d.Effective))
			}
		})
	}
}

// Removing an actor from configuration leaves its row behind; the policy
// has nothing to say about an actor it does not know.
func TestEvaluateIgnoresAnOverrideForAnUnknownActor(t *testing.T) {
	d := Evaluate(threeActors(), map[string]presenceapi.Override{"afk-bot-9": parkedAt(t0, at(-time.Hour), nil)}, nil, t0)
	if len(d.Remove) != 0 || len(d.Effective) != 3 {
		t.Errorf("decision = %+v, want the unknown row left alone", d)
	}
}

func TestChatPark(t *testing.T) {
	agent, bot := threeActors()[0], threeActors()[1]
	cases := []struct {
		name      string
		actor     Actor
		d         time.Duration
		wantUntil *time.Time
		wantWake  bool
	}{
		{"agent without a duration gets an hour and a wake", agent, 0, at(time.Hour), true},
		{"agent with a duration keeps the wake", agent, 30 * time.Minute, at(30 * time.Minute), true},
		{"bot without a duration lasts until unparked", bot, 0, nil, false},
		{"bot with a duration expires and nothing else", bot, 2 * time.Hour, at(2 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			until, wake := ChatPark(tc.actor, tc.d, t0)
			switch {
			case (until == nil) != (tc.wantUntil == nil):
				t.Errorf("until = %v, want %v", until, tc.wantUntil)
			case until != nil && !until.Equal(*tc.wantUntil):
				t.Errorf("until = %v, want %v", *until, *tc.wantUntil)
			}
			if got := wake != nil && wake.AnyPlayerJoin; got != tc.wantWake {
				t.Errorf("wake = %+v, want any_player_join %v", wake, tc.wantWake)
			}
		})
	}
}

func TestViewTakesTheOverrideStateWhenOneExists(t *testing.T) {
	bot := threeActors()[1]
	if v := View(bot, nil); v.Effective != presenceapi.StatePresent || v.Override != nil || v.ActorID != "afk-bot-1" {
		t.Errorf("View without override = %+v", v)
	}
	ov := parkedAt(t0, nil, nil)
	v := View(bot, &ov)
	if v.Effective != presenceapi.StateParked || v.Default != presenceapi.StatePresent || v.Override == nil {
		t.Errorf("View with override = %+v", v)
	}
	ov.Reason = "changed after"
	if v.Override.Reason == "changed after" {
		t.Error("View shares the caller's override; later edits would leak into a reply")
	}
}

func ptr[T any](v T) *T { return &v }
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/presence/ -run 'Evaluate|ChatPark|View'`
Expected: FAIL, build errors `undefined: Join`, `undefined: Evaluate`, `undefined: ChatPark`, `undefined: View`.

- [ ] **Step 3: Implement the policy**

`internal/presence/policy.go`:
```go
package presence

import (
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Join is a player arriving on the server.
type Join struct {
	Gamertag string
	At       time.Time
}

// Cause is why an override was written or removed, as the audit trail and
// the log record it.
type Cause string

const (
	CauseSet     Cause = "set"
	CauseCleared Cause = "cleared"
	CauseExpired Cause = "expired"
	CauseWoken   Cause = "woken"
)

// Removal is an override the policy says has ended. Override is the row as
// it was read: its Version is what makes the removal safe against a newer
// write landing in between.
type Removal struct {
	ActorID  string
	Cause    Cause
	Override presenceapi.Override
}

// Decision is what the policy concludes for one moment.
type Decision struct {
	Effective map[string]presenceapi.State
	Remove    []Removal
}

// Evaluate decides every actor's effective state from the stored overrides,
// the players who recently arrived, and the time.
//
// An override that has ended is reported for removal and its actor is given
// its default at once, so a tick that removes an override also acts on the
// result instead of waiting for the next one. Expiry is checked before a
// wake, so one override ending both ways is recorded as the timer that was
// set, not the arrival that coincided with it.
func Evaluate(actors []Actor, overrides map[string]presenceapi.Override, joins []Join, now time.Time) Decision {
	d := Decision{Effective: make(map[string]presenceapi.State, len(actors))}
	for _, a := range actors {
		ov, ok := overrides[a.ID]
		switch {
		case !ok:
			d.Effective[a.ID] = a.Default
		case ov.Until != nil && !now.Before(*ov.Until):
			d.Remove = append(d.Remove, Removal{ActorID: a.ID, Cause: CauseExpired, Override: ov})
			d.Effective[a.ID] = a.Default
		case ov.State == presenceapi.StateParked && woken(ov, actors, joins):
			d.Remove = append(d.Remove, Removal{ActorID: a.ID, Cause: CauseWoken, Override: ov})
			d.Effective[a.ID] = a.Default
		default:
			d.Effective[a.ID] = ov.State
		}
	}
	return d
}

// woken reports whether a join the override waits for has happened since it
// was set. An arrival from before set_at is not one: it is the player the
// operator parked the actor around, and counting it would undo the park on
// the very next tick.
func woken(ov presenceapi.Override, actors []Actor, joins []Join) bool {
	if ov.WakeOn == nil {
		return false
	}
	for _, j := range joins {
		if j.At.Before(ov.SetAt) || isActorName(actors, j.Gamertag) {
			continue
		}
		if ov.WakeOn.AnyPlayerJoin {
			return true
		}
		for _, p := range ov.WakeOn.Players {
			if strings.EqualFold(p, j.Gamertag) {
				return true
			}
		}
	}
	return false
}

// AgentParkWindow is how long a chat park of the agent lasts when no
// duration is given.
const AgentParkWindow = time.Hour

// ChatPark is the expiry and wake a park from chat gives actor a.
//
// A parked agent reads no chat, so a park typed in chat could never be typed
// away: the agent always gets a timer and a wake on the next player to
// arrive, and an explicit duration only replaces the timer. A bot keeps
// exactly what was asked for.
func ChatPark(a Actor, d time.Duration, now time.Time) (until *time.Time, wake *presenceapi.WakeOn) {
	if a.Kind == KindAgent {
		if d <= 0 {
			d = AgentParkWindow
		}
		t := now.Add(d)
		return &t, &presenceapi.WakeOn{AnyPlayerJoin: true}
	}
	if d <= 0 {
		return nil, nil
	}
	t := now.Add(d)
	return &t, nil
}

// View is the presence actor a has with ov as its override row, if it has
// one. The effective state is the row's state for as long as the row
// exists: an expiry that has passed waits for the leader's loop to remove
// it, and until then every reader sees the same answer.
func View(a Actor, ov *presenceapi.Override) presenceapi.Presence {
	p := presenceapi.Presence{ActorID: a.ID, Default: a.Default, Effective: a.Default}
	if ov != nil {
		c := *ov
		p.Override = &c
		p.Effective = ov.State
	}
	return p
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `gofmt -w internal/presence && gofmt -l internal/presence && go test -race ./internal/presence/ -run 'Evaluate|ChatPark|View' -v`
Expected: `gofmt -l` prints nothing. All 15 `TestEvaluate` subtests pass, as do the 4 `TestChatPark` subtests, `TestEvaluateIgnoresAnOverrideForAnUnknownActor` and `TestViewTakesTheOverrideStateWhenOneExists`.

- [ ] **Step 5: Commit**

```bash
git add internal/presence/policy.go internal/presence/policy_test.go
git commit -m "feat(presence): decide effective states, expiries and wakes as a pure policy" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 5: `Store` on Postgres, with versioned writes

**Files:**
- Create: `internal/presence/store.go`, `internal/presence/postgres.go`
- Test: `internal/presence/store_test.go`, `internal/presence/postgres_live_test.go` (build tag `livedb`)

**Interfaces:**
- Consumes: `presenceapi.Override`, `presenceapi.Status`, `presenceapi.WakeOn` (Task 1). The pool pattern of `internal/audit/postgres.go:11-20`. The live-test pattern of `internal/audit/postgres_live_test.go:24-42` (`MC_TEST_DSN`, skip when unset).
- Produces:
  - `var ErrDisabled`, `var ErrConflict`
  - `type Change struct { ActorID string; Prev, Now *presenceapi.Override }`
  - `type Store interface` with `Overrides`, `Set`, `SetMany`, `Clear`, `Remove`, `Statuses`, `PutStatus`, `Enabled` (signatures below)
  - `type Nop struct{}`, `func NewPostgres(pool *pgxpool.Pool) *Postgres`

The version rules are the contract's. A new row starts at 1, and every update increments it. `DELETE` removes the row, so the next override starts at 1 again. `Set` with `expect == 0` means "I expect no row".

- [ ] **Step 1: Write the failing tests**

`internal/presence/store_test.go`:
```go
package presence

import (
	"errors"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// With no database there is nothing to read an override from, so every call
// says so rather than answering "no overrides" -- which a caller would take
// for the truth and report every actor at its default.
func TestNopRefusesEveryCall(t *testing.T) {
	ctx := t.Context()
	var s Store = Nop{}
	if s.Enabled() {
		t.Error("Nop reports enabled")
	}
	checks := map[string]error{}
	_, checks["Overrides"] = s.Overrides(ctx)
	_, checks["Set"] = s.Set(ctx, "a", presenceapi.Override{}, 0)
	_, checks["SetMany"] = s.SetMany(ctx, map[string]presenceapi.Override{"a": {}})
	_, checks["Clear"] = s.Clear(ctx, []string{"a"})
	_, checks["Remove"] = s.Remove(ctx, "a", 1)
	_, checks["Statuses"] = s.Statuses(ctx)
	checks["PutStatus"] = s.PutStatus(ctx, "a", presenceapi.Status{})
	for name, err := range checks {
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("%s = %v, want ErrDisabled", name, err)
		}
	}
}
```

`internal/presence/postgres_live_test.go`:
```go
//go:build livedb

// Exercises the real SQL against the tables plan 2's V8 migration creates,
// as the runtime role. The version rules are enforced by these statements
// and nothing else, so only a real database can say they hold.
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd?sslmode=disable \
//	  go test -tags livedb ./internal/presence/
package presence

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Ids no real deployment uses, cleaned up before and after each test.
var liveIDs = []string{"zz-test-a", "zz-test-b", "zz-test-agent"}

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("MC_TEST_DSN")
	if dsn == "" {
		t.Skip("MC_TEST_DSN unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM minecraft.presence_overrides WHERE actor_id = ANY($1)`, liveIDs)
		_, _ = pool.Exec(ctx, `DELETE FROM minecraft.presence_status WHERE actor_id = ANY($1)`, liveIDs)
	}
	clean()
	t.Cleanup(func() { clean(); pool.Close() })
	return pool
}

func liveOverride(state presenceapi.State) presenceapi.Override {
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	return presenceapi.Override{
		State: state, Until: &until, WakeOn: &presenceapi.WakeOn{Players: []string{"Steve"}},
		Reason: "live test", SetBy: "api:test", SetAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestSetFollowsTheVersionRules(t *testing.T) {
	ctx := t.Context()
	s := NewPostgres(livePool(t))

	first, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StateParked), 0)
	if err != nil {
		t.Fatalf("first set: %v", err)
	}
	if first.Prev != nil || first.Now == nil || first.Now.Version != 1 {
		t.Fatalf("first set = %+v, want a new row at version 1", first)
	}

	stale, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StatePresent), 0)
	if !errors.Is(err, ErrConflict) || stale.Prev == nil || stale.Prev.Version != 1 {
		t.Fatalf("set expecting no row = %+v, %v; want ErrConflict carrying version 1", stale, err)
	}

	second, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StatePresent), 1)
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	if second.Prev == nil || second.Prev.State != presenceapi.StateParked || second.Now.Version != 2 {
		t.Fatalf("second set = %+v, want parked version 1 replaced by version 2", second)
	}

	got, err := s.Overrides(ctx)
	if err != nil {
		t.Fatalf("Overrides: %v", err)
	}
	row := got["zz-test-a"]
	if row.State != presenceapi.StatePresent || row.Until == nil || row.WakeOn == nil || len(row.WakeOn.Players) != 1 || row.SetBy != "api:test" {
		t.Errorf("stored row = %+v, want every field back", row)
	}

	if ok, err := s.Remove(ctx, "zz-test-a", 1); err != nil || ok {
		t.Errorf("Remove at a stale version = %v, %v; want nothing removed", ok, err)
	}
	if ok, err := s.Remove(ctx, "zz-test-a", 2); err != nil || !ok {
		t.Errorf("Remove at the current version = %v, %v; want removed", ok, err)
	}
	again, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StateParked), 0)
	if err != nil || again.Now.Version != 1 {
		t.Errorf("set after removal = %+v, %v; want a fresh row at version 1", again, err)
	}
}

func TestSetManyAndClearAreOneTransactionEach(t *testing.T) {
	ctx := t.Context()
	s := NewPostgres(livePool(t))
	both := map[string]presenceapi.Override{"zz-test-a": liveOverride(presenceapi.StateParked), "zz-test-b": liveOverride(presenceapi.StateParked)}

	if _, err := s.SetMany(ctx, both); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	changes, err := s.SetMany(ctx, both)
	if err != nil {
		t.Fatalf("SetMany again: %v", err)
	}
	for _, c := range changes {
		if c.Prev == nil || c.Now.Version != 2 {
			t.Errorf("second SetMany change = %+v, want version 2 over a prior row", c)
		}
	}
	cleared, err := s.Clear(ctx, []string{"zz-test-a", "zz-test-b", "zz-test-agent"})
	if err != nil || len(cleared) != 2 {
		t.Fatalf("Clear = %d changes, %v; want the two rows that existed", len(cleared), err)
	}
	if cleared[0].Now != nil || cleared[0].Prev == nil {
		t.Errorf("clear change = %+v, want the removed row and nothing after", cleared[0])
	}
}

func TestPutStatusUpserts(t *testing.T) {
	ctx := t.Context()
	s := NewPostgres(livePool(t))
	seen := time.Now().UTC().Truncate(time.Microsecond)
	for _, connected := range []bool{true, false} {
		if err := s.PutStatus(ctx, "zz-test-b", presenceapi.Status{Connected: connected, ObservedState: presenceapi.StatePresent, LastSeen: seen, ProcessVersion: "1.4.0"}); err != nil {
			t.Fatalf("PutStatus(%v): %v", connected, err)
		}
	}
	got, err := s.Statuses(ctx)
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	st := got["zz-test-b"]
	if st.Connected || !st.LastSeen.Equal(seen) || st.ProcessVersion != "1.4.0" {
		t.Errorf("status = %+v, want the second report", st)
	}
}
```

- [ ] **Step 2: Run the unit test to see it fail**

Run: `go test ./internal/presence/ -run Nop && go vet -tags livedb ./internal/presence/`
Expected: FAIL, build errors `undefined: Store`, `undefined: Nop`, `undefined: ErrDisabled`.

- [ ] **Step 3: Implement the interface and Nop**

`internal/presence/store.go`:
```go
package presence

import (
	"context"
	"errors"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// ErrDisabled is every Store call with no database configured.
var ErrDisabled = errors.New("presence: no database configured")

// ErrConflict is a versioned write whose expected version was not the
// current one. The Change returned with it carries the current row in Prev.
var ErrConflict = errors.New("presence: override changed since it was read")

// Change is one override write as it landed: the row before it, and the row
// after it (nil once removed).
type Change struct {
	ActorID string
	Prev    *presenceapi.Override
	Now     *presenceapi.Override
}

// Store holds overrides and observed status. In every write, the store
// assigns Version; the caller's value is ignored.
type Store interface {
	// Overrides returns every stored override by actor id.
	Overrides(ctx context.Context) (map[string]presenceapi.Override, error)
	// Set writes one actor's override if its current version is expect,
	// where 0 means no row. On a mismatch it returns ErrConflict with the
	// current row in Change.Prev.
	Set(ctx context.Context, actorID string, ov presenceapi.Override, expect int64) (Change, error)
	// SetMany writes every entry in one transaction, whatever their
	// versions: a group is one operator decision about its members.
	SetMany(ctx context.Context, ovs map[string]presenceapi.Override) ([]Change, error)
	// Clear removes the rows of ids in one statement; an id with no row is
	// not in the result.
	Clear(ctx context.Context, ids []string) ([]Change, error)
	// Remove deletes actorID's row only while it is still at version, and
	// reports whether it did.
	Remove(ctx context.Context, actorID string, version int64) (bool, error)
	Statuses(ctx context.Context) (map[string]presenceapi.Status, error)
	PutStatus(ctx context.Context, actorID string, s presenceapi.Status) error
	Enabled() bool
}

// Nop is the Store with no database configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Overrides(context.Context) (map[string]presenceapi.Override, error) {
	return nil, ErrDisabled
}
func (Nop) Set(context.Context, string, presenceapi.Override, int64) (Change, error) {
	return Change{}, ErrDisabled
}
func (Nop) SetMany(context.Context, map[string]presenceapi.Override) ([]Change, error) {
	return nil, ErrDisabled
}
func (Nop) Clear(context.Context, []string) ([]Change, error)          { return nil, ErrDisabled }
func (Nop) Remove(context.Context, string, int64) (bool, error)         { return false, ErrDisabled }
func (Nop) Statuses(context.Context) (map[string]presenceapi.Status, error) { return nil, ErrDisabled }
func (Nop) PutStatus(context.Context, string, presenceapi.Status) error { return ErrDisabled }
func (Nop) Enabled() bool                                               { return false }
```

- [ ] **Step 4: Implement Postgres**

`internal/presence/postgres.go`:
```go
package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Postgres persists to minecraft.presence_overrides and
// minecraft.presence_status, sharing the profile store's pool. Every
// replica reads and writes here directly, which is what lets a standby
// answer the API as well as the leader.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

const overrideColumns = `actor_id, state, until, wake_on, reason, set_by, set_at, version`

type scanner interface {
	Scan(dest ...any) error
}

func scanOverride(row scanner) (string, presenceapi.Override, error) {
	var (
		id, state string
		until     *time.Time
		wake      []byte
		ov        presenceapi.Override
	)
	if err := row.Scan(&id, &state, &until, &wake, &ov.Reason, &ov.SetBy, &ov.SetAt, &ov.Version); err != nil {
		return "", ov, err
	}
	ov.State = presenceapi.State(state)
	ov.SetAt = ov.SetAt.UTC()
	if until != nil {
		u := until.UTC()
		ov.Until = &u
	}
	if wake != nil {
		var w presenceapi.WakeOn
		if err := json.Unmarshal(wake, &w); err != nil {
			return "", ov, fmt.Errorf("presence: wake_on of %s: %w", id, err)
		}
		ov.WakeOn = &w
	}
	return id, ov, nil
}

// wakeParam is the wake_on argument: NULL for none, the JSON text otherwise,
// cast to jsonb by the statement.
func wakeParam(w *presenceapi.WakeOn) (any, error) {
	if w == nil {
		return nil, nil
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("presence: encode wake_on: %w", err)
	}
	return string(b), nil
}

func (p *Postgres) Overrides(ctx context.Context) (map[string]presenceapi.Override, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+overrideColumns+` FROM minecraft.presence_overrides`)
	if err != nil {
		return nil, fmt.Errorf("presence: read overrides: %w", err)
	}
	defer rows.Close()
	out := make(map[string]presenceapi.Override)
	for rows.Next() {
		id, ov, err := scanOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("presence: read overrides: %w", err)
		}
		out[id] = ov
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: read overrides: %w", err)
	}
	return out, nil
}

// lockRow reads actorID's row and holds it for the transaction, or reports
// nil when there is none -- in which case nothing is locked, and an insert
// racing this one is caught by the primary key instead.
func lockRow(ctx context.Context, tx pgx.Tx, actorID string) (*presenceapi.Override, error) {
	_, ov, err := scanOverride(tx.QueryRow(ctx, `SELECT `+overrideColumns+` FROM minecraft.presence_overrides WHERE actor_id = $1 FOR UPDATE`, actorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("presence: lock %s: %w", actorID, err)
	}
	return &ov, nil
}

func (p *Postgres) Set(ctx context.Context, actorID string, ov presenceapi.Override, expect int64) (Change, error) {
	wake, err := wakeParam(ov.WakeOn)
	if err != nil {
		return Change{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Change{}, fmt.Errorf("presence: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prev, err := lockRow(ctx, tx, actorID)
	if err != nil {
		return Change{}, err
	}
	var have int64
	if prev != nil {
		have = prev.Version
	}
	if have != expect {
		return Change{ActorID: actorID, Prev: prev}, ErrConflict
	}

	var row pgx.Row
	if prev == nil {
		// DO NOTHING rather than an upsert: two writers that both read "no
		// row" must not both succeed, and the second one finds out here.
		row = tx.QueryRow(ctx, `
			INSERT INTO minecraft.presence_overrides (actor_id, state, until, wake_on, reason, set_by, set_at, version)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, 1)
			ON CONFLICT (actor_id) DO NOTHING
			RETURNING `+overrideColumns,
			actorID, string(ov.State), ov.Until, wake, ov.Reason, ov.SetBy, ov.SetAt)
	} else {
		row = tx.QueryRow(ctx, `
			UPDATE minecraft.presence_overrides
			SET state = $2, until = $3, wake_on = $4::jsonb, reason = $5, set_by = $6, set_at = $7, version = version + 1
			WHERE actor_id = $1
			RETURNING `+overrideColumns,
			actorID, string(ov.State), ov.Until, wake, ov.Reason, ov.SetBy, ov.SetAt)
	}
	_, now, err := scanOverride(row)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		current, cerr := p.Overrides(ctx)
		if cerr != nil {
			return Change{}, cerr
		}
		var cur *presenceapi.Override
		if c, ok := current[actorID]; ok {
			cur = &c
		}
		return Change{ActorID: actorID, Prev: cur}, ErrConflict
	}
	if err != nil {
		return Change{}, fmt.Errorf("presence: write %s: %w", actorID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Change{}, fmt.Errorf("presence: commit %s: %w", actorID, err)
	}
	return Change{ActorID: actorID, Prev: prev, Now: &now}, nil
}

func (p *Postgres) SetMany(ctx context.Context, ovs map[string]presenceapi.Override) ([]Change, error) {
	// Locked in id order, so two group writes over overlapping members wait
	// for each other instead of deadlocking.
	ids := make([]string, 0, len(ovs))
	for id := range ovs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("presence: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	changes := make([]Change, 0, len(ids))
	for _, id := range ids {
		ov := ovs[id]
		wake, err := wakeParam(ov.WakeOn)
		if err != nil {
			return nil, err
		}
		prev, err := lockRow(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		_, now, err := scanOverride(tx.QueryRow(ctx, `
			INSERT INTO minecraft.presence_overrides (actor_id, state, until, wake_on, reason, set_by, set_at, version)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, 1)
			ON CONFLICT (actor_id) DO UPDATE
			SET state = EXCLUDED.state, until = EXCLUDED.until, wake_on = EXCLUDED.wake_on,
			    reason = EXCLUDED.reason, set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at,
			    version = minecraft.presence_overrides.version + 1
			RETURNING `+overrideColumns,
			id, string(ov.State), ov.Until, wake, ov.Reason, ov.SetBy, ov.SetAt))
		if err != nil {
			return nil, fmt.Errorf("presence: write %s: %w", id, err)
		}
		changes = append(changes, Change{ActorID: id, Prev: prev, Now: &now})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("presence: commit: %w", err)
	}
	return changes, nil
}

func (p *Postgres) Clear(ctx context.Context, ids []string) ([]Change, error) {
	rows, err := p.pool.Query(ctx, `DELETE FROM minecraft.presence_overrides WHERE actor_id = ANY($1) RETURNING `+overrideColumns, ids)
	if err != nil {
		return nil, fmt.Errorf("presence: clear: %w", err)
	}
	defer rows.Close()
	var changes []Change
	for rows.Next() {
		id, ov, err := scanOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("presence: clear: %w", err)
		}
		changes = append(changes, Change{ActorID: id, Prev: &ov})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: clear: %w", err)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].ActorID < changes[j].ActorID })
	return changes, nil
}

func (p *Postgres) Remove(ctx context.Context, actorID string, version int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM minecraft.presence_overrides WHERE actor_id = $1 AND version = $2`, actorID, version)
	if err != nil {
		return false, fmt.Errorf("presence: remove %s: %w", actorID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) Statuses(ctx context.Context) (map[string]presenceapi.Status, error) {
	rows, err := p.pool.Query(ctx, `SELECT actor_id, connected, observed_state, last_seen, process_version FROM minecraft.presence_status`)
	if err != nil {
		return nil, fmt.Errorf("presence: read status: %w", err)
	}
	defer rows.Close()
	out := make(map[string]presenceapi.Status)
	for rows.Next() {
		var id, observed string
		var st presenceapi.Status
		if err := rows.Scan(&id, &st.Connected, &observed, &st.LastSeen, &st.ProcessVersion); err != nil {
			return nil, fmt.Errorf("presence: read status: %w", err)
		}
		st.ObservedState = presenceapi.State(observed)
		st.LastSeen = st.LastSeen.UTC()
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: read status: %w", err)
	}
	return out, nil
}

func (p *Postgres) PutStatus(ctx context.Context, actorID string, s presenceapi.Status) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.presence_status (actor_id, connected, observed_state, last_seen, process_version)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (actor_id) DO UPDATE
		SET connected = EXCLUDED.connected, observed_state = EXCLUDED.observed_state,
		    last_seen = EXCLUDED.last_seen, process_version = EXCLUDED.process_version`,
		actorID, s.Connected, string(s.ObservedState), s.LastSeen, s.ProcessVersion,
	); err != nil {
		return fmt.Errorf("presence: write status of %s: %w", actorID, err)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `gofmt -w internal/presence && gofmt -l internal/presence && go vet -tags livedb ./internal/presence/ && go test -race ./internal/presence/`
Expected: `gofmt -l` and `go vet` print nothing, and the tests print `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/presence`.

Against a database migrated through V8 (see "Testing the store against a real database" in the README, applying `V8__minecraft_presence.sql` after the earlier migrations), run:
`MC_TEST_DSN='postgres://app:apppw@127.0.0.1:55432/jdwillmsen_prd?sslmode=disable' go test -tags livedb -race ./internal/presence/ -run 'Set|Clear|PutStatus' -v`
Expected: `TestSetFollowsTheVersionRules`, `TestSetManyAndClearAreOneTransactionEach` and `TestPutStatusUpserts` PASS. Without `MC_TEST_DSN` they SKIP.

- [ ] **Step 6: Commit**

```bash
git add internal/presence/store.go internal/presence/postgres.go internal/presence/store_test.go internal/presence/postgres_live_test.go
git commit -m "feat(presence): store overrides and observed status with versioned writes" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 6: `Service`: the one write path, with audit

**Files:**
- Create: `internal/presence/service.go`
- Create: `internal/presence/fakes_test.go` (shared by Tasks 6, 9, 10, 11)
- Test: `internal/presence/service_test.go`

**Interfaces:**
- Consumes: `Registry`, `Actor` (Task 3). `View`, `Cause*`, `Removal` (Task 4). `Store`, `Change`, `ErrConflict`, `ErrDisabled` (Task 5). `audit.Store`, `audit.Record`, `audit.OutcomeOK` (`internal/audit/audit.go:26-65`). `metrics.AuditWriteFailure()` (`internal/metrics/metrics.go:245-246`). `pgerr` is not needed here, because every store error is "unavailable" to a caller.
- Produces:
  - `var ErrNotFound, ErrInvalid, ErrUnavailable`
  - `type ConflictError struct { Current presenceapi.Presence }` (`Unwrap() == ErrConflict`)
  - `type Request struct { State presenceapi.State; Until *time.Time; WakeOn *presenceapi.WakeOn; Reason string; Version int64 }`
  - `type Source struct { SetBy, XUID, Gamertag, Permission string }`, `func APISource(tokenName string) Source`, `func ChatSource(xuid, gamertag, permission string) Source`
  - `func NewService(reg *Registry, store Store, auditor audit.Store, log *logging.Logger) *Service`
  - `(*Service)`: `Registry() *Registry`, `Enabled() bool`, `OnChange(fn func())`, `Presence(ctx, id string) (presenceapi.Presence, error)`, `List(ctx) ([]presenceapi.ActorView, error)`, `Set(ctx, id string, req Request, by Source) (presenceapi.Presence, error)`, `SetGroup(ctx, group string, req Request, by Source) ([]presenceapi.Presence, error)`, `SetEach(ctx, actors []Actor, build func(Actor) Request, by Source) ([]presenceapi.Presence, error)`, `Clear(ctx, actors []Actor, by Source) ([]presenceapi.Presence, error)`, `Expire(ctx, r Removal) (bool, error)`, `ReportStatus(ctx, id string, st presenceapi.Status) error`
  - Test fakes: `newFakeStore() *fakeStore`, `(*fakeStore).fail(err error)`, `fakeAudit`, `testRegistry(t) *Registry`, `newTestService(t, store *fakeStore, clock *time.Time) (*Service, *fakeAudit)`

The audit table (`minecraft.command_audit`) is command-shaped, and plan 2 adds no column to it. Presence changes use its columns as follows. `command` is `presence`. `args` is `actor=<id> from=<state> to=<state> cause=<set|cleared|expired|woken> source=<set_by> reason="<text>"`. `xuid` and `gamertag` hold the player for chat, and the `set_by` string or the token name for API writes. `permission` is the resolved level, `api` or `system`. The outcome is always `ok`.

- [ ] **Step 1: Write the shared fakes**

`internal/presence/fakes_test.go`:
```go
package presence

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// fakeStore keeps the version rules of the real one in memory, so the
// service, loop, API and chat tests exercise the same conflicts Postgres
// would produce.
type fakeStore struct {
	mu     sync.Mutex
	rows   map[string]presenceapi.Override
	status map[string]presenceapi.Status
	err    error
	// bumpOnRemove simulates a write landing between a read and a removal.
	bumpOnRemove bool
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]presenceapi.Override{}, status: map[string]presenceapi.Status{}}
}

func (f *fakeStore) fail(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }
func (f *fakeStore) Enabled() bool  { return true }

func (f *fakeStore) row(id string) (presenceapi.Override, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ov, ok := f.rows[id]
	return ov, ok
}

func (f *fakeStore) Overrides(context.Context) (map[string]presenceapi.Override, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]presenceapi.Override, len(f.rows))
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) upsert(id string, ov presenceapi.Override) Change {
	var prev *presenceapi.Override
	if p, ok := f.rows[id]; ok {
		prev = &p
		ov.Version = p.Version + 1
	} else {
		ov.Version = 1
	}
	f.rows[id] = ov
	return Change{ActorID: id, Prev: prev, Now: &ov}
}

func (f *fakeStore) Set(_ context.Context, id string, ov presenceapi.Override, expect int64) (Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Change{}, f.err
	}
	var have int64
	if p, ok := f.rows[id]; ok {
		have = p.Version
		if have != expect {
			return Change{ActorID: id, Prev: &p}, ErrConflict
		}
	} else if expect != 0 {
		return Change{ActorID: id}, ErrConflict
	}
	return f.upsert(id, ov), nil
}

func (f *fakeStore) SetMany(_ context.Context, ovs map[string]presenceapi.Override) ([]Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ids := make([]string, 0, len(ovs))
	for id := range ovs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Change
	for _, id := range ids {
		out = append(out, f.upsert(id, ovs[id]))
	}
	return out, nil
}

func (f *fakeStore) Clear(_ context.Context, ids []string) ([]Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []Change
	for _, id := range ids {
		if p, ok := f.rows[id]; ok {
			delete(f.rows, id)
			out = append(out, Change{ActorID: id, Prev: &p})
		}
	}
	return out, nil
}

func (f *fakeStore) Remove(_ context.Context, id string, version int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	p, ok := f.rows[id]
	if f.bumpOnRemove && ok {
		p.Version++
		f.rows[id] = p
	}
	if !ok || p.Version != version {
		return false, nil
	}
	delete(f.rows, id)
	return true, nil
}

func (f *fakeStore) Statuses(context.Context) (map[string]presenceapi.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]presenceapi.Status, len(f.status))
	for k, v := range f.status {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) PutStatus(_ context.Context, id string, s presenceapi.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.status[id] = s
	return nil
}

type fakeAudit struct {
	mu      sync.Mutex
	records []audit.Record
}

func (a *fakeAudit) Write(_ context.Context, r audit.Record) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, r)
	return nil
}
func (a *fakeAudit) Enabled() bool { return true }
func (a *fakeAudit) all() []audit.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audit.Record(nil), a.records...)
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(threeActors(), "agent")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// newTestService runs on *clock, so a test moves time by assigning to it.
func newTestService(t *testing.T, store *fakeStore, clock *time.Time) (*Service, *fakeAudit) {
	t.Helper()
	aud := &fakeAudit{}
	s := NewService(testRegistry(t), store, aud, logging.New("error"))
	s.now = func() time.Time { return *clock }
	return s, aud
}
```

- [ ] **Step 2: Write the failing test**

`internal/presence/service_test.go`:
```go
package presence

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

func parkReq(version int64) Request {
	return Request{State: presenceapi.StateParked, Reason: "chunk budget", Version: version}
}

func TestSetWritesAuditsAndNotifies(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	notified := 0
	svc.OnChange(func() { notified++ })

	p, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops"))
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if p.Effective != presenceapi.StateParked || p.Override.Version != 1 || p.Override.SetBy != "api:ops" || !p.Override.SetAt.Equal(t0) {
		t.Errorf("presence = %+v", p)
	}
	if notified != 1 {
		t.Errorf("notified %d times, want once", notified)
	}
	recs := aud.all()
	if len(recs) != 1 {
		t.Fatalf("audited %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Command != "presence" || r.XUID != "api:ops" || r.Permission != "api" ||
		r.Args != `actor=afk-bot-1 from=present to=parked cause=set source=api:ops reason="chunk budget"` {
		t.Errorf("audit record = %+v", r)
	}
}

func TestSetRefusesAStaleVersionWithTheCurrentRow(t *testing.T) {
	clock := t0
	svc, aud := newTestService(t, newFakeStore(), &clock)
	if _, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops")); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops"))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	if conflict.Current.Override == nil || conflict.Current.Override.Version != 1 || conflict.Current.ActorID != "afk-bot-1" {
		t.Errorf("current = %+v, want the version 1 row", conflict.Current)
	}
	if _, err := svc.Set(t.Context(), "afk-bot-1", Request{State: presenceapi.StatePresent, Reason: "r", Version: 1}, APISource("ops")); err != nil {
		t.Errorf("Set at the current version: %v", err)
	}
	if n := len(aud.all()); n != 2 {
		t.Errorf("audited %d records, want 2: a refused write is not a change", n)
	}
}

func TestSetValidates(t *testing.T) {
	clock := t0
	svc, _ := newTestService(t, newFakeStore(), &clock)
	past, future := t0.Add(-time.Minute), t0.Add(time.Hour)
	cases := map[string]Request{
		"unknown state":       {State: "gone", Reason: "r"},
		"no reason":           {State: presenceapi.StateParked, Reason: "  "},
		"long reason":         {State: presenceapi.StateParked, Reason: strings.Repeat("x", maxReasonChars+1)},
		"expiry in the past":  {State: presenceapi.StateParked, Reason: "r", Until: &past},
		"wake on present":     {State: presenceapi.StatePresent, Reason: "r", WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true}},
		"wake on both":        {State: presenceapi.StateParked, Reason: "r", Until: &future, WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true, Players: []string{"Steve"}}},
		"wake on nothing":     {State: presenceapi.StateParked, Reason: "r", WakeOn: &presenceapi.WakeOn{}},
		"wake on blank names": {State: presenceapi.StateParked, Reason: "r", WakeOn: &presenceapi.WakeOn{Players: []string{" "}}},
	}
	for name, req := range cases {
		if _, err := svc.Set(t.Context(), "afk-bot-1", req, APISource("ops")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := svc.Set(t.Context(), "afk-bot-9", parkReq(0), APISource("ops")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown actor: err = %v, want ErrNotFound", err)
	}
}

func TestStoreFailuresAreUnavailable(t *testing.T) {
	clock := t0
	store := newFakeStore()
	store.fail(errors.New("connection refused"))
	svc, _ := newTestService(t, store, &clock)
	if _, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Set: %v, want ErrUnavailable", err)
	}
	if _, err := svc.List(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("List: %v, want ErrUnavailable", err)
	}
	if _, err := svc.Presence(t.Context(), "agent"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Presence: %v, want ErrUnavailable", err)
	}
}

func TestSetGroupWritesEveryMember(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	got, err := svc.SetGroup(t.Context(), "bots", Request{State: presenceapi.StateParked, Reason: "r", Version: 99}, APISource("ops"))
	if err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if len(got) != 2 || got[0].ActorID != "afk-bot-1" || got[1].ActorID != "afk-bot-2" {
		t.Errorf("SetGroup = %+v, want both bots in configuration order", got)
	}
	if len(aud.all()) != 2 {
		t.Errorf("audited %d, want one per member", len(aud.all()))
	}
	if _, err := svc.SetGroup(t.Context(), "miners", parkReq(0), APISource("ops")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown group: %v, want ErrNotFound", err)
	}
}

func TestClearFallsBackToTheDefault(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	if _, err := svc.Set(t.Context(), "afk-bot-2", Request{State: presenceapi.StatePresent, Reason: "r"}, APISource("ops")); err != nil {
		t.Fatal(err)
	}
	bots, _ := svc.Registry().Group("bots")
	got, err := svc.Clear(t.Context(), bots, APISource("ops"))
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got[1].Effective != presenceapi.StateParked || got[1].Override != nil {
		t.Errorf("afk-bot-2 after clear = %+v, want its parked default", got[1])
	}
	recs := aud.all()
	if len(recs) != 2 || !strings.Contains(recs[1].Args, "cause=cleared") || !strings.Contains(recs[1].Args, "to=parked") {
		t.Errorf("audit = %+v, want one set and one clear, and no record for the bot that had nothing to clear", recs)
	}
}

func TestExpireRemovesOnlyTheVersionItRead(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	p, _ := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops"))

	stale := Removal{ActorID: "afk-bot-1", Cause: CauseExpired, Override: *p.Override}
	stale.Override.Version = 7
	if ok, err := svc.Expire(t.Context(), stale); ok || err != nil {
		t.Errorf("Expire at a stale version = %v, %v; want nothing removed", ok, err)
	}
	ok, err := svc.Expire(t.Context(), Removal{ActorID: "afk-bot-1", Cause: CauseWoken, Override: *p.Override})
	if !ok || err != nil {
		t.Fatalf("Expire = %v, %v; want removed", ok, err)
	}
	last := aud.all()[len(aud.all())-1]
	if last.XUID != "presence-loop" || !strings.Contains(last.Args, "cause=woken") || !strings.Contains(last.Args, "from=parked to=present") {
		t.Errorf("removal audit = %+v", last)
	}
}

// Staleness is judged against the agent's clock, so a bot's own clock never
// decides whether it looks connected.
func TestReportStatusStampsTheAgentsClock(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, _ := newTestService(t, store, &clock)
	err := svc.ReportStatus(t.Context(), "afk-bot-1", presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0.Add(-time.Hour), ProcessVersion: "1.4.0"})
	if err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if st := store.status["afk-bot-1"]; !st.LastSeen.Equal(t0) {
		t.Errorf("last_seen = %v, want the agent's %v", st.LastSeen, t0)
	}
	if err := svc.ReportStatus(t.Context(), "afk-bot-9", presenceapi.Status{ObservedState: presenceapi.StatePresent}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown actor: %v", err)
	}
	if err := svc.ReportStatus(t.Context(), "afk-bot-1", presenceapi.Status{ObservedState: "gone"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad state: %v", err)
	}
}

func TestListIncludesStatusAndNonNilGroups(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, _ := newTestService(t, store, &clock)
	store.status["afk-bot-1"] = presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0}
	views, err := svc.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 3 || views[0].Groups == nil || views[1].Status == nil || views[2].Status != nil {
		t.Errorf("views = %+v", views)
	}
}
```

- [ ] **Step 3: Run the test to see it fail**

Run: `go test ./internal/presence/ -run 'Set|Clear|Expire|Report|List|Unavailable'`
Expected: FAIL, build errors `undefined: NewService`, `undefined: Request`, `undefined: APISource`.

- [ ] **Step 4: Implement the service**

`internal/presence/service.go`:
```go
package presence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

var (
	ErrNotFound    = errors.New("presence: no such actor or group")
	ErrInvalid     = errors.New("presence: invalid request")
	ErrUnavailable = errors.New("presence: store unavailable")
)

// ConflictError is a write refused because the override changed since the
// caller read it. Current is what it is now, so the caller can decide again
// without a second read.
type ConflictError struct {
	Current presenceapi.Presence
}

func (e *ConflictError) Error() string {
	var v int64
	if e.Current.Override != nil {
		v = e.Current.Override.Version
	}
	return fmt.Sprintf("%s changed since it was read; it is now at version %d", e.Current.ActorID, v)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }

// Request is one override as a caller asks for it, before the service
// stamps who and when.
type Request struct {
	State   presenceapi.State
	Until   *time.Time
	WakeOn  *presenceapi.WakeOn
	Reason  string
	Version int64
}

// Source is who asked for a change, as set_by and the audit trail record it.
type Source struct {
	SetBy      string
	XUID       string
	Gamertag   string
	Permission string
}

// APISource is a write through the HTTP API. The audit columns are
// player-shaped, and an API client has no XUID, so the set_by string stands
// in for one.
func APISource(tokenName string) Source {
	by := "api:" + tokenName
	return Source{SetBy: by, XUID: by, Gamertag: tokenName, Permission: "api"}
}

// ChatSource is a write from a chat command.
func ChatSource(xuid, gamertag, permission string) Source {
	return Source{SetBy: "chat:" + gamertag, XUID: xuid, Gamertag: gamertag, Permission: permission}
}

// loopSource is the leader's loop removing an override that has ended.
var loopSource = Source{SetBy: "presence-loop", XUID: "presence-loop", Gamertag: "presence-loop", Permission: "system"}

const (
	// maxReasonChars bounds a reason, which is printed into chat replies and
	// audit rows.
	maxReasonChars = 280
	// maxStatusVersion bounds a reported process version, which is stored
	// and served back to every API reader.
	maxStatusVersion = 64
	// auditTimeout bounds the audit write, which runs after the change has
	// landed and must never be what makes a caller wait.
	auditTimeout = 2 * time.Second
)

// Service is the one path every read and write takes, whether from the API,
// chat or the loop, so validation, audit and notification cannot differ
// between them.
type Service struct {
	reg   *Registry
	store Store
	audit audit.Store
	log   *logging.Logger
	now   func() time.Time

	mu     sync.Mutex
	notify func()
}

func NewService(reg *Registry, store Store, auditor audit.Store, log *logging.Logger) *Service {
	return &Service{reg: reg, store: store, audit: auditor, log: log, now: time.Now, notify: func() {}}
}

func (s *Service) Registry() *Registry { return s.reg }

// Enabled reports whether any actor is configured.
func (s *Service) Enabled() bool { return s.reg.Enabled() }

// OnChange sets what runs after every write that landed. The loop registers
// itself here, so a change made on the leader acts at once instead of on the
// next tick.
func (s *Service) OnChange(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notify = fn
}

func (s *Service) changed() {
	s.mu.Lock()
	fn := s.notify
	s.mu.Unlock()
	fn()
}

func (s *Service) Presence(ctx context.Context, id string) (presenceapi.Presence, error) {
	a, ok := s.reg.Actor(id)
	if !ok {
		return presenceapi.Presence{}, ErrNotFound
	}
	ovs, err := s.store.Overrides(ctx)
	if err != nil {
		return presenceapi.Presence{}, unavailable(err)
	}
	return viewFrom(a, ovs), nil
}

func (s *Service) List(ctx context.Context) ([]presenceapi.ActorView, error) {
	ovs, err := s.store.Overrides(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	statuses, err := s.store.Statuses(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	views := make([]presenceapi.ActorView, 0, len(s.reg.actors))
	for _, a := range s.reg.Actors() {
		v := presenceapi.ActorView{ID: a.ID, Gamertag: a.Gamertag, Kind: a.Kind, Groups: a.Groups, Presence: viewFrom(a, ovs)}
		if st, ok := statuses[a.ID]; ok {
			v.Status = &st
		}
		views = append(views, v)
	}
	return views, nil
}

func (s *Service) Set(ctx context.Context, id string, req Request, by Source) (presenceapi.Presence, error) {
	a, ok := s.reg.Actor(id)
	if !ok {
		return presenceapi.Presence{}, ErrNotFound
	}
	ov, err := s.override(req, by)
	if err != nil {
		return presenceapi.Presence{}, err
	}
	ch, err := s.store.Set(ctx, id, ov, req.Version)
	if errors.Is(err, ErrConflict) {
		return presenceapi.Presence{}, &ConflictError{Current: View(a, ch.Prev)}
	}
	if err != nil {
		return presenceapi.Presence{}, unavailable(err)
	}
	s.record(ctx, a, ch, CauseSet, by)
	s.changed()
	return View(a, ch.Now), nil
}

// SetGroup applies one request to every member of group in one transaction.
// A group has no single version to compare, so Version is ignored.
func (s *Service) SetGroup(ctx context.Context, group string, req Request, by Source) ([]presenceapi.Presence, error) {
	actors, ok := s.reg.Group(group)
	if !ok {
		return nil, ErrNotFound
	}
	return s.SetEach(ctx, actors, func(Actor) Request { return req }, by)
}

// SetEach writes a request built per actor, for every actor, in one
// transaction and regardless of versions. Chat needs the per-actor build:
// "!park all" gives the agent a way back that it does not give a bot.
func (s *Service) SetEach(ctx context.Context, actors []Actor, build func(Actor) Request, by Source) ([]presenceapi.Presence, error) {
	ovs := make(map[string]presenceapi.Override, len(actors))
	for _, a := range actors {
		ov, err := s.override(build(a), by)
		if err != nil {
			return nil, err
		}
		ovs[a.ID] = ov
	}
	changes, err := s.store.SetMany(ctx, ovs)
	if err != nil {
		return nil, unavailable(err)
	}
	return s.landed(ctx, actors, changes, CauseSet, by), nil
}

// Clear removes the overrides of actors, returning each to its default.
func (s *Service) Clear(ctx context.Context, actors []Actor, by Source) ([]presenceapi.Presence, error) {
	ids := make([]string, 0, len(actors))
	for _, a := range actors {
		ids = append(ids, a.ID)
	}
	changes, err := s.store.Clear(ctx, ids)
	if err != nil {
		return nil, unavailable(err)
	}
	return s.landed(ctx, actors, changes, CauseCleared, by), nil
}

// landed audits every change and reports every actor's presence afterwards,
// in the caller's order. An actor with no change is reported as it is.
func (s *Service) landed(ctx context.Context, actors []Actor, changes []Change, cause Cause, by Source) []presenceapi.Presence {
	after := make(map[string]*presenceapi.Override, len(changes))
	touched := make(map[string]bool, len(changes))
	for _, ch := range changes {
		after[ch.ActorID] = ch.Now
		touched[ch.ActorID] = true
		if a, ok := s.reg.Actor(ch.ActorID); ok {
			s.record(ctx, a, ch, cause, by)
		}
	}
	out := make([]presenceapi.Presence, 0, len(actors))
	for _, a := range actors {
		out = append(out, View(a, after[a.ID]))
	}
	if len(changes) > 0 {
		s.changed()
	}
	return out
}

// Expire removes an override the policy says has ended, only if it is still
// the version the policy read. A newer write means an operator decided again
// in the meantime, and that decision stands until the next tick weighs it.
func (s *Service) Expire(ctx context.Context, r Removal) (bool, error) {
	a, ok := s.reg.Actor(r.ActorID)
	if !ok {
		return false, ErrNotFound
	}
	removed, err := s.store.Remove(ctx, r.ActorID, r.Override.Version)
	if err != nil {
		return false, unavailable(err)
	}
	if removed {
		prev := r.Override
		s.record(ctx, a, Change{ActorID: r.ActorID, Prev: &prev}, r.Cause, loopSource)
	}
	return removed, nil
}

// ReportStatus stores what an actor says it is doing.
func (s *Service) ReportStatus(ctx context.Context, id string, st presenceapi.Status) error {
	if _, ok := s.reg.Actor(id); !ok {
		return ErrNotFound
	}
	if !st.ObservedState.Valid() {
		return invalid("observed_state must be present or parked")
	}
	if len(st.ProcessVersion) > maxStatusVersion {
		return invalid(fmt.Sprintf("process_version is capped at %d bytes", maxStatusVersion))
	}
	// Stamped here rather than taken from the reporter: staleness is judged
	// against this clock, and a bot whose clock drifted would otherwise look
	// fresh long after it stopped reporting.
	st.LastSeen = s.now().UTC()
	if err := s.store.PutStatus(ctx, id, st); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) override(req Request, by Source) (presenceapi.Override, error) {
	now := s.now().UTC()
	reason := strings.TrimSpace(req.Reason)
	switch {
	case !req.State.Valid():
		return presenceapi.Override{}, invalid("state must be present or parked")
	case reason == "":
		return presenceapi.Override{}, invalid("reason is required")
	case utf8.RuneCountInString(reason) > maxReasonChars:
		return presenceapi.Override{}, invalid(fmt.Sprintf("reason is capped at %d characters", maxReasonChars))
	case req.Until != nil && !req.Until.After(now):
		return presenceapi.Override{}, invalid("until must be in the future")
	}
	if w := req.WakeOn; w != nil {
		if req.State != presenceapi.StateParked {
			return presenceapi.Override{}, invalid("wake_on only ends a parked override")
		}
		if w.AnyPlayerJoin == (len(w.Players) > 0) {
			return presenceapi.Override{}, invalid("wake_on takes exactly one of any_player_join and players")
		}
		for _, p := range w.Players {
			if strings.TrimSpace(p) == "" {
				return presenceapi.Override{}, invalid("wake_on.players holds a blank gamertag")
			}
		}
	}
	ov := presenceapi.Override{State: req.State, WakeOn: req.WakeOn, Reason: reason, SetBy: by.SetBy, SetAt: now}
	if req.Until != nil {
		u := req.Until.UTC()
		ov.Until = &u
	}
	return ov, nil
}

// record logs and audits one change. The audit write is bounded and
// detached from the caller: the change has already landed, and a caller who
// gave up must not leave it unrecorded.
func (s *Service) record(ctx context.Context, a Actor, ch Change, cause Cause, by Source) {
	from, to := stateOf(a, ch.Prev), stateOf(a, ch.Now)
	reason := ""
	switch {
	case ch.Now != nil:
		reason = ch.Now.Reason
	case ch.Prev != nil:
		reason = ch.Prev.Reason
	}
	s.log.Info("presence_changed", logging.Fields{"actor": a.ID, "from": string(from), "to": string(to), "cause": string(cause), "source": by.SetBy})
	if s.audit == nil || !s.audit.Enabled() {
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	err := s.audit.Write(auditCtx, audit.Record{
		XUID:       by.XUID,
		Gamertag:   by.Gamertag,
		Permission: by.Permission,
		Command:    "presence",
		Args:       fmt.Sprintf("actor=%s from=%s to=%s cause=%s source=%s reason=%q", a.ID, from, to, cause, by.SetBy, reason),
		Outcome:    audit.OutcomeOK,
		At:         s.now(),
	})
	if err != nil {
		metrics.AuditWriteFailure()
		s.log.Error("presence_audit_failed", logging.Fields{"actor": a.ID, "cause": string(cause), "error": err.Error()})
	}
}

func stateOf(a Actor, ov *presenceapi.Override) presenceapi.State {
	if ov == nil {
		return a.Default
	}
	return ov.State
}

func viewFrom(a Actor, ovs map[string]presenceapi.Override) presenceapi.Presence {
	if ov, ok := ovs[a.ID]; ok {
		return View(a, &ov)
	}
	return View(a, nil)
}

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

func unavailable(err error) error { return fmt.Errorf("%w: %w", ErrUnavailable, err) }
```

- [ ] **Step 5: Run the tests to see them pass**

Run: `gofmt -w internal/presence && gofmt -l internal/presence && go test -race ./internal/presence/`
Expected: `gofmt -l` prints nothing, and the tests print `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/presence`.

- [ ] **Step 6: Commit**

```bash
git add internal/presence/service.go internal/presence/service_test.go internal/presence/fakes_test.go
git commit -m "feat(presence): route every presence read and write through one audited service" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 7: `mc_presence_*` metrics

**Files:**
- Create: `internal/metrics/presence.go`
- Test: `internal/metrics/presence_test.go`

**Interfaces:**
- Consumes: the package's `promauto` pattern (`internal/metrics/metrics.go:77-167`), `metricstest.Value`, `metricstest.Delta` and `metricstest.Exists` (`internal/metrics/metricstest/metricstest.go`).
- Produces (package `metrics`): `func InitPresence(actors []string)`, `func PresenceDesired(actor string, present bool)`, `func PresenceObserved(actor string, connected bool)`, `func PresenceOverrideAge(actor string, age time.Duration, open bool)`, `func PresenceKick(actor string)`, `func ResetPresence()`.

The series live in `internal/metrics` with the rest, so the names that alerts are written against sit in one package. The spec's "Metrics" unit of `internal/presence` is the loop calling these recorders (Task 9).

- [ ] **Step 1: Write the failing test**

`internal/metrics/presence_test.go`:
```go
package metrics

import (
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
)

func TestPresenceGauges(t *testing.T) {
	t.Cleanup(ResetPresence)
	PresenceDesired("afk-bot-1", false)
	PresenceObserved("afk-bot-1", true)
	if got := metricstest.Value(t, "mc_presence_desired", "actor", "afk-bot-1"); got != 0 || !metricstest.Exists(t, "mc_presence_desired", "actor", "afk-bot-1") {
		t.Errorf("desired = %v, want an exported 0", got)
	}
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "afk-bot-1"); got != 1 {
		t.Errorf("observed = %v, want 1", got)
	}
}

// An override with an expiry cannot be forgotten, so it has no age series:
// the parked-long alert is about the ones that can.
func TestPresenceOverrideAgeExistsOnlyForOpenOverrides(t *testing.T) {
	t.Cleanup(ResetPresence)
	PresenceOverrideAge("afk-bot-2", 90*time.Minute, true)
	if got := metricstest.Value(t, "mc_presence_override_age_seconds", "actor", "afk-bot-2"); got != 5400 {
		t.Errorf("age = %v, want 5400", got)
	}
	PresenceOverrideAge("afk-bot-2", 0, false)
	if metricstest.Exists(t, "mc_presence_override_age_seconds", "actor", "afk-bot-2") {
		t.Error("age series survived its override gaining an expiry")
	}
}

func TestPresenceKicksStartAtZero(t *testing.T) {
	InitPresence([]string{"afk-bot-1"})
	if !metricstest.Exists(t, "mc_presence_kicks_total", "actor", "afk-bot-1") {
		t.Fatal("kick counter not initialised; increase() over its first kick would read 0")
	}
	if d := metricstest.Delta(t, func() { PresenceKick("afk-bot-1") }, "mc_presence_kicks_total", "actor", "afk-bot-1"); d != 1 {
		t.Errorf("kick moved the counter by %v, want 1", d)
	}
}

// A standby must not keep exporting what it last saw as leader beside what
// the new leader sees.
func TestResetPresenceWithdrawsTheGauges(t *testing.T) {
	PresenceDesired("agent", true)
	PresenceObserved("agent", true)
	PresenceOverrideAge("agent", time.Minute, true)
	ResetPresence()
	for _, name := range []string{"mc_presence_desired", "mc_presence_observed", "mc_presence_override_age_seconds"} {
		if metricstest.Exists(t, name, "actor", "agent") {
			t.Errorf("%s still exported after ResetPresence", name)
		}
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/metrics/ -run Presence`
Expected: FAIL, build errors `undefined: PresenceDesired`, `undefined: ResetPresence`.

- [ ] **Step 3: Implement the series**

`internal/metrics/presence.go`:
```go
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Presence series are labelled by actor id, which comes from the configured
// actor registry and never from chat, so the label set is bounded by the
// Helm values that list the actors.
var (
	presenceDesired = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_desired",
		Help: "1 if the actor's effective presence is present, 0 if parked. Exported by the leader only.",
	}, []string{"actor"})
	presenceObserved = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_observed",
		Help: "1 if the actor is connected by its own recent report, 0 otherwise. Exported by the leader only.",
	}, []string{"actor"})
	presenceOverrideAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_override_age_seconds",
		Help: "Age of the actor's override when it has no expiry; absent otherwise.",
	}, []string{"actor"})
	presenceKicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_presence_kicks_total",
		Help: "Kicks sent for an actor still on the server after its park grace.",
	}, []string{"actor"})
)

// InitPresence starts every actor's kick counter at zero, for the same
// reason init does for the fixed label sets.
func InitPresence(actors []string) {
	for _, a := range actors {
		presenceKicksTotal.WithLabelValues(a)
	}
}

func PresenceDesired(actor string, present bool) {
	presenceDesired.WithLabelValues(actor).Set(boolValue(present))
}

func PresenceObserved(actor string, connected bool) {
	presenceObserved.WithLabelValues(actor).Set(boolValue(connected))
}

// PresenceOverrideAge records how old an actor's open-ended override is.
// open false withdraws the series: an override with an expiry, or none at
// all, has no age worth alerting on.
func PresenceOverrideAge(actor string, age time.Duration, open bool) {
	if !open {
		presenceOverrideAge.DeleteLabelValues(actor)
		return
	}
	presenceOverrideAge.WithLabelValues(actor).Set(age.Seconds())
}

func PresenceKick(actor string) { presenceKicksTotal.WithLabelValues(actor).Inc() }

// ResetPresence withdraws every presence gauge when this process stops
// leading. The kick counter stays: a counter that vanished and came back
// would read as a reset to every rate() over it.
func ResetPresence() {
	presenceDesired.Reset()
	presenceObserved.Reset()
	presenceOverrideAge.Reset()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `gofmt -w internal/metrics && gofmt -l internal/metrics && go test -race ./internal/metrics/`
Expected: `gofmt -l` prints nothing, and the test run prints `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/metrics`.

- [ ] **Step 5: Commit**

```bash
git add internal/metrics/presence.go internal/metrics/presence_test.go
git commit -m "feat(metrics): export desired and observed presence per actor" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 8: `Gate`, `JoinLog` and the bridge kick

**Files:**
- Create: `internal/presence/gate.go`, `internal/presence/joinlog.go`, `internal/adapters/kick.go`
- Test: `internal/presence/gate_test.go`, `internal/presence/joinlog_test.go`, `internal/adapters/kick_test.go`

**Interfaces:**
- Consumes: plan 1's `sessionGate` contract (`Wanted() (present bool, changed <-chan struct{})`, where the channel is closed the next time the answer may change, and a spurious close is allowed). Also `(*BridgeClient).runCommand` (`internal/adapters/bridgeclient.go:51-62`) and plan 4's bridge rule, under which `POST /command` accepts exactly `kick <name>` or `kick "<name>"`.
- Produces:
  - `func NewGate(present bool) *Gate`, `(*Gate).Set(present bool)`, `(*Gate).Wanted() (bool, <-chan struct{})`
  - `func NewJoinLog() *JoinLog`, `(*JoinLog).Record(gamertag string)`, `(*JoinLog).RecordAt(gamertag string, at time.Time)`, `(*JoinLog).Recent() []Join`
  - `func (c *BridgeClient) Kick(ctx context.Context, gamertag string) error`

- [ ] **Step 1: Write the failing tests**

`internal/presence/gate_test.go`:
```go
package presence

import "testing"

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestGateClosesItsChannelOnlyOnAChange(t *testing.T) {
	g := NewGate(true)
	present, ch := g.Wanted()
	if !present || ch == nil {
		t.Fatalf("Wanted() = %v, %v; want present and a channel that can close", present, ch)
	}
	g.Set(true)
	if closed(ch) {
		t.Error("setting the same answer closed the channel")
	}
	g.Set(false)
	if !closed(ch) {
		t.Fatal("a change did not close the channel")
	}
	present, next := g.Wanted()
	if present || closed(next) {
		t.Errorf("after the change Wanted() = %v with a closed channel %v; want parked and a fresh channel", present, closed(next))
	}
}
```

`internal/presence/joinlog_test.go`:
```go
package presence

import (
	"testing"
	"time"
)

func TestJoinLogKeepsRecentArrivalsOnly(t *testing.T) {
	now := t0
	l := NewJoinLog()
	l.now = func() time.Time { return now }
	l.Record("Steve")
	now = now.Add(joinRetention)
	l.Record("Alex")
	got := l.Recent()
	if len(got) != 2 || got[0].Gamertag != "Steve" || !got[0].At.Equal(t0) {
		t.Fatalf("Recent() = %+v, want both, Steve first", got)
	}
	now = now.Add(time.Second)
	if got := l.Recent(); len(got) != 1 || got[0].Gamertag != "Alex" {
		t.Errorf("Recent() = %+v, want Steve aged out", got)
	}
}

func TestJoinLogKeepsTheSourcesOwnTime(t *testing.T) {
	now := t0
	l := NewJoinLog()
	l.now = func() time.Time { return now }
	l.RecordAt("Alex", t0.Add(-time.Minute))
	l.RecordAt("Old", t0.Add(-joinRetention-time.Second))
	got := l.Recent()
	if len(got) != 1 || got[0].Gamertag != "Alex" || !got[0].At.Equal(t0.Add(-time.Minute)) {
		t.Errorf("Recent() = %+v, want Alex at the time the console printed", got)
	}
}

// A join storm cannot grow the log without bound.
func TestJoinLogIsBounded(t *testing.T) {
	l := NewJoinLog()
	for i := 0; i < maxJoins+10; i++ {
		l.Record("Steve")
	}
	if n := len(l.Recent()); n != maxJoins {
		t.Errorf("kept %d joins, want the newest %d", n, maxJoins)
	}
}
```

`internal/adapters/kick_test.go`:
```go
package adapters

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKickSendsTheBridgesKickCommand(t *testing.T) {
	cases := map[string]string{
		"JdwAfk1":     "kick JdwAfk1",
		"Jdw Afk Two": `kick "Jdw Afk Two"`,
	}
	for gamertag, want := range cases {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req commandRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			got = req.Command
			_ = json.NewEncoder(w).Encode(commandResponse{Rule: "kick", Output: "Kicked " + gamertag})
		}))
		err := NewBridgeClient(srv.URL, "tok", time.Second).Kick(t.Context(), gamertag)
		srv.Close()
		if err != nil {
			t.Fatalf("Kick(%q): %v", gamertag, err)
		}
		if got != want {
			t.Errorf("Kick(%q) sent %q, want %q", gamertag, got, want)
		}
	}
}

// The bridge refuses a kick for anyone not on its actor list; that refusal
// has to reach the loop as an error, not vanish.
func TestKickReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kick is limited to actors", http.StatusForbidden)
	}))
	defer srv.Close()
	if err := NewBridgeClient(srv.URL, "tok", time.Second).Kick(t.Context(), "Steve"); err == nil {
		t.Error("a refused kick returned nil")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/presence/ -run 'Gate|JoinLog' ; go test ./internal/adapters/ -run Kick`
Expected: FAIL, build errors `undefined: NewGate`, `undefined: NewJoinLog`, `c.Kick undefined`.

- [ ] **Step 3: Implement**

`internal/presence/gate.go`:
```go
package presence

import "sync"

// Gate is whether this process's own actor should be in the world, as the
// leader's loop last decided. It satisfies the agent's sessionGate: the
// session lifecycle reads Wanted and waits on the channel.
type Gate struct {
	mu      sync.Mutex
	present bool
	changed chan struct{}
}

func NewGate(present bool) *Gate {
	return &Gate{present: present, changed: make(chan struct{})}
}

// Set records the answer, waking every waiter only when it differs: a
// session is torn down on a wake, so a spurious one every tick would be
// read again for nothing.
func (g *Gate) Set(present bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.present == present {
		return
	}
	g.present = present
	close(g.changed)
	g.changed = make(chan struct{})
}

// Wanted reports the answer and a channel closed when it next changes.
func (g *Gate) Wanted() (bool, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.present, g.changed
}
```

`internal/presence/joinlog.go`:
```go
package presence

import (
	"slices"
	"sync"
	"time"
)

const (
	// joinRetention is how long an arrival is kept for the wake rules. It is
	// several ticks rather than one, so a tick that could not read the
	// database does not lose the arrival that should have woken an actor.
	// Replaying an old arrival is harmless: the policy ignores any from
	// before an override was set.
	joinRetention = 5 * time.Minute
	// maxJoins bounds the log against a join storm. Far above what a
	// friends' server sees in five minutes.
	maxJoins = 256
)

// JoinLog is the players who recently arrived, fed from the session's roster
// events while the agent is in the world and from the console bridge while
// it is not.
type JoinLog struct {
	mu    sync.Mutex
	joins []Join
	now   func() time.Time
}

func NewJoinLog() *JoinLog { return &JoinLog{now: time.Now} }

// Record notes an arrival now, for a source that carries no time of its own.
func (l *JoinLog) Record(gamertag string) { l.RecordAt(gamertag, l.now()) }

// RecordAt notes an arrival at the time its source says it happened. A
// console line replayed from the bridge's backlog keeps its own time, so it
// cannot pass for a join that came after a park.
func (l *JoinLog) RecordAt(gamertag string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.joins = append(l.joins, Join{Gamertag: gamertag, At: at})
	if over := len(l.joins) - maxJoins; over > 0 {
		l.joins = slices.Delete(l.joins, 0, over)
	}
}

// Recent returns the arrivals of the last joinRetention, in the order they
// were recorded.
func (l *JoinLog) Recent() []Join {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-joinRetention)
	l.joins = slices.DeleteFunc(l.joins, func(j Join) bool { return j.At.Before(cutoff) })
	return slices.Clone(l.joins)
}
```

`internal/adapters/kick.go`:
```go
package adapters

import (
	"context"
	"fmt"
	"strings"
)

// Kick removes gamertag from the server through the bridge. The bridge takes
// it only for the actors on its own list, so a leaked bridge token cannot be
// turned on a real player.
//
// Quoted only when the name has a space: that is the one case the console
// cannot parse bare, and the bridge accepts exactly these two forms.
func (c *BridgeClient) Kick(ctx context.Context, gamertag string) error {
	cmd := "kick " + gamertag
	if strings.Contains(gamertag, " ") {
		cmd = `kick "` + gamertag + `"`
	}
	if _, err := c.runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("bridge: kick %s: %w", gamertag, err)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `gofmt -w internal/presence internal/adapters && gofmt -l internal/presence internal/adapters && go test -race ./internal/presence/ ./internal/adapters/`
Expected: `gofmt -l` prints nothing; both packages print `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/presence/gate.go internal/presence/gate_test.go internal/presence/joinlog.go internal/presence/joinlog_test.go internal/adapters/kick.go internal/adapters/kick_test.go
git commit -m "feat(presence): add the session gate, the join log and the bridge kick" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 9: The leader-only `Loop`

**Files:**
- Create: `internal/presence/loop.go`
- Test: `internal/presence/loop_test.go`, `internal/presence/loop_live_test.go` (build tag `livedb`)

**Interfaces:**
- Consumes: `Service` and its unexported `reg`, `store` and `now` (Task 6), `Evaluate` and `Decision` (Task 4), `Gate` and `JoinLog` (Task 8), the metrics recorders (Task 7). The console is satisfied in production by `*adapters.BridgeClient`: `OnlinePlayers` comes from plan 1 Task 1 (`func (c *BridgeClient) OnlinePlayers(ctx context.Context) ([]string, error)`), and `Kick` from Task 8.
- Produces:
  - `const TickInterval = 10 * time.Second`, `const KickGrace = 20 * time.Second`
  - `type Console interface { OnlinePlayers(ctx context.Context) ([]string, error); Kick(ctx context.Context, gamertag string) error }`
  - `type LoopConfig struct { Service *Service; Console Console; Joins *JoinLog; Gate *Gate; SessionUp func() bool; Version string; Log *logging.Logger }`
  - `func NewLoop(cfg LoopConfig) *Loop`, `(*Loop).Prime(ctx)`, `(*Loop).Run(ctx)`, `(*Loop).Nudge()`

The loop runs one pass per tick:
1. Read the overrides. On failure, skip the tick: the gate keeps its last answer, and no kick is sent.
2. Evaluate with the recent joins.
3. Remove what ended, each by the version it read.
4. Set the gate for this process's own actor.
5. Report this process's own status.
6. Export the metrics.
7. Kick every actor that has been parked for at least `KickGrace` and still appears in the bridge's `list`.

The grace is measured from the override's `set_at`, which is stored in Postgres, so a new leader continues it rather than restarting it. An actor that is parked by default, with no override, is eligible at once.

- [ ] **Step 1: Write the failing tests**

`internal/presence/loop_test.go`:
```go
package presence

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

type fakeConsole struct {
	mu     sync.Mutex
	online []string
	err    error
	lists  int
	kicked []string
}

func (c *fakeConsole) OnlinePlayers(context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	return append([]string(nil), c.online...), c.err
}

func (c *fakeConsole) Kick(_ context.Context, gamertag string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kicked = append(c.kicked, gamertag)
	return nil
}

type loopRig struct {
	clock   time.Time
	store   *fakeStore
	svc     *Service
	audit   *fakeAudit
	console *fakeConsole
	joins   *JoinLog
	gate    *Gate
	up      bool
	loop    *Loop
}

func newLoopRig(t *testing.T) *loopRig {
	t.Helper()
	t.Cleanup(metrics.ResetPresence)
	r := &loopRig{clock: t0, store: newFakeStore(), console: &fakeConsole{}, joins: NewJoinLog(), gate: NewGate(true), up: true}
	r.svc, r.audit = newTestService(t, r.store, &r.clock)
	r.joins.now = func() time.Time { return r.clock }
	r.loop = NewLoop(LoopConfig{
		Service: r.svc, Console: r.console, Joins: r.joins, Gate: r.gate,
		SessionUp: func() bool { return r.up }, Version: "test", Log: logging.New("error"),
	})
	return r
}

func (r *loopRig) set(t *testing.T, id string, req Request) {
	t.Helper()
	if _, err := r.svc.Set(t.Context(), id, req, ChatSource("x", "Op", "operator")); err != nil {
		t.Fatalf("Set %s: %v", id, err)
	}
}

func (r *loopRig) present() bool { p, _ := r.gate.Wanted(); return p }

func TestLoopParksAndRestoresItsOwnActor(t *testing.T) {
	r := newLoopRig(t)
	until := t0.Add(time.Hour)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Until: &until, WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true}, Reason: "r"})
	r.loop.tick(t.Context())
	if r.present() {
		t.Fatal("gate present after the agent was parked")
	}
	r.clock = until
	r.loop.tick(t.Context())
	if !r.present() {
		t.Error("gate still parked after the override expired")
	}
	if _, ok := r.store.row("agent"); ok {
		t.Error("expired override still stored")
	}
	if last := r.audit.all()[len(r.audit.all())-1]; !strings.Contains(last.Args, "cause=expired") {
		t.Errorf("last audit = %+v, want the expiry", last)
	}
}

func TestLoopWakesOnAPlayerWhoArrivesAfterThePark(t *testing.T) {
	r := newLoopRig(t)
	r.joins.Record("Steve")
	r.clock = t0.Add(time.Second)
	until := r.clock.Add(time.Hour)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Until: &until, WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true}, Reason: "r"})
	r.loop.tick(t.Context())
	if r.present() {
		t.Fatal("a join from before the park woke the agent")
	}
	r.clock = r.clock.Add(time.Minute)
	r.joins.Record("Alex")
	r.loop.tick(t.Context())
	if !r.present() {
		t.Error("a later join did not wake the agent")
	}
}

func TestLoopDoesNotRemoveANewerOverride(t *testing.T) {
	r := newLoopRig(t)
	until := t0.Add(time.Minute)
	r.set(t, "afk-bot-1", Request{State: presenceapi.StateParked, Until: &until, Reason: "r"})
	r.clock = until
	r.store.bumpOnRemove = true
	r.loop.tick(t.Context())
	if _, ok := r.store.row("afk-bot-1"); !ok {
		t.Error("the loop removed a row that changed after it was read")
	}
}

func TestLoopSkipsATickItCannotRead(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	r.loop.tick(t.Context())
	r.store.fail(errors.New("connection refused"))
	r.console.online = []string{"JdwAgent"}
	r.clock = t0.Add(time.Hour)
	r.loop.tick(t.Context())
	if r.present() {
		t.Error("a failed read changed the gate; it must keep its last answer")
	}
	if len(r.console.kicked) != 0 {
		t.Errorf("kicked %v on a tick that could not read the overrides", r.console.kicked)
	}
}

func TestLoopKicksAParkedActorStillOnTheServerAfterTheGrace(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "afk-bot-1", Request{State: presenceapi.StateParked, Reason: "r"})
	r.console.online = []string{"jdwafk1", "Steve"}

	// afk-bot-2 is parked by default and due at once, so list runs; it is
	// not online, and afk-bot-1 is still inside its grace.
	r.clock = t0.Add(KickGrace - time.Second)
	r.loop.tick(t.Context())
	if r.console.lists != 1 || len(r.console.kicked) != 0 {
		t.Fatalf("inside the grace: lists=%d kicked=%v, want one list and no kick", r.console.lists, r.console.kicked)
	}

	r.clock = t0.Add(KickGrace)
	d := metricstest.Delta(t, func() { r.loop.tick(t.Context()) }, "mc_presence_kicks_total", "actor", "afk-bot-1")
	if d != 1 {
		t.Errorf("kick counter moved by %v, want 1", d)
	}
	if got := r.console.kicked; len(got) != 1 || got[0] != "JdwAfk1" {
		t.Errorf("kicked %v, want JdwAfk1 by its configured name", got)
	}
}

func TestLoopDoesNotAskTheConsoleWhenNobodyIsDue(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "afk-bot-2", Request{State: presenceapi.StatePresent, Reason: "r"})
	r.loop.tick(t.Context())
	if r.console.lists != 0 {
		t.Errorf("ran list %d times with every actor present", r.console.lists)
	}
}

func TestLoopTreatsAStaleReportAsNotConnected(t *testing.T) {
	r := newLoopRig(t)
	r.store.status["afk-bot-1"] = presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0.Add(-statusStale - time.Second)}
	r.store.status["afk-bot-2"] = presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0.Add(-time.Second)}
	r.loop.tick(t.Context())
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "afk-bot-1"); got != 0 {
		t.Errorf("stale bot observed = %v, want 0", got)
	}
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "afk-bot-2"); got != 1 {
		t.Errorf("fresh bot observed = %v, want 1", got)
	}
	if got := metricstest.Value(t, "mc_presence_desired", "actor", "afk-bot-2"); got != 0 {
		t.Errorf("afk-bot-2 desired = %v, want 0 from its parked default", got)
	}
}

func TestLoopReportsItsOwnStatusAndOverrideAge(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "afk-bot-1", Request{State: presenceapi.StateParked, Reason: "r"})
	r.clock = t0.Add(time.Hour)
	r.up = false
	r.loop.tick(t.Context())
	st := r.store.status["agent"]
	if st.Connected || st.ObservedState != presenceapi.StatePresent || st.ProcessVersion != "test" || !st.LastSeen.Equal(r.clock) {
		t.Errorf("own status = %+v", st)
	}
	if got := metricstest.Value(t, "mc_presence_override_age_seconds", "actor", "afk-bot-1"); got != 3600 {
		t.Errorf("override age = %v, want 3600", got)
	}
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "agent"); got != 0 {
		t.Errorf("agent observed = %v, want 0 while its session is down", got)
	}
}

func TestPrimeDecidesTheGateBeforeRunStarts(t *testing.T) {
	r := newLoopRig(t)
	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	r.loop.Prime(t.Context())
	if r.present() {
		t.Error("Prime returned with the gate still present; the agent would join before its first tick")
	}
}

func TestRunActsOnANudgeAndWithdrawsMetricsOnExit(t *testing.T) {
	r := newLoopRig(t)
	r.loop.interval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { r.loop.Run(ctx); close(done) }()

	r.set(t, "agent", Request{State: presenceapi.StateParked, Reason: "r"})
	r.loop.Nudge()
	deadline := time.Now().Add(2 * time.Second)
	for r.present() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.present() {
		t.Fatal("a nudge did not tick the loop")
	}
	cancel()
	<-done
	if metricstest.Exists(t, "mc_presence_desired", "actor", "agent") {
		t.Error("desired still exported after the leader's loop ended")
	}
}

func TestLoopFollowsTheDefaultWithNoDatabase(t *testing.T) {
	reg, _ := NewRegistry([]Actor{{ID: "agent", Gamertag: "A", Kind: KindAgent, Default: presenceapi.StateParked}}, "agent")
	gate := NewGate(true)
	l := NewLoop(LoopConfig{Service: NewService(reg, Nop{}, &fakeAudit{}, logging.New("error")), Console: &fakeConsole{}, Joins: NewJoinLog(), Gate: gate, SessionUp: func() bool { return false }, Log: logging.New("error")})
	l.Prime(t.Context())
	if p, _ := gate.Wanted(); p {
		t.Error("with no database the gate must follow the configured default")
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/presence/ -run 'Loop|Prime|Run'`
Expected: FAIL, build errors `undefined: NewLoop`, `undefined: LoopConfig`, `undefined: KickGrace`, `undefined: statusStale`.

- [ ] **Step 3: Implement the loop**

`internal/presence/loop.go`:
```go
package presence

import (
	"context"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	// TickInterval is how often the leader re-decides every actor.
	TickInterval = 10 * time.Second
	// KickGrace is how long a parked actor may stay on the server before it
	// is kicked. Bedrock keeps a session open for a while after a client
	// leaves, and the chunks around it keep ticking until it goes.
	KickGrace = 20 * time.Second
	// statusStale is how old an actor's last report may be and still count
	// as connected: six bot polls. A bot that crashed stops reporting, and
	// its last row would otherwise say connected forever.
	statusStale = 60 * time.Second
	// tickTimeout keeps one tick inside its interval, so a hung database
	// cannot stack ticks behind it.
	tickTimeout = 8 * time.Second
)

// Console is the part of the server console the loop needs.
type Console interface {
	OnlinePlayers(ctx context.Context) ([]string, error)
	Kick(ctx context.Context, gamertag string) error
}

type LoopConfig struct {
	Service *Service
	Console Console
	Joins   *JoinLog
	Gate    *Gate
	// SessionUp reports whether this process's own session is in the world.
	SessionUp func() bool
	// Version is this process's release, reported in its own status row.
	Version string
	Log     *logging.Logger
}

// Loop is the leader's half of presence: the only thing that removes ended
// overrides, kicks actors that stayed, and exports the presence metrics.
// Everything it acts on is read back from Postgres each tick, so a new
// leader carries on where the last one stopped.
type Loop struct {
	svc      *Service
	console  Console
	joins    *JoinLog
	gate     *Gate
	up       func() bool
	version  string
	log      *logging.Logger
	interval time.Duration
	nudge    chan struct{}
}

func NewLoop(cfg LoopConfig) *Loop {
	return &Loop{
		svc: cfg.Service, console: cfg.Console, joins: cfg.Joins, gate: cfg.Gate,
		up: cfg.SessionUp, version: cfg.Version, log: cfg.Log,
		interval: TickInterval, nudge: make(chan struct{}, 1),
	}
}

// Nudge asks for a tick now rather than at the next interval. Never blocks:
// one pending nudge covers any number of writes.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

// Prime runs one tick before this leader joins the world, so its own gate
// already holds the stored answer when the session lifecycle first reads
// it. Without it a leader whose actor is parked would join and be pulled
// straight back out.
func (l *Loop) Prime(ctx context.Context) { l.tick(ctx) }

// Run ticks until ctx ends, which is when this process stops leading. Its
// gauges go with it, so a standby never exports a stale view beside the new
// leader's.
func (l *Loop) Run(ctx context.Context) {
	ids := make([]string, 0, len(l.svc.reg.actors))
	for _, a := range l.svc.reg.Actors() {
		ids = append(ids, a.ID)
	}
	metrics.InitPresence(ids)
	defer metrics.ResetPresence()

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-l.nudge:
		}
		l.tick(ctx)
	}
}

func (l *Loop) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, tickTimeout)
	defer cancel()
	reg, store := l.svc.reg, l.svc.store
	self, _ := reg.Actor(reg.SelfID())
	now := l.svc.now()

	if !store.Enabled() {
		// No database, so no overrides: git's default is the whole answer.
		l.gate.Set(self.Default == presenceapi.StatePresent)
		return
	}
	overrides, err := store.Overrides(ctx)
	if err != nil {
		// Skipped rather than guessed: the gate keeps its last answer, so a
		// database blink neither pulls the agent out nor puts it back.
		l.log.Warn("presence_tick_skipped", logging.Fields{"error": err.Error()})
		return
	}

	d := Evaluate(reg.Actors(), overrides, l.joins.Recent(), now)
	for _, r := range d.Remove {
		removed, err := l.svc.Expire(ctx, r)
		if err != nil {
			l.log.Error("presence_expire_failed", logging.Fields{"actor": r.ActorID, "cause": string(r.Cause), "error": err.Error()})
			continue
		}
		if removed {
			delete(overrides, r.ActorID)
		}
	}

	l.gate.Set(d.Effective[self.ID] == presenceapi.StatePresent)
	l.reportSelf(ctx, self, d)
	l.export(ctx, overrides, d, now)
	l.kick(ctx, overrides, d, now)
}

// reportSelf writes this process's own status row, which the bots write
// through the API. Observed state is what it is trying to do, connected
// whether it has managed to.
func (l *Loop) reportSelf(ctx context.Context, self Actor, d Decision) {
	st := presenceapi.Status{Connected: l.up(), ObservedState: d.Effective[self.ID], ProcessVersion: l.version}
	if err := l.svc.ReportStatus(ctx, self.ID, st); err != nil {
		l.log.Warn("presence_self_status_failed", logging.Fields{"error": err.Error()})
	}
}

func (l *Loop) export(ctx context.Context, overrides map[string]presenceapi.Override, d Decision, now time.Time) {
	statuses, err := l.svc.store.Statuses(ctx)
	if err != nil {
		// Every actor then reads as not connected, which is what the loop
		// can actually vouch for.
		l.log.Warn("presence_status_read_failed", logging.Fields{"error": err.Error()})
	}
	selfID := l.svc.reg.SelfID()
	for _, a := range l.svc.reg.Actors() {
		metrics.PresenceDesired(a.ID, d.Effective[a.ID] == presenceapi.StatePresent)
		var connected bool
		if a.ID == selfID {
			connected = l.up()
		} else if st, ok := statuses[a.ID]; ok {
			connected = st.Connected && now.Sub(st.LastSeen) <= statusStale
		}
		metrics.PresenceObserved(a.ID, connected)
		ov, ok := overrides[a.ID]
		metrics.PresenceOverrideAge(a.ID, now.Sub(ov.SetAt), ok && ov.Until == nil)
	}
}

// kick removes every actor that has been parked for KickGrace and still
// appears in the server's own list. The console is asked only when someone
// is due, since every list is a console command and a line in the server
// log.
func (l *Loop) kick(ctx context.Context, overrides map[string]presenceapi.Override, d Decision, now time.Time) {
	var due []Actor
	for _, a := range l.svc.reg.Actors() {
		if d.Effective[a.ID] != presenceapi.StateParked {
			continue
		}
		// Parked by its default with no override: parked since before this
		// process could see it, so the grace has long passed.
		var since time.Time
		if ov, ok := overrides[a.ID]; ok {
			since = ov.SetAt
		}
		if now.Sub(since) >= KickGrace {
			due = append(due, a)
		}
	}
	if len(due) == 0 {
		return
	}
	online, err := l.console.OnlinePlayers(ctx)
	if err != nil {
		l.log.Warn("presence_list_failed", logging.Fields{"error": err.Error()})
		return
	}
	for _, a := range due {
		if !containsFold(online, a.Gamertag) {
			continue
		}
		if err := l.console.Kick(ctx, a.Gamertag); err != nil {
			l.log.Warn("presence_kick_failed", logging.Fields{"actor": a.ID, "error": err.Error()})
			continue
		}
		metrics.PresenceKick(a.ID)
		l.log.Info("presence_kicked", logging.Fields{"actor": a.ID})
	}
}

func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Write the leader-handover test**

`internal/presence/loop_live_test.go`:
```go
//go:build livedb

package presence

import (
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// One leader parks the agent for a while and a bot indefinitely, then goes
// away. A second process, sharing nothing with the first but the database,
// takes over after the expiry: it must remove what expired, keep what did
// not, and decide its own gate from the stored rows alone.
func TestOverridesAndExpiriesSurviveALeaderHandover(t *testing.T) {
	ctx := t.Context()
	actors := []Actor{
		{ID: "zz-test-agent", Gamertag: "ZzAgent", Kind: KindAgent, Default: presenceapi.StatePresent},
		{ID: "zz-test-a", Gamertag: "ZzBot", Kind: KindAFKBot, Default: presenceapi.StatePresent},
	}
	reg, err := NewRegistry(actors, "zz-test-agent")
	if err != nil {
		t.Fatal(err)
	}
	quiet := logging.New("error")

	first := NewService(reg, NewPostgres(livePool(t)), &fakeAudit{}, quiet)
	until := time.Now().Add(time.Minute)
	if _, err := first.Set(ctx, "zz-test-agent", Request{State: presenceapi.StateParked, Until: &until, Reason: "handover"}, APISource("test")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Set(ctx, "zz-test-a", Request{State: presenceapi.StateParked, Reason: "handover"}, APISource("test")); err != nil {
		t.Fatal(err)
	}

	secondStore := NewPostgres(livePool(t))
	second := NewService(reg, secondStore, &fakeAudit{}, quiet)
	second.now = func() time.Time { return until.Add(time.Second) }
	gate := NewGate(false)
	NewLoop(LoopConfig{Service: second, Console: &fakeConsole{}, Joins: NewJoinLog(), Gate: gate, SessionUp: func() bool { return false }, Log: quiet}).Prime(ctx)

	if p, _ := gate.Wanted(); !p {
		t.Error("the new leader kept the agent parked past its stored expiry")
	}
	rows, err := secondStore.Overrides(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows["zz-test-agent"]; ok {
		t.Error("the expired agent override survived the handover")
	}
	if rows["zz-test-a"].State != presenceapi.StateParked {
		t.Error("the open-ended bot override was lost in the handover")
	}
}
```

`livePool` is called twice here, so it runs its cleanup twice. That is harmless because the cleanup is idempotent. Each call gives the process its own pool, as two pods would have.

- [ ] **Step 5: Run the tests to see them pass**

Run: `gofmt -w internal/presence && gofmt -l internal/presence && go vet -tags livedb ./internal/presence/ && go test -race ./internal/presence/ -run 'Loop|Prime|Run' -v`
Expected: `gofmt -l` and `go vet` print nothing, and each `TestLoop*`, `TestPrime*` and `TestRun*` test passes. With `MC_TEST_DSN` set, `go test -tags livedb -race ./internal/presence/ -run Handover -v` passes too.

- [ ] **Step 6: Commit**

```bash
git add internal/presence/loop.go internal/presence/loop_test.go internal/presence/loop_live_test.go
git commit -m "feat(presence): run the leader's policy loop with expiry, wake and guaranteed unload" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 10: The `/v1` HTTP API

**Files:**
- Create: `internal/presence/api.go`, `internal/httpapi/presence.go`
- Test: `internal/presence/api_test.go`, `internal/httpapi/presence_test.go`

**Interfaces:**
- Consumes: `Service` and its errors and `ConflictError`, plus `APISource` (Task 6). `presenceapi.*` including the `Code*` constants (Task 1). The auth pattern of `internal/httpapi/announce.go:96-176`: sha256 digests compared with `subtle.ConstantTimeCompare`, a case-insensitive `Bearer ` scheme and `WWW-Authenticate: Bearer` on 401. The mount pattern of `MountAnnouncements` (`announce.go:51-67`).
- Produces:
  - `const ScopeRead = "presence:read"`, `ScopeWrite = "presence:write"`, `ScopeReport = "presence:report"`
  - `type Token struct { Name, Secret string; Scopes []string; Actor string }`
  - `func NewAPI(svc *Service, tokens []Token, log *logging.Logger) *API`, `(*API).Enabled() bool`, `(*API).ServeHTTP`
  - `func ETag(p presenceapi.Presence) string`
  - `httpapi`: `type PresenceAPI interface { http.Handler; Enabled() bool }`, `func (s *Server) MountPresence(api PresenceAPI) bool`

A token bound to an actor (every bot token) speaks only for that actor. Its status reports, its single-actor writes and all of its group writes for any other actor are refused with 403. A write sets `set_by` to `api:<token-name>` (see Deviations).

- [ ] **Step 1: Write the failing tests**

`internal/presence/api_test.go`:
```go
package presence

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	opsSecret    = "bbbbbbbbbbbbbbbb"
	readSecret   = "cccccccccccccccc"
	botSecret    = "dddddddddddddddd"
	writerSecret = "eeeeeeeeeeeeeeee"
)

type apiRig struct {
	clock time.Time
	store *fakeStore
	api   *API
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	r := &apiRig{clock: t0, store: newFakeStore()}
	svc, _ := newTestService(t, r.store, &r.clock)
	r.api = NewAPI(svc, []Token{
		{Name: "ops", Secret: opsSecret, Scopes: []string{ScopeRead, ScopeWrite}},
		{Name: "reader", Secret: readSecret, Scopes: []string{ScopeRead}},
		{Name: "afk-bot-1", Secret: botSecret, Scopes: []string{ScopeRead, ScopeReport}, Actor: "afk-bot-1"},
		{Name: "writer-1", Secret: writerSecret, Scopes: []string{ScopeWrite}, Actor: "afk-bot-1"},
	}, logging.New("error"))
	r.api.now = func() time.Time { return r.clock }
	return r
}

func (r *apiRig) do(method, path, secret, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	r.api.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) presenceapi.Error {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), status)
	}
	e := decodeBody[presenceapi.Error](t, rec)
	if e.Code != code {
		t.Errorf("code = %q, want %q", e.Code, code)
	}
	return e
}

const parkBody = `{"state":"parked","reason":"chunk budget","version":0}`

func TestAPIRequiresAKnownToken(t *testing.T) {
	r := newAPIRig(t)
	for _, secret := range []string{"", "not-a-token-at-all"} {
		rec := r.do(http.MethodGet, "/v1/actors", secret, "")
		wantError(t, rec, http.StatusUnauthorized, presenceapi.CodeUnauthorized)
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Error("401 without WWW-Authenticate: Bearer")
		}
	}
}

func TestAPIEnforcesScopes(t *testing.T) {
	r := newAPIRig(t)
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", readSecret, parkBody), http.StatusForbidden, presenceapi.CodeForbidden)
	wantError(t, r.do(http.MethodPost, "/v1/actors/afk-bot-1/status", opsSecret, `{"connected":true,"observed_state":"present"}`), http.StatusForbidden, presenceapi.CodeForbidden)
	if _, ok := r.store.row("afk-bot-1"); ok {
		t.Error("a refused write was stored")
	}
}

func TestAPIPresenceETag(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodGet, "/v1/actors/afk-bot-1/presence", botSecret, "")
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"0-present"` {
		t.Fatalf("GET = %d with ETag %q, want 200 and \"0-present\"", rec.Code, rec.Header().Get("ETag"))
	}
	again := r.do(http.MethodGet, "/v1/actors/afk-bot-1/presence", botSecret, "", "If-None-Match", `"0-present"`)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("matching If-None-Match = %d with %d body bytes, want an empty 304", again.Code, again.Body.Len())
	}
	if rec := r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	changed := r.do(http.MethodGet, "/v1/actors/afk-bot-1/presence", botSecret, "", "If-None-Match", `"0-present"`)
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") != `"1-parked"` {
		t.Errorf("after a park = %d with ETag %q, want 200 and \"1-parked\"", changed.Code, changed.Header().Get("ETag"))
	}
	if p := decodeBody[presenceapi.Presence](t, changed); p.Effective != presenceapi.StateParked || p.Override.SetBy != "api:ops" {
		t.Errorf("body = %+v", p)
	}
}

func TestAPIConflictCarriesTheCurrentRow(t *testing.T) {
	r := newAPIRig(t)
	r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody)
	e := wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody), http.StatusConflict, presenceapi.CodeConflict)
	if e.Current == nil || e.Current.Override == nil || e.Current.Override.Version != 1 {
		t.Errorf("current = %+v, want the version 1 row", e.Current)
	}
}

func TestAPIRefusesMalformedRequests(t *testing.T) {
	r := newAPIRig(t)
	for name, body := range map[string]string{
		"not JSON":          `{`,
		"unknown field":     `{"state":"parked","reason":"r","version":0,"expires":"2h"}`,
		"trailing data":     parkBody + `{}`,
		"until and duration": `{"state":"parked","reason":"r","version":0,"until":"2026-09-23T20:00:00Z","duration":"2h"}`,
		"bad duration":      `{"state":"parked","reason":"r","version":0,"duration":"soon"}`,
		"negative duration": `{"state":"parked","reason":"r","version":0,"duration":"-1h"}`,
		"no reason":         `{"state":"parked","version":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, body), http.StatusBadRequest, presenceapi.CodeInvalid)
		})
	}
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-9/presence", opsSecret, parkBody), http.StatusNotFound, presenceapi.CodeNotFound)
	wantError(t, r.do(http.MethodGet, "/v1/actors/afk-bot-9/presence", opsSecret, ""), http.StatusNotFound, presenceapi.CodeNotFound)
}

func TestAPIDurationBecomesAnExpiry(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, `{"state":"parked","duration":"2h","reason":"r","version":0}`)
	p := decodeBody[presenceapi.Presence](t, rec)
	if p.Override == nil || p.Override.Until == nil || !p.Override.Until.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("override = %+v, want until two hours on", p.Override)
	}
}

func TestAPIDeleteReturnsTheDefault(t *testing.T) {
	r := newAPIRig(t)
	r.do(http.MethodPut, "/v1/actors/afk-bot-2/presence", opsSecret, `{"state":"present","reason":"r","version":0}`)
	rec := r.do(http.MethodDelete, "/v1/actors/afk-bot-2/presence", opsSecret, "")
	if p := decodeBody[presenceapi.Presence](t, rec); rec.Code != http.StatusOK || p.Effective != presenceapi.StateParked || p.Override != nil {
		t.Errorf("DELETE = %d %+v, want 200 and the parked default", rec.Code, p)
	}
}

func TestAPIGroupWrite(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodPut, "/v1/groups/bots/presence", opsSecret, `{"state":"parked","reason":"r","version":42}`)
	if ps := decodeBody[[]presenceapi.Presence](t, rec); rec.Code != http.StatusOK || len(ps) != 2 {
		t.Errorf("group PUT = %d %+v, want both bots", rec.Code, ps)
	}
	wantError(t, r.do(http.MethodPut, "/v1/groups/miners/presence", opsSecret, parkBody), http.StatusNotFound, presenceapi.CodeNotFound)
}

func TestBoundTokenCannotSpeakForAnotherActor(t *testing.T) {
	r := newAPIRig(t)
	status := `{"connected":true,"observed_state":"present","last_seen":"2026-09-23T18:00:00Z","process_version":"1.4.0"}`
	if rec := r.do(http.MethodPost, "/v1/actors/afk-bot-1/status", botSecret, status); rec.Code != http.StatusNoContent {
		t.Fatalf("own status = %d: %s", rec.Code, rec.Body.String())
	}
	wantError(t, r.do(http.MethodPost, "/v1/actors/afk-bot-2/status", botSecret, status), http.StatusForbidden, presenceapi.CodeForbidden)
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-2/presence", writerSecret, parkBody), http.StatusForbidden, presenceapi.CodeForbidden)
	wantError(t, r.do(http.MethodPut, "/v1/groups/bots/presence", writerSecret, parkBody), http.StatusForbidden, presenceapi.CodeForbidden)
	if _, ok := r.store.row("afk-bot-2"); ok {
		t.Error("a forbidden write was stored")
	}
	if rec := r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", writerSecret, parkBody); rec.Code != http.StatusOK {
		t.Errorf("bound token writing its own actor = %d", rec.Code)
	}
}

func TestAPIStoreOutageIsUnavailable(t *testing.T) {
	r := newAPIRig(t)
	r.store.fail(errors.New("connection refused"))
	wantError(t, r.do(http.MethodGet, "/v1/actors", opsSecret, ""), http.StatusServiceUnavailable, presenceapi.CodeUnavailable)
	wantError(t, r.do(http.MethodPut, "/v1/actors/afk-bot-1/presence", opsSecret, parkBody), http.StatusServiceUnavailable, presenceapi.CodeUnavailable)
}

// The bot and CLI decode groups as a list; null would be a different answer.
func TestAPIListEncodesEmptyGroupsAsAList(t *testing.T) {
	r := newAPIRig(t)
	rec := r.do(http.MethodGet, "/v1/actors", readSecret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"groups":[]`) {
		t.Errorf("GET /v1/actors = %d %s, want the agent's groups as []", rec.Code, rec.Body.String())
	}
}
```

`internal/httpapi/presence_test.go`:
```go
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakePresenceAPI struct{ enabled bool }

func (f fakePresenceAPI) Enabled() bool { return f.enabled }
func (f fakePresenceAPI) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTeapot)
}

func TestMountPresence(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		srv, err := New("127.0.0.1:0")
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		mounted := srv.MountPresence(fakePresenceAPI{enabled: enabled})
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/actors", nil))
		srv.ln.Close()
		if mounted != enabled {
			t.Errorf("enabled=%v: MountPresence reported %v", enabled, mounted)
		}
		want := http.StatusNotFound
		if enabled {
			want = http.StatusTeapot
		}
		if rec.Code != want {
			t.Errorf("enabled=%v: /v1/actors answered %d, want %d", enabled, rec.Code, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/presence/ -run API ; go test ./internal/httpapi/ -run MountPresence`
Expected: FAIL, build errors `undefined: NewAPI`, `undefined: Token`, `undefined: ScopeRead`, `srv.MountPresence undefined`.

- [ ] **Step 3: Implement the API**

`internal/presence/api.go`:
```go
package presence

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	ScopeRead   = "presence:read"
	ScopeWrite  = "presence:write"
	ScopeReport = "presence:report"
)

const (
	// maxPresenceRequest caps a request body. A valid one is a few short
	// fields and a reason of at most maxReasonChars.
	maxPresenceRequest = 8 << 10
	// presenceRequestTimeout keeps each request inside the server's
	// WriteTimeout, so a slow database produces a 503 rather than a dropped
	// connection.
	presenceRequestTimeout = 5 * time.Second
)

// Token is one PRESENCE_TOKENS entry.
type Token struct {
	Name   string
	Secret string
	Scopes []string
	// Actor, when set, is the only actor this token may speak for.
	Actor string
}

type credential struct {
	name   string
	digest [sha256.Size]byte
	scopes map[string]bool
	actor  string
}

// mayActFor reports whether the token may speak for actor id.
func (c credential) mayActFor(id string) bool { return c.actor == "" || c.actor == id }

// API serves the /v1 presence routes. It answers from Postgres on any
// replica, so a standby serves it as well as the leader.
type API struct {
	svc   *Service
	creds []credential
	log   *logging.Logger
	mux   *http.ServeMux
	now   func() time.Time
}

func NewAPI(svc *Service, tokens []Token, log *logging.Logger) *API {
	a := &API{svc: svc, log: log, now: time.Now}
	for _, t := range tokens {
		c := credential{name: t.Name, digest: sha256.Sum256([]byte(t.Secret)), scopes: make(map[string]bool, len(t.Scopes)), actor: t.Actor}
		for _, s := range t.Scopes {
			c.scopes[s] = true
		}
		a.creds = append(a.creds, c)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/actors", a.guard(ScopeRead, a.list))
	mux.HandleFunc("GET /v1/actors/{id}/presence", a.guard(ScopeRead, a.get))
	mux.HandleFunc("PUT /v1/actors/{id}/presence", a.guard(ScopeWrite, a.put))
	mux.HandleFunc("DELETE /v1/actors/{id}/presence", a.guard(ScopeWrite, a.del))
	mux.HandleFunc("PUT /v1/groups/{group}/presence", a.guard(ScopeWrite, a.putGroup))
	mux.HandleFunc("POST /v1/actors/{id}/status", a.guard(ScopeReport, a.status))
	a.mux = mux
	return a
}

// Enabled reports whether there is anything to serve: at least one token,
// and at least one actor for it to act on.
func (a *API) Enabled() bool { return len(a.creds) > 0 && a.svc.Enabled() }

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

type handler func(w http.ResponseWriter, r *http.Request, c credential)

// guard authenticates before anything else is read, so an unauthenticated
// caller learns nothing about what a valid request looks like.
func (a *API) guard(scope string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.authenticate(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, presenceapi.Error{Code: presenceapi.CodeUnauthorized, Message: "a valid bearer token is required"})
			return
		}
		if !c.scopes[scope] {
			writeError(w, http.StatusForbidden, presenceapi.Error{Code: presenceapi.CodeForbidden, Message: "this token lacks " + scope})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), presenceRequestTimeout)
		defer cancel()
		h(w, r.WithContext(ctx), c)
	}
}

// authenticate compares the presented token against every configured one,
// as digests over equal lengths, and never stops at the first match: the
// time taken says nothing about which token matched or how long any is.
func (a *API) authenticate(header string) (credential, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return credential{}, false
	}
	got := sha256.Sum256([]byte(header[len(scheme):]))
	var found credential
	ok := false
	for _, c := range a.creds {
		if subtle.ConstantTimeCompare(got[:], c.digest[:]) == 1 {
			found, ok = c, true
		}
	}
	return found, ok
}

func (a *API) list(w http.ResponseWriter, r *http.Request, _ credential) {
	views, err := a.svc.List(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *API) get(w http.ResponseWriter, r *http.Request, _ credential) {
	p, err := a.svc.Presence(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	tag := ETag(p)
	w.Header().Set("ETag", tag)
	if etagMatches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) put(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("id")
	if !c.mayActFor(id) {
		forbidden(w, "this token may only change "+c.actor)
		return
	}
	req, err := a.decodeSet(w, r)
	if err != nil {
		a.fail(w, err)
		return
	}
	p, err := a.svc.Set(r.Context(), id, req, APISource(c.name))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *API) del(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("id")
	if !c.mayActFor(id) {
		forbidden(w, "this token may only change "+c.actor)
		return
	}
	actor, ok := a.svc.Registry().Actor(id)
	if !ok {
		a.fail(w, ErrNotFound)
		return
	}
	ps, err := a.svc.Clear(r.Context(), []Actor{actor}, APISource(c.name))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps[0])
}

func (a *API) putGroup(w http.ResponseWriter, r *http.Request, c credential) {
	if c.actor != "" {
		forbidden(w, "a token bound to one actor cannot change a group")
		return
	}
	req, err := a.decodeSet(w, r)
	if err != nil {
		a.fail(w, err)
		return
	}
	ps, err := a.svc.SetGroup(r.Context(), r.PathValue("group"), req, APISource(c.name))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (a *API) status(w http.ResponseWriter, r *http.Request, c credential) {
	id := r.PathValue("id")
	if !c.mayActFor(id) {
		forbidden(w, "this token reports only for "+c.actor)
		return
	}
	var st presenceapi.Status
	if err := decodeStrict(w, r, &st); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.svc.ReportStatus(r.Context(), id, st); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeSet reads a SetRequest and turns a duration into an expiry. The
// duration is the CLI's natural input and is resolved here, on the agent's
// clock, so the CLI's clock never decides when a park ends.
func (a *API) decodeSet(w http.ResponseWriter, r *http.Request) (Request, error) {
	var body presenceapi.SetRequest
	if err := decodeStrict(w, r, &body); err != nil {
		return Request{}, err
	}
	req := Request{State: body.State, Until: body.Until, WakeOn: body.WakeOn, Reason: body.Reason, Version: body.Version}
	if body.Duration != "" {
		if body.Until != nil {
			return Request{}, invalid("until and duration are exclusive")
		}
		d, err := time.ParseDuration(body.Duration)
		if err != nil || d <= 0 {
			return Request{}, invalid("duration must be a positive Go duration such as 30m or 2h")
		}
		until := a.now().Add(d)
		req.Until = &until
	}
	return req, nil
}

// decodeStrict reads exactly one JSON object of bounded size. Unknown fields
// are refused: a caller who wrote "expires" meant something by it.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPresenceRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("invalid JSON: " + err.Error())
	}
	if dec.More() {
		return invalid("trailing data after the JSON object")
	}
	return nil
}

// ETag is the validator GET /v1/actors/{id}/presence serves: the override's
// version (0 without one) and the effective state. A bot polling with it
// gets a 304 until either changes.
func ETag(p presenceapi.Presence) string {
	var v int64
	if p.Override != nil {
		v = p.Override.Version
	}
	return `"` + strconv.FormatInt(v, 10) + "-" + string(p.Effective) + `"`
}

func etagMatches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		c := strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if c == tag || c == "*" {
			return true
		}
	}
	return false
}

func (a *API) fail(w http.ResponseWriter, err error) {
	var conflict *ConflictError
	switch {
	case errors.As(err, &conflict):
		current := conflict.Current
		writeError(w, http.StatusConflict, presenceapi.Error{Code: presenceapi.CodeConflict, Message: err.Error(), Current: &current})
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, presenceapi.Error{Code: presenceapi.CodeNotFound, Message: "no such actor or group"})
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, presenceapi.Error{Code: presenceapi.CodeInvalid, Message: strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")})
	default:
		// ErrUnavailable, and anything the service did not classify: either
		// way the store could not answer, and a retry is the caller's move.
		a.log.Error("presence_api_unavailable", logging.Fields{"error": err.Error()})
		writeError(w, http.StatusServiceUnavailable, presenceapi.Error{Code: presenceapi.CodeUnavailable, Message: "the presence store is unavailable; retry"})
	}
}

func forbidden(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusForbidden, presenceapi.Error{Code: presenceapi.CodeForbidden, Message: msg})
}

func writeError(w http.ResponseWriter, status int, e presenceapi.Error) { writeJSON(w, status, e) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

`internal/httpapi/presence.go`:
```go
package httpapi

import "net/http"

// PresenceAPI is the actor presence API: the routes under /v1, and whether
// it has anything to serve.
type PresenceAPI interface {
	http.Handler
	Enabled() bool
}

// MountPresence serves api under /v1/ when it is enabled, and reports
// whether it did. On the same terms as MountAnnouncements: with no tokens
// nothing is mounted, and the paths answer the mux's own 404.
//
// Mounted for the process rather than for a turn as the live agent: every
// route reads and writes Postgres, which a standby reaches as well as the
// leader does.
func (s *Server) MountPresence(api PresenceAPI) bool {
	if api == nil || !api.Enabled() {
		return false
	}
	s.mux.Handle("/v1/", api)
	return true
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `gofmt -w internal/presence internal/httpapi && gofmt -l internal/presence internal/httpapi && go vet ./internal/presence/ ./internal/httpapi/ && go test -race ./internal/presence/ ./internal/httpapi/`
Expected: `gofmt -l` and `go vet` print nothing; both packages print `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/presence/api.go internal/presence/api_test.go internal/httpapi/presence.go internal/httpapi/presence_test.go
git commit -m "feat(presence): serve the /v1 presence API with scoped tokens, ETags and conflicts" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 11: `ChatPlugin`: `!presence`, `!park`, `!unpark`, `!leave` and `@server leave`

**Files:**
- Create: `internal/presence/chat.go`
- Test: `internal/presence/chat_test.go`

**Interfaces:**
- Consumes: `plugin.Plugin`, `plugin.EventHandler`, `plugin.Command`, `plugin.Invocation`, `plugin.Permission*` (`internal/plugin/plugin.go:40-195`). `roster.JoinKind` and `roster.JoinEvent` (a value type, as `internal/plugins/welcome.go:89` asserts it). `chat.ServerOrigin` and `chat.MentionToken` (`internal/chat/parse.go:16-23`). `Service.List`, `SetEach`, `Clear`, `Registry` (Task 6), `ChatPark` (Task 4), `JoinLog.Record` (Task 8).
- Produces:
  - `type NameResolver interface { NameFor(xuid string) (string, bool) }`, which `*roster.Roster` satisfies (`roster.go:311`)
  - `func NewChatPlugin(svc *Service, names NameResolver, joins *JoinLog) *ChatPlugin` (plugin name `presence`, commands `presence` (member), `park`, `unpark` and `leave` (operator))
  - `func LeaveArgs(message string) ([]string, bool)`

The plugin is registered whether or not actors are configured. Without them each command answers that presence control is not set up. The registry and the command metrics therefore do not change shape between deployments.

- [ ] **Step 1: Write the failing test**

`internal/presence/chat_test.go`:
```go
package presence

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

type names map[string]string

func (n names) NameFor(xuid string) (string, bool) { v, ok := n[xuid]; return v, ok }

type chatRig struct {
	clock  time.Time
	store  *fakeStore
	audit  *fakeAudit
	joins  *JoinLog
	plugin *ChatPlugin
}

func newChatRig(t *testing.T) *chatRig {
	t.Helper()
	r := &chatRig{clock: t0, store: newFakeStore(), joins: NewJoinLog()}
	var svc *Service
	svc, r.audit = newTestService(t, r.store, &r.clock)
	r.plugin = NewChatPlugin(svc, names{"2535400000000001": "Jdwillmsen"}, r.joins)
	r.plugin.now = func() time.Time { return r.clock }
	return r
}

func (r *chatRig) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	for _, c := range r.plugin.Commands() {
		if c.Name == name {
			reply, err := c.Run(t.Context(), &plugin.Context{}, plugin.Invocation{ActorXUID: "2535400000000001", ActorPermission: plugin.PermissionOperator, Args: args})
			if err != nil {
				t.Fatalf("!%s %v: %v", name, args, err)
			}
			return reply
		}
	}
	t.Fatalf("no command %q", name)
	return ""
}

func TestChatCommandPermissions(t *testing.T) {
	r := newChatRig(t)
	want := map[string]plugin.Permission{"presence": plugin.PermissionMember, "park": plugin.PermissionOperator, "unpark": plugin.PermissionOperator, "leave": plugin.PermissionOperator}
	reg := plugin.NewRegistry()
	if err := reg.Register(r.plugin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for name, perm := range want {
		cmd, ok := reg.Lookup(name)
		if !ok || cmd.Permission != perm {
			t.Errorf("!%s registered = %v at %v, want %v", name, ok, cmd.Permission, perm)
		}
	}
	_, err := reg.Dispatch(t.Context(), &plugin.Context{}, "park", plugin.Invocation{ActorXUID: "x", ActorPermission: plugin.PermissionMember, Args: []string{"bots"}})
	if !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Errorf("a member's !park = %v, want ErrPermissionDenied", err)
	}
}

func TestParkBotsForADuration(t *testing.T) {
	r := newChatRig(t)
	reply := r.run(t, "park", "bots", "2h")
	for _, id := range []string{"afk-bot-1", "afk-bot-2"} {
		ov, ok := r.store.row(id)
		if !ok || ov.State != presenceapi.StateParked || ov.Until == nil || !ov.Until.Equal(t0.Add(2*time.Hour)) || ov.WakeOn != nil {
			t.Errorf("%s = %+v, %v; want parked for two hours with no wake", id, ov, ok)
		}
		if ov.SetBy != "chat:Jdwillmsen" {
			t.Errorf("%s set_by = %q", id, ov.SetBy)
		}
	}
	if !strings.Contains(reply, "afk-bot-1 parked") || !strings.Contains(reply, "20:00 UTC") {
		t.Errorf("reply = %q", reply)
	}
}

func TestParkAllGivesTheAgentAWayBack(t *testing.T) {
	r := newChatRig(t)
	r.run(t, "park", "all")
	agent, _ := r.store.row("agent")
	if agent.Until == nil || !agent.Until.Equal(t0.Add(AgentParkWindow)) || agent.WakeOn == nil || !agent.WakeOn.AnyPlayerJoin {
		t.Errorf("agent = %+v, want an hour and a wake on any join", agent)
	}
	bot, _ := r.store.row("afk-bot-1")
	if bot.Until != nil || bot.WakeOn != nil {
		t.Errorf("bot = %+v, want parked until unparked", bot)
	}
}

func TestLeaveParksTheAgent(t *testing.T) {
	r := newChatRig(t)
	reply := r.run(t, "leave", "30m")
	agent, ok := r.store.row("agent")
	if !ok || !agent.Until.Equal(t0.Add(30*time.Minute)) || !agent.WakeOn.AnyPlayerJoin {
		t.Errorf("agent = %+v, want thirty minutes and a wake", agent)
	}
	if !strings.Contains(reply, "18:30 UTC") || !strings.Contains(reply, "player joins") {
		t.Errorf("reply = %q, want when it comes back and that a join brings it back", reply)
	}
}

func TestUnparkReturnsToTheDefault(t *testing.T) {
	r := newChatRig(t)
	r.run(t, "park", "afk-bot-1")
	reply := r.run(t, "unpark", "afk-bot-1")
	if _, ok := r.store.row("afk-bot-1"); ok {
		t.Error("override still stored after !unpark")
	}
	if !strings.Contains(reply, "afk-bot-1 present (default)") {
		t.Errorf("reply = %q", reply)
	}
}

func TestParkRefusesBadArgumentsWithUsage(t *testing.T) {
	r := newChatRig(t)
	for _, args := range [][]string{{}, {"miners"}, {"bots", "soon"}, {"bots", "-1h"}, {"bots", "1h", "extra"}} {
		reply := r.run(t, "park", args...)
		if !strings.HasPrefix(reply, "Usage: !park") || !strings.Contains(reply, "afk-bot-1, afk-bot-2, bots, all") {
			t.Errorf("!park %v = %q, want the usage line", args, reply)
		}
	}
	if len(r.audit.all()) != 0 {
		t.Error("a refused !park wrote something")
	}
}

func TestPresenceListsEveryActor(t *testing.T) {
	r := newChatRig(t)
	r.run(t, "park", "afk-bot-1", "2h")
	got := r.run(t, "presence")
	want := "agent present (default); afk-bot-1 parked by chat:Jdwillmsen until 20:00 UTC; afk-bot-2 parked (default)"
	if got != want {
		t.Errorf("!presence = %q, want %q", got, want)
	}
}

func TestChatSaysSoWhenTheStoreIsDown(t *testing.T) {
	r := newChatRig(t)
	r.store.fail(errors.New("connection refused"))
	if got := r.run(t, "park", "bots"); !strings.Contains(got, "database") {
		t.Errorf("!park = %q, want it to say the database is not answering", got)
	}
	if got := r.run(t, "presence"); !strings.Contains(got, "database") {
		t.Errorf("!presence = %q", got)
	}
}

func TestChatWithoutActorsSaysItIsNotSetUp(t *testing.T) {
	reg, _ := NewRegistry(nil, "agent")
	p := NewChatPlugin(NewService(reg, Nop{}, &fakeAudit{}, logging.New("error")), names{}, NewJoinLog())
	for _, c := range p.Commands() {
		reply, err := c.Run(t.Context(), &plugin.Context{}, plugin.Invocation{ActorPermission: plugin.PermissionOperator})
		if err != nil || reply != notConfigured {
			t.Errorf("!%s = %q, %v; want %q", c.Name, reply, err, notConfigured)
		}
	}
}

func TestConsoleIsNamedConsole(t *testing.T) {
	r := newChatRig(t)
	for _, c := range r.plugin.Commands() {
		if c.Name == "park" {
			if _, err := c.Run(t.Context(), &plugin.Context{}, plugin.Invocation{ActorXUID: chat.ServerOrigin, ActorPermission: plugin.PermissionOperator, Args: []string{"afk-bot-1"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ov, _ := r.store.row("afk-bot-1"); ov.SetBy != "chat:console" {
		t.Errorf("set_by = %q, want chat:console", ov.SetBy)
	}
}

func TestJoinEventsFeedTheJoinLog(t *testing.T) {
	r := newChatRig(t)
	if !slices.Contains(r.plugin.Kinds(), roster.JoinKind) {
		t.Fatalf("Kinds() = %v, want the roster join", r.plugin.Kinds())
	}
	r.joins.now = func() time.Time { return t0 }
	if err := r.plugin.HandleEvent(t.Context(), &plugin.Context{}, roster.JoinEvent{Entry: roster.Entry{XUID: "1", Username: "Steve"}}); err != nil {
		t.Fatal(err)
	}
	if got := r.joins.Recent(); len(got) != 1 || got[0].Gamertag != "Steve" {
		t.Errorf("join log = %+v", got)
	}
}

func TestLeaveArgs(t *testing.T) {
	cases := []struct {
		message string
		args    []string
		ok      bool
	}{
		{"@server leave", []string{}, true},
		{"@Server LEAVE 30m", []string{"30m"}, true},
		{"  @server   leave  2h ", []string{"2h"}, true},
		{"@server please leave", nil, false},
		{"@server leave now please", nil, false},
		{"can @server leave?", nil, false},
		{"@server where is the leaves farm", nil, false},
	}
	for _, tc := range cases {
		args, ok := LeaveArgs(tc.message)
		if ok != tc.ok || (ok && !slices.Equal(args, tc.args)) {
			t.Errorf("LeaveArgs(%q) = %v, %v; want %v, %v", tc.message, args, ok, tc.args, tc.ok)
		}
	}
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./internal/presence/ -run 'Chat|Park|Leave|Unpark|Presence|Console|Join'`
Expected: FAIL, build errors `undefined: NewChatPlugin`, `undefined: LeaveArgs`, `undefined: notConfigured`.

- [ ] **Step 3: Implement the plugin**

`internal/presence/chat.go`:
```go
package presence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// NameResolver names the player behind an XUID. Satisfied by *roster.Roster.
type NameResolver interface {
	NameFor(xuid string) (string, bool)
}

const (
	notConfigured   = "Presence control isn't set up on this server."
	storeDownRead   = "I can't read presence right now - the database isn't answering."
	storeDownWrite  = "I couldn't save that - the database isn't answering."
	chatParkReason  = "parked from chat"
	chatLeaveReason = "asked to leave from chat"
)

// ChatPlugin is presence as players and operators reach it in chat. It also
// feeds the join log from the session's roster events, which is how a
// player arriving wakes a parked actor while the agent is in the world.
type ChatPlugin struct {
	svc   *Service
	names NameResolver
	joins *JoinLog
	now   func() time.Time
}

var (
	_ plugin.Plugin       = (*ChatPlugin)(nil)
	_ plugin.EventHandler = (*ChatPlugin)(nil)
)

func NewChatPlugin(svc *Service, names NameResolver, joins *JoinLog) *ChatPlugin {
	return &ChatPlugin{svc: svc, names: names, joins: joins, now: time.Now}
}

func (*ChatPlugin) Name() string { return "presence" }

func (p *ChatPlugin) Kinds() []string { return []string{roster.JoinKind} }

func (p *ChatPlugin) HandleEvent(_ context.Context, _ *plugin.Context, ev bus.Event) error {
	if j, ok := ev.(roster.JoinEvent); ok {
		p.joins.Record(j.Username)
	}
	return nil
}

func (p *ChatPlugin) Commands() []plugin.Command {
	return []plugin.Command{
		{Name: "presence", Description: "Show which actors are in the world, and why.", Permission: plugin.PermissionMember, Run: p.status},
		{Name: "park", Description: "Take an actor, group or all out of the world: !park <target> [duration].", Permission: plugin.PermissionOperator, Run: p.park},
		{Name: "unpark", Description: "Return an actor, group or all to its default: !unpark <target>.", Permission: plugin.PermissionOperator, Run: p.unpark},
		{Name: "leave", Description: "Send me out of the world: !leave [duration], or @server leave.", Permission: plugin.PermissionOperator, Run: p.leave},
	}
}

func (p *ChatPlugin) status(ctx context.Context, _ *plugin.Context, _ plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	views, err := p.svc.List(ctx)
	if errors.Is(err, ErrUnavailable) {
		return storeDownRead, nil
	}
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, describe(v.Presence))
	}
	return strings.Join(parts, "; "), nil
}

func (p *ChatPlugin) park(ctx context.Context, _ *plugin.Context, inv plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	if len(inv.Args) < 1 || len(inv.Args) > 2 {
		return p.usage("park <target> [duration]"), nil
	}
	actors, ok := p.svc.Registry().Resolve(inv.Args[0])
	if !ok {
		return p.usage("park <target> [duration]"), nil
	}
	var d time.Duration
	if len(inv.Args) == 2 {
		var err error
		if d, err = time.ParseDuration(inv.Args[1]); err != nil || d <= 0 {
			return p.usage("park <target> [duration]"), nil
		}
	}
	return p.parkActors(ctx, actors, d, chatParkReason, inv, "Parked: ")
}

func (p *ChatPlugin) leave(ctx context.Context, _ *plugin.Context, inv plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	var d time.Duration
	switch len(inv.Args) {
	case 0:
	case 1:
		var err error
		if d, err = time.ParseDuration(inv.Args[0]); err != nil || d <= 0 {
			return "Usage: !leave [duration], or @server leave [duration]. Durations look like 30m or 2h.", nil
		}
	default:
		return "Usage: !leave [duration], or @server leave [duration]. Durations look like 30m or 2h.", nil
	}
	self, _ := p.svc.Registry().Actor(p.svc.Registry().SelfID())
	views, err := p.svc.SetEach(ctx, []Actor{self}, func(a Actor) Request {
		until, wake := ChatPark(a, d, p.now())
		return Request{State: presenceapi.StateParked, Until: until, WakeOn: wake, Reason: chatLeaveReason}
	}, p.source(inv))
	if errors.Is(err, ErrUnavailable) {
		return storeDownWrite, nil
	}
	if err != nil {
		return "", err
	}
	// Said before the loop acts on it: this reply goes out through the
	// console bridge, which does not need the session that is about to end.
	return fmt.Sprintf("Leaving the world. I'll be back at %s, or as soon as a player joins.", utcClock(*views[0].Override.Until)), nil
}

func (p *ChatPlugin) unpark(ctx context.Context, _ *plugin.Context, inv plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	if len(inv.Args) != 1 {
		return p.usage("unpark <target>"), nil
	}
	actors, ok := p.svc.Registry().Resolve(inv.Args[0])
	if !ok {
		return p.usage("unpark <target>"), nil
	}
	views, err := p.svc.Clear(ctx, actors, p.source(inv))
	if errors.Is(err, ErrUnavailable) {
		return storeDownWrite, nil
	}
	if err != nil {
		return "", err
	}
	return "Back to default: " + describeAll(views), nil
}

func (p *ChatPlugin) parkActors(ctx context.Context, actors []Actor, d time.Duration, reason string, inv plugin.Invocation, prefix string) (string, error) {
	now := p.now()
	views, err := p.svc.SetEach(ctx, actors, func(a Actor) Request {
		until, wake := ChatPark(a, d, now)
		return Request{State: presenceapi.StateParked, Until: until, WakeOn: wake, Reason: reason}
	}, p.source(inv))
	if errors.Is(err, ErrUnavailable) {
		return storeDownWrite, nil
	}
	if err != nil {
		return "", err
	}
	return prefix + describeAll(views), nil
}

// source names who typed the command. The console has no gamertag, and the
// roster may not know a name the moment after a reconnect, when the XUID is
// the only honest record.
func (p *ChatPlugin) source(inv plugin.Invocation) Source {
	name := inv.ActorXUID
	switch {
	case inv.ActorXUID == chat.ServerOrigin:
		name = "console"
	case p.names != nil:
		if n, ok := p.names.NameFor(inv.ActorXUID); ok && n != "" {
			name = n
		}
	}
	return ChatSource(inv.ActorXUID, name, inv.ActorPermission.String())
}

func (p *ChatPlugin) usage(form string) string {
	return fmt.Sprintf("Usage: !%s, where target is one of: %s. Durations look like 30m or 2h.", form, strings.Join(p.svc.Registry().Targets(), ", "))
}

func describeAll(views []presenceapi.Presence) string {
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, describe(v))
	}
	return strings.Join(parts, "; ")
}

// describe is one actor in one short clause: chat lines wrap early, and a
// reply about three actors has to stay readable.
func describe(v presenceapi.Presence) string {
	if v.Override == nil {
		return fmt.Sprintf("%s %s (default)", v.ActorID, v.Effective)
	}
	s := fmt.Sprintf("%s %s by %s", v.ActorID, v.Effective, v.Override.SetBy)
	if v.Override.Until != nil {
		s += " until " + utcClock(*v.Override.Until)
	}
	if w := v.Override.WakeOn; w != nil {
		if w.AnyPlayerJoin {
			s += " or a player joins"
		} else {
			s += " or " + strings.Join(w.Players, "/") + " joins"
		}
	}
	return s
}

func utcClock(t time.Time) string { return t.UTC().Format("15:04 UTC") }

// LeaveArgs recognises "@server leave [duration]" and returns its arguments.
// Only the exact form, at the start of the message: "@server where are the
// leaves" is a question for the model, not a command to walk out.
func LeaveArgs(message string) ([]string, bool) {
	fields := strings.Fields(message)
	if len(fields) < 2 || len(fields) > 3 || !strings.EqualFold(fields[0], chat.MentionToken) || !strings.EqualFold(fields[1], "leave") {
		return nil, false
	}
	return fields[2:], true
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `gofmt -w internal/presence && gofmt -l internal/presence && go test -race ./internal/presence/`
Expected: `gofmt -l` prints nothing, and the test run prints `ok  	github.com/jdwillmsen/minecraft-server-agent/internal/presence`.

- [ ] **Step 5: Commit**

```bash
git add internal/presence/chat.go internal/presence/chat_test.go
git commit -m "feat(presence): add !presence, !park, !unpark and !leave" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 12: Wire presence into the agent

**Files:**
- Create: `cmd/agent/presence.go`
- Test: `cmd/agent/presence_test.go`
- Modify: `internal/httpapi/metrics.go` (`SetConnected`, lines 45-52), with a test appended to `internal/httpapi/presence_test.go`
- Modify: `cmd/agent/bridgeroster.go` (plan 1's `bridgeRoster` struct and `apply`), with a test appended to `cmd/agent/bridgeroster_test.go`
- Modify: `cmd/agent/main.go`: construction after the store block (origin/main lines 196-210), `registerPlugins` (lines 362-388), the announcement mount (lines 266-269), `handleText`'s mention case (lines 919-924), and plan 1's turn loop (the `var gate sessionGate = alwaysPresent{}` line, the `bridgeFollower` line, and the `runSessions` call)

**Interfaces:**
- Consumes (plan 1, exact names): `type sessionGate interface { Wanted() (present bool, changed <-chan struct{}) }`, `alwaysPresent{}`, `type sessionModes struct { present func(context.Context); left func(); absent func(context.Context) }`, `runSessions(ctx, gate, modes, log)`, `newBridgeRoster(feed, r, joins, archive, log) *bridgeRoster` with `(*bridgeRoster).apply(ctx, adapters.BridgeEvent)`, `adapters.BridgeEvent{ID, Type, Time, Player, Raw}`, `adapters.BridgeEventConnect`, and the test helpers `fakeBridgeFeed`, `recordedNames` and `quiet()`. Also `(*adapters.BridgeClient).OnlinePlayers` from plan 1 Task 1 and `(*adapters.BridgeClient).Kick` from Task 8. From this plan: `presence.NewRegistry`, `NewPostgres`, `Nop`, `NewService`, `NewGate`, `NewJoinLog`, `NewLoop`, `NewAPI`, `NewChatPlugin`, `LeaveArgs`, and `(*httpapi.Server).MountPresence`.
- Produces: `httpapi.Connected() bool`; `bridgeRoster.onJoin func(gamertag string, at time.Time)`; `newPresence(cfg config.Config, profiles store.Store, auditor audit.Store, bridge *adapters.BridgeClient, names presence.NameResolver, log *logging.Logger) (*presenceRuntime, error)`; `(*presenceRuntime).sessionGate() sessionGate`; `(*presenceRuntime).lead(ctx)`; `presenceModes(modes sessionModes, setReady func(bool), rejoined func(context.Context)) sessionModes`; `announceRejoin(voice plugin.Voice, log *logging.Logger) func(context.Context)`; `registerPlugins(..., extra ...plugin.Plugin)`.

Plan 1's notes leave three decisions to this plan, and they are made here:
- **Readiness while parked:** a parked leader answers `/readyz` as ready. It is doing its job (monitoring, the policy loop, the API), and an unready leader would drop out of the Service that the bots poll.
- **Joins in both modes:** in the session, `roster.JoinEvent` reaches the join log through the chat plugin (Task 11). While absent, the bridge follower's `apply` calls `onJoin` for every connect line with the line's own time, *before* it resolves an XUID. A first-time player whom nobody can resolve yet must still wake a parked agent.
- **Rejoin announcement:** `say` goes out through the bridge as the present mode starts after an absence.

- [ ] **Step 1: Write the failing tests**

Append to `internal/httpapi/presence_test.go`:
```go
func TestConnectedReadsBackWhatWasSet(t *testing.T) {
	t.Cleanup(func() { SetConnected(false) })
	SetConnected(true)
	if !Connected() {
		t.Error("Connected() = false after SetConnected(true)")
	}
	SetConnected(false)
	if Connected() {
		t.Error("Connected() = true after SetConnected(false)")
	}
}
```

Append to `cmd/agent/bridgeroster_test.go`:
```go
// A first-time player has no XUID anyone recorded, and is exactly who should
// wake a parked agent, so every connect line is reported before resolution.
// The line's own time travels with it: a replayed backlog line must not pass
// for an arrival after a park.
func TestBridgeRosterReportsEveryConnectToOnJoin(t *testing.T) {
	b := newBridgeRoster(&fakeBridgeFeed{}, roster.New(), newJoinTimes(), recordedNames{}, quiet())
	type join struct {
		name string
		at   time.Time
	}
	var got []join
	b.onJoin = func(name string, at time.Time) { got = append(got, join{name, at}) }
	at := time.Date(2026, 9, 23, 18, 0, 0, 0, time.UTC)
	b.apply(t.Context(), adapters.BridgeEvent{ID: 1, Type: adapters.BridgeEventConnect, Time: at, Player: "Sam"})
	b.apply(t.Context(), adapters.BridgeEvent{ID: 2, Type: adapters.BridgeEventDisconnect, Time: at, Player: "Sam"})
	if len(got) != 1 || got[0].name != "Sam" || !got[0].at.Equal(at) {
		t.Errorf("onJoin saw %+v, want Sam's connect alone, at the line's time", got)
	}
}
```

`cmd/agent/presence_test.go`:
```go
package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/presence"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
)

func unusedBridge() *adapters.BridgeClient {
	return adapters.NewBridgeClient("http://127.0.0.1:1", "test-token", 50*time.Millisecond)
}

func TestNewPresenceWithoutActorsChangesNothing(t *testing.T) {
	rt, err := newPresence(config.Config{PresenceSelfID: "agent"}, store.Nop{}, audit.Nop{}, unusedBridge(), roster.New(), quiet())
	if err != nil {
		t.Fatalf("newPresence: %v", err)
	}
	if _, ok := rt.sessionGate().(alwaysPresent); !ok {
		t.Errorf("gate = %T, want alwaysPresent when no actors are configured", rt.sessionGate())
	}
	if rt.api.Enabled() {
		t.Error("the presence API reports enabled with nothing configured")
	}
	rt.lead(t.Context())
}

func TestNewPresenceGatesTheSessionOnItsOwnActor(t *testing.T) {
	cfg := config.Config{
		PresenceSelfID: "agent",
		PresenceActors: []config.PresenceActor{{ID: "agent", Gamertag: "JdwAgent", Kind: "agent", DefaultState: "parked"}},
		PresenceTokens: []config.PresenceToken{{Name: "ops", Token: "aaaaaaaaaaaaaaaa", Scopes: []string{"presence:read"}}},
	}
	rt, err := newPresence(cfg, store.Nop{}, audit.Nop{}, unusedBridge(), roster.New(), quiet())
	if err != nil {
		t.Fatalf("newPresence: %v", err)
	}
	gate := rt.sessionGate()
	if _, ok := gate.(*presence.Gate); !ok {
		t.Fatalf("gate = %T, want the presence gate", gate)
	}
	if present, _ := gate.Wanted(); present {
		t.Error("an agent parked by default starts with the gate present; it would join before the first tick")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt.lead(ctx)
	if present, _ := gate.Wanted(); present {
		t.Error("with no database the gate must stay at the configured default")
	}
	if !rt.api.Enabled() {
		t.Error("tokens and actors are configured, but the API reports disabled")
	}
}

func TestPresenceModesAnnounceAReturnAndReportAParkedLeaderReady(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
		ready atomic.Bool
	)
	record := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }
	modes := presenceModes(sessionModes{
		present: func(context.Context) { record("present") },
		left:    func() { record("left") },
		absent:  func(context.Context) { record(fmt.Sprintf("absent ready=%v", ready.Load())) },
	}, ready.Store, func(context.Context) { record("rejoined") })

	ctx := t.Context()
	modes.present(ctx)
	modes.left()
	modes.absent(ctx)
	modes.present(ctx)

	want := []string{"present", "left", "absent ready=true", "rejoined", "present"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if ready.Load() {
		t.Error("still ready after the absence ended; the session sets it again on spawn")
	}
}

func TestAnnounceRejoinSaysSoThroughTheBridge(t *testing.T) {
	voice := &recordingVoice{}
	announceRejoin(voice, quiet())(t.Context())
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], rejoinLine) {
		t.Errorf("voice = %v, want the rejoin line said once", out)
	}
}

func TestRegisterPluginsIncludesExtraPlugins(t *testing.T) {
	registry := plugin.NewRegistry()
	reg, _ := presence.NewRegistry(nil, "agent")
	extra := presence.NewChatPlugin(presence.NewService(reg, presence.Nop{}, audit.Nop{}, quiet()), roster.New(), presence.NewJoinLog())
	if err := registerPlugins(t.Context(), registry, nil, newJoinTimes(), nil, quiet(), extra); err != nil {
		t.Fatalf("registerPlugins: %v", err)
	}
	for _, name := range []string{"presence", "park", "unpark", "leave"} {
		if _, ok := registry.Lookup(name); !ok {
			t.Errorf("!%s not registered", name)
		}
	}
}

type leaveStub struct{ got chan []string }

func (leaveStub) Name() string { return "leavestub" }

func (s leaveStub) Commands() []plugin.Command {
	return []plugin.Command{{
		Name: "leave", Description: "stub", Permission: plugin.PermissionOperator,
		Run: func(_ context.Context, _ *plugin.Context, inv plugin.Invocation) (string, error) {
			s.got <- inv.Args
			return "leaving", nil
		},
	}}
}

func TestServerLeaveMentionRunsTheLeaveCommand(t *testing.T) {
	registry, pctx, voice, eventBus, _, playerRoster, _ := newHarness(t)
	stub := leaveStub{got: make(chan []string, 2)}
	if err := registry.Register(stub); err != nil {
		t.Fatal(err)
	}
	perms := fakePermResolver(t, map[string]string{playerXUID: "operator"})

	handlePacket(t.Context(), chatPacket(playerXUID, "Steve", "@server leave 30m"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	select {
	case args := <-stub.got:
		if !slices.Equal(args, []string{"30m"}) {
			t.Errorf("leave got args %v, want [30m]", args)
		}
	default:
		t.Fatal("@server leave did not reach the leave command")
	}
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], "leaving") {
		t.Errorf("voice = %v, want the command's reply", out)
	}

	handlePacket(t.Context(), chatPacket(playerXUID, "Steve", "@server where are the leaves"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	select {
	case args := <-stub.got:
		t.Errorf("a question about leaves ran !leave with %v", args)
	default:
	}
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./internal/httpapi/ -run Connected ; go test ./cmd/agent/ -run 'Presence|OnJoin|Rejoin|ExtraPlugins|ServerLeave'`
Expected: FAIL, build errors `undefined: Connected`, `b.onJoin undefined`, `undefined: newPresence`, `undefined: presenceModes`, `undefined: announceRejoin`, `too many arguments in call to registerPlugins`.

- [ ] **Step 3: Add `Connected`**

In `internal/httpapi/metrics.go`, add `"sync/atomic"` to the imports and replace `SetConnected` (lines 45-52) with:

```go
// connected mirrors connectedGauge for readers in this process: a gauge
// cannot be read back, and the presence loop reports the agent's own status
// from the same fact the metric exports.
var connected atomic.Bool

// SetConnected records whether a Bedrock session is currently established.
func SetConnected(up bool) {
	connected.Store(up)
	if up {
		connectedGauge.Set(1)
	} else {
		connectedGauge.Set(0)
	}
}

// Connected reports what SetConnected last recorded.
func Connected() bool { return connected.Load() }
```

- [ ] **Step 4: Report the bridge's joins**

In `cmd/agent/bridgeroster.go` (plan 1), add a field to `bridgeRoster` after `archive nameArchive`:

```go
	// onJoin, when set, hears every connect line with the line's own time.
	// It is how a player arriving wakes a parked agent while the follower,
	// not a session, is watching.
	onJoin func(gamertag string, at time.Time)
```

and in `apply`, after the `switch e.Type { ... }` that returns for other types, and before `xuid := e.XUID()`, insert:

```go
	// Before resolving an XUID: a first-time player nobody has recorded yet
	// is exactly who should bring a parked agent back.
	if !remove && b.onJoin != nil {
		at := e.Time
		if at.IsZero() {
			at = time.Now()
		}
		b.onJoin(e.Player, at)
	}
```

- [ ] **Step 5: Implement the runtime**

`cmd/agent/presence.go`:
```go
package main

import (
	"context"
	"runtime/debug"
	"sync/atomic"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/presence"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// rejoinLine is what the agent says as it comes back into the world after
// being parked, so the players who saw it leave know it is back.
const rejoinLine = "I'm back in the world."

// presenceRuntime is actor presence as this binary runs it: built once for
// the process, led once per turn as the live agent.
type presenceRuntime struct {
	svc    *presence.Service
	gate   *presence.Gate
	joins  *presence.JoinLog
	loop   *presence.Loop
	api    *presence.API
	plugin *presence.ChatPlugin
}

// newPresence builds every presence part from configuration. It is called
// before the profile store is wrapped, because, like the other stores, it
// borrows the concrete Postgres's pool.
func newPresence(cfg config.Config, profiles store.Store, auditor audit.Store, bridge *adapters.BridgeClient, names presence.NameResolver, log *logging.Logger) (*presenceRuntime, error) {
	actors := make([]presence.Actor, 0, len(cfg.PresenceActors))
	for _, a := range cfg.PresenceActors {
		actors = append(actors, presence.Actor{ID: a.ID, Gamertag: a.Gamertag, Kind: a.Kind, Groups: a.Groups, Default: presenceapi.State(a.DefaultState)})
	}
	reg, err := presence.NewRegistry(actors, cfg.PresenceSelfID)
	if err != nil {
		return nil, err
	}
	var st presence.Store = presence.Nop{}
	if pg, ok := profiles.(*store.Postgres); ok && pg.Pool() != nil {
		st = presence.NewPostgres(pg.Pool())
	}
	svc := presence.NewService(reg, st, auditor, log)

	// The configured default until the loop's first tick reads the stored
	// answer, so a leader parked by git never joins on its way to finding out.
	present := true
	if self, ok := reg.Actor(reg.SelfID()); ok {
		present = self.Default == presenceapi.StatePresent
	}
	gate := presence.NewGate(present)
	joins := presence.NewJoinLog()
	loop := presence.NewLoop(presence.LoopConfig{
		Service: svc, Console: bridge, Joins: joins, Gate: gate,
		SessionUp: httpapi.Connected, Version: processVersion(), Log: log,
	})
	svc.OnChange(loop.Nudge)

	tokens := make([]presence.Token, 0, len(cfg.PresenceTokens))
	for _, t := range cfg.PresenceTokens {
		tokens = append(tokens, presence.Token{Name: t.Name, Secret: t.Token, Scopes: t.Scopes, Actor: t.Actor})
	}
	return &presenceRuntime{
		svc: svc, gate: gate, joins: joins, loop: loop,
		api:    presence.NewAPI(svc, tokens, log),
		plugin: presence.NewChatPlugin(svc, names, joins),
	}, nil
}

// sessionGate decides whether the live agent is in the world: by its own
// actor's effective presence when presence is configured, and always
// otherwise, exactly as before presence existed.
func (p *presenceRuntime) sessionGate() sessionGate {
	if !p.svc.Enabled() {
		return alwaysPresent{}
	}
	return p.gate
}

// lead starts presence's share of a turn as the live agent. The first tick
// runs before it returns, so the gate holds the stored answer by the time
// the session lifecycle first asks it.
func (p *presenceRuntime) lead(ctx context.Context) {
	if !p.svc.Enabled() {
		return
	}
	p.loop.Prime(ctx)
	go p.loop.Run(ctx)
}

// presenceModes adds what presence needs around plan-1 session modes.
//
// A parked leader reports ready. It is doing its job -- monitoring, the
// policy loop, the API -- and an unready one would fall out of the Service
// the bots poll. The session sets readiness again when it reaches spawn,
// so readiness is cleared as the absence ends.
//
// runSessions never runs two modes at once, but it runs each on its own
// goroutine, so the flag between them is atomic.
func presenceModes(modes sessionModes, setReady func(bool), rejoined func(context.Context)) sessionModes {
	var away atomic.Bool
	return sessionModes{
		present: func(ctx context.Context) {
			if away.Swap(false) {
				rejoined(ctx)
			}
			modes.present(ctx)
		},
		left: modes.left,
		absent: func(ctx context.Context) {
			away.Store(true)
			setReady(true)
			defer setReady(false)
			modes.absent(ctx)
		},
	}
}

// announceRejoin says the agent is back, through the bridge. It does not
// need the session that is only now being dialled.
func announceRejoin(voice plugin.Voice, log *logging.Logger) func(context.Context) {
	return func(ctx context.Context) {
		sayCtx, cancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
		defer cancel()
		if err := voice.Say(sayCtx, rejoinLine); err != nil {
			log.Error("presence_rejoin_say_failed", logging.Fields{"error": err.Error()})
		}
	}
}

// processVersion is this binary's module version, as reported in the
// agent's own status row. An image built without VCS metadata reports
// "(devel)", which is still the truth about what the build knew.
func processVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}
```

- [ ] **Step 6: Wire it into `main.go`**

1. **Build the runtime.** Directly after the store block's closing `}` (origin/main line 210, after `log.Info("knowledge_ready", nil)` and its `}`), insert:

```go
	// Built here, before playerStore is wrapped below, for the same reason
	// the stores above are: it borrows the concrete Postgres's pool.
	presenceRT, err := newPresence(cfg, playerStore, auditor, bridgeClient, playerRoster, log)
	if err != nil {
		log.Error("presence_config_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}
```

2. **Register the plugin.** Change the `registerPlugins` call (origin/main line 242) to:

```go
	if err := registerPlugins(ctx, registry, deliverer, joins, cfg.ModerationTerms, log, presenceRT.plugin); err != nil {
```

and in the function (origin/main lines 362-381), change the signature and the loop head to:

```go
func registerPlugins(ctx context.Context, registry *plugin.Registry, deliverer plugins.AnnounceDeliverer, conns plugins.Connections, moderationTerms []string, log *logging.Logger, extra ...plugin.Plugin) error {
```
```go
	for _, p := range append([]plugin.Plugin{
		plugins.NewCore(),
		plugins.NewStats(),
		plugins.NewKnowledge(),
		plugins.NewWaypoints(),
		plugins.NewWelcome(ctx, welcomeDelay, log, plugins.WithGreetConnections(conns)),
		plugins.NewAnnounce(),
		plugins.NewAnnounceDrain(ctx, deliverer, announceDrainDelay, log, plugins.WithConnections(conns)),
		mod,
		plugins.NewSchedule(),
	}, extra...) {
```

Variadic, so the three existing test call sites (`metrics_test.go:104`, `reply_logging_test.go:73`, `announcing_test.go:341`) compile unchanged.

3. **Mount the API.** After `log.Info("announce_api", ...)` (origin/main line 269), insert:

```go
	presenceOn := httpServer.MountPresence(presenceRT.api)
	log.Info("presence_api", logging.Fields{"enabled": presenceOn, "actors": len(cfg.PresenceActors)})
```

4. **Route `@server leave`.** In `handleText`, replace the mention case (origin/main lines 922-923):

```go
	case chat.TriggerMention:
		startAnswer(ctx, id, trigger, chat.IsPrivateType(text.TextType), log, pctx, ans, playerRoster)
```

with:

```go
	case chat.TriggerMention:
		// A command in a mention's clothing: dispatched like !leave, so it
		// gets the same permission check, rate limit and audit row, and never
		// reaches the model.
		if args, ok := presence.LeaveArgs(trigger.Message); ok {
			handleCommand(ctx, id, chat.Trigger{Kind: chat.TriggerCommand, Command: "leave", Args: args}, chat.IsPrivateType(text.TextType), log, registry, pctx, limiter, permResolver, auditor, playerRoster)
			return
		}
		startAnswer(ctx, id, trigger, chat.IsPrivateType(text.TextType), log, pctx, ans, playerRoster)
```

and add `"github.com/jdwillmsen/minecraft-server-agent/internal/presence"` to `main.go`'s imports.

5. **Swap the gate, feed the joins, lead the loop and wrap the modes.** In plan 1's turn loop, replace:

```go
	// Nothing decides presence yet, so the live agent is always in the world.
	var gate sessionGate = alwaysPresent{}
```

with:

```go
	// The agent's own effective presence when actors are configured, and
	// always present otherwise.
	gate := presenceRT.sessionGate()
```

After the `bridgeFollower := newBridgeRoster(...)` line, add:

```go
	bridgeFollower.onJoin = presenceRT.joins.RecordAt
```

After `startLiveWork(liveCtx, ...)` inside the loop, add:

```go
		// Leader-only, like startLiveWork, and before runSessions: its first
		// tick decides the gate that runSessions reads first.
		presenceRT.lead(liveCtx)
```

Wrap the `sessionModes{...}` literal passed to `runSessions` so the call reads:

```go
		runSessions(liveCtx, gate, presenceModes(sessionModes{
			present: func(sessionCtx context.Context) {
				runConnectLoop(sessionCtx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, audience, siblings, permResolver, ans, playerStore, auditor, link, joins)
			},
			// Leaving on purpose owes what a recycle owes: everyone still
			// here keeps the time they were watched for, instead of the
			// next connection's CloseOrphans rewriting it as unknown. No
			// term, because the lock is not being passed on.
			left: func() {
				handover(liveCtx, nil, playerStore, playerRoster.Since(), log)
			},
			absent: bridgeFollower.run,
		}, httpServer.SetReady, announceRejoin(voice, log)), log)
```

- [ ] **Step 7: Run the tests to see them pass**

Run: `gofmt -w cmd/agent internal/httpapi && gofmt -l cmd/agent internal/httpapi && go vet ./cmd/agent/ ./internal/httpapi/ && go test -race ./cmd/agent/ ./internal/httpapi/`
Expected: `gofmt -l` and `go vet` print nothing, and both packages print `ok`, including plan 1's `runSessions` tests and every existing flow test.

- [ ] **Step 8: Check the unconfigured path is unchanged**

Run: `git diff origin/main -- cmd/agent/main.go | grep -E '^\+' | grep -v '^+++'`
Expected: the only added lines are the five insertions above. With `PRESENCE_ACTORS` unset, `sessionGate()` returns `alwaysPresent{}`, `lead` returns at once, `MountPresence` mounts nothing, and the chat commands answer "not set up". `TestNewPresenceWithoutActorsChangesNothing` pins all of this.

- [ ] **Step 9: Commit**

```bash
git add cmd/agent/presence.go cmd/agent/presence_test.go cmd/agent/main.go cmd/agent/bridgeroster.go cmd/agent/bridgeroster_test.go internal/httpapi/metrics.go internal/httpapi/presence_test.go
git commit -m "feat(agent): gate the session on the agent's own presence and lead the presence loop" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---
### Task 13: README, full verification and the `presenceapi/v0.1.0` tag

**Files:**
- Modify: `README.md`: the environment table (after the `ANNOUNCE_API_TOKEN` row, line 347), the metrics table (after `mc_agent_session_recycles_total`, line 374), "Testing the store against a real database" (after the knowledge/waypoints paragraph, around line 1520), and a new section "Parking actors" placed before "## Targeting a reply" (line 1079)

**Interfaces:**
- Consumes: every name above, spelled as in the contract.
- Produces: operator documentation. After merge, the tag `presenceapi/v0.1.0` that plan 5 (AFK bot) requires.

- [ ] **Step 1: Document the environment variables**

After the `ANNOUNCE_API_TOKEN` row, insert:

```markdown
| `PRESENCE_ACTORS` | *(empty disables presence)* | JSON list of every account that puts a player into the world: `[{"id","gamertag","kind","groups":[...],"default_state"}]`. `id` matches `^[a-z0-9][a-z0-9-]{0,62}$`, `kind` is `agent` or `afk-bot`, `default_state` is `present` or `parked`, and `all` is an implicit group. Rendered by the chart from the same values list as the bot Deployments. Unset keeps the agent always in the world, as before |
| `PRESENCE_TOKENS` | *(empty disables the API)* | JSON list of `/v1` bearer tokens: `[{"name","token","scopes":[...],"actor"}]`. Scopes are `presence:read`, `presence:write` and `presence:report`. `actor` binds a token to one actor, and every `presence:report` token needs one. Tokens are at least 16 characters. Unset leaves `/v1` unmounted. Created by a human, never by an agent |
| `PRESENCE_SELF_ID` | `agent` | Which actor in `PRESENCE_ACTORS` this process is; it must be of kind `agent` |
```

- [ ] **Step 2: Document the metrics**

After the `mc_agent_session_recycles_total` row, insert:

```markdown
| `mc_presence_desired` | gauge | `actor` | per policy tick on the leader: 1 present, 0 parked |
| `mc_presence_observed` | gauge | `actor` | per policy tick on the leader: 1 when the actor's own report says connected and is under 60s old (the agent reads its own session), 0 otherwise |
| `mc_presence_override_age_seconds` | gauge | `actor` | per policy tick, only for an override with no `until`; absent otherwise |
| `mc_presence_kicks_total` | counter | `actor` | per `kick` sent for an actor still listed 20s after it was parked; starts at zero |
```

and after the paragraph on `mc_agent_server_tps` (ending "reads as stale rather than as missing."), add:

```markdown
The `mc_presence_*` gauges exist only on the pod that leads, and they are
withdrawn when it stops leading, so a standby never exports a stale view beside
the leader's. `actor` is always an id from `PRESENCE_ACTORS`. An actor that
should be present and is not reads desired 1 and observed 0, which is what the
degraded and disconnected alerts are written against. A deliberate park reads
0 and 0, and pages nobody.
```

- [ ] **Step 3: Write "Parking actors"**

Insert before `## Targeting a reply`:

````markdown
## Parking actors

Every actor that puts a player into the world keeps chunks loaded around it.
Parking an actor disconnects it, which releases those chunks. Git stays the
baseline: each actor has a `default_state` in `PRESENCE_ACTORS`, and a park
is a runtime override with an owner, a reason and usually an expiry. Clearing
an override always falls back to what git says. Overrides live in Postgres
(`minecraft.presence_overrides`), so they survive restarts, rollouts and
leader handovers.

The leader runs a policy loop every 10 seconds. It removes overrides whose
`until` has passed and wakes those whose `wake_on` player has joined. It also
sends `kick <gamertag>` through the console bridge for any actor that is
still in the server's `list` 20 seconds after it was parked, because Bedrock
keeps a session open after the client goes. The agent itself is an actor.
Parked, it leaves the world and keeps monitoring through the bridge. While it
is parked it reads no chat, so chat commands and `@server` questions are
unavailable until it is back.

### From chat

| Command | Who | Effect |
|---|---|---|
| `!presence` | member | Each actor's effective state, who set it and until when |
| `!park <actor\|group\|all> [duration]` | operator | Park. With no duration a bot stays parked until unparked |
| `!unpark <actor\|group\|all>` | operator | Remove the override; the actor returns to its default |
| `!leave [duration]`, `@server leave [duration]` | operator | Park the agent |

A park of the agent from chat, including `!park all`, always comes back by
itself. It lasts one hour, or the duration given, and it ends early as soon
as a player joins. The agent says so before it leaves, and says it is back
when it rejoins. Durations use Go syntax: `30m`, `2h`.

### Over HTTP

Mounted on `HTTP_ADDR` when `PRESENCE_TOKENS` is set. Every replica answers
from Postgres, the standby included.

| Method and path | Scope | Answers |
|---|---|---|
| `GET /v1/actors` | `presence:read` | every actor with default, override, effective and observed state |
| `GET /v1/actors/{id}/presence` | `presence:read` | effective state; `ETag: "<version>-<effective>"`, 304 on `If-None-Match` |
| `PUT /v1/actors/{id}/presence` | `presence:write` | set an override; stale `version` is 409 with the current row |
| `DELETE /v1/actors/{id}/presence` | `presence:write` | remove the override |
| `PUT /v1/groups/{group}/presence` | `presence:write` | set every member in one transaction; `version` ignored |
| `POST /v1/actors/{id}/status` | `presence:report` | a bot's own observed status; 403 for any other actor |

```sh
curl -sS -X PUT http://<release>-server-agent:8080/v1/actors/afk-bot-1/presence \
  -H "Authorization: Bearer $PRESENCE_TOKEN" -H 'Content-Type: application/json' \
  -d '{"state":"parked","duration":"2h","reason":"chunk budget for the build","version":0}'
```

`version` is the override version the caller last read. It is 0 for "I
expect no override", and a mismatch answers 409 with the current row in
`current`. `until` and `duration` are exclusive, and a duration is resolved on
the agent's clock. Every error body is `{"code","message"}`, where `code` is
one of `not_found`, `conflict`, `forbidden`, `unauthorized`, `invalid` or
`unavailable`. A database that cannot answer is a 503, and the bots keep
acting on their last answer. The request and response types live in the
`github.com/jdwillmsen/minecraft-server-agent/presenceapi` module, which
imports nothing beyond the standard library.

Every override write, removal and wake is also recorded in
`minecraft.command_audit` with `command = 'presence'`. The `args` column
reads `actor=… from=… to=… cause=set|cleared|expired|woken source=… reason="…"`.
For an API write, `xuid` holds `api:<token-name>`. For the loop's own
removals it holds `presence-loop`.

`replicas: 0` in the chart remains the way to take a bot away for
maintenance. For gameplay, park it.
````

- [ ] **Step 4: Document the live tests**

After the paragraph on the knowledge and waypoints live tests, add:

```markdown
`internal/presence` has them too, against `minecraft.presence_overrides` and
`minecraft.presence_status` from `V8__minecraft_presence.sql`: the version
rules, the group transaction, and a leader handover in which a second process
removes the first one's expired override. Apply V8 after the earlier
migrations and run `go test -tags livedb ./internal/presence/`.
```

- [ ] **Step 5: Run the full verification**

Run:
```bash
gofmt -l . \
  && go vet ./... && go vet -tags livedb ./... \
  && CGO_ENABLED=0 go build ./... \
  && go test -race ./... \
  && (cd presenceapi && go vet ./... && go test -race ./... && golangci-lint run ./...) \
  && golangci-lint run ./... \
  && docker build -t presence-check .
```
Expected: `gofmt -l` prints nothing. Every `go test` line ends in `ok` or `[no test files]`, including `internal/presence`, `internal/config`, `internal/metrics`, `internal/httpapi`, `internal/adapters`, `cmd/agent` and `presenceapi`. Both `golangci-lint run` invocations print nothing and exit 0. The image builds.

Against a database migrated through V8:
`MC_TEST_DSN='postgres://app:apppw@127.0.0.1:55432/jdwillmsen_prd?sslmode=disable' go test -tags livedb -race ./internal/presence/ -v`
Expected: `TestSetFollowsTheVersionRules`, `TestSetManyAndClearAreOneTransactionEach`, `TestPutStatusUpserts` and `TestOverridesAndExpiriesSurviveALeaderHandover` PASS.

- [ ] **Step 6: Check nothing leaked**

Run: `git diff origin/main | grep -nE 'JDWLABS-[0-9]+' ; git diff origin/main --stat -- presenceapi/go.mod && cat presenceapi/go.mod`
Expected: the grep prints nothing. `presenceapi/go.mod` has only the `module` and `go 1.27` lines, and no `require`.

- [ ] **Step 7: Commit**

```bash
git add README.md
git commit -m "docs: document parking actors, the presence API and its metrics" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 8: After the PR merges, tag the contract module**

The AFK bot (plan 5) requires `github.com/jdwillmsen/minecraft-server-agent/presenceapi v0.1.0`. Go resolves a module in a subdirectory from a tag with the directory as its prefix. semantic-release's `tagFormat` (`v${version}`, `.releaserc.json`) and the release workflow's `v[0-9]*` trigger both ignore a `presenceapi/` tag, so this cuts no agent release. On a fresh `origin/main` that contains the merge:

```bash
git fetch origin
git diff origin/main --stat -- presenceapi   # must print nothing: the tag goes on the merged tree
git tag -s presenceapi/v0.1.0 origin/main -m "presenceapi v0.1.0"
git push origin presenceapi/v0.1.0
GOPROXY=https://proxy.golang.org GOFLAGS=-mod=mod go list -m github.com/jdwillmsen/minecraft-server-agent/presenceapi@v0.1.0
```
Expected: the push is accepted, and `go list` prints `github.com/jdwillmsen/minecraft-server-agent/presenceapi v0.1.0`. If `tag -s` fails for lack of a signing key, use `git tag -a` with the same arguments.

---

## Deviations from spec/contract

1. **Audit rows reuse `minecraft.command_audit`'s command-shaped columns.** The spec asks for "actor, old state, new state, source and reason". That table has only `xuid, gamertag, permission, command, args, outcome, occurred_at`, and plan 2 adds no column. Presence rows use `command = 'presence'` with `args = actor=… from=… to=… cause=… source=… reason="…"`. For API writes, `xuid` holds `api:<token-name>`, and for the loop's removals it holds `presence-loop`.
2. **`set_by: cli:<token-name>` is never produced.** The CLI is a thin client over the same HTTP API, so every API write is `api:<name>`. Resolved in the contract: the `cli:` prefix is dropped and the CLI's token is named `tools-mc`, so its rows read `api:tools-mc`.
3. **"Recent joins" is a five-minute window, not "events since the last tick".** The policy ignores any join from before an override's `set_at`. Replaying older joins is therefore harmless, and a tick that failed to read Postgres does not lose the arrival that should have woken an actor. Bridge joins carry the console line's own time for the same reason.
4. **`@server leave` is dispatched as a registered operator command `leave`,** so `!leave` also works. This gives it the command path's permission check, rate limit and audit row, and keeps it away from the model. The spec's chat table lists only `@server leave`.
5. **Units beyond the spec's table.** `Service` is the one write path shared by the API, chat and loop, so validation and audit cannot differ between them. `Gate` satisfies plan 1's `sessionGate`, and `JoinLog` feeds `wake_on` from both session modes. The spec's `Metrics` unit is recorders in `internal/metrics` (the repo's single home for metric names), which the loop calls.
6. **Observed status goes stale after 60s.** The spec does not say how old a report may be. A crashed bot's last row says `connected: true` forever, so `mc_presence_observed` counts only reports under 60s old (six bot polls). The agent reads its own session directly.
7. **`Status.last_seen` from a bot is replaced by the agent's clock** on receipt, so a bot's clock skew cannot make it look fresh or stale.
8. **A token bound to an actor is bound for writes too.** The spec only specifies 403 for status reports to another actor. A bound token that also has `presence:write` may change only its own actor, and it may never change a group.
9. **A parked leader answers `/readyz` as ready.** Plan 1 left this open. A parked leader is doing its job, and an unready one would drop out of the Service the bots poll.
10. **The kick is `kick "<name>"` when the gamertag has a space,** following plan 4's accepted forms. For the same reason, `PRESENCE_ACTORS` refuses a gamertag containing a quote.
11. **This plan edits plan 1's `cmd/agent/bridgeroster.go`,** adding `onJoin`, which plan 1's notes anticipated. It must execute after plan 1 has merged.
12. **`process_version` is the module version from `debug.ReadBuildInfo`.** The image is built with `.git` excluded (`.dockerignore`), so the agent reports `(devel)`. Passing the release version through a build argument would change the release workflow, which is outside this phase.
