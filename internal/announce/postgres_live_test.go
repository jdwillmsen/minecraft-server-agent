//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// PendingFor is the one query in this package worth distrusting until it has
// actually run: it leans on a partial index, an anti-join against
// announcement_deliveries, and a boolean-in-ORDER-BY trick to put expedited
// rows first, and every one of those is the kind of thing that reads fine
// and is subtly wrong -- an index Postgres declines to use, an anti-join
// that matches the wrong column, an ORDER BY that sorts the boolean the
// other way. A fake store built from the same assumptions as the code under
// test would just agree with itself.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/announce/
package announce

import (
	"context"
	"os"
	"testing"
	"time"

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

// recentPlayer returns the XUID of the most recently seen player.
// minecraft.announcement_deliveries has a NOT NULL foreign key to
// minecraft.players, so this test needs a row that already satisfies it
// rather than a XUID invented for the occasion.
func recentPlayer(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var xuid string
	err := pool.QueryRow(context.Background(),
		`SELECT xuid FROM minecraft.players ORDER BY last_seen_at DESC LIMIT 1`,
	).Scan(&xuid)
	if err != nil {
		t.Skip("minecraft.players has no recorded players")
	}
	return xuid
}

// cleanupAnnouncement deletes the row and, by cascade, any delivery record
// against it, so a run of this test never leaves the outbox with rows a
// future run has to work around.
func cleanupAnnouncement(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM minecraft.announcements WHERE announcement_id = $1`, id)
	})
}

func TestPendingForTracksDeliveryLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	s := NewPostgres(pool)
	xuid := recentPlayer(t, pool)
	now := time.Now().UTC()

	id, err := s.Insert(ctx, Announcement{
		Body:         "__test whisper",
		Source:       SourceCommand,
		TargetKind:   TargetPlayer,
		TargetValue:  xuid,
		Priority:     PriorityNormal,
		Delivery:     DeliveryWhisper,
		DeliverAfter: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	cleanupAnnouncement(t, pool, id)

	pending, err := s.PendingFor(ctx, xuid, "", now)
	if err != nil {
		t.Fatalf("PendingFor: %v", err)
	}
	if !containsID(pending, id) {
		t.Fatalf("PendingFor did not return the announcement just inserted for this player")
	}

	if err := s.MarkDelivered(ctx, id, xuid, now); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}

	pending, err = s.PendingFor(ctx, xuid, "", now)
	if err != nil {
		t.Fatalf("PendingFor after delivery: %v", err)
	}
	if containsID(pending, id) {
		t.Fatalf("PendingFor returned an announcement already delivered to this player")
	}

	// A second deliverer racing the same join must not fail: the conflict
	// means the player already has it, not that something went wrong.
	if err := s.MarkDelivered(ctx, id, xuid, now); err != nil {
		t.Fatalf("MarkDelivered a second time: %v", err)
	}
}

func TestPendingForExcludesExpiredAndOnlineOnly(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	s := NewPostgres(pool)
	xuid := recentPlayer(t, pool)
	now := time.Now().UTC()
	expired := now.Add(-time.Minute)

	expiredID, err := s.Insert(ctx, Announcement{
		Body:         "__test expired",
		Source:       SourceCommand,
		TargetKind:   TargetPlayer,
		TargetValue:  xuid,
		Priority:     PriorityNormal,
		Delivery:     DeliveryWhisper,
		DeliverAfter: now.Add(-time.Hour),
		ExpiresAt:    &expired,
	})
	if err != nil {
		t.Fatalf("Insert expired: %v", err)
	}
	cleanupAnnouncement(t, pool, expiredID)

	onlineOnlyID, err := s.Insert(ctx, Announcement{
		Body:         "__test online only",
		Source:       SourceCommand,
		TargetKind:   TargetOnlineOnly,
		Priority:     PriorityNormal,
		Delivery:     DeliveryBroadcast,
		DeliverAfter: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Insert online_only: %v", err)
	}
	cleanupAnnouncement(t, pool, onlineOnlyID)

	pending, err := s.PendingFor(ctx, xuid, "", now)
	if err != nil {
		t.Fatalf("PendingFor: %v", err)
	}
	if containsID(pending, expiredID) {
		t.Fatalf("PendingFor returned an announcement past its expires_at")
	}
	if containsID(pending, onlineOnlyID) {
		t.Fatalf("PendingFor returned an online_only announcement; it has no queue to be pending in")
	}
}

func TestPendingForOrdersExpeditedFirst(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	s := NewPostgres(pool)
	xuid := recentPlayer(t, pool)
	now := time.Now().UTC()

	normalID, err := s.Insert(ctx, Announcement{
		Body:         "__test normal",
		Source:       SourceCommand,
		TargetKind:   TargetPlayer,
		TargetValue:  xuid,
		Priority:     PriorityNormal,
		Delivery:     DeliveryWhisper,
		DeliverAfter: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("Insert normal: %v", err)
	}
	cleanupAnnouncement(t, pool, normalID)

	expeditedID, err := s.Insert(ctx, Announcement{
		Body:        "__test expedited",
		Source:      SourceCommand,
		TargetKind:  TargetPlayer,
		TargetValue: xuid,
		Priority:    PriorityExpedited,
		Delivery:    DeliveryWhisper,
		// Inserted after the normal one, so an ordering that ignores
		// priority and falls back to created_at would still get this wrong.
		DeliverAfter: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("Insert expedited: %v", err)
	}
	cleanupAnnouncement(t, pool, expeditedID)

	pending, err := s.PendingFor(ctx, xuid, "", now)
	if err != nil {
		t.Fatalf("PendingFor: %v", err)
	}
	normalIdx, expeditedIdx := indexOf(pending, normalID), indexOf(pending, expeditedID)
	if normalIdx == -1 || expeditedIdx == -1 {
		t.Fatalf("PendingFor did not return both test announcements: %+v", pending)
	}
	if expeditedIdx > normalIdx {
		t.Fatalf("expedited announcement sorted after normal: %+v", pending)
	}
}

func containsID(as []Announcement, id int64) bool {
	return indexOf(as, id) != -1
}

func indexOf(as []Announcement, id int64) int {
	for i, a := range as {
		if a.ID == id {
			return i
		}
	}
	return -1
}
