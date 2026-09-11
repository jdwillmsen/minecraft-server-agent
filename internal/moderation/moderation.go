// Package moderation records public chat that broke a server rule.
//
// Only the flagged messages are stored, never chat as a whole. A complete
// chat log would be a far larger privacy exposure than the problem this
// solves -- knowing who keeps crossing a line -- and nothing here needs one.
// What is stored is kept for Retention and then pruned.
//
// Nothing here kicks, mutes or bans. The console bridge's allowlist does not
// permit those, and widening it is a security decision for a human rather
// than a side effect of this package: the record is what makes that later
// decision an informed one.
package moderation

import (
	"context"
	"time"
)

// Rule is which check a message failed. The values are exactly the set the
// schema's CHECK constraint allows; one that drifts is rejected at the first
// write, in production, not at compile time.
type Rule string

const (
	RuleTerm  Rule = "term"
	RuleFlood Rule = "flood"
	RuleCaps  Rule = "caps"
)

// Action is what the agent did about a flag, as opposed to what it decided
// to do: a warning whose whisper failed is recorded as logged, because the
// record is read later as a statement of what the player was told.
type Action string

const (
	ActionLogged Action = "logged"
	ActionWarned Action = "warned"
)

// Retention is how long a flag is kept. Long enough to see a pattern across
// a season of play; short enough that a player's worst afternoon does not
// follow them forever.
const Retention = 90 * 24 * time.Hour

// Event is one flag on one message. A message that fails two rules is two
// events, because rule is a single column and "which rules" is the thing an
// operator filters on.
type Event struct {
	ID       int64
	XUID     string
	Gamertag string
	// Message is the chat line as the player typed it.
	Message string
	Rule    Rule
	// Detail says why the rule fired: the configured term that matched, or
	// the measurement that crossed a threshold.
	Detail     string
	Action     Action
	OccurredAt time.Time
}

// Store persists flags. Every method must tolerate being called on a
// disabled implementation.
type Store interface {
	Record(ctx context.Context, e Event) error
	// Recent returns the newest flags first, at most limit of them: one
	// player's when xuid is set, everyone's when it is empty.
	Recent(ctx context.Context, xuid string, limit int) ([]Event, error)
	// Prune deletes flags recorded before cutoff and reports how many.
	Prune(ctx context.Context, cutoff time.Time) (int64, error)
	Enabled() bool
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Record(context.Context, Event) error                  { return nil }
func (Nop) Recent(context.Context, string, int) ([]Event, error) { return nil, nil }
func (Nop) Prune(context.Context, time.Time) (int64, error)      { return 0, nil }
func (Nop) Enabled() bool                                        { return false }
