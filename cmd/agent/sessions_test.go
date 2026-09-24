package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGate is a presence decision a test flips by hand.
type fakeGate struct {
	mu      sync.Mutex
	present bool
	changed chan struct{}
}

func newFakeGate(present bool) *fakeGate {
	return &fakeGate{present: present, changed: make(chan struct{})}
}

func (g *fakeGate) Wanted() (bool, <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.present, g.changed
}

// set records a new answer and wakes whoever is waiting, whether or not the
// answer differs, as a store re-read would.
func (g *fakeGate) set(present bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.present = present
	close(g.changed)
	g.changed = make(chan struct{})
}

// recordedModes logs each mode starting and ending, and each settled leave,
// into one ordered list.
func recordedModes(order *steps) sessionModes {
	return sessionModes{
		present: func(ctx context.Context) {
			order.add("present")
			<-ctx.Done()
			order.add("present_ended")
		},
		left: func() { order.add("left") },
		absent: func(ctx context.Context) {
			order.add("absent")
			<-ctx.Done()
			order.add("absent_ended")
		},
	}
}

func startSessions(t *testing.T, gate sessionGate, modes sessionModes) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSessions(ctx, gate, modes, quiet())
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("runSessions did not return after its context ended")
		}
	}
	t.Cleanup(func() { cancel(); <-done })
	return stop
}

func equalSteps(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// Today's behavior, which this phase must keep: the leader is always in the
// world, and a shutdown is not a deliberate leave.
func TestAlwaysPresentRunsTheSessionForTheWholeTurn(t *testing.T) {
	order := &steps{}
	stop := startSessions(t, alwaysPresent{}, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	stop()

	if got, want := order.list(), []string{"present", "present_ended"}; !equalSteps(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestClosingTheGateEndsTheSessionThenFollowsFromOutside(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(true)
	startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	gate.set(false)

	want := []string{"present", "present_ended", "left", "absent"}
	waitUntil(t, func() bool { return equalSteps(order.list(), want) }, "closing the gate did not end the session, settle it and start following")
}

func TestReopeningTheGateStopsFollowingAndRejoins(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(false)
	startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "following never started")

	gate.set(true)

	want := []string{"absent", "absent_ended", "present"}
	waitUntil(t, func() bool { return equalSteps(order.list(), want) }, "reopening the gate did not stop following and rejoin")
}

func TestASpuriousGateSignalDoesNotRestartTheSession(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(true)
	startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	gate.set(true)
	gate.set(true)
	// Nothing to wait for: the assertion is that nothing happens. The
	// sleep only gives a wrong implementation time to act.
	time.Sleep(20 * time.Millisecond)

	if got := order.list(); !equalSteps(got, []string{"present"}) {
		t.Errorf("steps = %v, want the one session still running", got)
	}
}

func TestShutdownWhileAbsentDoesNotSettleALeave(t *testing.T) {
	order := &steps{}
	gate := newFakeGate(true)
	stop := startSessions(t, gate, recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")
	gate.set(false)
	waitUntil(t, func() bool { return len(order.list()) == 4 }, "following never started")

	stop()

	want := []string{"present", "present_ended", "left", "absent", "absent_ended"}
	if got := order.list(); !equalSteps(got, want) {
		t.Errorf("steps = %v, want %v: the leave was already settled once", got, want)
	}
}

// The turn's own handover settles a session that ends with the turn, so
// settling it here as well would close the same playtime twice.
func TestShutdownWhilePresentDoesNotSettleALeave(t *testing.T) {
	order := &steps{}
	stop := startSessions(t, newFakeGate(true), recordedModes(order))
	waitUntil(t, func() bool { return len(order.list()) == 1 }, "the session never started")

	stop()

	for _, s := range order.list() {
		if s == "left" {
			t.Fatalf("steps = %v: a session ended by the turn was settled as a deliberate leave", order.list())
		}
	}
}

// The session and the bridge follower write the same roster, and the
// follower's refresh reads it and then replaces it in two steps. Each mode
// lingers after its context ends, so a switch that did not wait for the old
// mode to return would start the new one while the old one still ran.
func TestTheSessionAndTheFollowerNeverRunAtTheSameTime(t *testing.T) {
	var running, overlaps, started atomic.Int32
	mode := func(ctx context.Context) {
		if running.Add(1) > 1 {
			overlaps.Add(1)
		}
		started.Add(1)
		<-ctx.Done()
		time.Sleep(time.Millisecond)
		running.Add(-1)
	}
	gate := newFakeGate(true)
	stop := startSessions(t, gate, sessionModes{present: mode, left: func() {}, absent: mode})
	waitUntil(t, func() bool { return started.Load() == 1 }, "the session never started")

	for i := range 20 {
		want := int32(i + 2)
		gate.set(i%2 == 1)
		waitUntil(t, func() bool { return started.Load() == want }, "the gate change did not switch modes")
	}
	stop()

	if n := overlaps.Load(); n != 0 {
		t.Errorf("a mode started %d times while the other was still running", n)
	}
	if n := running.Load(); n != 0 {
		t.Errorf("%d modes still running after runSessions returned", n)
	}
}
