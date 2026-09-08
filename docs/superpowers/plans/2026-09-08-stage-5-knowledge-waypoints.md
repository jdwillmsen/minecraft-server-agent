# Stage 5 — Knowledge and Waypoints Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the `@server` answer path read-only tools over curated server knowledge and per-player waypoints, so the model answers from stored facts instead of inventing them.

**Architecture:** Two new Postgres-backed stores (`internal/knowledge`, `internal/waypoints`) are reachable two ways: through `!kb` / `!wp` commands with permission checks, and through a read-only tool registry (`internal/tools`) that the LLM client offers to the model in a bounded 2-round tool-calling loop. The model gets no write tool. Mention answering moves off the Bedrock packet read loop into its own goroutine, because a multi-round loop on that thread would deafen the agent.

**Tech Stack:** Go 1.24, `pgx/v5` + `pgxpool`, `sandertv/gophertunnel`, stdlib `net/http` and `net/http/httptest`, Flyway 11.2 for migrations, Helm for deployment.

**Spec:** `docs/superpowers/specs/2026-09-08-stage-5-knowledge-waypoints-design.md`

## Global Constraints

- Identity is the XUID, never the gamertag. Every store key, tool argument and permission check uses XUID.
- The model has no write path. No tool mutates state; every mutation goes through a `!` command with a resolved actor permission.
- `MaxReplyChars = 200`, `MaxQuestionChars = 256` (`internal/adapters/llm.go`) are unchanged and still apply to the final answer.
- Every new capability on `plugin.Context` is nil-tolerant. Plugins must guard; a capability documented as never-nil and believed once panicked the event dispatcher.
- No database configured is a supported state, not a degraded one. Stores degrade to `Nop`, tools are unregistered, commands say they are unconfigured, and everything else works.
- Tool-loop cap: 2 tool rounds, then one final call with tools omitted.
- New config: `LLM_TOTAL_TIMEOUT_MS`, default 20000. `LLM_TIMEOUT_MS` (default 8000) keeps bounding each individual call.
- `LLM_MAX_TOKENS` default rises 96 to 192.
- Dimension values are exactly `overworld`, `nether`, `end`.
- One connection pool. Both new stores take the `*pgxpool.Pool` that `store.Open` already created (`MaxConns = 4`); they do not open their own. This is an amendment to the spec, which did not say where the pool comes from.
- Commit after every task. Run `go build ./... && go vet ./... && go test ./...` before each commit.

---

### Task 1: Platform migration V3

Runs in the **platform** repo (`~/projects/jdwlabs/platform`), not this one. It must merge and sync before Task 2's live-DB tests can pass.

**Files:**
- Modify: `tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml`

**Interfaces:**
- Consumes: nothing.
- Produces: tables `minecraft.knowledge` and `minecraft.waypoints`, readable and writable by the `app` role.

- [ ] **Step 1: Create a worktree for the platform change**

```bash
cd ~/projects/jdwlabs/platform
git fetch origin
git worktree add -b feat/minecraft-knowledge-waypoints-schema \
  ~/worktrees/platform/minecraft-knowledge-waypoints origin/main
```

- [ ] **Step 2: Add the V3 migration to the ConfigMap**

Append to `data:` in `migrations-configmap.yaml`, after the `V2__fix_permissions.sql` block, at the same indentation:

```yaml
  V3__minecraft_knowledge.sql: |
    -- Curated server facts the @server answer path reads.
    --
    -- Operator-written on purpose: everything knowledge_lookup returns is
    -- quoted to players as the server's own word, so the write side is gated
    -- on the same permissions.json the ! commands already resolve against.
    CREATE TABLE IF NOT EXISTS minecraft.knowledge
    (
        topic       TEXT        NOT NULL PRIMARY KEY,
        body        TEXT        NOT NULL,
        -- SET NULL, not CASCADE: deleting a player must not delete the server
        -- rules they happened to write.
        author_xuid TEXT        REFERENCES minecraft.players (xuid) ON DELETE SET NULL,
        created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
        search      TSVECTOR    GENERATED ALWAYS AS
            (to_tsvector('english', topic || ' ' || body)) STORED
    );

    CREATE INDEX IF NOT EXISTS knowledge_search_idx
        ON minecraft.knowledge USING GIN (search);

    -- Personal named coordinates. CASCADE here, unlike knowledge above: these
    -- are the player's own data and go with them.
    CREATE TABLE IF NOT EXISTS minecraft.waypoints
    (
        xuid       TEXT        NOT NULL REFERENCES minecraft.players (xuid) ON DELETE CASCADE,
        name       TEXT        NOT NULL,
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

No grants block accompanies this. `V2__fix_permissions.sql` set `ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA minecraft ... TO app` so that exactly this migration would not need one. Step 6 verifies that held.

- [ ] **Step 3: Validate the YAML parses**

Run: `python3 -c "import yaml,sys; d=yaml.safe_load(open('tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml')); print(sorted(d['data']))"`
Expected: `['V1__minecraft_init.sql', 'V2__fix_permissions.sql', 'V3__minecraft_knowledge.sql']`

- [ ] **Step 4: Commit and open the PR**

```bash
git add tenants/jdwillmsen/services/jdwillmsen-schemas/postInstall/migrations-configmap.yaml
git commit -m "feat(jdwillmsen-schemas): add minecraft knowledge and waypoint tables"
git push -u origin feat/minecraft-knowledge-waypoints-schema
gh pr create --fill
```

This path is codeowner-gated and cannot be self-merged. Stop here until it is approved and merged.

- [ ] **Step 5: Confirm ArgoCD ran the migration**

Run: `kubectl get jobs -n database | grep flyway-jdwillmsen`
Expected: the job shows `1/1` completions after the sync.

- [ ] **Step 6: Verify the app role can write both tables**

This is the step that proves V2's default privileges did their job. Run in a shell with the superuser secret, against `jdwillmsen_prd`:

```sql
SET ROLE app;
INSERT INTO minecraft.knowledge (topic, body) VALUES ('__probe', 'probe');
DELETE FROM minecraft.knowledge WHERE topic = '__probe';
```

Expected: both statements succeed. A `permission denied` here means the default-privileges grant did not cover new tables and a V4 grants migration is required before Task 2 can pass.

---

### Task 2: `internal/knowledge` store

**Files:**
- Create: `internal/knowledge/knowledge.go`
- Create: `internal/knowledge/postgres.go`
- Test: `internal/knowledge/knowledge_test.go`
- Test: `internal/knowledge/postgres_livedb_test.go`

**Interfaces:**
- Consumes: `*pgxpool.Pool` from `store.Postgres` (Task 5 adds the accessor; until then the live test opens its own pool from `PG_*` env).
- Produces:
  - `knowledge.Entry{Topic, Body, AuthorXUID string; UpdatedAt time.Time}`
  - `knowledge.Store` interface: `Lookup(ctx, query string, limit int) ([]Entry, error)`, `Get(ctx, topic string) (Entry, bool, error)`, `Upsert(ctx, topic, body, authorXUID string) error`, `Delete(ctx, topic string) error`, `List(ctx) ([]Entry, error)`, `Enabled() bool`
  - `knowledge.Nop{}` implementing `Store`
  - `knowledge.NewPostgres(pool *pgxpool.Pool) *knowledge.Postgres`
  - `knowledge.NormalizeTopic(s string) string`

- [ ] **Step 1: Write the failing test for topic normalisation and Nop**

`internal/knowledge/knowledge_test.go`:

```go
package knowledge

import (
	"context"
	"testing"
)

