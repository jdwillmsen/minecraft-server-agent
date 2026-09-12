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
// Deliberately not part of the roster. The roster answers who is here from
// the packets it is given and has no clock of its own; adding one would put
// time in the type that decides joins and leaves, where a wrong answer is
// worse than this.
type joinTimes struct {
	mu  sync.Mutex
	at  map[string]time.Time
	now func() time.Time
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

// forget drops everything, for a new connection. Players the server reports
// as already online are not arrivals: their clients have been rendering chat
// for however long they have been playing.
func (j *joinTimes) forget() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.at = make(map[string]time.Time)
}

// SinceJoin implements announce.JoinClock.
func (j *joinTimes) SinceJoin(xuid string) (time.Duration, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	at, ok := j.at[xuid]
	if !ok {
		return 0, false
	}
	return j.now().Sub(at), true
}
