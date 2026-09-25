package presence

import (
	"slices"
	"sync"
	"time"
)

const (
	// joinRetention is how long an arrival is kept for the wake rules. It is
	// several ticks rather than one, so a tick that could not read the
	// database does not lose the arrival that should have woken an actor.
	// Replaying an old arrival is harmless: the policy ignores any from
	// before an override was set.
	joinRetention = 5 * time.Minute
	// maxJoins bounds the log against a join storm. Far above what a
	// friends' server sees in five minutes.
	maxJoins = 256
)

// JoinLog is the players who recently arrived, fed from the session's roster
// events while the agent is in the world and from the console bridge while
// it is not.
type JoinLog struct {
	mu    sync.Mutex
	joins []Join
	now   func() time.Time
}

func NewJoinLog() *JoinLog { return &JoinLog{now: time.Now} }

// Record notes an arrival now, for a source that carries no time of its own.
func (l *JoinLog) Record(gamertag string) { l.RecordAt(gamertag, l.now()) }

// RecordAt notes an arrival at the time its source says it happened. A
// console line replayed from the bridge's backlog keeps its own time, so it
// cannot pass for a join that came after a park. One already past retention
// is dropped here rather than at the next read, so a long stale backlog
// cannot crowd fresh arrivals out of the bounded log.
func (l *JoinLog) RecordAt(gamertag string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at.Before(l.now().Add(-joinRetention)) {
		return
	}
	l.joins = append(l.joins, Join{Gamertag: gamertag, At: at})
	if over := len(l.joins) - maxJoins; over > 0 {
		l.joins = slices.Delete(l.joins, 0, over)
	}
}

// Recent returns the arrivals of the last joinRetention, in the order they
// were recorded.
func (l *JoinLog) Recent() []Join {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-joinRetention)
	l.joins = slices.DeleteFunc(l.joins, func(j Join) bool { return j.At.Before(cutoff) })
	return slices.Clone(l.joins)
}
