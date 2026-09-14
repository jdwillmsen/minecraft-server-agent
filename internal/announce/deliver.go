package announce

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// Voice is what a Deliverer speaks through. Declared here — structurally
// identical to plugin.Voice — rather than imported from the plugin package,
// so this package states its own dependency instead of reaching into a
// package that exists to dispatch commands, not to deliver announcements.
// plugin.Voice's real implementation satisfies this interface for free.
type Voice interface {
	// Tell whispers message to the player identified by xuid.
	Tell(ctx context.Context, xuid, message string) error
	// Say broadcasts message to everyone.
	Say(ctx context.Context, message string) error
}

// Roster is who a Deliverer can currently reach. Presence only: a Deliverer
// never needs a gamertag (Tell and the store both key on XUID), so this
// deliberately doesn't ask for NameFor -- and a name would be no evidence of
// presence anyway, since it outlives the session that taught it.
type Roster interface {
	// Online returns the XUIDs currently connected.
	Online() []string
	// IsOnline reports whether one XUID is still among them, without
	// building the whole list to look.
	IsOnline(xuid string) bool
	// Knows reports whether this roster can answer who is on the server at
	// all: false in the gap between connections, and false again after one
	// opens until its first roster packet arrives. An empty Online() means
	// "nobody" only when this is true.
	Knows() bool
}

// JoinClock tells a Deliverer how long ago a player joined, so a message
// sent in the seconds right after an arrival is not recorded as delivered to
// a client that cannot render it yet. Not part of Roster: Roster answers who
// is reachable, this answers how recently, and only one caller needs it.
type JoinClock interface {
	// SinceJoin is how long ago xuid was seen to arrive, and whether an
	// arrival of their own was seen at all -- a player already online when
	// the agent connected has none.
	SinceJoin(xuid string) (time.Duration, bool)
	// SinceConnect is how long ago the agent's own connection began, and
	// whether it has begun at all. It stands in for the arrival of everyone
	// in the opening roster snapshot, who may have reconnected moments
	// before the agent did and be loading still.
	SinceConnect() (time.Duration, bool)
}

// Leadership answers whether this process is the one currently playing the
// agent, as opposed to a standby waiting for its turn. A Deliverer is built
// once for the process and reached by the announcement API, which is mounted
// for the process too, so a publish can land on a replica that is in no game
// at all -- and a broadcast goes out over that replica's own console bridge,
// which is up regardless. Declared here rather than imported so this package
// states what it needs instead of depending on the HTTP server that happens
// to hold the answer.
type Leadership interface {
	// Live reports whether this process holds the agent lock right now.
	Live() bool
}

// Permissions resolves a player's current permission level, as a plain
// string rather than the plugin package's enum — again so this package
// doesn't have to import plugin just to describe what it needs from it.
type Permissions interface {
	Resolve(ctx context.Context, xuid string) string
}

// Deliverer decides who actually hears an announcement, sends it, and
// records that it was heard. Every method tolerates a disabled store
// (Store.Enabled false) by returning a zero value and no error: a
// deployment with no announcements table configured yet must still be able
// to run join/command handling that calls into a Deliverer unconditionally.
type Deliverer struct {
	store  Store
	voice  Voice
	roster Roster
	perms  Permissions
	log    *logging.Logger
	// xuidLocks serialises DrainForJoin and DrainAll per xuid. PendingFor
	// and MarkDelivered are two separate store calls, not one transaction,
	// so a rapid leave-and-rejoin (or a join landing alongside that same
	// player's own !inbox) can run two drains that both read the same
	// pending rows before either records a delivery -- each then sends
	// every one of them, and MarkDelivered's ON CONFLICT DO NOTHING makes
	// the bookkeeping right afterward but does nothing to un-send a message
	// the player already heard twice. It lives here rather than in a
	// caller because DrainForJoin (a join) and DrainAll (!inbox) are two
	// different plugins reaching this one store for the same player -- a
	// guard placed in either plugin cannot see the other's call.
	xuidLocks sync.Map // xuid string -> *sync.Mutex
	// joins and joinGrace defer delivery to a player who has only just
	// arrived. Both zero unless WithFreshJoinGrace is passed, which keeps
	// every existing caller and test on the old behaviour.
	joins     JoinClock
	joinGrace time.Duration
	// leader gates a broadcast that no roster backs. Nil unless
	// WithLeadership is passed, which reads as live: a deployment with no
	// lock to wait for has always been the live agent.
	leader Leadership
}