func TestNormalizeTopic(t *testing.T) {
	cases := map[string]string{
		"Gold Farm":  "gold farm",
		"  RULES  ":  "rules",
		"Nether\tHub": "nether hub",
		"":           "",
	}
	for in, want := range cases {
		if got := NormalizeTopic(in); got != want {
			t.Errorf("NormalizeTopic(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNopAnswersNothingKnown(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Fatal("Nop reports itself enabled")
	}
	entries, err := s.Lookup(context.Background(), "anything", 3)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Lookup returned %d entries, want 0", len(entries))
	}
	if _, found, err := s.Get(context.Background(), "rules"); err != nil || found {
		t.Fatalf("Get = (found %v, err %v), want (false, nil)", found, err)
	}
	if err := s.Upsert(context.Background(), "rules", "be nice", "xuid"); err != nil {
		t.Fatalf("Upsert on Nop must not error: %v", err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/knowledge/ -run 'TestNormalizeTopic|TestNop' -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write `internal/knowledge/knowledge.go`**

```go
// Package knowledge stores the curated facts the agent is allowed to state
// as the server's own word.
//
// Separate from internal/store, which owns presence: these rows are written
// by operators through a command and read by the LLM answer path, and
// keeping them apart means the answer path cannot reach player sessions.
package knowledge

import (
	"context"
	"strings"
	"time"
)

// Entry is one curated fact.
type Entry struct {
	Topic string
	Body  string
	// AuthorXUID is empty when the author's player row has since been
	// deleted -- the fact outlives the account that wrote it.
	AuthorXUID string
	UpdatedAt  time.Time
}

// Store reads and writes curated facts. Every method must tolerate being
// called on a disabled implementation.
type Store interface {
	// Lookup returns the entries best matching a free-text query, most
	// relevant first, capped at limit.
	Lookup(ctx context.Context, query string, limit int) ([]Entry, error)
	// Get returns one entry by exact topic. found is false when there is no
	// such topic, which is not an error.
	Get(ctx context.Context, topic string) (entry Entry, found bool, err error)
	Upsert(ctx context.Context, topic, body, authorXUID string) error
	Delete(ctx context.Context, topic string) error
	List(ctx context.Context) ([]Entry, error)
	Enabled() bool
}

// NormalizeTopic is the single definition of what makes two topics the same
// one. Applied on every write and every read, so "Gold Farm" typed by an
// operator and "gold farm" asked by a player reach the same row.
func NormalizeTopic(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Lookup(context.Context, string, int) ([]Entry, error)     { return nil, nil }
func (Nop) Get(context.Context, string) (Entry, bool, error)         { return Entry{}, false, nil }
func (Nop) Upsert(context.Context, string, string, string) error     { return nil }
func (Nop) Delete(context.Context, string) error                     { return nil }
func (Nop) List(context.Context) ([]Entry, error)                    { return nil, nil }
func (Nop) Enabled() bool                                            { return false }
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `go test ./internal/knowledge/ -v`
Expected: PASS.

- [ ] **Step 5: Write `internal/knowledge/postgres.go`**

```go
package knowledge

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgxpool"
)

// Postgres persists to minecraft.knowledge.
//
// Takes the pool the profile store already opened rather than opening its
// own: this workload is a handful of rows read per question, and a second
// pool would reserve connections on a shared cluster to sit idle.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

// Lookup ranks by Postgres full-text relevance and falls back to a prefix
// match on the topic, so a player asking "gold" still finds "gold farm" when
// the body shares no stemmed words with the question.
func (p *Postgres) Lookup(ctx context.Context, query string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 3
	}
	normalized := NormalizeTopic(query)
	if normalized == "" {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT topic, body, COALESCE(author_xuid, ''), updated_at
		FROM minecraft.knowledge
		WHERE search @@ plainto_tsquery('english', $1)
		   OR topic LIKE $2
		ORDER BY ts_rank(search, plainto_tsquery('english', $1)) DESC, updated_at DESC
		LIMIT $3`,
		normalized, normalized+"%", limit,
	)
	if err != nil {
		return nil, fmt.Errorf("knowledge: lookup: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Topic, &e.Body, &e.AuthorXUID, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("knowledge: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) Get(ctx context.Context, topic string) (Entry, bool, error) {
	var e Entry
	err := p.pool.QueryRow(ctx, `
		SELECT topic, body, COALESCE(author_xuid, ''), updated_at
		FROM minecraft.knowledge WHERE topic = $1`,
		NormalizeTopic(topic),
	).Scan(&e.Topic, &e.Body, &e.AuthorXUID, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("knowledge: get: %w", err)
	}
	return e, true, nil
}

func (p *Postgres) Upsert(ctx context.Context, topic, body, authorXUID string) error {
	// author_xuid is written as NULL rather than '' when unknown: the column
	// is a foreign key, and '' is not a player.
	var author any
	if authorXUID != "" {
		author = authorXUID
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.knowledge (topic, body, author_xuid)
		VALUES ($1, $2, $3)
		ON CONFLICT (topic) DO UPDATE
		SET body = EXCLUDED.body, author_xuid = EXCLUDED.author_xuid, updated_at = now()`,
		NormalizeTopic(topic), body, author,
	)
	if err != nil {
		return fmt.Errorf("knowledge: upsert: %w", err)
	}
	return nil
}

func (p *Postgres) Delete(ctx context.Context, topic string) error {
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM minecraft.knowledge WHERE topic = $1`, NormalizeTopic(topic),
	); err != nil {
		return fmt.Errorf("knowledge: delete: %w", err)
	}
	return nil
}

func (p *Postgres) List(ctx context.Context) ([]Entry, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT topic, body, COALESCE(author_xuid, ''), updated_at
		FROM minecraft.knowledge ORDER BY topic`)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Topic, &e.Body, &e.AuthorXUID, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("knowledge: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
```

Fix the import path if `go build` complains: the pool package is `github.com/jackc/pgx/v5/pgxpool`, matching `internal/store/postgres.go`.

- [ ] **Step 6: Write the live-database test**

`internal/knowledge/postgres_livedb_test.go`, following the pattern the profile store's live test already uses (skip unless `PG_HOST` is set):

```go
package knowledge

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	host := os.Getenv("PG_HOST")
	if host == "" {
		t.Skip("PG_HOST not set; skipping live database test")
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s",
		os.Getenv("PG_USERNAME"), os.Getenv("PG_PASSWORD"),
		host, os.Getenv("PG_PORT"), os.Getenv("PG_DATABASE"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewPostgres(livePool(t))
	topic := "__test gold farm"
	t.Cleanup(func() { _ = s.Delete(ctx, topic) })

	if err := s.Upsert(ctx, "__TEST  Gold Farm", "It is under spawn at y 12.", ""); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, found, err := s.Get(ctx, topic)
	if err != nil || !found {
		t.Fatalf("Get = (found %v, err %v), want (true, nil)", found, err)
	}
	if got.Body != "It is under spawn at y 12." {
		t.Fatalf("Body = %q", got.Body)
	}

	entries, err := s.Lookup(ctx, "gold farm", 3)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("Lookup found nothing for a topic that exists")
	}
}
```

- [ ] **Step 7: Run both tests**

Run: `go test ./internal/knowledge/ -v` (unit only), then with the live database:
`PG_HOST=... PG_PORT=5432 PG_DATABASE=jdwillmsen_prd PG_USERNAME=app PG_PASSWORD=... go test ./internal/knowledge/ -run TestPostgresRoundTrip -v`
Expected: PASS both. A `permission denied` on the live run means Task 1 Step 6 was not really verified.

- [ ] **Step 8: Commit**

```bash
git add internal/knowledge
git commit -m "feat(knowledge): store curated server facts

The @server path had no source of server-specific truth, so it invented
one. Full-text lookup with a topic-prefix fallback, because a player
asking 'gold' shares no stemmed words with a body about spawn coordinates."
```

---

### Task 3: `internal/waypoints` store

**Files:**
- Create: `internal/waypoints/waypoints.go`
- Create: `internal/waypoints/postgres.go`
- Test: `internal/waypoints/waypoints_test.go`
- Test: `internal/waypoints/postgres_livedb_test.go`

**Interfaces:**
- Consumes: `*pgxpool.Pool`, as Task 2.
- Produces:
  - `waypoints.Waypoint{Name string; X, Y, Z int; Dimension string; UpdatedAt time.Time}`
  - `waypoints.Store` interface: `Get(ctx, xuid, name string) (Waypoint, bool, error)`, `Set(ctx, xuid string, wp Waypoint) error`, `Delete(ctx, xuid, name string) error`, `List(ctx, xuid string) ([]Waypoint, error)`, `Enabled() bool`
  - `waypoints.Nop{}`, `waypoints.NewPostgres(pool *pgxpool.Pool) *waypoints.Postgres`
  - `waypoints.NormalizeName(s string) string`
  - `waypoints.NormalizeDimension(s string) (string, error)`
  - `waypoints.ErrUnknownDimension`

- [ ] **Step 1: Write the failing test**

`internal/waypoints/waypoints_test.go`:

```go
package waypoints

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeDimension(t *testing.T) {
	ok := map[string]string{
		"":           "overworld",
		"Overworld":  "overworld",
		"NETHER":     "nether",
		"end":        "end",
		"the_nether": "nether",
		"the end":    "end",
	}
	for in, want := range ok {
		got, err := NormalizeDimension(in)
		if err != nil {
			t.Errorf("NormalizeDimension(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeDimension(%q) = %q, want %q", in, got, want)
		}
	}

	if _, err := NormalizeDimension("moon"); !errors.Is(err, ErrUnknownDimension) {
		t.Errorf("NormalizeDimension(\"moon\") err = %v, want ErrUnknownDimension", err)
	}
}

func TestNopStoresNothing(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Fatal("Nop reports itself enabled")
	}
	if err := s.Set(context.Background(), "xuid", Waypoint{Name: "base"}); err != nil {
		t.Fatalf("Set on Nop must not error: %v", err)
	}
	if _, found, err := s.Get(context.Background(), "xuid", "base"); err != nil || found {
		t.Fatalf("Get = (found %v, err %v), want (false, nil)", found, err)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/waypoints/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/waypoints/waypoints.go`**

```go
// Package waypoints stores each player's own named coordinates.
//
// Per-player by design: a waypoint is a base location, and a shared
// namespace would both collide on names and hand every player everyone
// else's coordinates.
package waypoints

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Waypoint is one named coordinate belonging to one player.
type Waypoint struct {
	Name      string
	X, Y, Z   int
	Dimension string
	UpdatedAt time.Time
}

// Store holds waypoints. Every method must tolerate being called on a
// disabled implementation.
type Store interface {
	Get(ctx context.Context, xuid, name string) (wp Waypoint, found bool, err error)
	Set(ctx context.Context, xuid string, wp Waypoint) error
	Delete(ctx context.Context, xuid, name string) error
	List(ctx context.Context, xuid string) ([]Waypoint, error)
	Enabled() bool
}

// ErrUnknownDimension is returned for a dimension the schema's CHECK
// constraint would reject. Caught here so a typo is a chat reply rather
// than a database error.
var ErrUnknownDimension = errors.New("unknown dimension")

func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NormalizeDimension maps what players actually type onto the three values
// the schema allows. An empty dimension means the overworld, which is where
// almost every waypoint is.
func NormalizeDimension(s string) (string, error) {
	switch NormalizeName(s) {
	case "", "overworld", "the overworld":
		return "overworld", nil
	case "nether", "the nether", "the_nether":
		return "nether", nil
	case "end", "the end", "the_end":
		return "end", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownDimension, s)
	}
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Get(context.Context, string, string) (Waypoint, bool, error) {
	return Waypoint{}, false, nil
}
func (Nop) Set(context.Context, string, Waypoint) error          { return nil }
func (Nop) Delete(context.Context, string, string) error         { return nil }
func (Nop) List(context.Context, string) ([]Waypoint, error)     { return nil, nil }
func (Nop) Enabled() bool                                        { return false }
```

- [ ] **Step 4: Run the test and watch it pass**

Run: `go test ./internal/waypoints/ -v`
Expected: PASS.

- [ ] **Step 5: Write `internal/waypoints/postgres.go`**

```go
package waypoints

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists to minecraft.waypoints, sharing the profile store's pool.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

func (p *Postgres) Get(ctx context.Context, xuid, name string) (Waypoint, bool, error) {
	var wp Waypoint
	err := p.pool.QueryRow(ctx, `
		SELECT name, x, y, z, dimension, updated_at
		FROM minecraft.waypoints WHERE xuid = $1 AND name = $2`,
		xuid, NormalizeName(name),
	).Scan(&wp.Name, &wp.X, &wp.Y, &wp.Z, &wp.Dimension, &wp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Waypoint{}, false, nil
	}
	if err != nil {
		return Waypoint{}, false, fmt.Errorf("waypoints: get: %w", err)
	}
	return wp, true, nil
}

// Set inserts the waypoint, requiring the player row to already exist -- it
// does, because a player must be online to run the command and joining
// records them.
func (p *Postgres) Set(ctx context.Context, xuid string, wp Waypoint) error {
	dimension, err := NormalizeDimension(wp.Dimension)
	if err != nil {
		return err
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.waypoints (xuid, name, x, y, z, dimension)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (xuid, name) DO UPDATE
		SET x = EXCLUDED.x, y = EXCLUDED.y, z = EXCLUDED.z,
		    dimension = EXCLUDED.dimension, updated_at = now()`,
		xuid, NormalizeName(wp.Name), wp.X, wp.Y, wp.Z, dimension,
	); err != nil {
		return fmt.Errorf("waypoints: set: %w", err)
	}
	return nil
}

func (p *Postgres) Delete(ctx context.Context, xuid, name string) error {
	if _, err := p.pool.Exec(ctx,
		`DELETE FROM minecraft.waypoints WHERE xuid = $1 AND name = $2`,
		xuid, NormalizeName(name),
	); err != nil {
		return fmt.Errorf("waypoints: delete: %w", err)
	}
	return nil
}

func (p *Postgres) List(ctx context.Context, xuid string) ([]Waypoint, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT name, x, y, z, dimension, updated_at
		FROM minecraft.waypoints WHERE xuid = $1 ORDER BY name`, xuid)
	if err != nil {
		return nil, fmt.Errorf("waypoints: list: %w", err)
	}
	defer rows.Close()

	var out []Waypoint
	for rows.Next() {
		var wp Waypoint
		if err := rows.Scan(&wp.Name, &wp.X, &wp.Y, &wp.Z, &wp.Dimension, &wp.UpdatedAt); err != nil {
			return nil, fmt.Errorf("waypoints: scan: %w", err)
		}
		out = append(out, wp)
	}
	return out, rows.Err()
}
```

- [ ] **Step 6: Write the live-database test proving isolation between players**

`internal/waypoints/postgres_livedb_test.go` — copy the `livePool` helper from Task 2 Step 6 verbatim into this package (it is unexported and per-package), then:

```go
func TestWaypointsAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	s := NewPostgres(livePool(t))

	// Both XUIDs must exist in minecraft.players; use two real ones already
	// recorded by the agent, discovered with:
	//   SELECT xuid FROM minecraft.players ORDER BY last_seen_at DESC LIMIT 2;
	owner, other := os.Getenv("TEST_XUID_A"), os.Getenv("TEST_XUID_B")
	if owner == "" || other == "" {
		t.Skip("TEST_XUID_A/TEST_XUID_B not set")
	}
	t.Cleanup(func() { _ = s.Delete(ctx, owner, "__test base") })

	if err := s.Set(ctx, owner, Waypoint{Name: "__TEST Base", X: 100, Y: 64, Z: -200}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, found, err := s.Get(ctx, owner, "__test base"); err != nil || !found {
		t.Fatalf("owner Get = (found %v, err %v), want (true, nil)", found, err)
	}
	if _, found, err := s.Get(ctx, other, "__test base"); err != nil || found {
		t.Fatalf("other player could read the waypoint: found %v, err %v", found, err)
	}
}
```

Add `"os"` to that file's imports.

- [ ] **Step 7: Run the tests**

Run: `go test ./internal/waypoints/ -v`, then the live variant with `PG_*` and `TEST_XUID_A`/`TEST_XUID_B` set.
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/waypoints
git commit -m "feat(waypoints): store per-player named coordinates

Bedrock has no waypoint of its own, so players keep base coordinates in
their heads or in chat scrollback. Scoped to the owning XUID: coordinates
are the one piece of player state that leaks into griefing when shared."
```

---

### Task 4: `internal/tools` registry

**Files:**
- Create: `internal/tools/tools.go`
- Test: `internal/tools/tools_test.go`

**Interfaces:**
- Consumes: nothing (the registry is generic; Task 8 populates it).
- Produces:
  - `tools.Tool{Name, Description string; Schema json.RawMessage; Invoke func(ctx context.Context, args json.RawMessage, caller string) (string, error)}`
  - `tools.Definition{Type string; Function FunctionDefinition}` — the OpenAI wire shape, JSON-tagged
  - `tools.NewRegistry(list ...Tool) *Registry`
  - `(*Registry).Definitions() []Definition`
  - `(*Registry).Invoke(ctx context.Context, name string, args json.RawMessage, caller string) (string, error)`
  - `(*Registry).Len() int`
  - `tools.ErrUnknownTool`
  - `tools.MaxToolResultChars = 400`

- [ ] **Step 1: Write the failing test**

`internal/tools/tools_test.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func testTool(name string, out string, err error) Tool {
	return Tool{
		Name:        name,
		Description: "test tool",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) {
			return out, err
		},
	}
}

func TestDefinitionsAreWireShaped(t *testing.T) {
	r := NewRegistry(testTool("players_online", "two players", nil))
	defs := r.Definitions()
	if len(defs) != 1 {
		t.Fatalf("Definitions len = %d, want 1", len(defs))
	}
	encoded, err := json.Marshal(defs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"type":"function"`, `"name":"players_online"`, `"parameters"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("definition JSON %s missing %s", encoded, want)
		}
	}
}

func TestInvokeUnknownTool(t *testing.T) {
	r := NewRegistry(testTool("known", "ok", nil))
	if _, err := r.Invoke(context.Background(), "nope", nil, "xuid"); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("Invoke unknown err = %v, want ErrUnknownTool", err)
	}
}

func TestInvokePassesCallerAndTruncates(t *testing.T) {
	var gotCaller string
	r := NewRegistry(Tool{
		Name:   "echo_caller",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(_ context.Context, _ json.RawMessage, caller string) (string, error) {
			gotCaller = caller
			return strings.Repeat("x", MaxToolResultChars+50), nil
		},
	})
	out, err := r.Invoke(context.Background(), "echo_caller", nil, "2535417035391439")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotCaller != "2535417035391439" {
		t.Errorf("caller = %q, want the XUID the loop injected", gotCaller)
	}
	if len(out) != MaxToolResultChars {
		t.Errorf("result len = %d, want it truncated to %d", len(out), MaxToolResultChars)
	}
}

func TestRegistrySkipsMalformedTools(t *testing.T) {
	r := NewRegistry(
		testTool("", "unnamed", nil),
		Tool{Name: "no_invoke", Schema: json.RawMessage(`{}`)},
		testTool("good", "ok", nil),
	)
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 -- unnamed and nil-Invoke tools must be dropped", r.Len())
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/tools/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/tools/tools.go`**

```go
// Package tools is the read-only capability surface the LLM may call.
//
// Every tool here answers a question; none of them change anything. That is
// the whole security model of the answer path: the input is untrusted
// player-typed chat, so a prompt-injection attempt to rewrite the rules or
// move someone's waypoint has nothing to call. Writes live in ! commands,
// which carry a real actor and a permission check.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MaxToolResultChars bounds what one tool feeds back into the model's
// context. The reply itself is capped at 200 characters, so a tool result
// larger than this buys nothing and costs prompt tokens on a small local
// model with a finite window.
const MaxToolResultChars = 400

// Tool is one capability the model may call.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema for the arguments object, as the backend
	// expects it under function.parameters.
	Schema json.RawMessage
	// Invoke runs the tool. caller is the asking player's XUID, injected by
	// the loop and never supplied by the model -- which is what stops a tool
	// from being asked for another player's data.
	Invoke func(ctx context.Context, args json.RawMessage, caller string) (string, error)
}

// FunctionDefinition and Definition are the OpenAI-compatible wire shapes.
type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Definition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// ErrUnknownTool is returned when the model names a tool that is not
// registered -- which happens: models invent plausible names.
var ErrUnknownTool = errors.New("unknown tool")

// Registry holds the tools offered for one answer.
type Registry struct {
	order []string
	byName map[string]Tool
}

// NewRegistry builds a registry, silently dropping tools that could not be
// called anyway (no name, or no Invoke). A malformed tool is a wiring bug,
// and offering the model a name that panics on call is worse than not
// offering it.
func NewRegistry(list ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(list))}
	for _, t := range list {
		name := strings.TrimSpace(t.Name)
		if name == "" || t.Invoke == nil {
			continue
		}
		if _, exists := r.byName[name]; exists {
			continue
		}
		t.Name = name
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.order)
}

// Definitions returns what is sent to the model, in registration order.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	out := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, Definition{
			Type: "function",
			Function: FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return out
}

// Invoke runs one tool call and returns its result, truncated.
func (r *Registry) Invoke(ctx context.Context, name string, args json.RawMessage, caller string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	t, ok := r.byName[strings.TrimSpace(name)]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	out, err := t.Invoke(ctx, args, caller)
	if err != nil {
		return "", err
	}
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > MaxToolResultChars {
		out = out[:MaxToolResultChars]
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/tools/ -v`
Expected: PASS, all four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/tools
git commit -m "feat(tools): read-only capability registry for the answer path

The caller XUID is injected by the registry rather than accepted from the
model, so a tool cannot be talked into reading another player's data."
```

---

### Task 5: Expose the pool and the new capabilities on `plugin.Context`

**Files:**
- Modify: `internal/store/postgres.go` (add a `Pool()` accessor)
- Modify: `internal/plugin/plugin.go` (add `Knowledge`, `Waypoints` fields and their narrowed interfaces)
- Test: `internal/plugin/plugin_test.go` (extend)

**Interfaces:**
- Consumes: `knowledge.Store` and `waypoints.Store` from Tasks 2-3.
- Produces:
  - `(*store.Postgres).Pool() *pgxpool.Pool`
  - `plugin.KnowledgeStore` interface: `Lookup(ctx, query string, limit int) ([]knowledge.Entry, error)`, `Get(ctx, topic string) (knowledge.Entry, bool, error)`, `Upsert(ctx, topic, body, authorXUID string) error`, `Delete(ctx, topic string) error`, `List(ctx) ([]knowledge.Entry, error)`, `Enabled() bool`
  - `plugin.WaypointStore` interface: the five `waypoints.Store` methods verbatim
  - `plugin.Context.Knowledge plugin.KnowledgeStore`, `plugin.Context.Waypoints plugin.WaypointStore`

- [ ] **Step 1: Write the failing test**

Append to `internal/plugin/plugin_test.go`:

```go
func TestContextKnowledgeAndWaypointsMayBeNil(t *testing.T) {
	// The zero Context is what a test or an unconfigured binary hands a
	// plugin. Reading these fields must not panic; plugins guard.
	var pctx Context
	if pctx.Knowledge != nil {
		t.Error("zero Context.Knowledge should be nil")
	}
	if pctx.Waypoints != nil {
		t.Error("zero Context.Waypoints should be nil")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/plugin/ -run TestContextKnowledge -v`
Expected: FAIL — `pctx.Knowledge undefined`.

- [ ] **Step 3: Add the interfaces and fields**

In `internal/plugin/plugin.go`, add to the imports `"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"` and `"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"`, then after the `PlayerStore` declaration:

```go
// KnowledgeStore is the curated-fact surface a plugin may touch. Identical
// to knowledge.Store today; declared here so the plugin package states its
// own dependency rather than inheriting whatever that package grows.
type KnowledgeStore interface {
	Lookup(ctx context.Context, query string, limit int) ([]knowledge.Entry, error)
	Get(ctx context.Context, topic string) (knowledge.Entry, bool, error)
	Upsert(ctx context.Context, topic, body, authorXUID string) error
	Delete(ctx context.Context, topic string) error
	List(ctx context.Context) ([]knowledge.Entry, error)
	Enabled() bool
}

// WaypointStore is the per-player coordinate surface a plugin may touch.
// Every method takes the owning XUID: there is no "all waypoints" read,
// because no command and no tool has a reason for one.
type WaypointStore interface {
	Get(ctx context.Context, xuid, name string) (waypoints.Waypoint, bool, error)
	Set(ctx context.Context, xuid string, wp waypoints.Waypoint) error
	Delete(ctx context.Context, xuid, name string) error
	List(ctx context.Context, xuid string) ([]waypoints.Waypoint, error)
	Enabled() bool
}
```

And in `Context`, after `Profiles`:

```go
	// Knowledge and Waypoints are nil when no database is configured. Both
	// are guarded at every use for the same reason Profiles is.
	Knowledge KnowledgeStore
	Waypoints WaypointStore
```

- [ ] **Step 4: Add the pool accessor**

In `internal/store/postgres.go`, after `Close`:

```go
// Pool exposes the connection pool so the knowledge and waypoint stores can
// share it. One pool, not three: this whole workload is a few rows per
// player visit, and extra pools would reserve connections on a shared
// cluster to sit idle. Returns nil for a zero-valued Postgres.
func (p *Postgres) Pool() *pgxpool.Pool {
	if p == nil {
		return nil
	}
	return p.pool
}
```

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go test ./internal/plugin/ ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/plugin internal/store
git commit -m "feat(plugin): expose knowledge and waypoint capabilities"
```

---

### Task 6: `!kb` command plugin

**Files:**
- Create: `internal/plugins/knowledge.go`
- Test: `internal/plugins/knowledge_test.go`

**Interfaces:**
- Consumes: `plugin.KnowledgeStore` from Task 5.
- Produces: `plugins.NewKnowledge() *plugins.Knowledge`, registering one command, `kb`.

- [ ] **Step 1: Write the failing test**

`internal/plugins/knowledge_test.go`:

```go
package plugins

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

type fakeKnowledge struct {
	entries  map[string]knowledge.Entry
	upserted int
}

func newFakeKnowledge() *fakeKnowledge {
	return &fakeKnowledge{entries: map[string]knowledge.Entry{}}
}

func (f *fakeKnowledge) Lookup(_ context.Context, q string, limit int) ([]knowledge.Entry, error) {
	var out []knowledge.Entry
	for _, e := range f.entries {
		if strings.Contains(e.Topic, knowledge.NormalizeTopic(q)) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fakeKnowledge) Get(_ context.Context, topic string) (knowledge.Entry, bool, error) {
	e, ok := f.entries[knowledge.NormalizeTopic(topic)]
	return e, ok, nil
}
func (f *fakeKnowledge) Upsert(_ context.Context, topic, body, author string) error {
	f.upserted++
	f.entries[knowledge.NormalizeTopic(topic)] = knowledge.Entry{
		Topic: knowledge.NormalizeTopic(topic), Body: body, AuthorXUID: author, UpdatedAt: time.Now(),
	}
	return nil
}
func (f *fakeKnowledge) Delete(_ context.Context, topic string) error {
	delete(f.entries, knowledge.NormalizeTopic(topic))
	return nil
}
func (f *fakeKnowledge) List(context.Context) ([]knowledge.Entry, error) {
	out := make([]knowledge.Entry, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e)
	}
	return out, nil
}
func (f *fakeKnowledge) Enabled() bool { return true }

func kbCommand(t *testing.T) plugin.Command {
	t.Helper()
	for _, c := range NewKnowledge().Commands() {
		if c.Name == "kb" {
			return c
		}
	}
	t.Fatal("kb command not registered")
	return plugin.Command{}
}

func TestKBWriteRequiresOperator(t *testing.T) {
	cmd := kbCommand(t)
	if cmd.Permission != plugin.PermissionVisitor {
		t.Fatalf("kb base permission = %v, want visitor (reads are open)", cmd.Permission)
	}

	fake := newFakeKnowledge()
	pctx := &plugin.Context{Knowledge: fake}
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "member",
		ActorPermission: plugin.PermissionMember,
		Args:            []string{"set", "rules", "be", "nice"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.upserted != 0 {
		t.Fatal("a member wrote to the knowledge store")
	}
	if !strings.Contains(strings.ToLower(reply), "operator") {
		t.Errorf("reply %q should say why the write was refused", reply)
	}
}

func TestKBSetThenGet(t *testing.T) {
	cmd := kbCommand(t)
	fake := newFakeKnowledge()
	pctx := &plugin.Context{Knowledge: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"set", "gold", "farm", "is", "under", "spawn"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "visitor",
		ActorPermission: plugin.PermissionVisitor,
		Args:            []string{"gold"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(reply, "under spawn") {
		t.Errorf("reply = %q, want the stored body", reply)
	}
}

func TestKBWithoutStore(t *testing.T) {
	cmd := kbCommand(t)
	reply, err := cmd.Run(context.Background(), &plugin.Context{}, plugin.Invocation{
		ActorXUID: "someone", ActorPermission: plugin.PermissionVisitor, Args: []string{"rules"},
	})
	if err != nil {
		t.Fatalf("a nil Knowledge must not error: %v", err)
	}
	if reply == "" {
		t.Error("reply should explain the feature is unconfigured")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/plugins/ -run TestKB -v`
Expected: FAIL — `NewKnowledge` undefined.

- [ ] **Step 3: Implement `internal/plugins/knowledge.go`**

The permission split lives inside `Run`, not in `Command.Permission`: one command name serves both reads and writes, and the registry checks a single level per command.

```go
package plugins

import (
	"context"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// Knowledge exposes the curated fact store in chat.
//
// One command with a subcommand rather than !kb, !kbset and !kbdel: !help
// lists every command to every player, and three entries for one feature
// crowds out the rest on a Bedrock chat line.
type Knowledge struct{}

func NewKnowledge() *Knowledge { return &Knowledge{} }

func (*Knowledge) Name() string { return "knowledge" }

func (*Knowledge) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "kb",
			Description: "Look up what the server knows: !kb <topic>, !kb list.",
			// Visitor, because reads are open. The write subcommands check
			// the actor's level themselves -- see runKB.
			Permission: plugin.PermissionVisitor,
			Run:        runKB,
		},
	}
}

func runKB(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Knowledge == nil || !pctx.Knowledge.Enabled() {
		return "I have no knowledge store configured.", nil
	}
	if len(inv.Args) == 0 {
		return "Usage: !kb <topic>, !kb list, !kb set <topic> <text>, !kb del <topic>.", nil
	}

	switch strings.ToLower(inv.Args[0]) {
	case "list":
		entries, err := pctx.Knowledge.List(ctx)
		if err != nil {
			return "", fmt.Errorf("knowledge: !kb list: %w", err)
		}
		if len(entries) == 0 {
			return "I know nothing yet. An operator can teach me with !kb set.", nil
		}
		topics := make([]string, 0, len(entries))
		for _, e := range entries {
			topics = append(topics, e.Topic)
		}
		return "I know about: " + strings.Join(topics, ", "), nil

	case "set":
		if inv.ActorPermission < plugin.PermissionOperator {
			return "Only an operator can teach me new facts.", nil
		}
		if len(inv.Args) < 3 {
			return "Usage: !kb set <topic> <text>.", nil
		}
		topic, body := inv.Args[1], strings.Join(inv.Args[2:], " ")
		if err := pctx.Knowledge.Upsert(ctx, topic, body, inv.ActorXUID); err != nil {
			return "", fmt.Errorf("knowledge: !kb set: %w", err)
		}
		return "Learned: " + topic + ".", nil

	case "del":
		if inv.ActorPermission < plugin.PermissionOperator {
			return "Only an operator can make me forget.", nil
		}
		if len(inv.Args) < 2 {
			return "Usage: !kb del <topic>.", nil
		}
		if err := pctx.Knowledge.Delete(ctx, inv.Args[1]); err != nil {
			return "", fmt.Errorf("knowledge: !kb del: %w", err)
		}
		return "Forgotten: " + inv.Args[1] + ".", nil

	default:
		query := strings.Join(inv.Args, " ")
		entries, err := pctx.Knowledge.Lookup(ctx, query, 1)
		if err != nil {
			return "", fmt.Errorf("knowledge: !kb: %w", err)
		}
		if len(entries) == 0 {
			return "I don't know anything about " + query + ".", nil
		}
		return entries[0].Topic + ": " + entries[0].Body, nil
	}
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/plugins/ -run TestKB -v`
Expected: PASS, all three.

- [ ] **Step 5: Commit**

```bash
git add internal/plugins/knowledge.go internal/plugins/knowledge_test.go
git commit -m "feat(plugins): add !kb, operator-written and open to read"
```

---

### Task 7: `!wp` command plugin

**Files:**
- Create: `internal/plugins/waypoints.go`
- Test: `internal/plugins/waypoints_test.go`

**Interfaces:**
- Consumes: `plugin.WaypointStore` from Task 5.
- Produces: `plugins.NewWaypoints() *plugins.Waypoints`, registering one command, `wp`.

- [ ] **Step 1: Write the failing test**

`internal/plugins/waypoints_test.go`:

```go
package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

type fakeWaypoints struct {
	byOwner map[string]map[string]waypoints.Waypoint
}

func newFakeWaypoints() *fakeWaypoints {
	return &fakeWaypoints{byOwner: map[string]map[string]waypoints.Waypoint{}}
}

func (f *fakeWaypoints) Get(_ context.Context, xuid, name string) (waypoints.Waypoint, bool, error) {
	wp, ok := f.byOwner[xuid][waypoints.NormalizeName(name)]
	return wp, ok, nil
}
func (f *fakeWaypoints) Set(_ context.Context, xuid string, wp waypoints.Waypoint) error {
	dim, err := waypoints.NormalizeDimension(wp.Dimension)
	if err != nil {
		return err
	}
	wp.Dimension = dim
	wp.Name = waypoints.NormalizeName(wp.Name)
	if f.byOwner[xuid] == nil {
		f.byOwner[xuid] = map[string]waypoints.Waypoint{}
	}
	f.byOwner[xuid][wp.Name] = wp
	return nil
}
func (f *fakeWaypoints) Delete(_ context.Context, xuid, name string) error {
	delete(f.byOwner[xuid], waypoints.NormalizeName(name))
	return nil
}
func (f *fakeWaypoints) List(_ context.Context, xuid string) ([]waypoints.Waypoint, error) {
	out := make([]waypoints.Waypoint, 0, len(f.byOwner[xuid]))
	for _, wp := range f.byOwner[xuid] {
		out = append(out, wp)
	}
	return out, nil
}
func (f *fakeWaypoints) Enabled() bool { return true }

func wpCommand(t *testing.T) plugin.Command {
	t.Helper()
	for _, c := range NewWaypoints().Commands() {
		if c.Name == "wp" {
			return c
		}
	}
	t.Fatal("wp command not registered")
	return plugin.Command{}
}

func TestWPSetAndGetIsPerPlayer(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "Base", "100", "64", "-200"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember, Args: []string{"base"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(reply, "100") || !strings.Contains(reply, "-200") {
		t.Errorf("reply = %q, want the coordinates", reply)
	}

	other, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-b", ActorPermission: plugin.PermissionMember, Args: []string{"base"},
	})
	if err != nil {
		t.Fatalf("other player get: %v", err)
	}
	if strings.Contains(other, "100") {
		t.Errorf("player-b read player-a's waypoint: %q", other)
	}
}

func TestWPRejectsBadCoordinatesAndDimension(t *testing.T) {
	cmd := wpCommand(t)
	pctx := &plugin.Context{Waypoints: newFakeWaypoints()}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "x", "64", "-200"},
	})
	if err != nil {
		t.Fatalf("a bad coordinate must be a reply, not an error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "number") {
		t.Errorf("reply = %q, want it to name the problem", reply)
	}

	reply, err = cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "1", "2", "3", "moon"},
	})
	if err != nil {
		t.Fatalf("a bad dimension must be a reply, not an error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "overworld") {
		t.Errorf("reply = %q, want it to list the valid dimensions", reply)
	}
}

func TestWPWithoutStore(t *testing.T) {
	cmd := wpCommand(t)
	reply, err := cmd.Run(context.Background(), &plugin.Context{}, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember, Args: nil,
	})
	if err != nil {
		t.Fatalf("a nil Waypoints must not error: %v", err)
	}
	if reply == "" {
		t.Error("reply should explain the feature is unconfigured")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/plugins/ -run TestWP -v`
Expected: FAIL — `NewWaypoints` undefined.

- [ ] **Step 3: Implement `internal/plugins/waypoints.go`**

```go
package plugins

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

// Waypoints lets a player keep their own named coordinates.
//
// Member level, not visitor: this writes rows keyed to the actor, and a
// server that lets anyone who joined once fill the table has a spam problem
// rather than a feature.
type Waypoints struct{}

func NewWaypoints() *Waypoints { return &Waypoints{} }

func (*Waypoints) Name() string { return "waypoints" }

func (*Waypoints) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "wp",
			Description: "Your saved coordinates: !wp, !wp <name>, !wp set <name> <x> <y> <z>.",
			Permission:  plugin.PermissionMember,
			Run:         runWP,
		},
	}
}

func runWP(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Waypoints == nil || !pctx.Waypoints.Enabled() {
		return "I have no waypoint store configured.", nil
	}

	if len(inv.Args) == 0 {
		list, err := pctx.Waypoints.List(ctx, inv.ActorXUID)
		if err != nil {
			return "", fmt.Errorf("waypoints: !wp: %w", err)
		}
		if len(list) == 0 {
			return "You have no waypoints. Save one with !wp set <name> <x> <y> <z>.", nil
		}
		names := make([]string, 0, len(list))
		for _, wp := range list {
			names = append(names, wp.Name)
		}
		return "Your waypoints: " + strings.Join(names, ", "), nil
	}

	switch strings.ToLower(inv.Args[0]) {
	case "set":
		if len(inv.Args) < 5 {
			return "Usage: !wp set <name> <x> <y> <z> [overworld|nether|end].", nil
		}
		coords := make([]int, 3)
		for i, raw := range inv.Args[2:5] {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return "Coordinates must be whole numbers: " + raw + " is not a number.", nil
			}
			coords[i] = n
		}
		dimension := ""
		if len(inv.Args) >= 6 {
			dimension = inv.Args[5]
		}
		wp := waypoints.Waypoint{
			Name: inv.Args[1], X: coords[0], Y: coords[1], Z: coords[2], Dimension: dimension,
		}
		if err := pctx.Waypoints.Set(ctx, inv.ActorXUID, wp); err != nil {
			if errors.Is(err, waypoints.ErrUnknownDimension) {
				return "Dimension must be overworld, nether or end.", nil
			}
			return "", fmt.Errorf("waypoints: !wp set: %w", err)
		}
		return fmt.Sprintf("Saved %s at %d %d %d.", waypoints.NormalizeName(wp.Name), wp.X, wp.Y, wp.Z), nil

	case "del":
		if len(inv.Args) < 2 {
			return "Usage: !wp del <name>.", nil
		}
		if err := pctx.Waypoints.Delete(ctx, inv.ActorXUID, inv.Args[1]); err != nil {
			return "", fmt.Errorf("waypoints: !wp del: %w", err)
		}
		return "Deleted " + waypoints.NormalizeName(inv.Args[1]) + ".", nil

	default:
		name := strings.Join(inv.Args, " ")
		wp, found, err := pctx.Waypoints.Get(ctx, inv.ActorXUID, name)
		if err != nil {
			return "", fmt.Errorf("waypoints: !wp: %w", err)
		}
		if !found {
			return "You have no waypoint called " + waypoints.NormalizeName(name) + ".", nil
		}
		return fmt.Sprintf("%s: %d %d %d (%s).", wp.Name, wp.X, wp.Y, wp.Z, wp.Dimension), nil
	}
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/plugins/ -v`
Expected: PASS, including the existing core/stats/welcome tests.

- [ ] **Step 5: Commit**

```bash
git add internal/plugins/waypoints.go internal/plugins/waypoints_test.go
git commit -m "feat(plugins): add !wp for per-player coordinates

Bad input is a chat reply, not an error: a typo'd coordinate is a player
mistake, and turning it into a logged failure tells them nothing."
```

---

### Task 8: The tool-calling loop in the LLM client

**Files:**
- Modify: `internal/adapters/llm.go`
- Test: `internal/adapters/llm_test.go` (extend)

**Interfaces:**
- Consumes: `*tools.Registry` from Task 4.
- Produces:
  - `(*LLMClient).AnswerWithTools(ctx context.Context, asker, callerXUID, question string, registry *tools.Registry) (string, error)`
  - `MaxToolRounds = 2`
  - `Answer` keeps its current signature and behaviour, implemented as `AnswerWithTools` with a nil registry.

- [ ] **Step 1: Write the failing tests**

Append to `internal/adapters/llm_test.go`:

```go
func toolBackend(t *testing.T, replies []string) (*httptest.Server, *int) {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if calls >= len(replies) {
			t.Errorf("backend called %d times, only %d replies scripted", calls+1, len(replies))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply := replies[calls]
		calls++
		// The final call must not offer tools -- that is what forces text.
		if calls == len(replies) && strings.Contains(string(body), `"tools"`) && len(replies) > MaxToolRounds {
			t.Errorf("final call still offered tools: %s", body)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const textReply = `{"choices":[{"message":{"content":"The gold farm is under spawn."},"finish_reason":"stop"}]}`

func toolCallReply(name, args string) string {
	return `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"` +
		name + `","arguments":` + strconv.Quote(args) + `}}]},"finish_reason":"tool_calls"}]}`
}

func TestAnswerWithToolsOneRound(t *testing.T) {
	srv, calls := toolBackend(t, []string{toolCallReply("knowledge_lookup", `{"query":"gold farm"}`), textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)

	var gotCaller string
	registry := tools.NewRegistry(tools.Tool{
		Name:   "knowledge_lookup",
		Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		Invoke: func(_ context.Context, args json.RawMessage, caller string) (string, error) {
			gotCaller = caller
			return "gold farm: under spawn at y 12", nil
		},
	})

	got, err := client.AnswerWithTools(context.Background(), "Dot", "xuid-1", "where is the gold farm", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if got != "The gold farm is under spawn." {
		t.Errorf("answer = %q", got)
	}
	if *calls != 2 {
		t.Errorf("backend calls = %d, want 2", *calls)
	}
	if gotCaller != "xuid-1" {
		t.Errorf("tool caller = %q, want the asking XUID", gotCaller)
	}
}

func TestAnswerWithToolsStopsAtRoundCap(t *testing.T) {
	// Three tool-call replies, but the cap is 2 rounds: the client must stop
	// asking for tools and take the third call's text.
	srv, calls := toolBackend(t, []string{
		toolCallReply("t", "{}"), toolCallReply("t", "{}"), textReply,
	})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)
	registry := tools.NewRegistry(tools.Tool{
		Name:   "t",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) { return "ok", nil },
	})

	got, err := client.AnswerWithTools(context.Background(), "Dot", "xuid-1", "q", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if got == "" {
		t.Error("expected the forced text answer")
	}
	if *calls != 3 {
		t.Errorf("backend calls = %d, want 3 (2 tool rounds + 1 forced)", *calls)
	}
}

func TestAnswerWithToolsHandlesToolErrorAndUnknownTool(t *testing.T) {
	srv, _ := toolBackend(t, []string{toolCallReply("broken", "{}"), textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)
	registry := tools.NewRegistry(tools.Tool{
		Name:   "broken",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) {
			return "", errors.New("bridge unreachable")
		},
	})
	got, err := client.AnswerWithTools(context.Background(), "Dot", "x", "q", registry)
	if err != nil {
		t.Fatalf("a failing tool must not fail the answer: %v", err)
	}
	if got == "" {
		t.Error("expected the model's answer despite the tool error")
	}

	srv2, _ := toolBackend(t, []string{toolCallReply("invented_tool", "{}"), textReply})
	client2 := NewLLMClient(srv2.URL, "m", "", 192, 5*time.Second)
	if _, err := client2.AnswerWithTools(context.Background(), "Dot", "x", "q", registry); err != nil {
		t.Fatalf("an invented tool name must not fail the answer: %v", err)
	}
}

func TestAnswerWithNilRegistryMakesOneCall(t *testing.T) {
	srv, calls := toolBackend(t, []string{textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)
	if _, err := client.AnswerWithTools(context.Background(), "Dot", "x", "q", nil); err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if *calls != 1 {
		t.Errorf("backend calls = %d, want 1 when no tools are offered", *calls)
	}
}
```

Add to that file's imports as needed: `encoding/json`, `errors`, `io`, `net/http`, `net/http/httptest`, `strconv`, `strings`, `time`, and `github.com/jdwillmsen/minecraft-server-agent/internal/tools`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/adapters/ -run TestAnswerWith -v`
Expected: FAIL — `AnswerWithTools` undefined.

- [ ] **Step 3: Extend the wire types in `internal/adapters/llm.go`**

Replace `chatMessage` and `chatRequest`, and add the response shapes:

```go
// MaxToolRounds caps how many times the model may ask for tools before it
// is made to answer. Two covers the questions this serves -- one lookup,
// occasionally two -- and an uncapped loop driven by a chat message is an
// unbounded cost per message.
const MaxToolRounds = 2

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is echoed back verbatim in the assistant turn: the backend
	// matches tool results to it by id, and dropping it makes the tool
	// messages orphans.
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set only on role:"tool" messages.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type chatRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []chatMessage      `json:"messages"`
	Tools     []tools.Definition `json:"tools,omitempty"`
}
```

Add `"github.com/jdwillmsen/minecraft-server-agent/internal/tools"` to the imports.

- [ ] **Step 4: Add a parser for tool calls**

```go
// ExtractToolCalls returns the tool calls in a completion, or nil when the
// model answered with text. Like ExtractText, it treats anything it cannot
// parse as "nothing", so an unfamiliar backend degrades to a plain answer.
func ExtractToolCalls(payload []byte) []toolCall {
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil || len(parsed.Choices) == 0 {
		return nil
	}
	return parsed.Choices[0].Message.ToolCalls
}
```

- [ ] **Step 5: Refactor the HTTP call out of `Answer`**

Extract the existing body of `Answer` (from `json.Marshal` through `io.ReadAll`) into:

```go
// post sends one chat-completions request and returns the raw payload. Each
// call carries its own timeout: without a per-call bound, one stalled
// request would consume the whole loop's budget.
func (c *LLMClient) post(ctx context.Context, body chatRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: backend returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
```

Keep `BuildRequest` as it is — its truncation and header rules are tested directly.

- [ ] **Step 6: Write the loop**

```go
// AnswerWithTools asks the model, letting it call read-only tools first.
//
// callerXUID is passed to every tool the model invokes and is never taken
// from the model's own arguments: that is what keeps waypoint_lookup from
// being talked into reading someone else's coordinates.
func (c *LLMClient) AnswerWithTools(ctx context.Context, asker, callerXUID, question string, registry *tools.Registry) (string, error) {
	if !c.Enabled() {
		return "", nil
	}
	if len(question) > MaxQuestionChars {
		question = question[:MaxQuestionChars]
	}

	messages := []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: asker + " asked: " + question},
	}

	for round := 0; ; round++ {
		body := chatRequest{Model: c.model, MaxTokens: c.maxTokens, Messages: messages}
		// Tools are withheld on the final pass, which is what forces text
		// out of a model that would otherwise keep calling tools forever.
		if registry.Len() > 0 && round < MaxToolRounds {
			body.Tools = registry.Definitions()
		}

		payload, err := c.post(ctx, body)
		if err != nil {
			return "", err
		}

		calls := ExtractToolCalls(payload)
		if len(calls) == 0 || round >= MaxToolRounds {
			return ExtractText(payload), nil
		}

		messages = append(messages, chatMessage{Role: "assistant", ToolCalls: calls})
		for _, call := range calls {
			result, err := registry.Invoke(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments), callerXUID)
			if err != nil {
				// Handed back to the model rather than aborting: it can
				// answer around a missing fact, and an operator reads the
				// failure in the logs. Never spoken in chat.
				result = "error: " + err.Error()
			}
			messages = append(messages, chatMessage{
				Role: "tool", ToolCallID: call.ID, Content: result,
			})
		}
	}
}

// Answer asks the model with no tools available. Retained for callers that
// have no registry to offer, and as the behaviour the unconfigured path
// keeps.
func (c *LLMClient) Answer(ctx context.Context, asker, question string) (string, error) {
	return c.AnswerWithTools(ctx, asker, "", question, nil)
}
```

- [ ] **Step 7: Run the whole adapters suite**

Run: `go test ./internal/adapters/ -v`
Expected: PASS, including the pre-existing `BuildRequest` and `ExtractText` tests.

- [ ] **Step 8: Commit**

```bash
git add internal/adapters/llm.go internal/adapters/llm_test.go
git commit -m "feat(llm): let the model call read-only tools before answering

Tools are withheld on the final round rather than trusting the model to
stop, so a model that would keep calling tools is made to produce text."
```

---

### Task 9: Wire it up, and take mention answering off the packet loop

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/agent/main.go`
- Create: `cmd/agent/toolset.go`
- Test: `internal/config/config_test.go` (extend)
- Test: `cmd/agent/toolset_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2-8.
- Produces:
  - `config.Config.LLMTotalTimeoutMs int` (env `LLM_TOTAL_TIMEOUT_MS`, default 20000); `LLM_MAX_TOKENS` default becomes 192
  - `buildToolset(pctx *plugin.Context, playtime plugin.PlayerStore) *tools.Registry` in `cmd/agent/toolset.go`

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go`:

```go
func TestLLMTotalTimeoutDefaults(t *testing.T) {
	t.Setenv("MC_HOST", "server")
	t.Setenv("MC_USERNAME", "agent")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLMTotalTimeoutMs != 20000 {
		t.Errorf("LLMTotalTimeoutMs = %d, want 20000", cfg.LLMTotalTimeoutMs)
	}
	if cfg.LLMMaxTokens != 192 {
		t.Errorf("LLMMaxTokens = %d, want 192 -- 96 cannot hold tool arguments plus an answer", cfg.LLMMaxTokens)
	}
}
```

If `Load` in this repo takes arguments or has a different name, match the existing tests in that file rather than this sketch.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/config/ -run TestLLMTotalTimeout -v`
Expected: FAIL — field undefined and max-tokens default still 96.

- [ ] **Step 3: Add the config field**

In `internal/config/config.go`, change the `LLM_MAX_TOKENS` default from 96 to 192, add next to the other parses:

```go
	llmTotalTimeout, err := positiveInt("LLM_TOTAL_TIMEOUT_MS", 20000)
	if err != nil {
		return Config{}, err
	}
```

wire `LLMTotalTimeoutMs: llmTotalTimeout` into the returned struct, and document the field:

```go
	// LLMTotalTimeoutMs bounds one whole answering attempt including every
	// tool round trip. Separate from LLMTimeoutMs, which bounds each
	// individual call: without the per-call bound one stalled request eats
	// the entire budget, and without this one a model that keeps calling
	// tools answers arbitrarily late.
	LLMTotalTimeoutMs int
```

- [ ] **Step 4: Run the config test**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Write the toolset test**

`cmd/agent/toolset_test.go`:

```go
package main

import (
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

func TestBuildToolsetOmitsAbsentCapabilities(t *testing.T) {
	// A context with nothing configured must produce no tools at all: the
	// model cannot call what it was never offered, which is stronger than
	// refusing the call afterwards.
	if got := buildToolset(&plugin.Context{}, nil).Len(); got != 0 {
		t.Errorf("empty context produced %d tools, want 0", got)
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test ./cmd/agent/ -run TestBuildToolset -v`
Expected: FAIL — `buildToolset` undefined.

- [ ] **Step 7: Write `cmd/agent/toolset.go`**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
)

// noArgs is the schema for a tool that takes nothing. Sent rather than
// omitted because some OpenAI-compatible backends reject a function with no
// parameters object at all.
var noArgs = json.RawMessage(`{"type":"object","properties":{}}`)

// buildToolset assembles the read-only tools for one answer.
//
// A capability that is not configured contributes no tool. That is the
// design's central safety property expressed in wiring: the model's
// available actions are exactly what this function registers, and none of
// them write.
func buildToolset(pctx *plugin.Context, _ plugin.PlayerStore) *tools.Registry {
	var list []tools.Tool

	if pctx.Knowledge != nil && pctx.Knowledge.Enabled() {
		list = append(list, tools.Tool{
			Name:        "knowledge_lookup",
			Description: "Look up what this server's operators have recorded about a topic: rules, farm locations, build sites.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look up"}},"required":["query"]}`),
			Invoke: func(ctx context.Context, args json.RawMessage, _ string) (string, error) {
				var a struct {
					Query string `json:"query"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("knowledge_lookup: bad arguments: %w", err)
				}
				entries, err := pctx.Knowledge.Lookup(ctx, a.Query, 3)
				if err != nil {
					return "", err
				}
				if len(entries) == 0 {
					return "nothing recorded about that", nil
				}
				parts := make([]string, 0, len(entries))
				for _, e := range entries {
					parts = append(parts, e.Topic+": "+e.Body)
				}
				return strings.Join(parts, " | "), nil
			},
		})
	}

	if pctx.Waypoints != nil && pctx.Waypoints.Enabled() {
		list = append(list,
			tools.Tool{
				Name:        "waypoint_lookup",
				Description: "Get the coordinates the asking player saved under a name, such as their base.",
				Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The waypoint name"}},"required":["name"]}`),
				Invoke: func(ctx context.Context, args json.RawMessage, caller string) (string, error) {
					var a struct {
						Name string `json:"name"`
					}
					if err := json.Unmarshal(args, &a); err != nil {
						return "", fmt.Errorf("waypoint_lookup: bad arguments: %w", err)
					}
					// caller, never an argument: the model names a waypoint,
					// it does not choose whose.
					wp, found, err := pctx.Waypoints.Get(ctx, caller, a.Name)
					if err != nil {
						return "", err
					}
					if !found {
						return "no waypoint by that name", nil
					}
					return fmt.Sprintf("%s is at %d %d %d in the %s", wp.Name, wp.X, wp.Y, wp.Z, wp.Dimension), nil
				},
			},
			tools.Tool{
				Name:        "waypoint_list",
				Description: "List the names of the waypoints the asking player has saved.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, caller string) (string, error) {
					list, err := pctx.Waypoints.List(ctx, caller)
					if err != nil {
						return "", err
					}
					if len(list) == 0 {
						return "no saved waypoints", nil
					}
					names := make([]string, 0, len(list))
					for _, wp := range list {
						names = append(names, wp.Name)
					}
					return strings.Join(names, ", "), nil
				},
			},
		)
	}

	if pctx.Facts != nil {
		list = append(list, tools.Tool{
			Name:        "players_online",
			Description: "Who is currently connected to the server.",
			Schema:      noArgs,
			Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
				return pctx.Facts.PlayersOnline(ctx)
			},
		})
	}

	if pctx.ServerInfo != nil {
		list = append(list,
			tools.Tool{
				Name:        "server_status",
				Description: "Server health, player count and responsiveness.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
					return pctx.ServerInfo.ServerStatus(ctx)
				},
			},
			tools.Tool{
				Name:        "server_version",
				Description: "Which Bedrock version this server runs.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
					return pctx.ServerInfo.Version(ctx)
				},
			},
			tools.Tool{
				Name:        "backup_status",
				Description: "How recently the world was backed up and how large that backup was.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
					return pctx.ServerInfo.BackupStatus(ctx)
				},
			},
		)
	}

	return tools.NewRegistry(list...)
}
```

The spec listed a `player_playtime` tool. It is deliberately not built here: `plugin.PlayerStore` exposes only `RecordJoin` and `Enabled`, so there is no read path for playtime that does not also record a join. Adding one is a separate change to the store interface, and pretending otherwise would mean a tool that writes. The unused parameter is kept so the signature is ready for it. Note this in the PR description.

- [ ] **Step 8: Run the toolset test**

Run: `go test ./cmd/agent/ -run TestBuildToolset -v`
Expected: PASS.

- [ ] **Step 9: Wire the stores into `main`**

In `cmd/agent/main.go`, after `playerStore := openStore(ctx, cfg, log)` (line ~104), add:

```go
	// Both share the profile store's pool; a Nop store yields nil, which the
	// constructors below turn into the disabled implementations.
	var knowledgeStore knowledge.Store = knowledge.Nop{}
	var waypointStore waypoints.Store = waypoints.Nop{}
	if pg, ok := playerStore.(*store.Postgres); ok && pg.Pool() != nil {
		knowledgeStore = knowledge.NewPostgres(pg.Pool())
		waypointStore = waypoints.NewPostgres(pg.Pool())
		log.Info("knowledge_ready", nil)
	}
