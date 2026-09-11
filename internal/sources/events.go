// Package sources holds the announcement sources no person types: things
// the server notices about itself, and reminders set to repeat.
//
// Each one's whole job is to describe an announcement correctly and hand it
// to the outbox. Who hears it, when, and whether it is still worth saying
// belong to the deliverer, exactly as they do for !announce -- which is what
// lets a source be this small.
package sources

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// Publisher stores an announcement and sends it to whoever is online.
// Satisfied by announce.Deliverer.
type Publisher interface {
	Publish(ctx context.Context, a announce.Announcement) (id int64, sent int, err error)
}

// publishTimeout bounds one event's store write and immediate delivery. A
// permission target is a whisper per online operator, each its own bridge
// call, so this matches the join drain's budget rather than one command's.
const publishTimeout = 30 * time.Second

// PlaytimeMilestones are the totals worth saying out loud, ascending.
var PlaytimeMilestones = []time.Duration{10 * time.Hour, 24 * time.Hour, 100 * time.Hour, 500 * time.Hour}

// CrossedMilestone reports the highest milestone that before is short of
// and after has reached.
//
// Reaching one exactly counts: before < m <= after. The asymmetry is what
// stops a milestone firing twice -- a player who left at exactly 10h has
// before = 10h next time, which is no longer short of it. Only the highest
// of several crossed at once is reported: one departure is one piece of
// news, and a player whose first recorded session was forty hours long has
// passed 24 hours, which says everything 10 hours would have.
func CrossedMilestone(before, after time.Duration) (time.Duration, bool) {
	var crossed time.Duration
	for _, m := range PlaytimeMilestones {
		if before < m && m <= after {
			crossed = m
		}
	}
	return crossed, crossed > 0
}

// operatorTarget is the permission level an operator-only notice is aimed
// at, spelt the way the deliverer resolves levels. Taken from the enum so a
// rename there cannot leave these notices aimed at a level nobody holds.
var operatorTarget = plugin.PermissionOperator.String()

// Events turns what the agent observes about players into announcements:
// the first sight of a player, whispered to operators, and a playtime
// milestone, broadcast.
type Events struct {
	// rootCtx is the process lifetime: an event is observed on the packet
	// read loop, and its delivery must outlive the moment that noticed it.
	rootCtx context.Context
	pub     Publisher
	log     *logging.Logger
	now     func() time.Time
	// spawn runs a publish. A goroutine in production, because delivery
	// makes bridge calls and the caller is the packet read loop; inline in
	// tests, so a test can assert on what was published without sleeping.
	// Unbounded on purpose: both events are rare by construction -- a first
	// join happens once per player, a milestone four times per player ever.
	spawn   func(func())
	unready sync.Once
}

// NewEvents builds the player event source.
func NewEvents(rootCtx context.Context, pub Publisher, log *logging.Logger) *Events {
	return &Events{rootCtx: rootCtx, pub: pub, log: log, now: time.Now, spawn: func(f func()) { go f() }}
}

// Joined takes the profile RecordJoin returned -- the player as they stood
// before this arrival -- and tells operators about a first-ever one.
//
// Operators rather than the server: the welcome already greets the player
// in public, and a second public line about the same arrival is noise. The
// caller must only pass a profile a real store produced; a disabled store
// reports every arrival as new because it remembers nobody.
//
// First-ever means no row existed, not a join count of zero: a player first
// met already online has a row and no counted join, and was announced by
// FirstSeenOnline when that row was created. One notice per player, ever.
func (e *Events) Joined(gamertag string, prior store.Profile) {
	if !prior.FirstSeen.IsZero() || gamertag == "" {
		return
	}
	e.publish("first_join", announce.TargetPermission, operatorTarget,
		fmt.Sprintf("First time here: %s just joined the server for the first time.", gamertag))
}

// FirstSeenOnline tells operators about a player the store just recorded for
// the first time without watching them arrive -- one already connected when
// the agent began watching. The same notice as Joined, worded for what the
// agent actually saw.
func (e *Events) FirstSeenOnline(gamertag string) {
	if gamertag == "" {
		return
	}
	e.publish("first_seen", announce.TargetPermission, operatorTarget,
		fmt.Sprintf("New player: %s, first seen while already online.", gamertag))
}

// Left announces a playtime milestone the departure just carried the player
// past, if it carried them past one.
func (e *Events) Left(pt store.Playtime) {
	m, ok := CrossedMilestone(pt.Before, pt.After)
	if !ok || pt.Gamertag == "" {
		return
	}
	e.publish("playtime_milestone", announce.TargetEveryone, "",
		fmt.Sprintf("%s has now played %d hours on this server.", pt.Gamertag, int(m.Hours())))
}

func (e *Events) publish(event string, target announce.Target, value, body string) {
	a := eventAnnouncement(body, target, value, e.now())
	e.spawn(func() {
		ctx, cancel := context.WithTimeout(e.rootCtx, publishTimeout)
		defer cancel()
		_, _, err := e.pub.Publish(ctx, a)
		reportPublish(e.log, &e.unready, event, err)
	})
}

