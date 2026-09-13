package leader

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSession is one dedicated connection, with the lock it would hold
// modelled as a counter of how many try attempts still report it taken.
//
// It also records whether anything ever used it from two goroutines at once.
// That is not a stylistic check: the real session is a single PostgreSQL
// connection, pgx connections are not safe for concurrent use, and the
// liveness probe runs on its own goroutine alongside whatever the caller
// does with the term.
type fakeSession struct {
	mu sync.Mutex
	// heldElsewhere counts the try attempts that report the lock taken by
	// somebody else before one succeeds.
	heldElsewhere int
	tryErr        error
	aliveErr      error
	// aliveAfter is how many successful probes precede aliveErr, so a test
	// can let a term settle before the connection dies under it.
	aliveAfter int

	tries    int
	probes   int
	unlocked int
	closed   int

	busy       atomic.Bool
	concurrent atomic.Bool
}

func (s *fakeSession) enter() func() {
	if !s.busy.CompareAndSwap(false, true) {
		s.concurrent.Store(true)
	}
	return func() { s.busy.Store(false) }
}

func (s *fakeSession) TryLock(context.Context) (bool, error) {
	defer s.enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tries++
	if s.tryErr != nil {
		return false, s.tryErr
	}
	if s.heldElsewhere > 0 {
		s.heldElsewhere--
		return false, nil
	}
	return true, nil
}

func (s *fakeSession) Unlock(context.Context) error {
	defer s.enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unlocked++
	return nil
}

func (s *fakeSession) Alive(context.Context) error {
	defer s.enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes++
	if s.aliveErr != nil && s.probes > s.aliveAfter {
		return s.aliveErr
	}
	return nil
}

func (s *fakeSession) Close() {
	defer s.enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
}

func (s *fakeSession) counts() (tries, unlocked, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tries, s.unlocked, s.closed
}

// dialing hands out the given sessions in order, reporting err for as many
// attempts as errs says before the first one.
type dialing struct {
	mu       sync.Mutex
	sessions []*fakeSession
	failures int
	err      error
	calls    int
}

func (d *dialing) dial(context.Context) (Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.failures > 0 {
		d.failures--
		return nil, d.err
	}
	if len(d.sessions) == 0 {
		return nil, errors.New("no sessions left to dial")
	}
	s := d.sessions[0]
	d.sessions = d.sessions[1:]
	return s, nil
}

func (d *dialing) dialled() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// fast makes a test election poll and probe at a rate a test can wait on.
// Nothing here sleeps for a poll interval on purpose: the intervals are the
// production defaults' shape, not their duration.
func fast() []Option {
	return []Option{WithPoll(time.Millisecond), WithProbe(time.Millisecond)}
}

func TestCampaignTakesAFreeLockAndHoldsTheConnectionItTookItOn(t *testing.T) {
	sess := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, fast()...).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}

	tries, _, closed := sess.counts()
	if tries != 1 {
		t.Errorf("try attempts = %d, want 1", tries)
	}
	// The connection the lock was taken on must still be held: a
	// session-scoped advisory lock belongs to one connection, and handing
	// that connection back to the pool releases the lock while the caller
	// still believes it leads.
	if closed != 0 {
		t.Errorf("the connection holding the lock was released %d times", closed)
	}
	select {
	case <-term.Lost():
		t.Error("leadership reported lost while the connection is healthy")
	default:
	}
}

func TestCampaignPollsWhileAnotherProcessHoldsTheLock(t *testing.T) {
	// Two refusals, then the holder goes away. Refusals rather than a
	// blocking lock call: a standby has to stay cancellable, and
	// pg_advisory_lock would sit in the database where no context can reach
	// it.
	sess := &fakeSession{heldElsewhere: 2}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, fast()...).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	defer func() { _ = term.Release(t.Context()) }()

	tries, _, closed := sess.counts()
	if tries != 3 {
		t.Errorf("try attempts = %d, want 3 (two refusals then the lock)", tries)
	}
	if closed != 0 {
		t.Errorf("the polling connection was released %d times while still wanted", closed)
	}
	if d.dialled() != 1 {
		t.Errorf("dials = %d, want 1: a refusal leaves the connection usable", d.dialled())
	}
}