// Option configures a Deliverer at construction.
type Option func(*Deliverer)

// WithFreshJoinGrace makes SendNow leave a just-joined player's copy pending
// instead of recording it as delivered.
//
// A whisper or broadcast that lands within grace of an arrival reaches a
// client that is still loading: the server accepts it, the player never sees
// it, and a delivery row would stop anything from ever retrying it. Leaving
// the row pending hands the message to that player's own join drain, which
// defers on the same grace when it wakes -- so grace must be shorter than
// the drain's wait, or a drain would never deliver anything.
func WithFreshJoinGrace(j JoinClock, grace time.Duration) Option {
	return func(d *Deliverer) { d.joins, d.joinGrace = j, grace }
}

// WithLeadership stops a standby from broadcasting into a server it is not
// playing on.
//
// Only a broadcast with nobody on the roster is affected, because that is
// the one case the roster cannot distinguish: a live agent between
// connections has an empty roster for the length of its backoff and must
// still be heard, while a standby has an empty roster for its whole life and
// must not be. Leadership is what tells them apart.
func WithLeadership(l Leadership) Option {
	return func(d *Deliverer) { d.leader = l }
}

// NewDeliverer builds a Deliverer over the given Store, Voice, Roster and
// Permissions.
func NewDeliverer(s Store, v Voice, r Roster, p Permissions, log *logging.Logger, opts ...Option) *Deliverer {
	d := &Deliverer{store: s, voice: v, roster: r, perms: p, log: log}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// live reports whether this process may speak into the game at all. True
// when no Leadership was wired: an agent running without a database has no
// lock to wait for and is the live agent by default.
func (d *Deliverer) live() bool {
	return d.leader == nil || d.leader.Live()
}

// mayBroadcastBlind reports whether an empty roster means "who is here is not
// known" rather than "nobody is here", and this process is the one entitled
// to act on that.
//
// Not known covers two states, and the roster tells both from the third: the
// gap between connections, and the moments after one opens before its first
// roster packet. A server the agent is watching with nobody on it is the
// third, where the roster is right and a broadcast would be a console line no
// player could hear. A standby fails the other half -- its roster never knows
// anything, and the server it would speak into belongs to whoever holds the
// lock.
func (d *Deliverer) mayBroadcastBlind() bool {
	return d.live() && !d.roster.Knows()
}

// departed reports whether xuid has left since the roster named them.
//
// Deliberately asked of the roster's online list rather than of whether a
// gamertag resolves: a name outlives the session that taught it, because a
// reply already in flight still has to be addressable, so name resolution is
// no evidence at all that the player is still there. A whisper recorded
// against someone who has gone is the permanent loss this package exists to
// avoid -- nothing retries a delivery that has a row.
func (d *Deliverer) departed(xuid string) bool {
	return !d.roster.IsOnline(xuid)
}

// stillLoading reports whether xuid's client may be too freshly loaded to
// see a message sent right now: their own arrival if one was seen, and
// otherwise the agent's connection, since the opening snapshot cannot tell
// an hour-old builder from a player who reconnected a second earlier.
func (d *Deliverer) stillLoading(xuid string) bool {
	if d.joins == nil || d.joinGrace <= 0 {
		return false
	}
	if since, ok := d.joins.SinceJoin(xuid); ok {
		return since < d.joinGrace
	}
	since, ok := d.joins.SinceConnect()
	return ok && since < d.joinGrace
}

// justArrived reports whether xuid made an arrival of their own too recently
// to be served. Only an arrival counts: the connection standing in for one
// is a guess about who might be loading, fine for withholding a delivery row
// but not for cancelling a drain that a real join scheduled.
func (d *Deliverer) justArrived(xuid string) bool {
	if d.joins == nil || d.joinGrace <= 0 {
		return false
	}
	since, ok := d.joins.SinceJoin(xuid)
	return ok && since < d.joinGrace
}

// recipients resolves which currently-online XUIDs a should reach. everyone
// and online_only both mean every online player; player means that one
// XUID if they happen to be online right now (if not, they stay pending
// for their eventual join — that's what Queues is for); permission means
// every online XUID whose resolved level matches target_value. An
// unrecognized target kind resolves to nobody: same fail-toward-silence
// stance DeliveryFor takes, applied to who hears it rather than how.
func (d *Deliverer) recipients(ctx context.Context, a Announcement) []string {
	online := d.roster.Online()
	switch a.TargetKind {
	case TargetEveryone, TargetOnlineOnly:
		return online
	case TargetPlayer:
		for _, xuid := range online {
			if xuid == a.TargetValue {
				return []string{xuid}
			}
		}
		return nil
	case TargetPermission:
		var out []string
		for _, xuid := range online {
			if d.perms.Resolve(ctx, xuid) == a.TargetValue {
				out = append(out, xuid)
			}
		}
		return out
	default:
		return nil
	}
}

// Reach is what one immediate send actually achieved.
//
// Players alone could not say it: a broadcast that goes out while the roster
// cannot answer who is here reaches whoever is on the server, and this
// process has no way to count them. Reporting that as zero would read as
// "nobody heard it" -- the same value a suppressed send returns -- and a
// caller retrying on zero would broadcast into the server twice.
type Reach struct {
	// Players is how many are known to have received it. Meaningful only
	// when Counted is true.
	Players int
	// Counted is whether Players is an answer at all. False only for a
	// broadcast said to an audience the roster could not name.
	Counted bool
}

// reached is a counted answer: this many players, and the count is real.
func reached(players int) Reach { return Reach{Players: players, Counted: true} }

// uncounted is a broadcast that went out to an audience this process could
// not see.
var uncounted = Reach{}

// SendNow delivers a immediately to whoever is online and matches its
// target, and reports what that reached.
func (d *Deliverer) SendNow(ctx context.Context, a Announcement, id int64) (Reach, error) {
	if !d.store.Enabled() {
		return reached(0), nil
	}
	targets := d.recipients(ctx, a)
	now := time.Now()

	// Delivery is derived from the target, never trusted off the row: this
	// is exactly the guard announce.go's DeliveryFor exists for. Nothing
	// writes a.Delivery today, but the moment something does — a future
	// source, or a row PendingFor reads back after a bad write — a
	// TargetPlayer row that happened to carry DeliveryBroadcast must still
	// whisper, not broadcast a private message to the whole server.
	if DeliveryFor(a.TargetKind) == DeliveryBroadcast {
		// Said even when the roster names nobody, which is what the live
		// agent's disconnect gap looks like from here. The console bridge is
		// a separate process that stays up, so the server can still speak to
		// whoever is on it; what the gap makes unknowable is who that was.
		// Nothing is recorded, because there is no roster snapshot to
		// record from -- a target that queues stays pending and may be
		// whispered on a later join, and online-only never queues, so this
		// is its only chance to be heard at all.
		//
		// Only a roster that cannot answer earns that -- see
		// mayBroadcastBlind. A watching roster with nobody on it is right,
		// and a standby's roster never knows anything at all.
		if len(targets) == 0 && !d.mayBroadcastBlind() {
			d.log.Info("announce_say_skipped_no_audience", logging.Fields{"announcement_id": id, "roster_knows": d.roster.Knows(), "live": d.live()})
			return reached(0), nil
		}
		blind := len(targets) == 0
		err := d.voice.Say(ctx, a.Body)
		metrics.AnnounceDelivery(metrics.DeliveryBroadcast, err)
		if err != nil {
			// Say never went out, so nothing was heard — recording a
			// delivery here would make an online player's next join
			// silently skip a message they never actually received.
			d.log.Error("announce_say_failed", logging.Fields{"announcement_id": id, "error": err.Error()})
			return reached(0), nil
		}
		// Say is one console command with no per-recipient receipt, so
		// "who heard this" has to come from the roster snapshot taken at
		// send time, one row per player present — without those rows,
		// anyone online right now sees the same announcement again on
		// their next join.
		delivered := 0
		deferred := 0
		heard := 0
		for _, xuid := range targets {
			if ctx.Err() != nil {
				// Cancelled: stop rather than attempt (and log) a store
				// write for every remaining recipient that would fail anyway.
				break
			}
			if d.departed(xuid) {
				// Left during the Say, which is one bridge round-trip long.
				// They are gone, so a row for them would suppress the
				// redelivery their next join would otherwise pay -- the same
				// permanent loss the whisper loop refuses below.
				continue
			}
			if d.stillLoading(xuid) {
				// Heard by everyone whose client is up, but not by this one:
				// recording it would be the same permanent loss a whisper to
				// a loading client used to be. Left pending, so their own
				// drain owes it to them -- unless the target is one that
				// never queues, in which case they have simply missed it.
				deferred++
				if !d.justArrived(xuid) {
					// Withheld on a guess, not on an arrival: nothing says
					// this player is loading beyond the agent having only
					// just connected, and one Say reaches every client that
					// is up. They heard it; only their row was skipped.
					heard++
				}
				continue
			}
			if err := d.store.MarkDelivered(ctx, id, xuid, now); err != nil {
				d.log.Error("announce_mark_delivered_failed", logging.Fields{"announcement_id": id, "xuid": xuid, "error": err.Error()})
				continue
			}
			delivered++
		}
		if deferred > 0 {
			d.log.Info("announce_deferred_for_joining", logging.Fields{"announcement_id": id, "players": deferred})
		}
		if blind || !d.roster.Knows() {
			// Said, with no roster to count from: either there was none when
			// the recipients were chosen, or the connection died during the
			// Say, which is one bridge round-trip long. Whoever was on the
			// server heard it either way; what became impossible is naming
			// them, and a counted zero would say the opposite -- sending a
			// caller who retries on it to broadcast the same line twice.
			return uncounted, nil
		}
		// Counted as reached: everyone recorded, plus everyone withheld on
		// nothing worse than a guess. A player who demonstrably just
		// arrived is not counted -- their client rendered nothing, and
		// their own drain still owes them the same text.
		return reached(delivered + heard), nil
	}

	if len(targets) == 0 {
		// Nobody to whisper to and nothing to record; the announcement
		// stays pending in the store (if it queues at all) for whoever
		// joins later. Unlike a broadcast, a whisper needs an XUID to go
		// to, so there is nothing to send into the gap.
		return reached(0), nil
	}

	// Whisper: each recipient gets their own Tell, and only a recipient
	// whose Tell actually succeeded is marked delivered. A failed send is
	// logged and left pending — the next join retries it — rather than
	// recorded as delivered, which would lose the message for good since
	// nothing else retries it.
	delivered := 0
	deferred := 0
	for _, xuid := range targets {
		if ctx.Err() != nil {
			// Cancelled: stop rather than run up a failed bridge attempt
			// (and an error line) for every recipient still left to try.
			break
		}
		if d.departed(xuid) {
			// Left between the roster naming them and their turn in this
			// loop. Nothing is sent and nothing recorded, so what they are
			// owed survives for their next join.
			continue
		}
		if d.stillLoading(xuid) {
			// Their client is not rendering chat yet, so this Tell would be
			// accepted by the server and seen by nobody. Not sent and not
			// recorded: their own join drain owes it to them.
			deferred++
			continue
		}
		err := d.voice.Tell(ctx, xuid, a.Body)
		metrics.AnnounceDelivery(metrics.DeliveryWhisper, err)
		if err != nil {
			d.log.Error("announce_tell_failed", logging.Fields{"announcement_id": id, "xuid": xuid, "error": err.Error()})
			continue
		}
		if err := d.store.MarkDelivered(ctx, id, xuid, now); err != nil {
			d.log.Error("announce_mark_delivered_failed", logging.Fields{"announcement_id": id, "xuid": xuid, "error": err.Error()})
			continue
		}
		delivered++
	}
	if deferred > 0 {
		d.log.Info("announce_deferred_for_joining", logging.Fields{"announcement_id": id, "players": deferred})
	}
	return reached(delivered), nil
}

// ErrDisabled is what Publish returns when there is no store to write an
// announcement to. A sentinel rather than a silent zero because the callers
// differ in what it means to them: an event source shrugs, while the HTTP
// API must tell its caller that nothing was stored.
var ErrDisabled = errors.New("announce: no announcement store configured")

// Publish stores a and then sends it to whoever is online and matches it,
// reporting the stored id and what the immediate send reached.
//
// The one path for every source that has no command reply to shape: a
// schedule, a server event, the HTTP API. Delivery is re-derived here from
// the target whatever the caller set, for the same reason SendNow never
// trusts it off a row. A send that reaches nobody is not an error -- the
// row is stored and the queue owns it from here.
func (d *Deliverer) Publish(ctx context.Context, a Announcement) (id int64, sent Reach, err error) {
	if !d.store.Enabled() {
		return 0, reached(0), ErrDisabled
	}
	a.Delivery = DeliveryFor(a.TargetKind)
	id, err = d.store.Insert(ctx, a)
	if err != nil {
		return 0, reached(0), err
	}
	a.ID = id
	sent, err = d.SendNow(ctx, a, id)
	return id, sent, err
}

// sendPending whispers each of msgs to xuid in order, marking every
// successful send delivered. A send or a mark that fails is logged and
// simply not counted: the announcement is left pending in the store, so it
// is retried on this player's next join or !inbox rather than lost.
//
// A player who is no longer online is not whispered to at all. This drain
// was scheduled seconds ago by their arrival, and a player who quits inside
// that wait would otherwise be sent their whole backlog and have every
// message of it recorded -- the console accepts a tellraw that matches
// nobody, so the send reports success and the backlog is gone for good.
func (d *Deliverer) sendPending(ctx context.Context, xuid string, now time.Time, msgs []Announcement) int {
	delivered := 0
	for _, a := range msgs {
		if ctx.Err() != nil {
			// Cancelled: stop here rather than turn the rest of a backlog
			// into that many more failed bridge attempts and error lines.
			break
		}
		if d.departed(xuid) {
			d.log.Info("announce_drain_stopped_player_left", logging.Fields{"announcement_id": a.ID, "xuid": xuid})
			break
		}
		err := d.voice.Tell(ctx, xuid, a.Body)
		metrics.AnnounceDelivery(metrics.DeliveryWhisper, err)
		if err != nil {
			d.log.Error("announce_tell_failed", logging.Fields{"announcement_id": a.ID, "xuid": xuid, "error": err.Error()})
			continue
		}
		if err := d.store.MarkDelivered(ctx, a.ID, xuid, now); err != nil {
			d.log.Error("announce_mark_delivered_failed", logging.Fields{"announcement_id": a.ID, "xuid": xuid, "error": err.Error()})
			continue
		}
		delivered++
	}
	return delivered
}

// lockXUID returns the mutex guarding xuid's own drains, creating it on
// first use. Never removed from xuidLocks: the set of distinct players who
// ever drain is bounded and small next to a process lifetime, and deleting
// entries would only reopen the race against whichever call is mid-Lock.
func (d *Deliverer) lockXUID(xuid string) *sync.Mutex {
	m, _ := d.xuidLocks.LoadOrStore(xuid, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// splitByPriority separates pending into expedited and normal, preserving
// PendingFor's ordering within each (expedited-first, then oldest-first)
// since that ordering is what "oldest first" and "uncapped" both rely on.
func splitByPriority(pending []Announcement) (expedited, normal []Announcement) {
	for _, a := range pending {
		if a.Priority == PriorityExpedited {
			expedited = append(expedited, a)
		} else {
			normal = append(normal, a)
		}
	}
	return expedited, normal
}

// DrainForJoin sends xuid everything they're owed on login: every expedited
// message, uncapped, then up to MaxNormalPerJoin normal ones oldest-first.
// remaining reports how many of everything pending — expedited or normal —
// is still owed after this call: total pending minus however many were
// actually delivered. That equals the unsent-normal-message count in the
// ordinary case, but it also has to hold when a send is attempted and
// fails: a message that failed to send is not delivered, so it must still
// count as remaining rather than silently drop out of the total just
// because this call already had a turn at it. The caller uses this to
// decide whether to point the player at !inbox instead of dumping the
// whole backlog into their first moments in the world.
func (d *Deliverer) DrainForJoin(ctx context.Context, xuid string, now time.Time) (delivered, remaining int, err error) {
	// Acquired before anything else, released on every return (including a
	// panic unwinding through here): see xuidLocks' doc comment for why a
	// per-xuid guard belongs at this layer rather than in a caller.
	mu := d.lockXUID(xuid)
	mu.Lock()
	defer mu.Unlock()

	if !d.store.Enabled() {
		return 0, 0, nil
	}
	if d.justArrived(xuid) {
		// This drain belongs to an arrival the player has already replaced:
		// they dropped and rejoined inside its wait. Whispering the backlog
		// now would hand it to a loading client and record it, which is the
		// permanent loss the wait exists to prevent. Nothing delivered and
		// nothing owed to report, so no summary line is spoken either --
		// the newer arrival's own drain, a full wait behind it, owes them
		// everything.
		return 0, 0, nil
	}
	permission := d.perms.Resolve(ctx, xuid)
	pending, err := d.store.PendingFor(ctx, xuid, permission, now)
	if err != nil {
		return 0, 0, err
	}
	expedited, normal := splitByPriority(pending)

	// Expedited bypasses the cap entirely: it exists precisely so something
	// urgent (a restart countdown, say) is never held back by the same
	// throttle that keeps a login from being buried under routine text.
	limit := len(normal)
	if limit > MaxNormalPerJoin {
		limit = MaxNormalPerJoin
	}
	toSend := make([]Announcement, 0, len(expedited)+limit)
	toSend = append(toSend, expedited...)
	toSend = append(toSend, normal[:limit]...)

	delivered = d.sendPending(ctx, xuid, now, toSend)
	remaining = len(pending) - delivered
	return delivered, remaining, nil
}

// DrainAll sends xuid up to MaxPerInbox pending messages, oldest-priority
// order — the !inbox command's job, for a player who has explicitly asked
// for the rest rather than having it trickle in across future joins.
// remaining reports what is still owed afterwards, counted the same way
// DrainForJoin counts it: total pending minus what actually went out, so a
// send that failed still shows as owed rather than vanishing because this
// call had a turn at it. A caller with remaining > 0 tells the player to
// ask again.
func (d *Deliverer) DrainAll(ctx context.Context, xuid string, now time.Time) (delivered, remaining int, err error) {
	// Same guard as DrainForJoin, over the same per-xuid lock: this is the
	// other half of the race xuidLocks exists for -- a player's !inbox
	// landing while their own join drain is still in flight.
	mu := d.lockXUID(xuid)
	mu.Lock()
	defer mu.Unlock()

	if !d.store.Enabled() {
		return 0, 0, nil
	}
	permission := d.perms.Resolve(ctx, xuid)
	pending, err := d.store.PendingFor(ctx, xuid, permission, now)
	if err != nil {
		return 0, 0, err
	}
	limit := len(pending)
	if limit > MaxPerInbox {
		limit = MaxPerInbox
	}
	delivered = d.sendPending(ctx, xuid, now, pending[:limit])
	return delivered, len(pending) - delivered, nil
}
