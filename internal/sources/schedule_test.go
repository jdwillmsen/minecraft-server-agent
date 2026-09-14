package sources

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// claimStore holds schedules in memory with the same compare-and-set the
// Postgres claim makes: an occurrence moves only if the row still holds the
// NextFireAt the caller read. Built that way, and not as a store that
// simply records calls, so a loop that fired without claiming, or claimed
// against a stale read, fails here the way it would against the database.
type claimStore struct {
	mu        sync.Mutex
	rows      map[int64]announce.Schedule
	fired     []announce.Announcement
	dueErr    error
	enabled   bool
	staleRead bool // DueSchedules returns rows as they were before any claim
}

func newClaimStore(ss ...announce.Schedule) *claimStore {
	c := &claimStore{rows: map[int64]announce.Schedule{}, enabled: true}
	for _, s := range ss {
		s.Active = true
		c.rows[s.ID] = s
	}
	return c
}

func (c *claimStore) Enabled() bool { return c.enabled }

func (c *claimStore) DueSchedules(_ context.Context, now time.Time, limit int) ([]announce.Schedule, error) {
	if c.dueErr != nil {
		return nil, c.dueErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []announce.Schedule
	for _, s := range c.rows {
		if s.Active && !s.NextFireAt.After(now) && len(out) < limit {
			out = append(out, s)
		}
	}
	return out, nil
}

func (c *claimStore) FireSchedule(_ context.Context, s announce.Schedule, next time.Time, a announce.Announcement) (int64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.rows[s.ID]
	if !ok || !row.Active || !row.NextFireAt.Equal(s.NextFireAt) {
		return 0, false, nil
	}
	row.NextFireAt = next
	c.rows[s.ID] = row
	c.fired = append(c.fired, a)
	return int64(len(c.fired)), true, nil
}

func (c *claimStore) DeactivateSchedule(_ context.Context, id int64) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.rows[id]
	if !ok || !row.Active {
		return false, nil
	}
	row.Active = false
	c.rows[id] = row
	return true, nil
}

func (c *claimStore) row(id int64) announce.Schedule {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rows[id]
}

type recordingSender struct {
	mu   sync.Mutex
	sent []int64
}

func (r *recordingSender) SendNow(_ context.Context, _ announce.Announcement, id int64) (announce.Reach, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, id)
	return announce.Reach{Players: 1, Counted: true}, nil
}

func (r *recordingSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func schedulerAt(store ScheduleRunner, send Sender, now time.Time) *Scheduler {
	s := NewScheduler(store, send, logging.New("error"))
	s.now = func() time.Time { return now }
	return s
}

var tickNow = time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)

func TestTickFiresADueScheduleOnceAndMovesItOn(t *testing.T) {
	store := newClaimStore(announce.Schedule{
		ID: 1, Body: "vote", TargetKind: announce.TargetEveryone, Priority: announce.PriorityNormal,
		Cadence: announce.Cadence{Every: time.Hour}, NextFireAt: tickNow.Add(-time.Minute),
	})
	send := &recordingSender{}
	s := schedulerAt(store, send, tickNow)

	if fired := s.Tick(context.Background()); fired != 1 {
		t.Fatalf("fired %d, want 1", fired)
	}
	want := tickNow.Add(-time.Minute).Add(time.Hour)
	if got := store.row(1).NextFireAt; !got.Equal(want) {
		t.Errorf("next fire = %v, want %v", got, want)
	}
	a := store.fired[0]
	if a.Source != announce.SourceSchedule || a.ScheduleID != 1 {
		t.Errorf("announcement = %+v, want schedule-sourced with schedule id 1", a)
	}
	if a.ExpiresAt == nil || !a.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want the next occurrence %v", a.ExpiresAt, want)
	}
	if send.count() != 1 {
		t.Errorf("sent %d, want 1", send.count())
	}

	// Same minute again: nothing is due any more.
	if fired := s.Tick(context.Background()); fired != 0 {
		t.Errorf("a second tick fired %d, want 0", fired)
	}
}

// The agent was down for a day. An hourly reminder fires once for the slot
// it missed and moves to the next hour ahead -- not 24 times.
func TestTickNeverReplaysMissedOccurrences(t *testing.T) {
	cases := []struct {
		name    string
		cadence announce.Cadence
		due     time.Time
		next    time.Time
	}{
		{"hourly, a day behind", announce.Cadence{Every: time.Hour}, tickNow.Add(-24 * time.Hour), time.Date(2026, 9, 10, 13, 30, 0, 0, time.UTC)},
		{"daily, three days behind", announce.Cadence{Daily: true, At: 18 * time.Hour}, time.Date(2026, 9, 7, 18, 0, 0, 0, time.UTC), time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newClaimStore(announce.Schedule{ID: 1, Body: "x", TargetKind: announce.TargetEveryone, Cadence: tc.cadence, NextFireAt: tc.due})
			send := &recordingSender{}
			s := schedulerAt(store, send, tickNow)

			total := 0
			for i := 0; i < 5; i++ {
				total += s.Tick(context.Background())
			}
			if total != 1 || send.count() != 1 {
				t.Fatalf("fired %d and sent %d across five ticks, want exactly one of each", total, send.count())
			}
			if got := store.row(1).NextFireAt; !got.Equal(tc.next) {
				t.Errorf("next fire = %v, want %v", got, tc.next)
			}
			if !tc.next.After(tickNow) {
				t.Fatal("test case is wrong: the next occurrence must be in the future")
			}
		})
	}
}