```

Register the two new plugins next to the existing three (around line 89):

```go
	if err := registry.Register(plugins.NewKnowledge()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "knowledge", "error": err.Error()})
		os.Exit(1)
	}
	if err := registry.Register(plugins.NewWaypoints()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "waypoints", "error": err.Error()})
		os.Exit(1)
	}
```

Match the exact error-handling style the neighbouring `registry.Register` calls use.

Add the two fields to the `plugin.Context` literal (line ~403), after `Profiles`:

```go
		Knowledge: knowledgeStore,
		Waypoints: waypointStore,
```

The context is built in a function that will need both values passed in; thread them through as parameters rather than making them package-level.

- [ ] **Step 10: Move mention answering off the packet read loop**

Add the toolset and total budget to the `answering` struct:

```go
type answering struct {
	limiter *ratelimit.PerActor
	llm     *adapters.LLMClient
	// tools is rebuilt per answer rather than cached: it closes over the
	// plugin context's capabilities, and which of those are usable can
	// change while the process runs.
	toolsFor func(*plugin.Context) *tools.Registry
	// total bounds one whole answering attempt, tool rounds included.
	total time.Duration
}
```

In `handleText` (line ~379), replace the direct call with a goroutine:

```go
		// Answering runs off the read loop. handleMention makes up to three
		// HTTP calls now, and this function is called from the Bedrock
		// packet reader: doing it inline stops the agent hearing anything --
		// including !help and player joins -- for the whole exchange.
		go handleMention(ctx, id, trigger, log, pctx, ans, playerRoster)
