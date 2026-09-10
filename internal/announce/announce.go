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
func DeliveryFor(t Target) Delivery {
	switch t {
	case TargetPlayer, TargetPermission:
		return DeliveryWhisper
	default:
		return DeliveryBroadcast
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
// Online-only needs none, since it never queues to begin with. A message
// whispered to one player gets a week, because it's personal and still true
// next week; everything else gets a day, because it's addressed to whoever
// happens to be around when it's said.
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
