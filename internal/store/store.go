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
	// TotalSeconds sums only sessions whose departure the agent observed
	// (ended_reason 'left'). A session it never saw end contributes nothing,
	// whatever duration its row carries, so this is a lower bound -- see
	// UncleanSessions.
	TotalSeconds int64
	// UncleanSessions counts visits the agent never saw end, because a server
	// restart, an agent restart or a crash ended them instead. A profile with
	// many of these has playtime that is a floor, not a measurement.
	UncleanSessions int
	// Sessions counts every visit recorded before this one, watched
	// arrivals and presences resumed from a roster snapshot alike. Zero
	// means the agent has never seen the player, whether or not a bare row
	// was already written for them.
	Sessions int
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

// Playtime is a player's completed session time immediately before and
// after one departure was recorded.
//
// Both totals count exactly the sessions TotalSeconds does: those ended by
// an observed departure. Time in a session the agent never saw end is not
// in either, even where the row records a duration, so both are lower
// bounds and a milestone is never announced off time nobody watched. Equal
// totals mean the departure credited nothing: no open session was found,
// which is what a leave the agent already recorded looks like, or the only
// one open began before the agent's current connection and so was never
// watched through.
type Playtime struct {
	// Gamertag is who the closed session says they were, for a caller that
	// names the player after the roster has already forgotten them.
	Gamertag string
	Before   time.Duration
	After    time.Duration
}

// Store records presence. Every method must tolerate being called on a
// disabled implementation.
type Store interface {
	// RecordJoin notes an arrival and returns the profile as it stood before
	// it. Opening the session and reading the prior state happen together so
	// a greeting cannot describe a player as new after their own arrival has
	// already been counted.
	RecordJoin(ctx context.Context, xuid, gamertag string, at time.Time) (Profile, error)
	// RecordLeave closes the player's open session and reports their total
	// playtime on either side of it.
	//
	// since is when the current connection began watching. Only a session
	// that started at or after it is credited; an older one was open across
	// a gap the agent never saw, so it is closed as unobserved and adds
	// nothing, whatever the player did in between.
	//
	// Returned rather than read afterwards because "this session carried
	// them past a milestone" is a comparison of the two, and two separate
	// reads can straddle a concurrent write and disagree about what the
	// session added.
	RecordLeave(ctx context.Context, xuid string, since, at time.Time) (Playtime, error)
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
	// ResumeSession opens a session at `at` for a player who was already
	// connected when the agent began watching -- one in a connection's
	// opening roster snapshot -- without counting it as an arrival.
	//
	// Playtime counts only time the agent watched. The part of the visit
	// before this connection was closed at zero length when it began, so
	// without a fresh session here the player's eventual departure would
	// close nothing and the time watched from now on would be lost too.
	//
	// firstSeen reports whether the player had no session before this one:
	// the agent's first sight of them, whether or not a row existed.
	ResumeSession(ctx context.Context, xuid, gamertag string, at time.Time) (firstSeen bool, err error)
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
	// CloseOrphans marks every open session as ended without observation.
	// Called at the start of every connection: the agent learns of a
	// departure by being connected, so anything still open when it
	// (re)connects is a visit whose end nobody saw.
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
func (Nop) RecordLeave(context.Context, string, time.Time, time.Time) (Playtime, error) {
	return Playtime{}, nil
}
func (Nop) EnsurePlayer(context.Context, string, string, time.Time) error { return nil }
func (Nop) ResumeSession(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (Nop) XUIDForName(context.Context, string) (string, bool, error) { return "", false, nil }
func (Nop) CloseOrphans(context.Context, time.Time) (int, error)      { return 0, nil }
func (Nop) Close()                                                    {}
func (Nop) Enabled() bool                                             { return false }
