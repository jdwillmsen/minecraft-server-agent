package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestPerActor_AllowsUpToMaxWithinWindow(t *testing.T) {
	r := NewPerActor(3, time.Minute)
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !r.Allow("player-1", now) {
			t.Fatalf("call %d: denied, want allowed (within max)", i+1)
		}
	}
	if r.Allow("player-1", now) {
		t.Error("4th call within the window: allowed, want denied")
	}
}

func TestPerActor_DifferentActorsHaveIndependentBudgets(t *testing.T) {
	r := NewPerActor(1, time.Minute)
	now := time.Now()

	if !r.Allow("player-1", now) {
		t.Fatal("player-1's first call denied")
	}
	if !r.Allow("player-2", now) {
		t.Error("player-2's first call denied - budgets should not be shared across actors")
	}
	if r.Allow("player-1", now) {
		t.Error("player-1's second call allowed - should be at its own limit")
	}
}

func TestPerActor_OldEventsExpireOutOfTheWindow(t *testing.T) {
	r := NewPerActor(1, time.Minute)
	start := time.Now()

	if !r.Allow("player-1", start) {
		t.Fatal("first call denied")
	}
	if r.Allow("player-1", start.Add(30*time.Second)) {
		t.Error("second call inside the window: allowed, want denied")
	}
	if !r.Allow("player-1", start.Add(61*time.Second)) {
		t.Error("call after the window expired: denied, want allowed")
	}
}

func TestPerActor_DeniedCallsAreNotCountedAgainstFutureBudget(t *testing.T) {
	r := NewPerActor(1, time.Minute)
	now := time.Now()

	if !r.Allow("player-1", now) {
		t.Fatal("first call denied")
	}
	for i := 0; i < 5; i++ {
		r.Allow("player-1", now) // repeatedly denied; must not consume more budget
	}
	if !r.Allow("player-1", now.Add(61*time.Second)) {
		t.Error("call after window expiry was still denied - repeated denials must not extend the block")
	}
}

func TestPerActor_StaleActorsAreEvictedFromTheMap(t *testing.T) {
	r := NewPerActor(2, time.Minute)
	base := time.Unix(0, 0)

	for i := 0; i < 100; i++ {
		if !r.Allow(fmt.Sprintf("xuid-%d", i), base) {
			t.Fatalf("Allow(xuid-%d) = false on first call, want true", i)
		}
	}

	// One later call, past the window, must clear out every actor whose
	// events have all expired - otherwise the map grows for the life of the
	// process, one entry per actor that ever issued a command.
	if !r.Allow("recent", base.Add(2*time.Minute)) {
		t.Fatal("Allow(recent) = false, want true")
	}

	r.mu.Lock()
	got := len(r.events)
	r.mu.Unlock()
	if got != 1 {
		t.Errorf("tracked actors after the window elapsed = %d, want 1 (only the recent actor)", got)
	}
}

func TestPerActor_SweepKeepsActorsStillInsideTheWindow(t *testing.T) {
	r := NewPerActor(2, time.Minute)
	base := time.Unix(0, 0)

	if !r.Allow("alice", base) {
		t.Fatal("Allow(alice) at base = false, want true")
	}
	if !r.Allow("alice", base.Add(50*time.Second)) {
		t.Fatal("Allow(alice) at base+50s = false, want true")
	}
	// Far enough past the last sweep to trigger another one, while alice's
	// base+50s event is still inside her window.
	if !r.Allow("bob", base.Add(61*time.Second)) {
		t.Fatal("Allow(bob) = false, want true")
	}
	if !r.Allow("alice", base.Add(61*time.Second)) {
		t.Fatal("Allow(alice) at base+61s = false, want true")
	}
	if r.Allow("alice", base.Add(62*time.Second)) {
		t.Error("Allow(alice) at base+62s = true, want false - the sweep discarded her live events")
	}
}
