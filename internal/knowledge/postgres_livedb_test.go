package knowledge

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// livePool opens a pool from PG_* env vars rather than reuse store.Postgres's
// pool: Task 5 has not added the accessor yet, so this is the only way this
// test can reach a real database until then.
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