```

In `handleMention`, bound the whole attempt and use the tool path:

```go
	answerCtx, cancel := context.WithTimeout(ctx, ans.total)
	defer cancel()

	reply, err := ans.llm.AnswerWithTools(answerCtx, name, actorXUID, trigger.Message, ans.toolsFor(pctx))
```

Everything after that — the empty-reply guard, the nil-Voice guard, the broadcast — is unchanged.

Wire the new fields where `answering` is constructed in `main`:

```go
	ans := answering{
		limiter:  answerLimiter,
		llm:      llmClient,
		toolsFor: func(p *plugin.Context) *tools.Registry { return buildToolset(p, p.Profiles) },
		total:    time.Duration(cfg.LLMTotalTimeoutMs) * time.Millisecond,
	}
```

Match the existing construction's field names and surrounding variable names.

- [ ] **Step 11: Verify the race detector is clean**

The goroutine is new shared-state exposure: it reads `pctx` and the roster concurrently with the read loop.

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS with no race reports. If the race detector flags the roster, the fix is to resolve the gamertag before starting the goroutine and pass the string in, not to add a mutex.

- [ ] **Step 12: Commit**

```bash
git add cmd/agent internal/config
git commit -m "feat(agent): answer with tools, off the packet read loop

handleMention made one HTTP call inline on the Bedrock reader; with tool
rounds it makes up to three, and the agent would stop hearing joins and
commands for the whole exchange."
```

---

### Task 10: Documentation and release

**Files:**
- Modify: `README.md`
- Modify: chart values in the deployments repo

**Interfaces:**
- Consumes: everything above.
- Produces: a tagged release and a deployed chart.

- [ ] **Step 1: Update the README status block**

The `## Status` section still says Stage 2 and claims there is no database or LLM — both false since Stage 3 and 4 shipped. Replace it with a Stage 5 description covering `!kb`, `!wp`, and the tool-calling answer path, and extend the `internal/` component list with `knowledge`, `waypoints` and `tools`.

