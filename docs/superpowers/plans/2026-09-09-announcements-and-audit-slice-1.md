# Announcements and Command Audit — Slice 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the agent a way to tell people things on its own initiative — to everyone, one player, or a permission level, now or when they next log in — and make every command dispatch durably queryable.

**Architecture:** An outbox. Sources insert rows into `minecraft.announcements`; one deliverer resolves targets against the live roster, sends what it can now, and drains the rest on join. Every send writes a delivery row, which is both the idempotency key and the audit record. Command auditing is a separate table written on every dispatch, independent of announcements.

**Tech Stack:** Go 1.24, `pgx/v5` + `pgxpool`, the existing `plugin`/`bus`/`roster` packages, Flyway 11.2 for the migration.

**Spec:** `docs/superpowers/specs/2026-09-09-announcements-and-audit-design.md`

## Global Constraints

- Identity is the XUID, never the gamertag. Every target, delivery row and audit row keys on XUID.
- No database configured is a supported state. `!announce` and `!inbox` report themselves unconfigured; nothing else changes behaviour.
- Replies and announcements are one short plain line, no markdown, never ending in a question mark — a sibling automated bot speaks in the same chat and a question invites a loop.
- Malformed input is a chat reply, never a Go error.
- A player may only ever drain their own inbox. No command takes an argument naming another player's queue.
- The audit records who ran what at which resolved permission and the outcome. It does **not** record reply text: replies carry coordinates the agent deliberately whispers.
- An audit write that fails is logged and never blocks the command.
- Delivery work makes bridge calls and must not run on the Bedrock packet read loop.
- Comments explain WHY, never what. No ticket IDs, no URLs, no external references in code or comments.
- Every commit message ends with: `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`
- Before every commit: `go build ./...`, `go vet ./...`, `gofmt -l .` (silent), `go test ./...`, `go test -race ./...`

---

### Task 1: Platform migration V4

Runs in the **platform** repo (`~/projects/jdwlabs/platform`), not this one. Must merge and sync before the agent release that uses it.

**Files:**
- Modify: `tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces: `minecraft.announcements`, `minecraft.announcement_deliveries`, `minecraft.command_audit`, writable by the `app` role.

- [ ] **Step 1: Create a worktree**

```bash
cd ~/projects/jdwlabs/platform
git fetch origin
git worktree add -b feat/minecraft-announcements-schema \
  ~/worktrees/platform/minecraft-announcements origin/main
