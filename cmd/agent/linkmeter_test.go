package main

import (
	"testing"
	"time"
)

type fixedLatency time.Duration

func (f fixedLatency) Latency() time.Duration { return time.Duration(f) }

func TestLinkMeter_FollowsTheSession(t *testing.T) {
	var m linkMeter
	if _, ok := m.roundTrip(); ok {
		t.Fatal("round trip reported before any session")
	}

	m.beginSession(fixedLatency(3 * time.Millisecond))
	if got, ok := m.roundTrip(); !ok || got != 6*time.Millisecond {
		t.Errorf("round trip = %v (ok=%v), want 6ms: twice the half-RTT the connection reports", got, ok)
	}

	m.endSession()
	if _, ok := m.roundTrip(); ok {
		t.Error("round trip still reported after the session ended")
	}
}