func TestCampaignStopsWhenTheProcessIsShuttingDown(t *testing.T) {
	// Held forever: this standby never becomes the leader, which is exactly
	// the pod SIGTERM arrives at.
	sess := &fakeSession{heldElsewhere: 1 << 30}
	d := &dialing{sessions: []*fakeSession{sess}}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		_, err := New(d.dial, fast()...).Campaign(ctx)
		done <- err
	}()
	// Cancelled only once the standby is genuinely waiting on a connection
	// it holds, which is the state this asserts it gives up.
	waitFor(t, func() bool {
		tries, _, _ := sess.counts()
		return tries > 0
	}, "the standby never tried for the lock")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Campaign error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Campaign did not return when the process was cancelled")
	}

	waitFor(t, func() bool {
		_, _, closed := sess.counts()
		return closed == 1
	}, "the standby's connection was never handed back")
}

func TestADialFailureIsRetriedRatherThanEndingTheCampaign(t *testing.T) {
	// A database that is down when the pod starts is a wait, not a crash:
	// the old agent is still playing, and this one has nothing to do but
	// keep asking.
	sess := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{sess}, failures: 2, err: errors.New("pool exhausted")}

	if _, err := New(d.dial, fast()...).Campaign(t.Context()); err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	if d.dialled() != 3 {
		t.Errorf("dials = %d, want 3 (two failures then a connection)", d.dialled())
	}
}

func TestAFailedLockAttemptDiscardsTheConnectionItFailedOn(t *testing.T) {
	// An error from the lock statement says nothing reliable about the
	// connection's state. Reusing it risks polling forever on a connection
	// the server has already given up on, so it goes back and the next
	// attempt starts on a fresh one.
	broken := &fakeSession{tryErr: errors.New("connection reset")}
	good := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{broken, good}}

	if _, err := New(d.dial, fast()...).Campaign(t.Context()); err != nil {
		t.Fatalf("Campaign: %v", err)
	}

	if _, _, closed := broken.counts(); closed != 1 {
		t.Errorf("the failed connection was released %d times, want 1", closed)
	}
	if d.dialled() != 2 {
		t.Errorf("dials = %d, want 2: the failed connection must not be reused", d.dialled())
	}
}

func TestLeadershipIsLostWhenTheHeldConnectionStopsAnswering(t *testing.T) {
	// The whole reason the probe exists. A session-scoped lock dies with its
	// connection, so a connection that has quietly gone away means another
	// process can already be holding this lock -- and an agent that kept
	// playing on that assumption is the two-agents-one-account failure this
	// package prevents.
	sess := &fakeSession{aliveErr: errors.New("connection closed"), aliveAfter: 1}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, fast()...).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}

	select {
	case <-term.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("leadership was never reported lost after the connection died")
	}
	waitFor(t, func() bool {
		_, _, closed := sess.counts()
		return closed == 1
	}, "the dead connection was never handed back")
}

func TestReleasingALostTermDoesNotUnlockOnADeadConnection(t *testing.T) {
	sess := &fakeSession{aliveErr: errors.New("connection closed")}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, fast()...).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	<-term.Lost()
	if err := term.Release(t.Context()); err != nil {
		t.Errorf("Release of a lost term: %v", err)
	}

	if _, unlocked, _ := sess.counts(); unlocked != 0 {
		t.Errorf("unlock attempts = %d on a connection that is gone, want 0", unlocked)
	}
}

func TestReleaseUnlocksOnceAndThenReportsNothingHeld(t *testing.T) {
	sess := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, fast()...).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	if err := term.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Idempotent because the shutdown path and the lost-lock path both end
	// here, and a release that panicked or double-unlocked on the second
	// call would turn one failure into two.
	if err := term.Release(t.Context()); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	_, unlocked, closed := sess.counts()
	if unlocked != 1 {
		t.Errorf("unlocks = %d, want 1", unlocked)
	}
	if closed != 1 {
		t.Errorf("connection releases = %d, want 1", closed)
	}
	select {
	case <-term.Lost():
	default:
		t.Error("a released term still reports itself held")
	}
}

func TestTheHeldConnectionIsNeverUsedFromTwoGoroutinesAtOnce(t *testing.T) {
	sess := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, WithPoll(time.Millisecond), WithProbe(time.Microsecond)).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	// Long enough for the probe to be mid-flight when Release lands, which
	// is the collision a live pod hits on every SIGTERM.
	time.Sleep(20 * time.Millisecond)
	if err := term.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if sess.concurrent.Load() {
		t.Error("the liveness probe and the release used the same connection at once")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Error(msg)
}

