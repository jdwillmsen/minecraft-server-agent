package main

import (
	"context"
	"maps"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// bridgeRosterPoll spaces reads of the bridge's event log. The log is held in
// the bridge's memory and costs no console command, so the poll can be short
// enough that a join is on the roster before anything is sent to that player.
const bridgeRosterPoll = 2 * time.Second

// bridgeRosterReseed is how often the population is read again from `list`.
// The event log only reports changes. A player whose XUID could not be
// resolved at the last seed, or a line the bridge never parsed, stays wrong
// until the next seed corrects it.
const bridgeRosterReseed = 5 * time.Minute

// bridgeFeed is the console bridge as the roster follower reads it.
type bridgeFeed interface {
	Events(ctx context.Context, since int64) ([]adapters.BridgeEvent, error)
	OnlinePlayers(ctx context.Context) ([]string, error)
}

var _ bridgeFeed = (*adapters.BridgeClient)(nil)

// bridgeRoster keeps the live roster answering while the agent is
// deliberately out of the world. It writes the roster and the join clock that
// a session would otherwise feed, so the announcement deliverer reads the
// same two objects either way.
//
// It publishes no bus events and writes nothing to the profile store.
// Greetings, join drains and playtime all belong to a session that is
// watching, and running them off console lines would change what players get
// while the agent is away.
type bridgeRoster struct {
	feed    bridgeFeed
	roster  *roster.Roster
	joins   *joinTimes
	archive nameArchive
	// onJoin, when set, hears every connect line with the line's own time.
	// It is how a player arriving wakes a parked agent while the follower,
	// not a session, is watching.
	onJoin func(gamertag string, at time.Time)
	poll   time.Duration
	reseed time.Duration
	log    *logging.Logger
	now    func() time.Time
	wait   func(ctx context.Context, d time.Duration) bool

	// held is who the roster had online, by name, when the follower last
	// cleared it, so the seed that recovers can still name them.
	held map[string]string
	// unresolved is every gamertag already reported as unresolvable.
	unresolved map[string]struct{}
}

func newBridgeRoster(feed bridgeFeed, r *roster.Roster, joins *joinTimes, archive nameArchive, log *logging.Logger) *bridgeRoster {
	return &bridgeRoster{feed: feed, roster: r, joins: joins, archive: archive, poll: bridgeRosterPoll, reseed: bridgeRosterReseed, log: log,
		now: time.Now, wait: waitOrShutdown, unresolved: make(map[string]struct{})}
}

// run follows the server until ctx ends. It never returns early, because the
// caller treats its return as the end of the agent's absence.
func (b *bridgeRoster) run(ctx context.Context) {
	// Once nothing is following the bridge, what this learned stops being
	// true, just as a dead connection's roster does.
	defer connectionEnded(b.roster, b.joins)
	b.held, b.unresolved = nil, make(map[string]struct{})
	refresh, known, failures := false, false, 0
	for {
		wait := b.poll
		cursor, err := b.seed(ctx, refresh)
		if err != nil {
			b.seedFailed(err, known)
			refresh, known = false, false
			failures++
			wait = b.retryAfter(failures)
		} else {
			known, failures = true, 0
			refresh = b.follow(ctx, cursor)
		}
		if !b.wait(ctx, wait) {
			return
		}
	}
}

// retryAfter spaces seeds that keep failing. Each one sends `list` to the
// server's console, so a bridge that stays down would otherwise be asked
// every poll for as long as it is gone. It is capped at the reseed
// interval, which is already how stale a roster that is working may be.
func (b *bridgeRoster) retryAfter(failures int) time.Duration {
	d := b.poll
	for range failures - 1 {
		if d >= b.reseed {
			break
		}
		d *= 2
	}
	return min(d, b.reseed)
}

// seed replaces the roster with who `list` says is online and reports the
// newest event it has already taken into account, or the zero event when the
// backlog was empty.
//
// A refresh corrects a server the follower was already following, so it
// keeps the join clock's connection and every arrival it holds, and reports
// only the players who appeared or vanished since. Starting a new connection
// there would re-arm the fresh-join grace for everyone online on every
// reseed, and end deliveries that nothing had interrupted.
//
// The backlog is read before `list`, not after. Every event is applied again
// from that cursor onward, and applying a join or a leave the list already
// reflects changes nothing. Reading in the other order would lose any change
// that landed between the two reads.
func (b *bridgeRoster) seed(ctx context.Context, refresh bool) (adapters.BridgeEvent, error) {
	var cursor adapters.BridgeEvent
	backlog, err := b.feed.Events(ctx, 0)
	if err != nil {
		return cursor, err
	}
	logged := make(map[string]string)
	for _, e := range backlog {
		cursor = e
		if e.Type == adapters.BridgeEventConnect {
			// Arrivals the follower never applies: whoever came while the
			// session was unwinding into this absence, or while the bridge
			// was unreachable. Replaying older ones is harmless, since each
			// keeps its own time.
			b.reportJoin(e)
			if xuid := e.XUID(); xuid != "" {
				logged[e.Player] = xuid
			}
		}
	}

	names, err := b.feed.OnlinePlayers(ctx)
	if err != nil {
		return cursor, err
	}
	players := make([]roster.Entry, 0, len(names))
	for _, name := range names {
		xuid, ok := b.resolve(ctx, name, logged)
		if !ok {
			// Left out rather than guessed. The roster then knows the server
			// and not this player, which the deliverer reads as departed, so
			// their copy of an announcement stays pending and is not recorded.
			b.unresolvedPlayer(name)
			continue
		}
		players = append(players, roster.Entry{XUID: xuid, Username: name})
	}
	b.held = nil
	if refresh {
		before := make(map[string]string)
		for _, xuid := range b.roster.Online() {
			before[xuid], _ = b.roster.NameFor(xuid)
		}
		b.roster.Seed(players)
		b.refreshed(before, players)
	} else {
		b.roster.Seed(players)
		b.joins.connected()
	}
	b.log.Info("bridge_roster_seeded", logging.Fields{"players": len(players), "unresolved": len(names) - len(players), "refresh": refresh})
	return cursor, nil
}

// refreshed tells the join clock what a refresh changed: a player it found
// who was not on the roster has only just been seen to arrive, and one it
// did not find has left. before maps who was online to their username.
func (b *bridgeRoster) refreshed(before map[string]string, after []roster.Entry) {
	gone := maps.Clone(before)
	for _, p := range after {
		if _, ok := gone[p.XUID]; ok {
			delete(gone, p.XUID)
			continue
		}
		b.joins.joined(p.XUID)
		b.log.Info("bridge_player_joined", logging.Fields{"xuid": p.XUID, "username": p.Username})
	}
	for xuid, name := range gone {
		b.joins.left(xuid)
		b.log.Info("bridge_player_left", logging.Fields{"xuid": xuid, "username": name})
	}
}

// unresolvedPlayer reports a gamertag left off the roster. Every seed misses
// it again, so only the first miss is Info.
func (b *bridgeRoster) unresolvedPlayer(name string) {
	fields := logging.Fields{"gamertag": name}
	if _, seen := b.unresolved[name]; seen {
		b.log.Debug("bridge_roster_unresolved", fields)
		return
	}
	b.unresolved[name] = struct{}{}
	b.log.Info("bridge_roster_unresolved", fields)
}

// seedFailed drops what an earlier seed reported. A roster that cannot be
// refreshed must say it does not know, not repeat a population that may have
// left. Losing a roster that was known is a warning; the retries after it
// are Debug for the same reason as tps_sample_failed, since an outage would
// otherwise add a line every retry.
func (b *bridgeRoster) seedFailed(err error, wasKnown bool) {
	b.forget()
	fields := logging.Fields{"error": err.Error()}
	if wasKnown {
		b.log.Warn("bridge_roster_seed_failed", fields)
		return
	}
	b.log.Debug("bridge_roster_seed_failed", fields)
}

// follow applies the bridge's events after cursor until ctx ends, the log
// the cursor points into is gone, or it is time to seed again. It reports
// whether it stopped only because a reseed is due, in which case the roster
// still holds the server and the next seed is a refresh.
//
// The bridge's IDs restart with its process and it has no epoch to compare,
// and it drops its oldest events once its buffer is full. Either way the
// event at the cursor is missing, so each poll asks for it again: if it is
// not the first event returned, what happened since the cursor cannot be
// read back, and only a fresh seed can recover the population. The ID alone
// does not identify it, because a restarted bridge that has logged as many
// lines hands the same ID to a different one, so its time and line must
// match too.
//
// With no cursor, because the backlog was empty at the seed, a restart
// leaves nothing to compare. Every event the new process logs is still
// applied, and what happened while it was down waits for the periodic
// reseed.
func (b *bridgeRoster) follow(ctx context.Context, cursor adapters.BridgeEvent) (reseedDue bool) {
	reseedAt := b.now().Add(b.reseed)
	for b.wait(ctx, b.poll) {
		if b.now().After(reseedAt) {
			return true
		}
		events, err := b.feed.Events(ctx, max(cursor.ID-1, 0))
		if err != nil {
			// Kept, not cleared: the bridge holds its log across its own
			// outage, and the next poll picks up where this one stopped.
			b.log.Debug("bridge_roster_poll_failed", logging.Fields{"error": err.Error()})
			continue
		}
		if cursor.ID > 0 && (len(events) == 0 || !sameEvent(events[0], cursor)) {
			b.log.Info("bridge_roster_reset", logging.Fields{"cursor": cursor.ID, "reason": "bridge restart or eviction"})
			b.forget()
			return false
		}
		for _, e := range events {
			if e.ID <= cursor.ID {
				continue
			}
			b.apply(ctx, e)
			cursor = e
		}
	}
	return false
}

func sameEvent(a, b adapters.BridgeEvent) bool {
	return a.ID == b.ID && a.Time.Equal(b.Time) && a.Raw == b.Raw
}

// apply puts one connect or disconnect on the roster and the join clock, as
// handlePlayerList does for a session's packets.
func (b *bridgeRoster) apply(ctx context.Context, e adapters.BridgeEvent) {
	var remove bool
	switch e.Type {
	case adapters.BridgeEventConnect:
	case adapters.BridgeEventDisconnect:
		remove = true
	default:
		return
	}
	// Before resolving an XUID: a first-time player nobody has recorded yet
	// is exactly who should bring a parked agent back.
	if !remove {
		b.reportJoin(e)
	}
	xuid := e.XUID()
	if xuid == "" {
		var ok bool
		if xuid, ok = b.resolve(ctx, e.Player, nil); !ok {
			b.unresolvedPlayer(e.Player)
			return
		}
	}
	joins, leaves, _ := b.roster.Apply([]roster.PlayerListEntry{{XUID: xuid, Username: e.Player, Remove: remove}})
	for _, j := range joins {
		b.joins.joined(j.XUID)
		b.log.Info("bridge_player_joined", logging.Fields{"xuid": j.XUID, "username": j.Username})
	}
	for _, l := range leaves {
		b.joins.left(l.XUID)
		b.log.Info("bridge_player_left", logging.Fields{"xuid": l.XUID, "username": l.Username})
	}
}

func (b *bridgeRoster) reportJoin(e adapters.BridgeEvent) {
	if b.onJoin == nil {
		return
	}
	at := e.Time
	if at.IsZero() {
		at = b.now()
	}
	b.onJoin(e.Player, at)
}

// forget ends what the roster knows, keeping who it held.
func (b *bridgeRoster) forget() {
	held := make(map[string]string)
	for _, xuid := range b.roster.Online() {
		if name, ok := b.roster.NameFor(xuid); ok {
			held[name] = xuid
		}
	}
	if len(held) > 0 {
		b.held = held
	}
	connectionEnded(b.roster, b.joins)
}

// resolve turns a gamertag into the XUID everything else keys on: first from
// the bridge's own connect lines, then from who the roster holds or held,
// then from the profile store. The roster matters for anyone online longer
// than the bridge's buffer reaches back, whose connect line is gone and who
// may never have been written to the store.
func (b *bridgeRoster) resolve(ctx context.Context, name string, logged map[string]string) (string, bool) {
	if xuid, ok := logged[name]; ok {
		return xuid, true
	}
	if xuid, ok := b.roster.XUIDFor(name); ok {
		return xuid, true
	}
	if xuid, ok := b.held[name]; ok {
		return xuid, true
	}
	xuid, ok, err := b.archive.XUIDForName(ctx, name)
	if err != nil {
		b.log.Debug("bridge_roster_lookup_failed", logging.Fields{"gamertag": name, "error": err.Error()})
		return "", false
	}
	return xuid, ok
}
