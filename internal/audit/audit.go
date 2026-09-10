// Package audit records who ran which command, at what privilege, and what
// happened.
//
// Separate from the stdout logging that already exists: that record dies
// with the pod, cannot be queried, and is not something an auditor would
// accept. This one is a table.
//
// It stores the command and its arguments, and deliberately not the reply.
// That protects less than it might sound like, and the difference is worth
// stating: args holds the raw argument string, so "!wp set base 100 64 -200"
// records those coordinates verbatim, as does the body of a private
// "!announce @player". What the exclusion keeps out is everything a reply
// says that nobody typed -- the answer to a bare "!wp" lists every waypoint
// a player owns, including the ones this command never mentioned, and a
// trail that transcribed those would expose far more than the dispatch it
// is recording.
package audit

import (
	"context"
	"time"
)

// Outcome is what happened to a dispatch. The values are exactly the set
// the schema's CHECK constraint allows.
type Outcome string

const (
	OutcomeOK          Outcome = "ok"
	OutcomeDenied      Outcome = "denied"
	OutcomeUnknown     Outcome = "unknown"
	OutcomeError       Outcome = "error"
	OutcomeRateLimited Outcome = "rate_limited"
	OutcomeTimeout     Outcome = "timeout"
)

// Outcomes is every Outcome, so a caller that must enumerate them -- the
// command counter starts each one at zero -- cannot drift from this list.
func Outcomes() []Outcome {
	return []Outcome{OutcomeOK, OutcomeDenied, OutcomeUnknown, OutcomeError, OutcomeRateLimited, OutcomeTimeout}
}

// Record is one command dispatch.
type Record struct {
	XUID     string
	Gamertag string
	// Permission is the level the actor resolved at when the command ran,
	// not their level now.
	Permission string
	Command    string
	Args       string
	Outcome    Outcome
	At         time.Time
}

// Store persists audit records. Every method must tolerate being called on
// a disabled implementation.
type Store interface {
	Write(ctx context.Context, r Record) error
	Enabled() bool
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Write(context.Context, Record) error { return nil }
func (Nop) Enabled() bool                       { return false }
