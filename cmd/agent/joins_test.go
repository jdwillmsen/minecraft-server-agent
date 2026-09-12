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

// A new connection drops the previous one's arrivals, and times itself: the
// two are separate answers, because everyone in the opening snapshot may
// have reconnected moments before the agent did, and that is a guess where
// an arrival is a fact.
func TestJoinTimesReportsTheConnectionSeparatelyFromAnArrival(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)
	j.joined("111")
	now = now.Add(time.Hour)
	j.connected()

	if _, ok := j.SinceJoin("111"); ok {
		t.Error("a previous connection's arrival survived into this one")
	}

	now = now.Add(2 * time.Second)
	if since, ok := j.SinceConnect(); !ok || since != 2*time.Second {
		t.Errorf("SinceConnect = %v, %v, want 2s, true", since, ok)
	}
	if _, ok := j.SinceJoin("111"); ok {
		t.Error("reported an arrival for a player who only appeared in the snapshot")
	}

	j.joined("111")
	now = now.Add(time.Second)
	if since, ok := j.SinceJoin("111"); !ok || since != time.Second {
		t.Errorf("SinceJoin = %v, %v, want 1s, true: an arrival of their own is known exactly", since, ok)
	}
	if since, ok := j.SinceConnect(); !ok || since != 3*time.Second {
		t.Errorf("SinceConnect = %v, %v, want 3s, true: an arrival does not restart the connection", since, ok)
	}
}

// Nothing is known before the first connection.
func TestJoinTimesKnowsNothingBeforeAConnection(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)

	if _, ok := j.SinceConnect(); ok {
		t.Error("reported a connection before one began")
	}
	if _, ok := j.SinceJoin("111"); ok {
		t.Error("reported an arrival nobody made")
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
