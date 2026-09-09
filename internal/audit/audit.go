// Package audit records who ran which command, at what privilege, and what
// happened.
//
// Separate from the stdout logging that already exists: that record dies
// with the pod, cannot be queried, and is not something an auditor would
// accept. This one is a table.
//
// It deliberately does not store reply text. Replies to the waypoint
// commands carry coordinates the agent goes out of its way to whisper, and a
// trail that transcribes every private reply would be a larger exposure than
// the gap it closes.
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