```

- [ ] **Step 2: Append V4 to the ConfigMap**

Add as a fourth key under `data:`, same indentation as `V3__minecraft_knowledge.sql`:

```yaml
  V4__minecraft_announcements.sql: |
    -- Things the server wants said, and who has already heard them.
    --
    -- An outbox rather than a chat call: four different sources will want to
    -- announce, and every question about who sees a message, when, and
    -- whether it is still worth saying should be answered once rather than
    -- once per source.
    CREATE TABLE IF NOT EXISTS minecraft.announcements
    (
        announcement_id BIGINT      NOT NULL GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        body            TEXT        NOT NULL,
        source          TEXT        NOT NULL
            CHECK (source IN ('command', 'schedule', 'event', 'api')),
        -- Null for everything the server said on its own initiative.
        author_xuid     TEXT        REFERENCES minecraft.players (xuid) ON DELETE SET NULL,
        target_kind     TEXT        NOT NULL
            CHECK (target_kind IN ('everyone', 'player', 'permission', 'online_only')),
        -- The XUID for 'player', the level for 'permission', null otherwise.
        -- One nullable column rather than three: no target kind needs two.
        target_value    TEXT,
        priority        TEXT        NOT NULL DEFAULT 'normal'
            CHECK (priority IN ('normal', 'expedited')),
        -- Derived from target_kind on write rather than chosen freely: a
        -- 'player' announcement that broadcast would leak exactly what
        -- whispering a waypoint protects.
        delivery        TEXT        NOT NULL
            CHECK (delivery IN ('broadcast', 'whisper')),
        created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
        deliver_after   TIMESTAMPTZ NOT NULL DEFAULT now(),
        -- The column that makes this a queue rather than a nag. A restart
        -- warning delivered three days late is noise; a farm relocation is
        -- still true. Null means it never expires.
        expires_at      TIMESTAMPTZ
    );

    CREATE INDEX IF NOT EXISTS announcements_pending_idx
        ON minecraft.announcements (deliver_after);

    -- One row per person who actually received it: the idempotency key and
    -- the audit record at once. Written even for a broadcast, because
    -- otherwise a player who was present when it went out would receive it
    -- again on their next join.
    CREATE TABLE IF NOT EXISTS minecraft.announcement_deliveries
    (
        announcement_id BIGINT      NOT NULL REFERENCES minecraft.announcements (announcement_id) ON DELETE CASCADE,
        xuid            TEXT        NOT NULL REFERENCES minecraft.players (xuid) ON DELETE CASCADE,
        delivered_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
        PRIMARY KEY (announcement_id, xuid)
    );

    -- Every command dispatch, whatever the outcome.
    --
    -- xuid is deliberately not a foreign key, unlike everywhere else in this
    -- schema: an audit row must outlive the player it describes, which is the
    -- opposite of what waypoints need.
    CREATE TABLE IF NOT EXISTS minecraft.command_audit
    (
        audit_id     BIGINT      NOT NULL GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        xuid         TEXT        NOT NULL,
        gamertag     TEXT        NOT NULL,
        -- The level resolved at the time, not the actor's level now.
        -- Permissions change; what they were allowed to do when they ran it
        -- is the auditable fact.
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

No GRANT statements: V2 set default privileges for role `postgres` in this schema, which covers tables later migrations create. Step 4 verifies that rather than assuming it.

- [ ] **Step 3: Validate the YAML**

Run: `python3 -c "import yaml; d=yaml.safe_load(open('tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml')); print(sorted(d['data']))"`
Expected: four keys, `V1__minecraft_init.sql` through `V4__minecraft_announcements.sql`.

- [ ] **Step 4: Commit, then stop**

```bash
git add tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml
git commit -m "feat(jdwillmsen-schemas): add announcement and command audit tables

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

Do not push or open a PR. After it merges and ArgoCD syncs, confirm the `app` role can write all three tables before the agent release depends on them:

```sql
SET ROLE app;
INSERT INTO minecraft.command_audit (xuid, gamertag, permission, command, args, outcome)
VALUES ('__probe','__probe','visitor','probe','','ok');
DELETE FROM minecraft.command_audit WHERE xuid = '__probe';
```

---

### Task 2: `internal/audit` — the command audit store

**Files:**
- Create: `internal/audit/audit.go`
- Create: `internal/audit/postgres.go`
- Test: `internal/audit/audit_test.go`
- Test: `internal/audit/postgres_live_test.go`

**Interfaces:**
- Consumes: `*pgxpool.Pool` via `store.Postgres.Pool()`.
- Produces:
  - `audit.Outcome` string type with constants `OutcomeOK`, `OutcomeDenied`, `OutcomeUnknown`, `OutcomeError`, `OutcomeRateLimited`, `OutcomeTimeout`
  - `audit.Record{XUID, Gamertag, Permission, Command, Args string; Outcome Outcome; At time.Time}`
  - `audit.Store` interface: `Write(ctx context.Context, r Record) error`, `Enabled() bool`
  - `audit.Nop{}`, `audit.NewPostgres(pool *pgxpool.Pool) *audit.Postgres`

- [ ] **Step 1: Write the failing test**

`internal/audit/audit_test.go`:

```go
package audit

import (
	"context"
	"testing"
	"time"
)

func TestNopAcceptsEverythingAndPersistsNothing(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Fatal("Nop reports itself enabled")
	}
	err := s.Write(context.Background(), Record{
		XUID: "x", Gamertag: "g", Permission: "visitor",
		Command: "ping", Outcome: OutcomeOK, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Write on Nop must not error: %v", err)
	}
}

func TestOutcomesMatchTheSchemaCheckConstraint(t *testing.T) {
	// The migration constrains this column. A constant that drifts from it
	// fails at the first write of that kind, in production, on the one code
	// path nobody exercises by hand.
	want := map[Outcome]bool{
		"ok": true, "denied": true, "unknown": true,
		"error": true, "rate_limited": true, "timeout": true,
	}
	for _, got := range []Outcome{
		OutcomeOK, OutcomeDenied, OutcomeUnknown,
		OutcomeError, OutcomeRateLimited, OutcomeTimeout,
	} {
		if !want[got] {
			t.Errorf("outcome %q is not one the schema allows", got)
		}
		delete(want, got)
	}
	if len(want) != 0 {
		t.Errorf("schema allows outcomes with no constant: %v", want)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/audit/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/audit/audit.go`**

```go
// Package audit records who ran which command, at what privilege, and what
// happened.
//
// Separate from the stdout logging that already exists: that record dies
// with the pod, cannot be queried, and is not something an auditor would
// accept. This one is a table.
//
// It deliberately does not store reply text. Replies to !wp carry
// coordinates the agent goes out of its way to whisper, and a trail that
// transcribes every private reply is a larger exposure than the gap it
// closes.
package audit

import (
	"context"
	"time"
)

// Outcome is what happened to a dispatch. The values are exactly the set
// the schema's CHECK constraint allows.
type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeDenied      Outcome = "denied"
	OutcomeUnknown     Outcome = "unknown"
	OutcomeError       Outcome = "error"
	OutcomeRateLimited Outcome = "rate_limited"
	OutcomeTimeout     Outcome = "timeout"
)

// Record is one command dispatch.
type Record struct {
	XUID     string
	Gamertag string
	// Permission is the level the actor resolved at when the command ran,
	// not their level now.
	Permission string
	Command    string
	Args       string
	Outcome    Outcome
	At         time.Time
}

// Store persists audit records. Every method must tolerate being called on
// a disabled implementation.
type Store interface {
	Write(ctx context.Context, r Record) error
	Enabled() bool
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Write(context.Context, Record) error { return nil }
func (Nop) Enabled() bool                       { return false }
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `go test ./internal/audit/ -v`
Expected: PASS.

- [ ] **Step 5: Write `internal/audit/postgres.go`**

```go
package audit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres writes to minecraft.command_audit, sharing the profile store's
// pool.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

func (p *Postgres) Write(ctx context.Context, r Record) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.command_audit
		    (xuid, gamertag, permission, command, args, outcome, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.XUID, r.Gamertag, r.Permission, r.Command, r.Args, string(r.Outcome), r.At,
	); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}
```

- [ ] **Step 6: Write the live-database test**

`internal/audit/postgres_live_test.go`, following the convention in `internal/knowledge/postgres_live_test.go` — read that file first and mirror its build tag, its `MC_TEST_DSN` helper and its doc-comment voice. Write the justification in your own words, about this package: no query here has ever run against a real database, and the outcome values must satisfy a CHECK constraint that only a real database enforces.

```go
//go:build livedb

func TestWriteAcceptsEveryOutcomeTheSchemaAllows(t *testing.T) {
	ctx := context.Background()
	s := NewPostgres(livePool(t))
	t.Cleanup(func() {
		_, _ = livePool(t).Exec(ctx, `DELETE FROM minecraft.command_audit WHERE xuid = '__test'`)
	})

	for _, out := range []Outcome{
		OutcomeOK, OutcomeDenied, OutcomeUnknown,
		OutcomeError, OutcomeRateLimited, OutcomeTimeout,
	} {
		if err := s.Write(ctx, Record{
			XUID: "__test", Gamertag: "__test", Permission: "visitor",
			Command: "probe", Args: "", Outcome: out, At: time.Now(),
		}); err != nil {
			t.Fatalf("outcome %q rejected by the schema: %v", out, err)
		}
	}
}
```

- [ ] **Step 7: Run the tests**

Run: `go test ./internal/audit/ -v` then `go vet -tags livedb ./internal/audit/`
Expected: unit tests pass; the live test compiles and is excluded without the tag.

- [ ] **Step 8: Commit**

```bash
git add internal/audit
git commit -m "feat(audit): record command dispatches durably

Command history existed only in pod stdout, which dies with the pod and
cannot be queried. Reply text is deliberately not recorded: replies carry
coordinates the agent whispers on purpose.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Wire the audit into command dispatch

**Files:**
- Modify: `cmd/agent/main.go` (`handleCommand`, around lines 692-720)
- Modify: `internal/plugin/plugin.go` (add `ErrCommandTimedOut` mapping is already present; no change expected — verify)
- Test: `cmd/agent/main_test.go` or the existing command-flow test file — read `cmd/agent/chat_flow_test.go` first and extend it

**Interfaces:**
- Consumes: `audit.Store`, `audit.Record`, `audit.Outcome` constants from Task 2.
- Produces: every path out of `handleCommand` writes exactly one audit record.

- [ ] **Step 1: Write the failing test**

Add to the command-flow test file. Use the existing harness in that file — read how it builds a registry, a `plugin.Context` and a recording voice, and follow it rather than inventing a second harness.

```go
func TestEveryCommandOutcomeIsAudited(t *testing.T) {
	cases := []struct {
		name    string
		command string
		perm    plugin.Permission
		want    audit.Outcome
	}{
		{"successful command", "ping", plugin.PermissionVisitor, audit.OutcomeOK},
		{"unknown command", "nosuchcommand", plugin.PermissionVisitor, audit.OutcomeUnknown},
		{"denied command", "operatoronly", plugin.PermissionVisitor, audit.OutcomeDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingAudit{}
			// harness wiring: build the registry with ping and an
			// operator-only command, hand it rec as the audit store
			// ... dispatch tc.command as an actor at tc.perm ...
			if len(rec.records) != 1 {
				t.Fatalf("got %d audit records, want exactly 1", len(rec.records))
			}
			if rec.records[0].Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", rec.records[0].Outcome, tc.want)
			}
			if rec.records[0].Reply() != "" {
				t.Error("audit recorded reply text, which it must never do")
			}
		})
	}
}

func TestAFailingAuditWriteDoesNotFailTheCommand(t *testing.T) {
	// A compliance record that can take the server down is a worse
	// liability than a gap in the record.
	rec := &recordingAudit{err: errors.New("database on fire")}
	// ... dispatch !ping with rec as the audit store ...
	// assert the player still received "pong"
}
```

`recordingAudit` is a test double implementing `audit.Store`, collecting records in a slice and optionally returning a configured error. `Reply()` does not exist on `audit.Record` — that assertion is written as a compile-time reminder that reply text has no field; replace it with a check that no field of the record contains the reply string.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./cmd/agent/ -run TestEveryCommandOutcome -v`
Expected: FAIL — no audit store is wired.

- [ ] **Step 3: Thread the audit store into handleCommand**

`handleCommand` gains an `auditor audit.Store` parameter and a gamertag resolved from the roster (it already has the actor XUID; the roster is reachable the same way `handleMention` reaches it — read how that function resolves a name and mirror it).

Record on every exit path:

```go
	// One record per dispatch, whatever happened. Written after the outcome
	// is known and never in front of the reply: an audit trail that can
	// delay or fail a command is a worse liability than a gap in it.
	writeAudit := func(outcome audit.Outcome) {
		if auditor == nil || !auditor.Enabled() {
			return
		}
		if err := auditor.Write(ctx, audit.Record{
			XUID:       actorXUID,
			Gamertag:   gamertag,
			Permission: actorPermission.String(),
			Command:    trigger.Command,
			Args:       strings.Join(trigger.Args, " "),
			Outcome:    outcome,
			At:         time.Now(),
		}); err != nil {
			log.Error("audit_write_failed", logging.Fields{
				"command": trigger.Command, "actor": actorXUID, "error": err.Error(),
			})
		}
	}
```

Call it with `audit.OutcomeRateLimited` in the limiter branch, `OutcomeUnknown`, `OutcomeDenied`, `OutcomeTimeout` (when the error is `plugin.ErrCommandTimedOut`), `OutcomeError` for any other error, and `OutcomeOK` on the success path.

Order matters: check `ErrCommandTimedOut` before the generic `err != nil` branch, or a timeout records as a plain error.

- [ ] **Step 4: Run the tests**

Run: `go test ./cmd/agent/ -v`
Expected: PASS, including the pre-existing flow tests.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent
git commit -m "feat(agent): audit every command dispatch

Every exit path records one row, including the rate-limited and denied
paths that produce no chat reply and were previously invisible outside
stdout.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: `internal/announce` — domain types and expiry

**Files:**
- Create: `internal/announce/announce.go`
- Test: `internal/announce/announce_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `announce.Target` string type: `TargetEveryone`, `TargetPlayer`, `TargetPermission`, `TargetOnlineOnly`
  - `announce.Priority`: `PriorityNormal`, `PriorityExpedited`
  - `announce.Source`: `SourceCommand`, `SourceSchedule`, `SourceEvent`, `SourceAPI`
  - `announce.Delivery`: `DeliveryBroadcast`, `DeliveryWhisper`
  - `announce.Announcement{ID int64; Body string; Source Source; AuthorXUID string; TargetKind Target; TargetValue string; Priority Priority; Delivery Delivery; CreatedAt, DeliverAfter time.Time; ExpiresAt *time.Time}`
  - `announce.DeliveryFor(t Target) Delivery`
  - `announce.DefaultExpiry(s Source, t Target, now time.Time) *time.Time`
  - `announce.Queues(t Target) bool`
  - `announce.MaxNormalPerJoin = 3`

- [ ] **Step 1: Write the failing test**

`internal/announce/announce_test.go`:

```go
package announce

import (
	"testing"
	"time"
)

func TestDeliveryFollowsTheTarget(t *testing.T) {
	// A player-targeted announcement that broadcast would leak exactly what
	// whispering a waypoint protects, so this is derived, never chosen.
	cases := map[Target]Delivery{
		TargetEveryone:   DeliveryBroadcast,
		TargetOnlineOnly: DeliveryBroadcast,
		TargetPlayer:     DeliveryWhisper,
		TargetPermission: DeliveryWhisper,
	}
	for target, want := range cases {
		if got := DeliveryFor(target); got != want {
			t.Errorf("DeliveryFor(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestOnlineOnlyNeverQueues(t *testing.T) {
	if Queues(TargetOnlineOnly) {
		t.Error("online_only queued; a countdown delivered later is noise")
	}
	for _, target := range []Target{TargetEveryone, TargetPlayer, TargetPermission} {
		if !Queues(target) {
			t.Errorf("%q does not queue, so an offline player never hears it", target)
		}
	}
}

func TestDefaultExpiryVariesByWhatTheMessageIsFor(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	if got := DefaultExpiry(SourceCommand, TargetOnlineOnly, now); got != nil {
		t.Errorf("online_only got an expiry of %v; it never queues, so it needs none", got)
	}

	whisper := DefaultExpiry(SourceCommand, TargetPlayer, now)
	if whisper == nil || !whisper.Equal(now.Add(7*24*time.Hour)) {
		t.Errorf("player whisper expiry = %v, want 7 days out", whisper)
	}

	broadcast := DefaultExpiry(SourceCommand, TargetEveryone, now)
	if broadcast == nil || !broadcast.Equal(now.Add(24*time.Hour)) {
		t.Errorf("broadcast expiry = %v, want 24 hours out", broadcast)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/announce/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/announce/announce.go`**

```go
// Package announce is what the server wants said, and to whom.
//
// The types here are deliberately dumb: a source's whole job is to describe
// an announcement correctly, and every question about who sees it, when, and
// whether it is still worth saying belongs to the deliverer. That split is
// what keeps a fourth source as cheap as the first.
package announce

import "time"

type Target string

const (
	TargetEveryone   Target = "everyone"
	TargetPlayer     Target = "player"
	TargetPermission Target = "permission"
	TargetOnlineOnly Target = "online_only"
)

type Priority string

const (
	PriorityNormal    Priority = "normal"
	PriorityExpedited Priority = "expedited"
)

type Source string

const (
	SourceCommand  Source = "command"
	SourceSchedule Source = "schedule"
	SourceEvent    Source = "event"
	SourceAPI      Source = "api"
)

type Delivery string

const (
	DeliveryBroadcast Delivery = "broadcast"
	DeliveryWhisper   Delivery = "whisper"
)

// MaxNormalPerJoin caps ordinary messages delivered at one login. Chat is
// scarce and the join moment is already contested by the welcome message; a
// wall of text reads worse than a trickle, and !inbox gives the remainder
// to whoever wants it now.
const MaxNormalPerJoin = 3

// Announcement is one thing the server wants said.
type Announcement struct {
	ID           int64
	Body         string
	Source       Source
	AuthorXUID   string
	TargetKind   Target
	TargetValue  string
	Priority     Priority
	Delivery     Delivery
	CreatedAt    time.Time
	DeliverAfter time.Time
	// ExpiresAt nil means it never expires.
	ExpiresAt *time.Time
}

// DeliveryFor derives how an announcement is said from who it is for.
// Derived rather than chosen: a player-targeted message that broadcast
// would expose precisely what whispering protects.
func DeliveryFor(t Target) Delivery {
	switch t {
	case TargetPlayer, TargetPermission:
		return DeliveryWhisper
	default:
		return DeliveryBroadcast
	}
}

// Queues reports whether an undelivered announcement of this kind waits for
// a player to return.
func Queues(t Target) bool { return t != TargetOnlineOnly }

// DefaultExpiry is how long a message of this kind stays worth saying.
//
// This is the difference between a queue and a nag. A restart countdown is
// meaningless the moment it has passed; a farm relocation whispered to one
// player is still true next week.
func DefaultExpiry(s Source, t Target, now time.Time) *time.Time {
	if !Queues(t) {
		return nil
	}
	var d time.Duration
	switch {
	case t == TargetPlayer:
		d = 7 * 24 * time.Hour
	case s == SourceEvent:
		d = 24 * time.Hour
	default:
		d = 24 * time.Hour
	}
	at := now.Add(d)
	return &at
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/announce/ -v`
Expected: PASS, all three.

- [ ] **Step 5: Commit**

```bash
git add internal/announce
git commit -m "feat(announce): describe what the server wants said

Delivery is derived from the target rather than chosen, so a
player-targeted message cannot be broadcast by mistake.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: `internal/announce` Postgres store

**Files:**
- Create: `internal/announce/postgres.go`
- Test: `internal/announce/postgres_live_test.go`

**Interfaces:**
- Consumes: `*pgxpool.Pool`; the types from Task 4.
- Produces:
  - `announce.Store` interface:
    - `Insert(ctx context.Context, a Announcement) (int64, error)`
    - `PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]Announcement, error)`
    - `MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error`
    - `Enabled() bool`
  - `announce.Nop{}`, `announce.NewPostgres(pool *pgxpool.Pool) *announce.Postgres`

- [ ] **Step 1: Write `internal/announce/postgres.go`**

`PendingFor` is the load-bearing query. It returns everything this player has not yet received, that has not expired, whose `deliver_after` has passed, and whose target includes them — expedited first, then oldest first, so the caller can take the head of the list without re-sorting.

```go
package announce

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store interface {
	Insert(ctx context.Context, a Announcement) (int64, error)
	PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]Announcement, error)
	MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error
	Enabled() bool
}

type Nop struct{}

var _ Store = Nop{}

func (Nop) Insert(context.Context, Announcement) (int64, error) { return 0, nil }
func (Nop) PendingFor(context.Context, string, string, time.Time) ([]Announcement, error) {
	return nil, nil
}
func (Nop) MarkDelivered(context.Context, int64, string, time.Time) error { return nil }
func (Nop) Enabled() bool                                                 { return false }

type Postgres struct{ pool *pgxpool.Pool }

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

func (p *Postgres) Insert(ctx context.Context, a Announcement) (int64, error) {
	var author any
	if a.AuthorXUID != "" {
		author = a.AuthorXUID
	}
	var target any
	if a.TargetValue != "" {
		target = a.TargetValue
	}
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO minecraft.announcements
		    (body, source, author_xuid, target_kind, target_value,
		     priority, delivery, deliver_after, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING announcement_id`,
		a.Body, string(a.Source), author, string(a.TargetKind), target,
		string(a.Priority), string(a.Delivery), a.DeliverAfter, a.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("announce: insert: %w", err)
	}
	return id, nil
}

// PendingFor returns what this player has not yet heard.
//
// online_only is excluded by construction rather than by expiry: it has no
// queue at all, so a countdown cannot resurface hours later just because
// nobody set an expiry on it.
func (p *Postgres) PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]Announcement, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT a.announcement_id, a.body, a.source, COALESCE(a.author_xuid, ''),
		       a.target_kind, COALESCE(a.target_value, ''), a.priority,
		       a.delivery, a.created_at, a.deliver_after, a.expires_at
		FROM minecraft.announcements a
		WHERE a.target_kind <> 'online_only'
		  AND a.deliver_after <= $3
		  AND (a.expires_at IS NULL OR a.expires_at > $3)
		  AND (a.target_kind = 'everyone'
		       OR (a.target_kind = 'player' AND a.target_value = $1)
		       OR (a.target_kind = 'permission' AND a.target_value = $2))
		  AND NOT EXISTS (
		        SELECT 1 FROM minecraft.announcement_deliveries d
		        WHERE d.announcement_id = a.announcement_id AND d.xuid = $1)
		ORDER BY (a.priority = 'expedited') DESC, a.created_at ASC`,
		xuid, permission, now,
	)
	if err != nil {
		return nil, fmt.Errorf("announce: pending: %w", err)
	}
	defer rows.Close()

	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Body, &a.Source, &a.AuthorXUID,
			&a.TargetKind, &a.TargetValue, &a.Priority, &a.Delivery,
			&a.CreatedAt, &a.DeliverAfter, &a.ExpiresAt); err != nil {
			return nil, fmt.Errorf("announce: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkDelivered is idempotent: two deliverers racing the same join must not
// make the second one fail, and a conflict means the player already has it.
func (p *Postgres) MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.announcement_deliveries (announcement_id, xuid, delivered_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (announcement_id, xuid) DO NOTHING`,
		id, xuid, at,
	); err != nil {
		return fmt.Errorf("announce: mark delivered: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Write the live-database test**

`internal/announce/postgres_live_test.go`, behind `//go:build livedb`, mirroring the helper and voice of `internal/knowledge/postgres_live_test.go` with its own justification. Cover, using a real XUID discovered from `minecraft.players` the way the waypoint live test does:

- Insert a `player`-targeted announcement, assert `PendingFor` returns it.
- `MarkDelivered`, assert `PendingFor` no longer returns it.
- `MarkDelivered` twice, assert no error the second time.
- Insert one already expired, assert `PendingFor` excludes it.
- Insert an `online_only`, assert `PendingFor` excludes it.
- Insert an expedited and a normal, assert the expedited sorts first.

Clean up every row it inserts.

- [ ] **Step 3: Run the tests**

Run: `go test ./internal/announce/ -v` and `go vet -tags livedb ./internal/announce/`
Expected: unit tests pass; the live test compiles.

- [ ] **Step 4: Commit**

```bash
git add internal/announce
git commit -m "feat(announce): store the outbox and who has heard what

A delivery row is written even for a broadcast: without it, a player who
was present when it went out would receive it again on their next join.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: The deliverer

**Files:**
- Create: `internal/announce/deliver.go`
- Test: `internal/announce/deliver_test.go`

**Interfaces:**
- Consumes: `Store` (Task 5), the domain types (Task 4).
- Produces:
  - `announce.Voice` interface: `Tell(ctx context.Context, xuid, message string) error`, `Say(ctx context.Context, message string) error` — structurally identical to `plugin.Voice`, declared here so this package states its own dependency.
  - `announce.Roster` interface: `Online() []string` returning XUIDs.
  - `announce.Permissions` interface: `Resolve(ctx context.Context, xuid string) string`.
  - `announce.Deliverer` with `NewDeliverer(s Store, v Voice, r Roster, p Permissions, log Logger) *Deliverer`
  - `(*Deliverer).SendNow(ctx context.Context, a Announcement, id int64) (int, error)` — delivers to everyone matching who is online, returns how many received it.
  - `(*Deliverer).DrainForJoin(ctx context.Context, xuid string, now time.Time) (delivered int, remaining int, err error)`
  - `(*Deliverer).DrainAll(ctx context.Context, xuid string, now time.Time) (int, error)` — for `!inbox`, no cap.

- [ ] **Step 1: Write the failing tests**

`internal/announce/deliver_test.go`. Build fakes for `Store`, `Voice`, `Roster` and `Permissions` in this file — small structs collecting calls.

```go
func TestDrainForJoinCapsNormalButNotExpedited(t *testing.T) {
	// Expedited exists to bypass the cap; normal messages trickle so the
	// welcome message is not buried.
	pending := []Announcement{
		{ID: 1, Body: "urgent one", Priority: PriorityExpedited, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "urgent two", Priority: PriorityExpedited, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 3, Body: "normal one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 4, Body: "normal two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 5, Body: "normal three", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 6, Body: "normal four", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	// ... wire a fake store returning pending ...
	delivered, remaining, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 5 {
		t.Errorf("delivered = %d, want 5 (2 expedited uncapped + 3 normal capped)", delivered)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1", remaining)
	}
}

func TestAFailedSendIsNotRecordedAsDelivered(t *testing.T) {
	// Otherwise the message is lost: nothing retries it, and the delivery
	// row says the player already has it.
	// ... fake voice returning an error on Tell ...
	// ... assert the store recorded zero deliveries ...
}

func TestSendNowWritesOneDeliveryRowPerOnlinePlayerForABroadcast() {
	// The broadcast goes out once, but every player present must be
	// recorded, or they receive it again when they next join.
}

func TestSendNowSkipsPlayersWhoseTargetDoesNotIncludeThem() {
	// A permission-targeted announcement reaches only players resolving to
	// that level.
}
```

Write these four out in full when implementing, using the fakes; the bodies above show the assertions that matter.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/announce/ -run TestDrain -v`
Expected: FAIL — `Deliverer` does not exist.

- [ ] **Step 3: Implement `deliver.go`**

Key rules to encode, each with a comment saying why:

- `SendNow` resolves the audience: `everyone`/`online_only` is every online XUID; `player` is that one XUID if online; `permission` is every online XUID whose resolved level matches.
- A broadcast calls `Say` once, then writes a delivery row per online player. A whisper calls `Tell` per recipient and writes a row after each success.
- A send that returns an error is logged and **not** recorded as delivered, so a join retries it.
- `DrainForJoin` takes `PendingFor`, sends every expedited, then up to `MaxNormalPerJoin` normal ones, and returns how many are left.
- `DrainAll` is the same without the cap.
- Every method tolerates a disabled store by returning zero and no error.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/announce/ -v` then `go test -race ./internal/announce/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/announce
git commit -m "feat(announce): deliver to who is online and hold the rest

A send that fails is not recorded as delivered, so it is retried on the
next join rather than silently lost.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 7: `!announce` and `!inbox`

**Files:**
- Create: `internal/plugins/announce.go`
- Test: `internal/plugins/announce_test.go`
- Modify: `internal/plugin/plugin.go` (add the `Announcements` capability)

**Interfaces:**
- Consumes: `announce.Store`, `announce.Deliverer`, the domain types.
- Produces:
  - `plugin.AnnounceStore` interface, narrowed to what the plugins touch: `Insert`, `PendingFor`, `Enabled`
  - `plugin.Context.Announcements plugin.AnnounceStore` and `plugin.Context.Deliverer plugin.AnnounceDeliverer`
  - `plugins.NewAnnounce() *plugins.Announce` registering `announce` (operator) and `inbox` (member)

- [ ] **Step 1: Write the failing tests**

```go
func TestAnnounceRequiresOperator(t *testing.T) {
	// A member sees a plain refusal, and nothing is stored.
}

func TestAnnounceFlagsParseOnlyBeforeTheBody(t *testing.T) {
	// "!announce !urgent server restarting" is expedited.
	// "!announce the !urgent flag goes first" is a normal announcement
	// whose body contains the words, not a flag — the same ambiguity the
	// waypoint parser refuses to guess at.
}

func TestAnnounceToAPlayerWhispersAndQueues(t *testing.T) {
	// "!announce @Dotablaze the farm moved" targets that player, whispers,
	// and gets the seven-day expiry.
}

func TestAnnounceNowNeverQueues(t *testing.T) {
	// "!announce !now restarting in five" is online_only with no expiry.
}

func TestInboxDrainsOnlyTheCallersOwnQueue(t *testing.T) {
	// There is no argument naming another player, and the drain is called
	// with the caller's XUID.
}

func TestAnnounceWithoutAStoreSaysSo(t *testing.T) {
	// A nil or disabled store produces a plain reply, never a panic.
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/plugins/ -run 'TestAnnounce|TestInbox' -v`
Expected: FAIL — `NewAnnounce` undefined.

- [ ] **Step 3: Implement the plugin**

Command surface:

- `!announce <message>` — everyone, broadcast, 24h
- `!announce @<player> <message>` — that player, whisper, 7 days
- `!announce !now <message>` — online_only, no queue
- `!announce !urgent <message>` — expedited
- flags and `@player` may combine, and are consumed only while they appear before the first word of the body
- `!inbox` — member, drains the caller's own queue

Resolve `@<player>` to an XUID through the roster; refuse plainly if that name is not known, rather than storing an announcement nobody can ever receive.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/plugins/ -v`
Expected: PASS, including every pre-existing plugin test.

- [ ] **Step 5: Commit**

```bash
git add internal/plugins internal/plugin
git commit -m "feat(plugins): add !announce and !inbox

Flags are consumed only before the body, so a message that happens to
contain them is not silently reinterpreted.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 8: Join-time delivery

**Files:**
- Create: `internal/plugins/announcedrain.go`
- Test: `internal/plugins/announcedrain_test.go`

**Interfaces:**
- Consumes: `announce.Deliverer`, `roster.JoinEvent`, `roster.JoinKind`.
- Produces: `plugins.NewAnnounceDrain(...)` implementing `plugin.Plugin` and `plugin.EventHandler` with `Kinds() []string { return []string{roster.JoinKind} }` and no commands.

- [ ] **Step 1: Write the failing test**

```go
func TestJoinDeliversAndThenSummarises(t *testing.T) {
	// Six pending: two expedited, four normal. On join the player receives
	// the two expedited, three normal, then one line naming the remaining
	// one and !inbox.
}

func TestJoinWithNothingPendingSaysNothing(t *testing.T) {
	// The welcome message already owns this moment; an empty inbox must not
	// add a line to it.
}

func TestDrainRunsOffTheReadLoop(t *testing.T) {
	// HandleEvent must return promptly even when delivery is slow, because
	// the dispatcher that calls it is on the packet read loop.
}
```

- [ ] **Step 2: Run and watch fail**

Run: `go test ./internal/plugins/ -run TestJoin -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`HandleEvent` starts the drain in a goroutine and returns immediately — the event dispatcher runs on the read loop, and delivery makes bridge calls. Follow the pattern `cmd/agent/main.go` uses for mention answering: a bounded context, and nothing that can block the caller.

The summary line is sent only when `remaining > 0`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/plugins/ -v && go test -race ./internal/plugins/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/plugins
git commit -m "feat(plugins): deliver queued announcements on join

Off the read loop and capped: the welcome message already competes for
this moment, and a wall of text reads worse than a trickle.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 9: Wire it into the agent

**Files:**
- Modify: `cmd/agent/main.go`
- Test: `cmd/agent/chat_flow_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: a running agent with `!announce`, `!inbox`, join delivery and command auditing.

- [ ] **Step 1: Construct the stores**

Beside the existing knowledge and waypoint construction, build `audit` and `announce` stores from the same pool, falling back to their `Nop` implementations when no database is configured.

- [ ] **Step 2: Register the plugins**

Register `plugins.NewAnnounce()` and `plugins.NewAnnounceDrain(...)` alongside the existing five, matching the established error handling for a failed registration.

- [ ] **Step 3: Thread the auditor into handleCommand**

Task 3 changed the signature; update the call site.

- [ ] **Step 4: Keep the guardrail test passing**

`cmd/agent` has a test that reflects over every `plugin.Context` field and fails on a zero value. The two new capability fields must be wired to their `Nop` implementations when no database is configured, never left nil.

- [ ] **Step 5: Run everything**

Run: `go build ./... && go vet ./... && gofmt -l . && go test ./... && go test -race ./...`
Expected: all clean. The race run matters: join delivery is a new goroutine touching the roster and the voice.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent
git commit -m "feat(agent): wire announcements, the inbox and command audit

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

### Task 10: Documentation

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: a README that describes the commands, the delivery rules and the audit trail.

- [ ] **Step 1: Update the README**

Add `!announce` and `!inbox` to the command list with their permission levels. Describe, in the file's existing voice: that a message for an offline player waits and for how long, that expiry is what stops a stale message arriving, that expedited bypasses the join cap, and that the audit trail records who ran what but deliberately not the reply text.

Extend the `internal/` component list with `announce` and `audit`, explaining why each exists rather than what it contains.

- [ ] **Step 2: Verify and commit**

Run: `go build ./... && go test ./...`

```bash
git add README.md
git commit -m "docs: describe announcements, the inbox and the audit trail

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage:** data model (Task 1), audit store and wiring (Tasks 2-3), domain and decay (Task 4), outbox store (Task 5), delivery and targeting (Task 6), the command surface (Task 7), join drain with cap and summary (Task 8), wiring (Task 9), docs (Task 10). Broadcast deduplication is covered by Task 6's delivery-row rule and asserted in its tests. Failure behaviour is covered per task: no database in Tasks 2, 5, 7; failed send in Task 6; failed audit write in Task 3; off-the-read-loop in Task 8.

**Deliberate gap:** slices 2-4 (event-driven, scheduled and API sources) are not planned here. They were scoped out in the spec and are cheap once this exists.

**Type consistency:** `announce.Store`, `announce.Deliverer`, `audit.Store` and the `plugin.Context` capability fields are named identically everywhere they appear. `MaxNormalPerJoin` is defined once in Task 4 and referenced in Tasks 6 and 8.