// eventAnnouncement describes an event-sourced announcement. Its lifetime is
// DefaultExpiry's for the target -- a day, for every target an event uses --
// because a milestone is worth mentioning on the next visit, not a
// fortnight later.
func eventAnnouncement(body string, target announce.Target, value string, now time.Time) announce.Announcement {
	return announce.Announcement{
		Body:         body,
		Source:       announce.SourceEvent,
		TargetKind:   target,
		TargetValue:  value,
		Priority:     announce.PriorityNormal,
		Delivery:     announce.DeliveryFor(target),
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    announce.DefaultExpiry(announce.SourceEvent, target, now),
	}
}

// reportPublish logs how a publish ended. No store is a supported state and
// says nothing; a store the deploy has not readied yet is said once, since
// every later event hits the same wall until the release that fixes it.
func reportPublish(log *logging.Logger, unready *sync.Once, event string, err error) {
	switch {
	case err == nil, errors.Is(err, announce.ErrDisabled):
	case pgerr.Unready(err):
		unready.Do(func() {
			log.Info("announce_source_unready", logging.Fields{"event": event, "error": err.Error()})
		})
	default:
		log.Error("announce_source_failed", logging.Fields{"event": event, "error": err.Error()})
	}
}

// ServerState is what the server watcher reads. Satisfied by
// adapters.MetricsFacts.
type ServerState interface {
	StatusEnabled() bool
	BackupEnabled() bool
	CurrentVersion(ctx context.Context) (string, error)
	BackupState(ctx context.Context) (adapters.BackupState, error)
}

// PollInterval spaces the server watcher's readings. News about a version
// or a backup is not urgent to the minute, and each reading is two scrapes.
const PollInterval = 5 * time.Minute

// change remembers the last value seen, and reports a new one only once a
// baseline exists. Kept in memory on purpose: after a restart the first
// reading is the baseline, so a restart never re-announces old news.
type change[T comparable] struct {
	seen bool
	last T
}

func (c *change[T]) observe(v T) (changed bool) {
	if !c.seen {
		c.seen, c.last = true, v
		return false
	}
	changed = v != c.last
	c.last = v
	return changed
}

// Watcher announces changes to the server itself: a new Bedrock version,
// broadcast, and a backup gone stale, whispered to operators.
//
// Once per change, never once per poll. A backup that stays stale is
// announced once, and again only after it has recovered and gone stale
// anew; a reading that failed or said nothing is skipped rather than taken
// as a change, so an exporter blip is never mistaken for a recovery.
type Watcher struct {
	state   ServerState
	pub     Publisher
	log     *logging.Logger
	now     func() time.Time
	version change[string]
	stale   change[bool]
	unready sync.Once
}

// NewWatcher builds the server watcher.
func NewWatcher(state ServerState, pub Publisher, log *logging.Logger) *Watcher {
	return &Watcher{state: state, pub: pub, log: log, now: time.Now}
}

// Run polls at once for a baseline, then every PollInterval until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		w.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Poll takes one reading of each configured exporter and announces what
// changed since the last one.
//
// The state advances whether or not the announcement is stored. A change
// retried later is old news by the time it lands -- the same judgement
// that makes a restart start from a baseline.
func (w *Watcher) Poll(ctx context.Context) {
	if w.state.StatusEnabled() {
		v, err := w.state.CurrentVersion(ctx)
		switch {
		case err != nil:
			w.log.Debug("announce_watch_version_failed", logging.Fields{"error": err.Error()})
		case v != "" && w.version.observe(v):
			w.publish(ctx, "version_changed", announce.TargetEveryone, "",
				fmt.Sprintf("The server has been updated to Bedrock %s.", v))
		}
	}
	if w.state.BackupEnabled() {
		st, err := w.state.BackupState(ctx)
		switch {
		case err != nil:
			w.log.Debug("announce_watch_backup_failed", logging.Fields{"error": err.Error()})
		case !st.Completed || st.MaxAge == 0:
			// No backup yet, or no policy to be stale against: nothing to
			// compare, so neither a stale reading nor a recovery.
		case w.stale.observe(st.Stale()) && st.Stale():
			w.publish(ctx, "backup_stale", announce.TargetPermission, operatorTarget,
				fmt.Sprintf("Backup warning: the last world backup is %s old, past the %s the backup policy allows.",
					roundAge(st.Age), roundAge(st.MaxAge)))
		}
	}
}

func (w *Watcher) publish(ctx context.Context, event string, target announce.Target, value, body string) {
	pubCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	_, _, err := w.pub.Publish(pubCtx, eventAnnouncement(body, target, value, w.now()))
	reportPublish(w.log, &w.unready, event, err)
}

// roundAge renders a backup age to the minute; a policy is set in hours and
// seconds of precision only make the line harder to read.
func roundAge(d time.Duration) string {
	return d.Round(time.Minute).String()
}
