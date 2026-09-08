//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// No query in postgres.go has ever run against a real database, and the
// property this test asserts -- one player cannot read another player's
// waypoint -- is a row-scoping claim that only a real database can settle;
// a fake or in-memory store would just reimplement the same assumption
// under test.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/waypoints/
package waypoints

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

// twoRecentPlayers returns the XUIDs of the two most recently seen players.
// minecraft.waypoints has a NOT NULL foreign key to minecraft.players, so
// this test needs two rows that already satisfy it rather than a XUID
// invented for the occasion -- unlike knowledge, which has no such
// constraint to work around.
func twoRecentPlayers(t *testing.T, pool *pgxpool.Pool) (owner, other string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT xuid FROM minecraft.players ORDER BY last_seen_at DESC LIMIT 2`)
	if err != nil {
		t.Fatalf("query players: %v", err)
	}
	defer rows.Close()

	var xuids []string
	for rows.Next() {
		var xuid string
		if err := rows.Scan(&xuid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		xuids = append(xuids, xuid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read players: %v", err)
	}
	if len(xuids) < 2 {
		t.Skip("minecraft.players has fewer than two recorded players")
	}
	return xuids[0], xuids[1]
}

func TestWaypointsAreScopedToTheirOwner(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	s := NewPostgres(pool)
	owner, other := twoRecentPlayers(t, pool)
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