// Two agents reading the same due row at the same moment: the claim lets
// exactly one of them fire it.
func TestTickClaimsEachOccurrenceOnceAcrossRacingAgents(t *testing.T) {
	store := newClaimStore(announce.Schedule{
		ID: 1, Body: "x", TargetKind: announce.TargetEveryone,
		Cadence: announce.Cadence{Every: time.Hour}, NextFireAt: tickNow.Add(-time.Minute),
	})
	send := &recordingSender{}

	const agents = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	start := make(chan struct{})
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := schedulerAt(store, send, tickNow)
			<-start
			n := s.Tick(context.Background())
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if total != 1 || send.count() != 1 {
		t.Errorf("fired %d and sent %d across %d racing agents, want 1 and 1", total, send.count(), agents)
	}
}

// The deterministic half of the race: a read taken before another agent's
// claim loses, and sends nothing.
func TestFireAgainstAStaleReadIsNotClaimed(t *testing.T) {
	store := newClaimStore(announce.Schedule{
		ID: 1, Body: "x", TargetKind: announce.TargetEveryone,
		Cadence: announce.Cadence{Every: time.Hour}, NextFireAt: tickNow.Add(-time.Minute),
	})
	stale, _ := store.DueSchedules(context.Background(), tickNow, 10)

	if fired := schedulerAt(store, &recordingSender{}, tickNow).Tick(context.Background()); fired != 1 {
		t.Fatalf("first agent fired %d, want 1", fired)
	}
	next := stale[0].Cadence.Next(stale[0].NextFireAt, tickNow)
	if _, claimed, _ := store.FireSchedule(context.Background(), stale[0], next, stale[0].Announcement(tickNow, next)); claimed {
		t.Error("a claim against a read another agent already moved succeeded")
	}
}

func TestTickLeavesSchedulesThatAreNotDueAlone(t *testing.T) {
	store := newClaimStore(announce.Schedule{
		ID: 1, Body: "x", TargetKind: announce.TargetEveryone,
		Cadence: announce.Cadence{Every: time.Hour}, NextFireAt: tickNow.Add(time.Minute),
	})
	if fired := schedulerAt(store, &recordingSender{}, tickNow).Tick(context.Background()); fired != 0 {
		t.Errorf("fired %d, want 0 for a schedule a minute in the future", fired)
	}
}

// Against a database without the schedules table, or without the grant on
// it, the loop keeps running and fires nothing; with no store at all it
// does not even ask.
func TestTickDegradesWithoutAUsableStore(t *testing.T) {
	for _, code := range []string{"42P01", "42501"} {
		store := newClaimStore()
		store.dueErr = fmt.Errorf("announce: due schedules: %w", &pgconn.PgError{Code: code})
		s := schedulerAt(store, &recordingSender{}, tickNow)
		for i := 0; i < 3; i++ {
			if fired := s.Tick(context.Background()); fired != 0 {
				t.Errorf("%s: fired %d, want 0", code, fired)
			}
		}
	}

	if fired := schedulerAt(announce.Nop{}, &recordingSender{}, tickNow).Tick(context.Background()); fired != 0 {
		t.Errorf("a disabled store fired %d", fired)
	}
}

// A row the agent could never have written -- an unusable cadence, a player
// target -- is switched off rather than left due. Left due, a batch full of
// them would be read every tick and the valid schedule behind them would
// never fire.
func TestTickDeactivatesUnfireableRowsSoTheyCannotStarveOthers(t *testing.T) {
	var rows []announce.Schedule
	for i := int64(1); i <= maxDuePerTick; i++ {
		rows = append(rows, announce.Schedule{
			ID: i, Body: "broken", TargetKind: announce.TargetEveryone, Priority: announce.PriorityNormal,
			Cadence: announce.Cadence{Every: time.Minute}, NextFireAt: tickNow.Add(-time.Hour),
		})
	}
	rows = append(rows, announce.Schedule{
		ID: 100, Body: "whisper", TargetKind: announce.TargetPlayer, TargetValue: "2535400000000001", Priority: announce.PriorityNormal,
		Cadence: announce.Cadence{Every: time.Hour}, NextFireAt: tickNow.Add(-time.Hour),
	})
	valid := announce.Schedule{
		ID: 200, Body: "vote", TargetKind: announce.TargetEveryone, Priority: announce.PriorityNormal,
		Cadence: announce.Cadence{Every: time.Hour}, NextFireAt: tickNow.Add(-time.Minute),
	}
	store := newClaimStore(append(rows, valid)...)
	send := &recordingSender{}
	s := schedulerAt(store, send, tickNow)

	// Two ticks cover every row even at one batch per tick; with the
	// unfireable rows left due, the valid one could lose every draw.
	s.Tick(context.Background())
	s.Tick(context.Background())

	if got := store.row(valid.ID).NextFireAt; got.Equal(valid.NextFireAt) {
		t.Fatalf("the valid schedule never fired: it stayed due at %v behind the unfireable rows", got)
	}
	for _, r := range rows {
		if store.row(r.ID).Active {
			t.Errorf("schedule %d is still active; an unfireable row must be switched off", r.ID)
		}
	}
	if len(store.fired) != 1 || store.fired[0].ScheduleID != valid.ID {
		t.Errorf("fired = %+v, want only the valid schedule", store.fired)
	}
}
