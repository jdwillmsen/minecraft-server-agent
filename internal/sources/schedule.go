package sources

import (
	"context"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// ScheduleRunner is the part of the schedule store the loop needs: what is
// due, and a way to claim one occurrence of it.
type ScheduleRunner interface {
	DueSchedules(ctx context.Context, now time.Time, limit int) ([]announce.Schedule, error)
	FireSchedule(ctx context.Context, s announce.Schedule, next time.Time, a announce.Announcement) (id int64, claimed bool, err error)
	Enabled() bool
}

// Sender delivers an already-stored announcement to whoever is online.
// Satisfied by announce.Deliverer.
type Sender interface {
	SendNow(ctx context.Context, a announce.Announcement, id int64) (int, error)
}

// ScheduleInterval is how often the loop looks for due schedules. A daily
// time is to the minute, so looking more often finds nothing new.
const ScheduleInterval = time.Minute

// maxDuePerTick bounds one tick's work. Each fire is a transaction and a
// send; a backlog larger than this is worked through over the next ticks,
// most overdue first, rather than in one burst of broadcasts.
const maxDuePerTick = 20

// Scheduler fires due schedules.
type Scheduler struct {
	store   ScheduleRunner
	send    Sender
	log     *logging.Logger
	now     func() time.Time
	unready sync.Once
}

// NewScheduler builds the schedule loop.
func NewScheduler(store ScheduleRunner, send Sender, log *logging.Logger) *Scheduler {
	return &Scheduler{store: store, send: send, log: log, now: time.Now}
}

// Run ticks at once, so a schedule that fell due while the agent was down
// fires as soon as it is back, then every ScheduleInterval until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(ScheduleInterval)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick fires every schedule due now, once each, and reports how many this
// call claimed.
//
// A schedule that fell behind fires once, for the occurrence that was due,
// and moves to the next occurrence still in the future (Cadence.Next): its
// announcement expires there, so a reminder is never replayed for each slot
// the agent was away for. A claim this call loses -- another agent moved the
// row first -- is not an error and sends nothing.
func (s *Scheduler) Tick(ctx context.Context) (fired int) {
	if !s.store.Enabled() {
		return 0
	}
	now := s.now()
	due, err := s.store.DueSchedules(ctx, now, maxDuePerTick)
	if err != nil {
		s.report("schedule_due", err)
		return 0
	}
	for _, sch := range due {
		if ctx.Err() != nil {
			break
		}
		if err := sch.Cadence.Validate(); err != nil || sch.TargetKind == announce.TargetPlayer {
			// The table's CHECKs make this unreachable through the agent;
			// a row that got past them anyway is logged every tick until a
			// person looks, which is the point.
			s.log.Error("schedule_invalid", logging.Fields{"schedule_id": sch.ID, "target": string(sch.TargetKind)})
			continue
		}
		next := sch.Cadence.Next(sch.NextFireAt, now)
		a := sch.Announcement(now, next)
		id, claimed, err := s.store.FireSchedule(ctx, sch, next, a)
		if err != nil {
			s.report("schedule_fire", err)
			continue
		}
		if !claimed {
			s.log.Debug("schedule_claim_lost", logging.Fields{"schedule_id": sch.ID})
			continue
		}
		fired++
		a.ID = id
		sendCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		if _, err := s.send.SendNow(sendCtx, a, id); err != nil {
			// Stored already, so the queue still owns it for anyone who joins
			// before it expires.
			s.log.Error("schedule_send_failed", logging.Fields{"schedule_id": sch.ID, "announcement_id": id, "error": err.Error()})
		}
		cancel()
	}
	return fired
}

// report logs a store failure. A table not migrated or not granted yet is
// said once: the loop asks every minute and every answer would be the same.
func (s *Scheduler) report(op string, err error) {
	if pgerr.Unready(err) {
		s.unready.Do(func() {
			s.log.Info("schedule_store_unready", logging.Fields{"op": op, "error": err.Error()})
		})
		return
	}
	s.log.Error("schedule_store_failed", logging.Fields{"op": op, "error": err.Error()})
}
