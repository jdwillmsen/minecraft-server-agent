package announce

import (
	"context"
	"time"

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
}

// NewDeliverer builds a Deliverer over the given Store, Voice, Roster and
// Permissions.
func NewDeliverer(s Store, v Voice, r Roster, p Permissions, log *logging.Logger) *Deliverer {
	return &Deliverer{store: s, voice: v, roster: r, perms: p, log: log}
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

	if a.Delivery == DeliveryBroadcast {
		if err := d.voice.Say(ctx, a.Body); err != nil {
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
		for _, xuid := range targets {
			if err := d.store.MarkDelivered(ctx, id, xuid, now); err != nil {
				d.log.Error("announce_mark_delivered_failed", logging.Fields{"announcement_id": id, "xuid": xuid, "error": err.Error()})
				continue
			}
			delivered++
		}
		return delivered, nil
	}

	// Whisper: each recipient gets their own Tell, and only a recipient
	// whose Tell actually succeeded is marked delivered. A failed send is
	// logged and left pending — the next join retries it — rather than
	// recorded as delivered, which would lose the message for good since
	// nothing else retries it.
	delivered := 0
	for _, xuid := range targets {
		if err := d.voice.Tell(ctx, xuid, a.Body); err != nil {
			d.log.Error("announce_tell_failed", logging.Fields{"announcement_id": id, "xuid": xuid, "error": err.Error()})
			continue
		}
		if err := d.store.MarkDelivered(ctx, id, xuid, now); err != nil {
			d.log.Error("announce_mark_delivered_failed", logging.Fields{"announcement_id": id, "xuid": xuid, "error": err.Error()})
			continue
		}
		delivered++
	}
	return delivered, nil
}

// sendPending whispers each of msgs to xuid in order, marking every
// successful send delivered. A send or a mark that fails is logged and
// simply not counted: the announcement is left pending in the store, so it
// is retried on this player's next join or !inbox rather than lost.
func (d *Deliverer) sendPending(ctx context.Context, xuid string, now time.Time, msgs []Announcement) int {
	delivered := 0
	for _, a := range msgs {
		if err := d.voice.Tell(ctx, xuid, a.Body); err != nil {
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
// remaining reports how many normal messages were left unsent so the caller
// can decide whether to point the player at !inbox instead of dumping the
// whole backlog into their first moments in the world.
func (d *Deliverer) DrainForJoin(ctx context.Context, xuid string, now time.Time) (delivered, remaining int, err error) {
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
	remaining = len(normal) - limit
	return delivered, remaining, nil
}

// DrainAll sends xuid every pending message with no cap — the !inbox
// command's job, for a player who has explicitly asked for the rest rather
// than having it trickle in a few at a time across future joins.
func (d *Deliverer) DrainAll(ctx context.Context, xuid string, now time.Time) (int, error) {
	if !d.store.Enabled() {
		return 0, nil
	}
	permission := d.perms.Resolve(ctx, xuid)
	pending, err := d.store.PendingFor(ctx, xuid, permission, now)
	if err != nil {
		return 0, err
	}
	return d.sendPending(ctx, xuid, now, pending), nil
}
