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

// A new connection starts from nothing: everyone the snapshot reports has
// been playing, and their clients are rendering chat already.
func TestJoinTimesForgetEverythingOnANewConnection(t *testing.T) {
	now := time.Now()
	j := fixedJoinTimes(&now)
	j.joined("111")
	j.forget()

	if _, ok := j.SinceJoin("111"); ok {
		t.Error("a previous connection's arrival survived into this one")
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