- [ ] **Step 2: Run the full suite once more**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS.

- [ ] **Step 3: Commit, push, open the PR**

```bash
git add README.md
git commit -m "docs: describe Stage 5 and correct the stale Stage 2 status"
git push -u origin feat/stage-5-knowledge-waypoints
gh pr create --fill
```

The PR description must state that `player_playtime` from the spec was not built, and why (Task 9 Step 7).

- [ ] **Step 4: Tag the release after the PR merges**

```bash
git checkout main && git pull --ff-only
git tag v0.9.0 && git push origin v0.9.0
```

Confirm the release workflow published `ghcr.io/jdwillmsen/minecraft-server-agent:0.9.0`.

- [ ] **Step 5: Bump the chart**

In the deployments repo, set the server-agent image tag to `0.9.0` and add the two configuration changes: `LLM_MAX_TOKENS: 192` and `LLM_TOTAL_TIMEOUT_MS: 20000`. Open that PR separately; it deploys through ArgoCD.

- [ ] **Step 6: Verify live, in game**

After the sync, with the pod at 1/1:

```
!kb set gold farm It is under spawn at y 12.
!kb gold
!wp set base 100 64 -200
!wp
@server where is my base
@server where is the gold farm
```

Expected: the `!kb` and `!wp` replies match what was set; both `@server` questions answer from stored facts. Then confirm the tool path actually ran rather than the model guessing:

Run: `kubectl logs -n jdwillmsen-prd deploy/jdwillmsen-minecraft-fwb-prd-server-agent --tail=50 | grep mention`
Expected: `mention_answered` events, with no `mention_answer_failed` in between.

---

## Self-Review

**Spec coverage:** every spec section maps to a task — components (2, 3, 4, 6, 7), data model (1), tool loop (8), concurrency (9), commands (6, 7), configuration (9), failure behaviour (2, 3, 4, 6, 7, 8), security (4, 9), testing (throughout), rollout (1, 10). One deliberate gap: the spec's `player_playtime` tool, dropped in Task 9 Step 7 because `plugin.PlayerStore` has no read-only playtime method and adding one is its own change. Recorded there and in the PR description rather than silently skipped.

**Amendments to the spec, both recorded in Global Constraints:** the pool is shared from `store.Postgres` rather than opened per store, and `!kb`/`!wp` are single commands with subcommands rather than one command per operation.
