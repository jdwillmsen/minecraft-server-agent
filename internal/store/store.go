// Package store persists who plays on the server and for how long.
//
// The interface exists so the agent runs identically with no database
// configured, which is how it ran through Stages 1-4 and how it must keep
// running: a greeting is a nicety, and losing Postgres should cost the
// greeting's personalisation, never the agent's ability to answer !help.
package store

import (
	"context"
	"time"
)

// Profile is what the agent knows about a player at the moment they join.
type Profile struct {
	XUID     string
	Gamertag string
	// FirstSeen is zero for a player this server has never seen.
	FirstSeen time.Time
	// LastSeen is the previous visit, not this one: RecordJoin returns the
	// profile as it was *before* the join it is recording, because "how long
	// since I last saw you" is unanswerable once the current visit has
	// overwritten it.
	LastSeen time.Time
	// JoinCount includes the join being recorded, so a first-ever arrival
	// reads as 1 rather than 0.
	JoinCount int
	// TotalSeconds is completed session time only. Sessions that ended
	// without being observed contribute nothing, so this is a lower bound --
	// see UncleanSessions.
	TotalSeconds int64
	// UncleanSessions counts visits the agent never saw end, because a server
	// restart, an agent restart or a crash ended them instead. A profile with
	// many of these has playtime that is a floor, not a measurement.
	UncleanSessions int
}

// New reports whether this is the first time the server has seen the player.
func (p Profile) New() bool { return p.JoinCount <= 1 || p.FirstSeen.IsZero() }

// AwayFor reports how long the player was gone before this visit. Zero for a
// player who has never been seen before.
func (p Profile) AwayFor(now time.Time) time.Duration {
	if p.LastSeen.IsZero() {
		return 0
	}
	return now.Sub(p.LastSeen)
}

// Store records presence. Every method must tolerate being called on a
// disabled implementation.
type Store interface {
	// RecordJoin notes an arrival and returns the profile as it stood before
	// it. Opening the session and reading the prior state happen together so
	// a greeting cannot describe a player as new after their own arrival has
	// already been counted.
	RecordJoin(ctx context.Context, xuid, gamertag string, at time.Time) (Profile, error)
	// RecordLeave closes the player's open session.
	RecordLeave(ctx context.Context, xuid string, at time.Time) error
	// EnsurePlayer makes sure a row exists for xuid without treating it as
	// an arrival: no join counted, no session opened, an existing row left
	// exactly as it is.
	//
	// Every other table in this schema references minecraft.players, so a
	// write about someone the agent never watched arrive -- a player already
	// connected when it logged in -- is rejected by a foreign key unless
	// something puts them there first. This is that something, kept separate
	// from RecordJoin because "has a row" and "just joined" are different
	// facts and only one of them is worth greeting.
	EnsurePlayer(ctx context.Context, xuid, gamertag string, at time.Time) error
	// XUIDForName resolves a gamertag to the XUID it belongs to, from what
	// this server has recorded rather than from who is connected.
	//
	// The live roster answers the same question for players who are online
	// right now, and answers it faster; this is the half that survives a
	// logout, which is the whole point of being able to leave a message for
	// someone who is not here. ok is false when the server has never
	// recorded anyone by that name -- which is a different fact from
	// "offline", and the only one that justifies refusing to store a
	// message.
	XUIDForName(ctx context.Context, gamertag string) (xuid string, ok bool, err error)
	// CloseOrphans marks sessions still open from a previous run as ended
	// without observation. Called once at startup: the agent learns of a
	// departure by being connected, so anything still open when it starts is
	// a visit whose end nobody saw.
	CloseOrphans(ctx context.Context, at time.Time) (int, error)
	// Close releases resources.
	Close()
	// Enabled reports whether this store actually persists anything.
	Enabled() bool
}

// Nop is the Store used when no database is configured. It answers every
// question with "nothing known", which the welcome plugin renders as the
// plain greeting the agent used before Stage 3.
type Nop struct{}

var _ Store = Nop{}

func (Nop) RecordJoin(context.Context, string, string, time.Time) (Profile, error) {
	return Profile{}, nil
}
func (Nop) RecordLeave(context.Context, string, time.Time) error          { return nil }
func (Nop) EnsurePlayer(context.Context, string, string, time.Time) error { return nil }
func (Nop) XUIDForName(context.Context, string) (string, bool, error)     { return "", false, nil }
func (Nop) CloseOrphans(context.Context, time.Time) (int, error)          { return 0, nil }
func (Nop) Close()                                                        {}
func (Nop) Enabled() bool                                                 { return false }
