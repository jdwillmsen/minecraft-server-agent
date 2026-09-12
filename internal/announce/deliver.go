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

// Roster is who a Deliverer can currently reach. Online-only: a Deliverer
// never needs a gamertag (Tell and the store both key on XUID), so this
// deliberately doesn't ask for NameFor.
type Roster interface {
	// Online returns the XUIDs currently connected.
	Online() []string
}

// JoinClock tells a Deliverer how long ago a player joined, so a message
// sent in the seconds right after an arrival is not recorded as delivered to
// a client that cannot render it yet. Not part of Roster: Roster answers who
// is reachable, this answers how recently, and only one caller needs it.
type JoinClock interface {
	// SinceJoin is how long ago xuid joined, and whether that is known at
	// all -- a player already online when the agent connected is not a
	// fresh arrival and reports false.
	SinceJoin(xuid string) (time.Duration, bool)
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
// waits out the same window. grace should therefore be shorter than the
// drain's wait, or the drain would defer its own delivery.
func WithFreshJoinGrace(j JoinClock, grace time.Duration) Option {
	return func(d *Deliverer) { d.joins, d.joinGrace = j, grace }
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

// stillLoading reports whether xuid joined too recently to see a message
// sent right now.
func (d *Deliverer) stillLoading(xuid string) bool {
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

// SendNow delivers a immediately to whoever is online and matches its
// target, and returns how many actually received it.
func (d *Deliverer) SendNow(ctx context.Context, a Announcement, id int64) (int, error) {
	if !d.store.Enabled() {
		return 0, nil
	}
	targets := d.recipients(ctx, a)
	if len(targets) == 0 {
		// Nobody to tell right now and nothing to record; the announcement
		// stays pending in the store (if it queues at all) for whoever
		// joins later. Calling Say to an empty audience would broadcast
		// into the void with no delivery row to show for it.
		return 0, nil
	}
	now := time.Now()

	// Delivery is derived from the target, never trusted off the row: this
	// is exactly the guard announce.go's DeliveryFor exists for. Nothing
	// writes a.Delivery today, but the moment something does — a future
	// source, or a row PendingFor reads back after a bad write — a
	// TargetPlayer row that happened to carry DeliveryBroadcast must still
	// whisper, not broadcast a private message to the whole server.
	if DeliveryFor(a.TargetKind) == DeliveryBroadcast {
		err := d.voice.Say(ctx, a.Body)
		metrics.AnnounceDelivery(metrics.DeliveryBroadcast, err)
		if err != nil {
			// Say never went out, so nothing was heard — recording a
			// delivery here would make an online player's next join
			// silently skip a message they never actually received.
			d.log.Error("announce_say_failed", logging.Fields{"announcement_id": id, "error": err.Error()})
			return 0, nil
		}
		// Say is one console command with no per-recipient receipt, so
		// "who heard this" has to come from the roster snapshot taken at
		// send time, one row per player present — without those rows,
		// anyone online right now sees the same announcement again on
		// their next join.
		delivered := 0
		deferred := 0
		for _, xuid := range targets {
			if ctx.Err() != nil {
				// Cancelled: stop rather than attempt (and log) a store
				// write for every remaining recipient that would fail anyway.
				break
			}
			if d.stillLoading(xuid) {
				// Heard by everyone whose client is up, but not by this one:
				// recording it would be the same permanent loss a whisper to
				// a loading client used to be. Left pending for their drain.
				deferred++
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
		return delivered, nil
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
	return delivered, nil
}

// ErrDisabled is what Publish returns when there is no store to write an
// announcement to. A sentinel rather than a silent zero because the callers
// differ in what it means to them: an event source shrugs, while the HTTP
// API must tell its caller that nothing was stored.
var ErrDisabled = errors.New("announce: no announcement store configured")

// Publish stores a and then sends it to whoever is online and matches it,
// reporting the stored id and how many heard it immediately.
//
// The one path for every source that has no command reply to shape: a
// schedule, a server event, the HTTP API. Delivery is re-derived here from
// the target whatever the caller set, for the same reason SendNow never
// trusts it off a row. A send that reaches nobody is not an error -- the
// row is stored and the queue owns it from here.
func (d *Deliverer) Publish(ctx context.Context, a Announcement) (id int64, sent int, err error) {
	if !d.store.Enabled() {
		return 0, 0, ErrDisabled
	}
	a.Delivery = DeliveryFor(a.TargetKind)
	id, err = d.store.Insert(ctx, a)
	if err != nil {
		return 0, 0, err
	}
	a.ID = id
	sent, err = d.SendNow(ctx, a, id)
	return id, sent, err
}

// sendPending whispers each of msgs to xuid in order, marking every
// successful send delivered. A send or a mark that fails is logged and
// simply not counted: the announcement is left pending in the store, so it
// is retried on this player's next join or !inbox rather than lost.
func (d *Deliverer) sendPending(ctx context.Context, xuid string, now time.Time, msgs []Announcement) int {
	delivered := 0
	for _, a := range msgs {
		if ctx.Err() != nil {
			// Cancelled: stop here rather than turn the rest of a backlog
			// into that many more failed bridge attempts and error lines.
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
