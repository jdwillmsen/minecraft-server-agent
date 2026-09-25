package presence

import (
	"context"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	// TickInterval is how often the leader re-decides every actor.
	TickInterval = 10 * time.Second
	// KickGrace is how long a parked actor may stay on the server before it
	// is kicked. Bedrock keeps a session open for a while after a client
	// leaves, and the chunks around it keep ticking until it goes.
	KickGrace = 20 * time.Second
	// statusStale is how old an actor's last report may be and still count
	// as connected: six bot polls. A bot that crashed stops reporting, and
	// its last row would otherwise say connected forever.
	statusStale = 60 * time.Second
	// tickTimeout keeps one tick inside its interval, so a hung database
	// cannot stack ticks behind it.
	tickTimeout = 8 * time.Second
)

// Console is the part of the server console the loop needs.
type Console interface {
	OnlinePlayers(ctx context.Context) ([]string, error)
	Kick(ctx context.Context, gamertag string) error
}

// Roster is who the agent already believes is on the server, kept current by
// its own session or by the console bridge while it is parked. It is
// consulted before the console because every list is a console command and
// a line in the server log, and an actor parked by default is due for a kick
// on every tick.
type Roster interface {
	Knows() bool
	Online() []string
	NameFor(xuid string) (name string, ok bool)
}

type LoopConfig struct {
	Service *Service
	Console Console
	Roster  Roster
	Joins   *JoinLog
	Gate    *Gate
	// SessionUp reports whether this process's own session is in the world.
	SessionUp func() bool
	// Version is this process's release, reported in its own status row.
	Version string
	Log     *logging.Logger
}

// Loop is the leader's half of presence: the only thing that removes ended
// overrides, kicks actors that stayed, and exports the presence metrics.
// Everything it acts on is read back from Postgres each tick, so a new
// leader carries on where the last one stopped.
type Loop struct {
	svc      *Service
	console  Console
	roster   Roster
	joins    *JoinLog
	gate     *Gate
	up       func() bool
	version  string
	log      *logging.Logger
	interval time.Duration
	nudge    chan struct{}
	unready  sync.Once
}

// NewLoop also registers the loop for the service's writes, so a change made
// on the leader moves its own gate at once instead of on the next tick.
func NewLoop(cfg LoopConfig) *Loop {
	l := &Loop{
		svc: cfg.Service, console: cfg.Console, roster: cfg.Roster, joins: cfg.Joins, gate: cfg.Gate,
		up: cfg.SessionUp, version: cfg.Version, log: cfg.Log,
		interval: TickInterval, nudge: make(chan struct{}, 1),
	}
	cfg.Service.OnChange(l.Nudge)
	return l
}

// Nudge asks for a tick now rather than at the next interval. Never blocks:
// one pending nudge covers any number of writes.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

// Prime runs one tick before this leader joins the world, so its own gate
// already holds the stored answer when the session lifecycle first reads
// it. Without it a leader whose actor is parked would join and be pulled
// straight back out.
func (l *Loop) Prime(ctx context.Context) { l.tick(ctx) }

// Run ticks until ctx ends, which is when this process stops leading. Its
// gauges go with it, so a standby never exports a stale view beside the new
// leader's.
func (l *Loop) Run(ctx context.Context) {
	ids := make([]string, 0, len(l.svc.reg.actors))
	for _, a := range l.svc.reg.Actors() {
		ids = append(ids, a.ID)
	}
	metrics.InitPresence(ids)
	defer metrics.ResetPresence()

	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-l.nudge:
		}
		l.tick(ctx)
	}
}

func (l *Loop) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, tickTimeout)
	defer cancel()
	reg, store := l.svc.reg, l.svc.store
	self, _ := reg.Actor(reg.SelfID())
	now := l.svc.now()

	if !store.Enabled() {
		// No database, so no overrides: git's default is the whole answer.
		l.gate.Set(self.Default == presenceapi.StatePresent)
		return
	}
	overrides, err := store.Overrides(ctx)
	if pgerr.Unready(err) {
		// A release ahead of its migration: said once, since every tick
		// until the migration runs would repeat it and tell nobody more.
		l.unready.Do(func() {
			l.log.Info("presence_store_unready", logging.Fields{"error": err.Error()})
		})
		return
	}
	if err != nil {
		// Skipped rather than guessed: the gate keeps its last answer, so a
		// database blink neither pulls the agent out nor puts it back.
		l.log.Warn("presence_tick_skipped", logging.Fields{"error": err.Error()})
		return
	}

	d := Evaluate(reg.Actors(), overrides, l.joins.Recent(), now)
	for _, r := range d.Remove {
		removed, err := l.svc.Expire(ctx, r)
		if err != nil {
			l.log.Error("presence_expire_failed", logging.Fields{"actor": r.ActorID, "cause": string(r.Cause), "error": err.Error()})
			continue
		}
		if removed {
			delete(overrides, r.ActorID)
		}
	}

	l.gate.Set(d.Effective[self.ID] == presenceapi.StatePresent)
	l.reportSelf(ctx, self, d)
	l.export(ctx, overrides, d, now)
	l.kick(ctx, overrides, d, now)
}

