//go:build livedb

// The schedule SQL against a real PostgreSQL, as the runtime role. The
// claim is the query worth distrusting until it has run: a compare-and-set
// on a timestamp only works if the value read back compares equal to the
// value stored, and TIME and nullable INTEGER columns have to survive the
// round trip through pgtype for a cadence to mean what it meant.
package announce

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func cleanupSchedule(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM minecraft.announcements WHERE schedule_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM minecraft.announcement_schedules WHERE schedule_id = $1`, id)
	})
}

func findSchedule(ss []Schedule, id int64) (Schedule, bool) {
	for _, s := range ss {
		if s.ID == id {
			return s, true
		}
	}
	return Schedule{}, false
}

func TestScheduleClaimsEachOccurrenceOnce(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	p := NewPostgres(pool)
	now := time.Now().UTC()

	id, err := p.AddSchedule(ctx, Schedule{
		Body: "__test schedule", TargetKind: TargetEveryone, Priority: PriorityNormal,
		Cadence: Cadence{Every: MinEvery}, NextFireAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}
	cleanupSchedule(t, pool, id)

	due, err := p.DueSchedules(ctx, now, 1000)
	if err != nil {
		t.Fatalf("DueSchedules: %v", err)
	}
	s, ok := findSchedule(due, id)
	if !ok {
		t.Fatal("a schedule due a minute ago was not returned as due")
	}
	if s.Cadence != (Cadence{Every: MinEvery}) {
		t.Errorf("cadence read back as %+v", s.Cadence)
	}

	next := s.Cadence.Next(s.NextFireAt, now)
	annID, claimed, err := p.FireSchedule(ctx, s, next, s.Announcement(now, next))
	if err != nil || !claimed {
		t.Fatalf("first FireSchedule = (%d, %v, %v), want a claim", annID, claimed, err)
	}
	var scheduleID int64
	var source string
	if err := pool.QueryRow(ctx,
		`SELECT schedule_id, source FROM minecraft.announcements WHERE announcement_id = $1`, annID,
	).Scan(&scheduleID, &source); err != nil {
		t.Fatalf("read fired announcement: %v", err)
	}
	if scheduleID != id || source != "schedule" {
		t.Errorf("fired announcement = (schedule %d, %s), want (%d, schedule)", scheduleID, source, id)
	}

	// A second caller holding the same read loses: the row has moved.
	if _, claimed, err := p.FireSchedule(ctx, s, next, s.Announcement(now, next)); err != nil || claimed {
		t.Errorf("second FireSchedule claimed = %v (err %v), want false", claimed, err)
	}
	due, err = p.DueSchedules(ctx, now, 1000)
	if err != nil {
		t.Fatalf("DueSchedules after firing: %v", err)
	}
	if _, ok := findSchedule(due, id); ok {
		t.Error("a claimed schedule is still due")
	}
}

func TestScheduleDailyRoundTripAndDeactivate(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	p := NewPostgres(pool)
	daily := Cadence{Daily: true, At: 18*time.Hour + 30*time.Minute}

	id, err := p.AddSchedule(ctx, Schedule{
		Body: "__test daily", TargetKind: TargetOnlineOnly, Priority: PriorityExpedited,
		Cadence: daily, NextFireAt: daily.Next(time.Time{}, time.Now()),
	})
	if err != nil {
		t.Fatalf("AddSchedule: %v", err)
	}
	cleanupSchedule(t, pool, id)

	all, err := p.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules: %v", err)
	}
	s, ok := findSchedule(all, id)
	if !ok {
		t.Fatal("a new schedule is not listed")
	}
	if s.Cadence != daily || s.TargetKind != TargetOnlineOnly || s.Priority != PriorityExpedited || !s.Active {
		t.Errorf("schedule read back as %+v", s)
	}

	if ok, err := p.DeactivateSchedule(ctx, id); err != nil || !ok {
		t.Fatalf("DeactivateSchedule = (%v, %v), want true", ok, err)
	}
	if ok, err := p.DeactivateSchedule(ctx, id); err != nil || ok {
		t.Errorf("second DeactivateSchedule = (%v, %v), want false", ok, err)
	}
	all, err = p.ListSchedules(ctx)
	if err != nil {
		t.Fatalf("ListSchedules after deactivating: %v", err)
	}
	if _, ok := findSchedule(all, id); ok {
		t.Error("a deactivated schedule is still listed")
	}
}
