package main

import (
	"context"
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
	poll    time.Duration
	reseed  time.Duration
	log     *logging.Logger
	now     func() time.Time
	wait    func(ctx context.Context, d time.Duration) bool
}

func newBridgeRoster(feed bridgeFeed, r *roster.Roster, joins *joinTimes, archive nameArchive, log *logging.Logger) *bridgeRoster {
	return &bridgeRoster{feed: feed, roster: r, joins: joins, archive: archive, poll: bridgeRosterPoll, reseed: bridgeRosterReseed, log: log,
		now: time.Now, wait: waitOrShutdown}
}

// run follows the server until ctx ends. It never returns early, because the
// caller treats its return as the end of the agent's absence.
func (b *bridgeRoster) run(ctx context.Context) {
	// Once nothing is following the bridge, what this learned stops being
	// true, just as a dead connection's roster does.
	defer connectionEnded(b.roster, b.joins)
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
// newest event ID it has already taken into account.
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
func (b *bridgeRoster) seed(ctx context.Context, refresh bool) (int64, error) {
	backlog, err := b.feed.Events(ctx, 0)
	if err != nil {
		return 0, err
	}
	var cursor int64
	logged := make(map[string]string)
	for _, e := range backlog {
		cursor = e.ID
		if e.Type == adapters.BridgeEventConnect {
			if xuid := e.XUID(); xuid != "" {
				logged[e.Player] = xuid
			}
		}
	}

	names, err := b.feed.OnlinePlayers(ctx)
	if err != nil {
		return 0, err
	}
	players := make([]roster.Entry, 0, len(names))
	for _, name := range names {
		xuid, ok := b.resolve(ctx, name, logged)
		if !ok {
			// Left out rather than guessed. The roster then knows the server
			// and not this player, which the deliverer reads as departed, so
			// their copy of an announcement stays pending and is not recorded.
			b.log.Info("bridge_roster_unresolved", logging.Fields{"gamertag": name})
			continue
		}
		players = append(players, roster.Entry{XUID: xuid, Username: name})
	}
	if refresh {
		before := b.roster.Online()
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
// did not find has left.
func (b *bridgeRoster) refreshed(before []string, after []roster.Entry) {
	gone := make(map[string]struct{}, len(before))
	for _, xuid := range before {
		gone[xuid] = struct{}{}
	}
	for _, p := range after {
		if _, ok := gone[p.XUID]; ok {
			delete(gone, p.XUID)
			continue
		}
		b.joins.joined(p.XUID)
		b.log.Info("bridge_player_joined", logging.Fields{"xuid": p.XUID, "username": p.Username})
	}
	for xuid := range gone {
		b.joins.left(xuid)
		b.log.Info("bridge_player_left", logging.Fields{"xuid": xuid})
	}
}

// seedFailed drops what an earlier seed reported. A roster that cannot be
// refreshed must say it does not know, not repeat a population that may have
// left. Losing a roster that was known is a warning; the retries after it
// are Debug for the same reason as tps_sample_failed, since an outage would
// otherwise add a line every retry.
func (b *bridgeRoster) seedFailed(err error, wasKnown bool) {
	connectionEnded(b.roster, b.joins)
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
// read back, and only a fresh seed can recover the population.
func (b *bridgeRoster) follow(ctx context.Context, cursor int64) (reseedDue bool) {
	reseedAt := b.now().Add(b.reseed)
	for b.wait(ctx, b.poll) {
		if b.now().After(reseedAt) {
			return true
		}
		events, err := b.feed.Events(ctx, max(cursor-1, 0))
		if err != nil {
			// Kept, not cleared: the bridge holds its log across its own
			// outage, and the next poll picks up where this one stopped.
			b.log.Debug("bridge_roster_poll_failed", logging.Fields{"error": err.Error()})
			continue
		}
		if cursor > 0 && (len(events) == 0 || events[0].ID != cursor) {
			b.log.Info("bridge_roster_reset", logging.Fields{"cursor": cursor, "reason": "bridge restart or eviction"})
			connectionEnded(b.roster, b.joins)
			return false
		}
		for _, e := range events {
			if e.ID <= cursor {
				continue
			}
			b.apply(ctx, e)
			cursor = e.ID
		}
	}
	return false
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
	xuid := e.XUID()
	if xuid == "" {
		var ok bool
		if xuid, ok = b.resolve(ctx, e.Player, nil); !ok {
			b.log.Info("bridge_roster_unresolved", logging.Fields{"gamertag": e.Player})
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

// resolve turns a gamertag into the XUID everything else keys on: first from
// the bridge's own connect lines, then from the profile store.
func (b *bridgeRoster) resolve(ctx context.Context, name string, logged map[string]string) (string, bool) {
	if xuid, ok := logged[name]; ok {
		return xuid, true
	}
	xuid, ok, err := b.archive.XUIDForName(ctx, name)
	if err != nil {
		b.log.Debug("bridge_roster_lookup_failed", logging.Fields{"gamertag": name, "error": err.Error()})
		return "", false
	}
	return xuid, ok
}
