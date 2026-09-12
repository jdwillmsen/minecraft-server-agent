package main

import (
	"sync"
	"time"
)

// staleJoin is how long a remembered arrival is kept. Far longer than any
// grace that reads it: this exists only so a departure the agent never saw
// cannot leak an entry for the process's lifetime.
const staleJoin = time.Hour

// joinTimes remembers when each player was last seen to arrive.
//
// It answers one question, for the announcement deliverer: has this player
// only just arrived? A message sent to a client that is still loading is
// accepted by the server and displayed to nobody, and recording it as
// delivered loses it for good, so a fresh arrival's copy is left pending for
// their own join drain instead.
//
// A connection's own start counts as an arrival for everyone in its opening
// roster snapshot. The agent cannot tell a player who has been building for
// an hour from one who reconnected a second before it did -- and the second
// is exactly who is around during the restart wave this exists for -- so
// both read as fresh until the grace has passed since the connection began.
//
// Deliberately not part of the roster. The roster answers who is here from
// the packets it is given and has no clock of its own; adding one would put
// time in the type that decides joins and leaves, where a wrong answer is
// worse than this.
type joinTimes struct {
	mu    sync.Mutex
	at    map[string]time.Time
	since time.Time
	now   func() time.Time
}

func newJoinTimes() *joinTimes {
	return &joinTimes{at: make(map[string]time.Time), now: time.Now}
}

// joined records an arrival, and drops anything long enough past to be a
// departure nobody reported.
func (j *joinTimes) joined(xuid string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	for x, at := range j.at {
		if now.Sub(at) > staleJoin {
			delete(j.at, x)
		}
	}
	j.at[xuid] = now
}

// left forgets a departure: whoever returns under this XUID next is a new
// arrival, not the tail of this one.
func (j *joinTimes) left(xuid string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.at, xuid)
}

// connected starts a new connection: the previous one's arrivals are gone,
// and this one's start stands in for anyone the opening snapshot reports.
func (j *joinTimes) connected() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.at = make(map[string]time.Time)
	j.since = j.now()
}

// SinceJoin implements announce.JoinClock. A player's own arrival wins; a
// player without one is measured from the connection, which grows past any
// grace within seconds and leaves them reading as the settled player they
// almost certainly are.
func (j *joinTimes) SinceJoin(xuid string) (time.Duration, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if at, ok := j.at[xuid]; ok {
		return j.now().Sub(at), true
	}
	if j.since.IsZero() {
		return 0, false
	}
	return j.now().Sub(j.since), true
}
