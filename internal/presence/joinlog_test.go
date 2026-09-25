package presence

import (
	"testing"
	"time"
)

func TestJoinLogKeepsRecentArrivalsOnly(t *testing.T) {
	now := t0
	l := NewJoinLog()
	l.now = func() time.Time { return now }
	l.Record("Steve")
	now = now.Add(joinRetention)
	l.Record("Alex")
	got := l.Recent()
	if len(got) != 2 || got[0].Gamertag != "Steve" || !got[0].At.Equal(t0) {
		t.Fatalf("Recent() = %+v, want both, Steve first", got)
	}
	now = now.Add(time.Second)
	if got := l.Recent(); len(got) != 1 || got[0].Gamertag != "Alex" {
		t.Errorf("Recent() = %+v, want Steve aged out", got)
	}
}

func TestJoinLogKeepsTheSourcesOwnTime(t *testing.T) {
	now := t0
	l := NewJoinLog()
	l.now = func() time.Time { return now }
	l.RecordAt("Alex", t0.Add(-time.Minute))
	l.RecordAt("Old", t0.Add(-joinRetention-time.Second))
	got := l.Recent()
	if len(got) != 1 || got[0].Gamertag != "Alex" || !got[0].At.Equal(t0.Add(-time.Minute)) {
		t.Errorf("Recent() = %+v, want Alex at the time the console printed", got)
	}
}

// A long bridge backlog replayed at the start of an absence is all stale; it
// must not push a fresh arrival out of the bounded log.
func TestJoinLogDropsAStaleBacklogOnArrival(t *testing.T) {
	now := t0
	l := NewJoinLog()
	l.now = func() time.Time { return now }
	l.Record("Steve")
	for i := 0; i < maxJoins; i++ {
		l.RecordAt("Old", t0.Add(-joinRetention-time.Second))
	}
	if got := l.Recent(); len(got) != 1 || got[0].Gamertag != "Steve" {
		t.Errorf("Recent() = %+v, want only Steve", got)
	}
}

// A join storm cannot grow the log without bound.
func TestJoinLogIsBounded(t *testing.T) {
	l := NewJoinLog()
	for i := 0; i < maxJoins+10; i++ {
		l.Record("Steve")
	}
	if n := len(l.Recent()); n != maxJoins {
		t.Errorf("kept %d joins, want the newest %d", n, maxJoins)
	}
}
