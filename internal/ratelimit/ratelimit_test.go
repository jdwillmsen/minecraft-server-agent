package ratelimit

import (
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
