package main

import (
	"testing"
	"time"
)

func fixedJoinTimes(now *time.Time) *joinTimes {
	j := newJoinTimes()
	j.now = func() time.Time { return *now }
	return j
}

func TestJoinTimesReportsHowLongAgoSomeoneArrived(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)

	if _, ok := j.SinceJoin("111"); ok {
		t.Error("reported a join for a player who never arrived: a player already online is not a fresh arrival")
	}

	j.joined("111")
	now = now.Add(3 * time.Second)
	since, ok := j.SinceJoin("111")
	if !ok || since != 3*time.Second {
		t.Errorf("SinceJoin = %v, %v, want 3s, true", since, ok)
	}
}

func TestJoinTimesForgetsADeparture(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)
	j.joined("111")
	j.left("111")

	if _, ok := j.SinceJoin("111"); ok {
		t.Error("still reporting a join after the player left")
	}
}

// A new connection drops the previous one's arrivals, and stands in for the
// arrival of anyone in its opening snapshot: they may have reconnected
// moments before the agent did.
func TestJoinTimesMeasuresAnUnknownPlayerFromTheConnection(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)
	j.joined("111")
	now = now.Add(time.Hour)
	j.connected()

	since, ok := j.SinceJoin("111")
	if !ok || since != 0 {
		t.Errorf("SinceJoin = %v, %v, want 0, true: the previous connection's arrival must not survive", since, ok)
	}

	now = now.Add(2 * time.Second)
	if since, ok := j.SinceJoin("111"); !ok || since != 2*time.Second {
		t.Errorf("SinceJoin = %v, %v, want 2s, true", since, ok)
	}

	now = now.Add(time.Minute)
	if since, ok := j.SinceJoin("111"); !ok || since != 62*time.Second {
		t.Errorf("SinceJoin = %v, %v, want 62s, true: past any grace, they read as settled", since, ok)
	}
}

// Nothing is known before the first connection, so there is no arrival to
// measure anyone against.
func TestJoinTimesKnowsNothingBeforeAConnection(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)

	if _, ok := j.SinceJoin("111"); ok {
		t.Error("reported an arrival with no connection to measure it from")
	}
}

// An arrival of the player's own is what the deliverer must see, not the
// older connection time that would read as settled.
func TestJoinTimesPrefersAnArrivalOverTheConnection(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)
	j.connected()
	now = now.Add(time.Hour)
	j.joined("111")
	now = now.Add(time.Second)

	since, ok := j.SinceJoin("111")
	if !ok || since != time.Second {
		t.Errorf("SinceJoin = %v, %v, want 1s, true", since, ok)
	}
}

// A departure the agent never saw must not keep an entry forever.
func TestJoinTimesDropsEntriesOlderThanStale(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)
	j.joined("111")

	now = now.Add(staleJoin + time.Minute)
	j.joined("222")

	if _, ok := j.SinceJoin("111"); ok {
		t.Error("kept an arrival older than staleJoin")
	}
	if _, ok := j.SinceJoin("222"); !ok {
		t.Error("dropped the arrival that was just recorded")
	}
}
