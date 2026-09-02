package main

import (
	"testing"
	"time"
)

func TestNextDelay_ResetsAfterAStableSession(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	got := nextDelay(120*time.Second, stableSessionThreshold, min, max)
	if got != min {
		t.Errorf("nextDelay after a stable session = %v, want %v", got, min)
	}
}

func TestNextDelay_DoublesAfterAShortSession(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	got := nextDelay(10*time.Second, stableSessionThreshold-time.Nanosecond, min, max)
	if got != 20*time.Second {
		t.Errorf("nextDelay = %v, want 20s", got)
	}
}

func TestNextDelay_ClampsToMax(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	got := nextDelay(200*time.Second, time.Second, min, max)
	if got != max {
		t.Errorf("nextDelay = %v, want the max %v", got, max)
	}
	if got := nextDelay(max, time.Second, min, max); got != max {
		t.Errorf("nextDelay already at max = %v, want %v", got, max)
	}
}

func TestNextDelay_NeverDropsBelowMin(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	if got := nextDelay(0, time.Second, min, max); got != min {
		t.Errorf("nextDelay from a zero delay = %v, want the min %v", got, min)
	}
}

func TestNextDelay_ReachesMaxWithoutOvershooting(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	delay := min
	for i := 0; i < 50; i++ {
		delay = nextDelay(delay, time.Second, min, max)
		if delay < min || delay > max {
			t.Fatalf("iteration %d: delay %v outside [%v, %v]", i, delay, min, max)
		}
	}
	if delay != max {
		t.Errorf("delay after 50 failed sessions = %v, want the max %v", delay, max)
	}
}

func TestJitter_StaysWithinHalfToFullRange(t *testing.T) {
	d := 100 * time.Second
	for i := 0; i < 1000; i++ {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, d/2, d)
		}
	}
}

func TestJitter_NonPositiveIsReturnedUnchanged(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second); got != -time.Second {
		t.Errorf("jitter(-1s) = %v, want -1s", got)
	}
}

func TestJitter_SubNanosecondDurationDoesNotPanic(t *testing.T) {
	// half == 0 would make rand.Int63n(0) panic if the bound weren't +1.
	if got := jitter(1); got != 0 && got != 1 {
		t.Errorf("jitter(1ns) = %v, want 0 or 1ns", got)
	}
}

func TestJitter_VariesAcrossCalls(t *testing.T) {
	d := time.Hour
	first := jitter(d)
	for i := 0; i < 100; i++ {
		if jitter(d) != first {
			return
		}
	}
	t.Errorf("jitter(%v) returned %v on 101 consecutive calls; backoff is not randomised", d, first)
}