// reportSelf writes this process's own status row, which the bots write
// through the API. Observed state is what it is trying to do, connected
// whether it has managed to.
func (l *Loop) reportSelf(ctx context.Context, self Actor, d Decision) {
	st := presenceapi.Status{Connected: l.up(), ObservedState: d.Effective[self.ID], ProcessVersion: l.version}
	if err := l.svc.ReportStatus(ctx, self.ID, st); err != nil {
		l.log.Warn("presence_self_status_failed", logging.Fields{"error": err.Error()})
	}
}

func (l *Loop) export(ctx context.Context, overrides map[string]presenceapi.Override, d Decision, now time.Time) {
	statuses, err := l.svc.store.Statuses(ctx)
	if err != nil {
		// Every actor then reads as not connected, which is what the loop
		// can actually vouch for.
		l.log.Warn("presence_status_read_failed", logging.Fields{"error": err.Error()})
	}
	selfID := l.svc.reg.SelfID()
	for _, a := range l.svc.reg.Actors() {
		metrics.PresenceDesired(a.ID, d.Effective[a.ID] == presenceapi.StatePresent)
		var connected bool
		if a.ID == selfID {
			connected = l.up()
		} else if st, ok := statuses[a.ID]; ok {
			connected = st.Connected && now.Sub(st.LastSeen) <= statusStale
		}
		metrics.PresenceObserved(a.ID, connected)
		ov, ok := overrides[a.ID]
		metrics.PresenceOverrideAge(a.ID, now.Sub(ov.SetAt), ok && ov.Until == nil)
	}
}

// kick removes every actor that has been parked for KickGrace and that both
// the roster and the server's own list still show. The roster alone decides
// whether the console is asked at all; list then confirms, since a kick
// sent on a roster that lags a departure would be refused by the server.
// A roster that cannot answer sends nothing: it is reseeded from the same
// console, so a list now would most likely fail with it.
func (l *Loop) kick(ctx context.Context, overrides map[string]presenceapi.Override, d Decision, now time.Time) {
	var due []Actor
	for _, a := range l.svc.reg.Actors() {
		if d.Effective[a.ID] != presenceapi.StateParked {
			continue
		}
		// Parked by its default with no override: parked since before this
		// process could see it, so the grace has long passed.
		var since time.Time
		if ov, ok := overrides[a.ID]; ok {
			since = ov.SetAt
		}
		if now.Sub(since) >= KickGrace {
			due = append(due, a)
		}
	}
	due = l.onRoster(due)
	if len(due) == 0 {
		return
	}
	online, err := l.console.OnlinePlayers(ctx)
	if err != nil {
		l.log.Warn("presence_list_failed", logging.Fields{"error": err.Error()})
		return
	}
	for _, a := range due {
		if !containsFold(online, a.Gamertag) {
			continue
		}
		if err := l.console.Kick(ctx, a.Gamertag); err != nil {
			l.log.Warn("presence_kick_failed", logging.Fields{"actor": a.ID, "error": err.Error()})
			continue
		}
		metrics.PresenceKick(a.ID)
		l.log.Info("presence_kicked", logging.Fields{"actor": a.ID})
	}
}

// onRoster keeps the actors the roster shows on the server.
func (l *Loop) onRoster(actors []Actor) []Actor {
	if len(actors) == 0 || l.roster == nil || !l.roster.Knows() {
		return nil
	}
	var names []string
	for _, xuid := range l.roster.Online() {
		if n, ok := l.roster.NameFor(xuid); ok {
			names = append(names, n)
		}
	}
	var out []Actor
	for _, a := range actors {
		if containsFold(names, a.Gamertag) {
			out = append(out, a)
		}
	}
	return out
}

func containsFold(names []string, name string) bool {
	folded := text.FoldASCII(name)
	for _, n := range names {
		if text.FoldASCII(n) == folded {
			return true
		}
	}
	return false
}
