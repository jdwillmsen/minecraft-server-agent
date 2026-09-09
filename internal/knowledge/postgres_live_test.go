//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// Everything else in this package's tests runs against Nop or pure helpers, so
// no query in postgres.go has ever been executed against a real database: the
// full-text lookup and the NULL author_xuid handling on Upsert are both
// unverified until this test runs.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/knowledge/
package knowledge

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// livePool opens a pool from the runtime role's DSN rather than reuse
// store.Postgres's pool: Task 5 has not added the accessor yet, so this is
// the only way this test can reach a real database until then.
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
	t.Cleanup(pool.Close)
	return pool
}

func TestPostgresRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewPostgres(livePool(t))
	topic := "__test gold farm"
	t.Cleanup(func() { _, _ = s.Delete(ctx, topic) })

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

// TestLookupFindsCompoundTopicByAnyPhrasing reproduces the bug reported
// against production: an operator wrote a fact under a single compound
// word ("goldfarm"), and every plausible way a player might ask for it --
// the bare topic, the topic spaced into two words, and either of those with
// extra words tacked on -- came back "no record found" even though the row
// was there the whole time. It also checks the inverse: a query sharing no
// word and no substring with the topic must still find nothing, so the fix
// is a real match, not "return everything".
func TestLookupFindsCompoundTopicByAnyPhrasing(t *testing.T) {
	ctx := context.Background()
	s := NewPostgres(livePool(t))
	topic := "__test goldfarm"
	t.Cleanup(func() { _, _ = s.Delete(ctx, topic) })

	if err := s.Upsert(ctx, topic, "It is at x=232 y=89 z=275", ""); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	matching := []string{
		"goldfarm",
		"gold farm",
		"goldfarm location",
		"gold farm location",
	}
	for _, q := range matching {
		entries, err := s.Lookup(ctx, q, 3)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", q, err)
		}
		if !containsTopic(entries, topic) {
			t.Errorf("Lookup(%q) did not find %q", q, topic)
		}
	}

	entries, err := s.Lookup(ctx, "zzznonexistentqueryzzz", 3)
	if err != nil {
		t.Fatalf("Lookup(nonsense query): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Lookup(nonsense query) = %v, want no results", entries)
	}
}

func containsTopic(entries []Entry, topic string) bool {
	for _, e := range entries {
		if e.Topic == topic {
			return true
		}
	}
	return false
}
