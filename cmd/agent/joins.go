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
// It answers one question for the announcement deliverer: has this player
// only just arrived? A message sent to a client that is still loading is
// accepted by the server and displayed to nobody, and recording it as
// delivered loses it for good, so a fresh arrival's copy is left pending for
// their own join drain instead.
//
// It also remembers when this connection began, which is all the agent
// knows about anyone in the opening roster snapshot: it cannot tell a player
// who has been building for an hour from one who reconnected a second before
// it did, and the second is exactly who is around during a restart wave. The
// two are reported separately, because withholding a delivery row on a guess
// costs a duplicate whisper while acting on one can cancel a real drain.
//
// For the join drain it answers a second one: which connection is live, and
// when the one a waiting delivery belongs to ends -- a delivery that speaks
// after its own connection is gone lands on players who are mid-reconnect.
//
// Deliberately not part of the roster. The roster answers who is here from
// the packets it is given and has no clock of its own; adding one would put
// time in the type that decides joins and leaves, where a wrong answer is
// worse than this.
type joinTimes struct {
	mu sync.Mutex
	// ended is closed when the live connection ends, so a delivery already
	// under way stops rather than finishing against a connection that is
	// gone. A generation read once, before the send, cannot see that.
	ended chan struct{}
	at    map[string]time.Time
	since time.Time
	// gen counts connections. It is what tells a delivery scheduled before
	// a reconnect that it no longer speaks for anyone: the connection it
	// was scheduled in is gone, and the one that replaced it has reported
	// every player still there.
	gen uint64
	now func() time.Time
}

func newJoinTimes() *joinTimes {
	return &joinTimes{at: make(map[string]time.Time), ended: make(chan struct{}), now: time.Now}
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
	j.gen++
	j.endCurrent()
	j.ended = make(chan struct{})
}

// disconnected ends the current connection. Arrivals are kept: a player who
// joined moments before the drop is still loading, and a message published
// during the gap must not be recorded against them. What changes is the
// generation, so nothing scheduled under the dead connection still believes
// it speaks for anyone.
func (j *joinTimes) disconnected() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.gen++
	j.endCurrent()
}

// endCurrent closes the live connection's channel once. Callers hold the
// lock.
func (j *joinTimes) endCurrent() {
	select {
	case <-j.ended:
		// Already closed: two ends for one connection, which a drop
		// followed by the next connect produces.
	default:
		close(j.ended)
	}
}

// Ended implements plugins.Connections: a channel closed when the
// connection live at the time of the call ends. A delivery captures it
// before it waits, so a send already under way stops mid-backlog rather
// than whispering the rest at a player who is no longer being watched by
// this connection -- a generation read once, before the send, cannot see
// that happen.
func (j *joinTimes) Ended() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.ended
}

// Connected implements announce.Session: whether a Bedrock session is live
// right now. Read from the same channel Ended hands out, which connected
// opens and disconnected closes, so there is no second record of the fact to
// fall out of step with it.
func (j *joinTimes) Connected() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.since.IsZero() {
		// Never connected at all: the channel is open only because no
		// connection has opened one of its own yet.
		return false
	}
	select {
	case <-j.ended:
		return false
	default:
		return true
	}
}

// Generation implements plugins.Connections.
func (j *joinTimes) Generation() uint64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.gen
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

// SinceConnect implements announce.JoinClock.
func (j *joinTimes) SinceConnect() (time.Duration, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.since.IsZero() {
		return 0, false
	}
	return j.now().Sub(j.since), true
}
