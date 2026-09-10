// Package announce is what the server wants said, and to whom.
//
// The types here are deliberately dumb: a source's whole job is to describe
// an announcement correctly, and every question of who sees it, when, and
// whether it is still worth saying belongs to the store and the deliverer
// built on top. That split keeps a fourth source as cheap as the first.
package announce

import "time"

// Target is who an announcement is for. The values match the migration's
// target_kind CHECK constraint verbatim; a value that drifts is rejected at
// the first write, in production, not at compile time.
type Target string

const (
	TargetEveryone   Target = "everyone"
	TargetPlayer     Target = "player"
	TargetPermission Target = "permission"
	TargetOnlineOnly Target = "online_only"
)

// Priority is how eagerly an announcement should be delivered.
type Priority string

const (
	PriorityNormal    Priority = "normal"
	PriorityExpedited Priority = "expedited"
)

// Source is what produced an announcement.
type Source string

const (
	SourceCommand  Source = "command"
	SourceSchedule Source = "schedule"
	SourceEvent    Source = "event"
	SourceAPI      Source = "api"
)

// Delivery is how an announcement reaches a player.
type Delivery string

const (
	DeliveryBroadcast Delivery = "broadcast"
	DeliveryWhisper   Delivery = "whisper"
)

// MaxNormalPerJoin caps ordinary announcements delivered at one login. The
// join moment is already spent on the welcome message, and a wall of text
// reads worse than a trickle; !inbox is where the remainder goes for a
// player who wants it right away.
const MaxNormalPerJoin = 3

// MaxPerInbox caps how many announcements one !inbox delivers.
//
// !inbox runs inside a command dispatch's timeout, and every message on it
// costs a whisper through the console bridge plus a delivery row. An
// uncapped drain of a real backlog spends that budget before it finishes,
// the dispatch is abandoned as a timeout, and the player gets a partial
// trickle of messages and no reply at all -- the one outcome worse than
// being told to ask again. Larger than the per-join cap because the player
// asked for this and nothing else is competing for the moment; small enough
// that an ordinary backlog is answered rather than abandoned.
const MaxPerInbox = 5

// MaxBodyChars caps an announcement's body, in characters rather than
// bytes, for every source that lets a person or a system choose the words.
//
// Set at what an operator can already type into Bedrock chat, so the cap
// changes nothing for !announce and exists for the sources that are not
// bound by a chat box: a schedule repeats its body indefinitely, and the
// HTTP API accepts whatever a script hands it. Nothing downstream truncates
// -- the console bridge sends what it is given -- so a body this long is
// refused where it is written rather than cut off mid-word in chat.
const MaxBodyChars = 512

// Announcement is one thing the server wants said.
type Announcement struct {
	ID           int64
	Body         string
	Source       Source
	AuthorXUID   string
	TargetKind   Target
	TargetValue  string
	Priority     Priority
	Delivery     Delivery
	CreatedAt    time.Time
	DeliverAfter time.Time
	// ExpiresAt nil means the announcement never goes stale.
	ExpiresAt *time.Time
}

// DeliveryFor derives how an announcement is said from who it is for.
// Derived rather than chosen: a player- or permission-targeted announcement
// that broadcast would expose exactly what whispering a waypoint protects.
//
// The two branches are asymmetric on purpose: only everyone and online-only
// genuinely broadcast, and anything else — including a zero-value or
// future Target this switch doesn't recognize yet — whispers. Whispering
// something that could have been broadcast just reaches fewer people;
// broadcasting something that should have been whispered puts a player's
// coordinates in public chat. The default has to fail toward silence.
func DeliveryFor(t Target) Delivery {
	switch t {
	case TargetEveryone, TargetOnlineOnly:
		return DeliveryBroadcast
	default:
		return DeliveryWhisper
	}
}

// Queues reports whether an announcement aimed at t waits for a player who
// is offline right now. Online-only never does: a restart countdown
// delivered after the fact isn't a warning anymore, it's noise.
func Queues(t Target) bool {
	return t != TargetOnlineOnly
}

// DefaultExpiry is how long an announcement of this kind stays worth saying.
// This is the line between a queue and a nag: without it, a message
// eventually reaches whoever logs in next no matter how stale it has gone.
// Online-only needs none, since it never queues to begin with. The rest
// split on how personal the message is, not on whether it's whispered: a
// message addressed to one player gets a week, because it's still true for
// that specific person next week. A message addressed to a permission — a
// role rather than a person — gets a day like a broadcast does, because
// roles change hands and whoever holds one next may not be who it was
// written for.
//
// s doesn't change the window today, but the parameter stays so a schedule
// or an API-sourced announcement can earn its own window later without
// changing every call site.
func DefaultExpiry(s Source, t Target, now time.Time) *time.Time {
	if !Queues(t) {
		return nil
	}
	d := 24 * time.Hour
	if t == TargetPlayer {
		d = 7 * 24 * time.Hour
	}
	at := now.Add(d)
	return &at
}