func TestAPermanentlyHeldLockIsGivenUpOnWithinTheBound(t *testing.T) {
	// A pod that died without closing its socket -- SIGKILL, an OOM kill, a
	// node losing power -- leaves its backend and its lock behind until TCP
	// keepalive reaps it, which on this cluster's inherited defaults is over
	// two hours. Waiting that out would turn a hard kill into an outage far
	// longer than the one this package exists to shorten, so the wait is
	// bounded and the agent goes live without the lock at the end of it.
	sess := &fakeSession{heldElsewhere: 1 << 30}
	spare := &fakeSession{heldElsewhere: 1 << 30}
	d := &dialing{sessions: []*fakeSession{sess, spare}}
	bound := 50 * time.Millisecond

	started := time.Now()
	term, err := New(d.dial, WithPoll(time.Millisecond), WithMaxWait(bound)).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	defer func() { _ = term.Release(t.Context()) }()

	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the campaign waited %v before going live, want about %v", took, bound)
	}
	if term.Held() {
		t.Error("the term reports it holds a lock another connection is holding")
	}
	// Live, and not pretending otherwise: nothing has been lost, because
	// nothing was ever held.
	select {
	case <-term.Lost():
		t.Error("a term that never held the lock reports it lost one")
	default:
	}
}

func TestTheBoundIsNotReachedWhenTheLockFreesNormally(t *testing.T) {
	// An ordinary release: the departing agent gives the lock up as it leaves,
	// and the standby has it on its next poll. The bound exists for the pod
	// that never gets to release anything, and a handover must never touch it.
	sess := &fakeSession{heldElsewhere: 5}
	d := &dialing{sessions: []*fakeSession{sess}}

	term, err := New(d.dial, WithPoll(time.Millisecond), WithMaxWait(10*time.Second)).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	defer func() { _ = term.Release(t.Context()) }()

	if !term.Held() {
		t.Error("the standby went live without the lock although the lock came free")
	}
	if tries, _, _ := sess.counts(); tries != 6 {
		t.Errorf("try attempts = %d, want 6 (five refusals then the lock)", tries)
	}
}

func TestATermThatWentLiveUnlockedAdoptsTheLockWhenItFrees(t *testing.T) {
	// Otherwise the process spends the rest of its life unable to notice a
	// genuine conflict: with no lock held there is no connection to watch, so
	// nothing would tell it that another agent had taken over.
	sess := &fakeSession{heldElsewhere: 1 << 30}
	// The lock is free by the time the background pursuit dials again.
	later := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{sess, later}}

	term, err := New(d.dial, WithPoll(time.Millisecond), WithMaxWait(20*time.Millisecond), WithProbe(time.Millisecond)).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	defer func() { _ = term.Release(t.Context()) }()

	select {
	case <-term.Adopted():
	case <-time.After(2 * time.Second):
		t.Fatal("the lock came free and the term never adopted it")
	}
	if !term.Held() {
		t.Error("the term adopted the lock and still reports it does not hold one")
	}

	// And the guarantee is back: the connection carrying the adopted lock is
	// watched like any other, so a conflict is noticed again.
	later.mu.Lock()
	later.aliveErr = errors.New("connection closed")
	later.mu.Unlock()
	select {
	case <-term.Lost():
	case <-time.After(2 * time.Second):
		t.Fatal("the adopted lock is not being watched")
	}
}

func TestReleasingAnUnlockedTermStopsItChasingTheLock(t *testing.T) {
	// A released term must not end up holding a lock: nothing is watching its
	// connection any more, and a lock held by a process that has stood down is
	// a lock no successor can ever take.
	sess := &fakeSession{heldElsewhere: 1 << 30}
	later := &fakeSession{}
	d := &dialing{sessions: []*fakeSession{sess, later}}

	term, err := New(d.dial, WithPoll(time.Millisecond), WithMaxWait(time.Millisecond)).Campaign(t.Context())
	if err != nil {
		t.Fatalf("Campaign: %v", err)
	}
	if err := term.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}

	select {
	case <-term.Adopted():
		t.Error("a released term reported adopting the lock")
	default:
	}
	if term.Held() {
		t.Error("a released term reports it holds the lock")
	}
	// Whether the background pursuit got as far as taking the lock before the
	// release landed is a race, and both outcomes are acceptable. What is not
	// acceptable is taking it and keeping it.
	waitFor(t, func() bool {
		tries, unlocked, closed := later.counts()
		return tries == 0 || (unlocked == 1 && closed == 1)
	}, "the lock was left taken on a connection the released term abandoned")
}
