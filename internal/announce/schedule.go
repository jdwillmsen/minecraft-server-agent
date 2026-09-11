package announce

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MinEvery is the shortest interval a schedule may repeat at. The table's
// CHECK enforces the same floor; this is the one that answers first, with a
// reason, so a typo is told off in chat rather than by a constraint error.
// Fifteen minutes is where a reminder stops being one and becomes spam.
const MinEvery = 15 * time.Minute

// MaxEvery is the longest interval. Not in the schema: it exists because
// every_seconds is an INTEGER, and "every 999999h" must be refused plainly
// rather than overflow. A week is past any reminder anyone has asked for,
// and anything rarer is better sent by hand.
const MaxEvery = 7 * 24 * time.Hour

// Cadence is how often a schedule fires: every fixed interval, or once a
// day at a time of day.
//
// Daily times are UTC, not a configured zone, so no daylight-saving rule
// has to be built into this: 18:00 is 18:00 every day of the year, and
// every place that shows one says UTC so nobody has to guess.
type Cadence struct {
	// Every is the interval, for a repeating cadence. Zero when Daily.
	Every time.Duration
	Daily bool
	// At is how far past UTC midnight a daily schedule fires.
	At time.Duration
}

// Validate reports why c cannot be stored, or nil.
func (c Cadence) Validate() error {
	switch {
	case c.Daily && c.Every != 0:
		return errors.New("a schedule is either daily or every interval, not both")
	case c.Daily && (c.At < 0 || c.At >= 24*time.Hour):
		return errors.New("a daily time must be between 00:00 and 23:59")
	case c.Daily:
		return nil
	case c.Every < MinEvery:
		return fmt.Errorf("a schedule can repeat at most every %s", formatEvery(MinEvery))
	case c.Every > MaxEvery:
		return fmt.Errorf("a schedule must repeat at least every %s", formatEvery(MaxEvery))
	}
	return nil
}

// Next is the first occurrence strictly after now. due is the occurrence
// that has just fired, or zero for a schedule being created.
//
// Strictly after now, whatever due was, is what stops a schedule that fell
// behind -- the agent was down for a day -- from replaying every slot it
// missed: it fires once, for the slot that was due, and moves straight to
// the next one still in the future. An interval keeps its phase from due
// rather than restarting from now, so a reminder set for the top of the
// hour stays on the hour after an outage.
func (c Cadence) Next(due, now time.Time) time.Time {
	now = now.UTC()
	if c.Daily {
		y, m, d := now.Date()
		next := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(c.At)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		return next
	}
	if due.IsZero() || c.Every <= 0 {
		return now.Add(c.Every)
	}
	k := now.Sub(due)/c.Every + 1
	if k < 1 {
		k = 1
	}
	return due.Add(k * c.Every).UTC()
}

// String renders c the way !schedule add takes it, with UTC spelt out.
func (c Cadence) String() string {
	if c.Daily {
		return fmt.Sprintf("daily %02d:%02d UTC", int(c.At.Hours()), int(c.At.Minutes())%60)
	}
	return "every " + formatEvery(c.Every)
}

func formatEvery(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// Schedule is a reminder the server repeats on a cadence.
//
// It has no 'player' target: a recurring whisper to one person is a nag,
// and !announce @player already covers the one-off. The table's CHECK says
// the same.
type Schedule struct {
	ID          int64
	Body        string
	AuthorXUID  string
	TargetKind  Target
	TargetValue string
	Priority    Priority
	Cadence     Cadence
	NextFireAt  time.Time
	Active      bool
	CreatedAt   time.Time
}

// Announcement describes the one announcement this schedule produces when
// the occurrence due at s.NextFireAt fires, next being the occurrence after.
//
// It expires at next, so a reminder nobody was online to hear is replaced
// by the next one rather than stacked on top of it. No author: the row that
// carries a person's name is the schedule, which schedule_id points back to;
// the announcement itself has no human behind it at the moment it fires.
func (s Schedule) Announcement(now, next time.Time) Announcement {
	a := Announcement{
		Body:         s.Body,
		Source:       SourceSchedule,
		TargetKind:   s.TargetKind,
		TargetValue:  s.TargetValue,
		Priority:     s.Priority,
		Delivery:     DeliveryFor(s.TargetKind),
		CreatedAt:    now,
		DeliverAfter: now,
		ScheduleID:   s.ID,
	}
	if Queues(s.TargetKind) {
		a.ExpiresAt = &next
	}
	return a
}

// ScheduleStore persists schedules and hands each due occurrence out once.
type ScheduleStore interface {
	AddSchedule(ctx context.Context, s Schedule) (int64, error)
	// ListSchedules returns every active schedule, oldest first.
	ListSchedules(ctx context.Context) ([]Schedule, error)
	// DeactivateSchedule stops a schedule without deleting it, so the
	// announcements it already produced keep their provenance. ok is false
	// when no active schedule has that id.
	DeactivateSchedule(ctx context.Context, id int64) (ok bool, err error)
	// DueSchedules returns up to limit active schedules due at or before now,
	// most overdue first.
	DueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error)
	// FireSchedule claims the occurrence s.NextFireAt by moving the schedule
	// to next, and stores a as the announcement it produces, together. claimed
	// is false when another caller already moved it: the occurrence changes
	// hands exactly once, however many agents race for it.
	FireSchedule(ctx context.Context, s Schedule, next time.Time, a Announcement) (id int64, claimed bool, err error)
	Enabled() bool
}

var _ ScheduleStore = Nop{}

func (Nop) AddSchedule(context.Context, Schedule) (int64, error)    { return 0, nil }
func (Nop) ListSchedules(context.Context) ([]Schedule, error)       { return nil, nil }
func (Nop) DeactivateSchedule(context.Context, int64) (bool, error) { return false, nil }
func (Nop) DueSchedules(context.Context, time.Time, int) ([]Schedule, error) {
	return nil, nil
}
func (Nop) FireSchedule(context.Context, Schedule, time.Time, Announcement) (int64, bool, error) {
	return 0, false, nil
}
