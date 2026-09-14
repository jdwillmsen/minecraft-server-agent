package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// fakeTerm is the leadership a live agent holds, with the two facts the
// agent's own code reads off it: whether it still holds, and what happens
// when it gives it up.
type fakeTerm struct {
	mu       sync.Mutex
	lost     chan struct{}
	adopted  chan struct{}
	held     bool
	released int
	// order is shared with the store below, so a test can say what happened
	// before what. The ordering is the whole point of the handover: anything
	// recorded after the lock goes is recorded while a successor is joining.
	order *steps
	err   error
}

func newFakeTerm(order *steps) *fakeTerm {
	// Holding the lock is the ordinary case, so Adopted is already closed --
	// see leader.Term.
	adopted := make(chan struct{})
	close(adopted)
	return &fakeTerm{lost: make(chan struct{}), adopted: adopted, held: true, order: order}
}

// newUnlockedFakeTerm is the term a process leads under when the bounded wait
// for the lock expired.
func newUnlockedFakeTerm(order *steps) *fakeTerm {
	return &fakeTerm{lost: make(chan struct{}), adopted: make(chan struct{}), order: order}
}

func (f *fakeTerm) Lost() <-chan struct{}    { return f.lost }
func (f *fakeTerm) Adopted() <-chan struct{} { return f.adopted }
func (f *fakeTerm) Held() bool               { return f.held }

func (f *fakeTerm) Release(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	f.order.add("lock_released")
	return f.err
}

func (f *fakeTerm) releases() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

// steps records what the handover did, in order.
type steps struct {
	mu    sync.Mutex
	taken []string
}

func (s *steps) add(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.taken = append(s.taken, step)
}

func (s *steps) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.taken...)
}

// handingOverStore is a profile store that records the handover close and
// what it was told.
type handingOverStore struct {
	store.Nop
	order *steps

	mu    sync.Mutex
	calls int
	since time.Time
	at    time.Time
	err   error
}

func (h *handingOverStore) CloseForHandover(_ context.Context, since, at time.Time) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	h.since, h.at = since, at
	h.order.add("sessions_closed")
	if h.err != nil {
		return 0, h.err
	}
	return 2, nil
}

func (h *handingOverStore) Enabled() bool { return true }

func (h *handingOverStore) closed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// fakeElection hands out one term, when a test says the lock came free.
type fakeElection struct {
	free chan struct{}
	term leadership
	err  error
}

func (f *fakeElection) Campaign(ctx context.Context) (leadership, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.free:
		return f.term, f.err
	}
}

// roles records every role the agent reported, which is what /readyz serves
// and what Kubernetes decides with.
type roles struct {
	mu   sync.Mutex
	seen []httpapi.Role
}

func (r *roles) set(role httpapi.Role) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, role)
}

func (r *roles) last() (httpapi.Role, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return 0, false
	}
	return r.seen[len(r.seen)-1], true
}

func quiet() *logging.Logger { return logging.New("error") }

func TestAStandbyReportsItselfStandbyUntilItHoldsTheLock(t *testing.T) {
	// The pod is up, warm and deliberately out of the game. Every startup
	// cost is already paid; the only thing it is waiting for is the lock the
	// agent currently playing still holds.
	order := &steps{}
	election := &fakeElection{free: make(chan struct{}), term: newFakeTerm(order)}
	reported := &roles{}

	got := make(chan leadership, 1)
	go func() {
		term, _ := awaitLeadership(t.Context(), election, reported.set, quiet())
		got <- term
	}()

	waitUntil(t, func() bool {
		role, ok := reported.last()
		return ok && role == httpapi.RoleStandby
	}, "the waiting pod never reported itself a standby")

	close(election.free)
	select {
	case term := <-got:
		if term == nil {
			t.Fatal("no term returned after the lock came free")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitLeadership did not return after the lock came free")
	}

	// Live only now, which is the claim the whole design rests on: nothing
	// joins the game before this line.
	if role, _ := reported.last(); role != httpapi.RoleLive {
		t.Errorf("role after taking the lock = %v, want live", role)
	}
}

func TestWithNoDatabaseThereIsNoLockToWaitFor(t *testing.T) {
	// An unset PG_HOST is a supported deployment, not a degraded one. There
	// is no pool to take a lock on, so the agent is the live one by being the
	// only one -- and must not wait for a lock nothing will ever release.
	reported := &roles{}

	term, ok := awaitLeadership(t.Context(), nil, reported.set, quiet())
	if !ok {
		t.Fatal("awaitLeadership refused to go live without a database")
	}
	if term != nil {
		t.Error("a term was returned with no lock to hold")
	}
	if role, _ := reported.last(); role != httpapi.RoleLive {
		t.Errorf("role = %v, want live", role)
	}
}

func TestAStandbyThatIsShutDownNeverGoesLive(t *testing.T) {
	election := &fakeElection{free: make(chan struct{})}
	reported := &roles{}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan bool, 1)
	go func() {
		_, ok := awaitLeadership(ctx, election, reported.set, quiet())
		done <- ok
	}()
	waitUntil(t, func() bool {
		role, ok := reported.last()
		return ok && role == httpapi.RoleStandby
	}, "the pod never reported itself a standby")
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Error("a cancelled standby reported itself ready to lead")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitLeadership ignored the shutdown")
	}
	if role, _ := reported.last(); role != httpapi.RoleStandby {
		t.Errorf("role = %v, want standby: a pod that never led must not claim it did", role)
	}
}

func TestHandoverClosesWatchedSessionsBeforeGivingUpTheLock(t *testing.T) {
	// Ordering is the requirement, not the two calls. A successor joins the
	// instant it sees the lock free, so a session closed after that is
	// written while two agents are live -- and the playtime it credits is
	// time the other agent is already re-recording.
	order := &steps{}
	term := newFakeTerm(order)
	profiles := &handingOverStore{order: order}
	since := time.Now().Add(-time.Hour)

	handover(t.Context(), term, profiles, since, quiet())

	if got := order.list(); len(got) != 2 || got[0] != "sessions_closed" || got[1] != "lock_released" {
		t.Errorf("handover did %v, want the sessions closed before the lock was released", got)
	}
	if profiles.since != since {
		t.Errorf("sessions closed against since = %v, want the connection's own start %v", profiles.since, since)
	}
}

func TestHandoverRunsEvenThoughShutdownHasAlreadyBeenSignalled(t *testing.T) {
	// The path this exists for is SIGTERM, which has cancelled the process
	// context before any of this runs. Inheriting that context would mean
	// every handover silently doing nothing, which is the failure mode it was
	// written to fix.
	order := &steps{}
	term := newFakeTerm(order)
	profiles := &handingOverStore{order: order}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handover(ctx, term, profiles, time.Now(), quiet())

	if profiles.closed() != 1 {
		t.Errorf("sessions closed %d times on a cancelled context, want 1", profiles.closed())
	}
	if term.releases() != 1 {
		t.Errorf("lock released %d times on a cancelled context, want 1", term.releases())
	}
}

func TestTheLockIsReleasedEvenWhenClosingSessionsFails(t *testing.T) {
	// A database that refuses the close is a player's playtime lost. Holding
	// the lock over it would cost the server its agent entirely, which is
	// worse by an order of magnitude.
	order := &steps{}
	term := newFakeTerm(order)
	profiles := &handingOverStore{order: order, err: errors.New("deadlock detected")}

	handover(t.Context(), term, profiles, time.Now(), quiet())

	if term.releases() != 1 {
		t.Errorf("lock released %d times after a failed close, want 1", term.releases())
	}
}

func TestHandoverWithoutALockStillClosesSessions(t *testing.T) {
	// No database means no lock, but a restart still ends sessions the agent
	// was watching -- and with no database there is nothing to close them
	// either, so this only has to not panic on the nil term.
	order := &steps{}
	profiles := &handingOverStore{order: order}

	handover(t.Context(), nil, profiles, time.Now(), quiet())

	if profiles.closed() != 1 {
		t.Errorf("sessions closed %d times, want 1", profiles.closed())
	}
}

func TestLosingTheLockEndsTheTermRatherThanLettingTheAgentPlayOn(t *testing.T) {
	// The lock is what entitles this process to the login. Once it is gone
	// another process may already have both, so this one has to leave the
	// game rather than assume it is still alone in it.
	term := newFakeTerm(&steps{})
	ended := make(chan struct{})

	go endTermOnLockLoss(t.Context(), term, func() { close(ended) }, quiet())
	close(term.lost)

	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("losing the lock did not end the term")
	}
}

func TestAHealthyTermIsNotEndedWhenTheProcessStopsWatchingIt(t *testing.T) {
	term := newFakeTerm(&steps{})
	ctx, cancel := context.WithCancel(t.Context())
	var ended int32
	done := make(chan struct{})

	go func() {
		endTermOnLockLoss(ctx, term, func() { ended++ }, quiet())
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher outlived the term it was watching")
	}
	if ended != 0 {
		t.Error("the term was ended by its own shutdown rather than by losing the lock")
	}
}

// closeOnly is a Bedrock connection as the leave path uses it.
type closeOnly struct {
	mu     sync.Mutex
	closed int
}

func (c *closeOnly) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}

func (c *closeOnly) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func TestLeavingForAReconnectDoesNotWaitForTheDisconnect(t *testing.T) {
	// The server has already dropped this session, or is about to accept
	// another one from the same process. Nothing is waiting on the login, so
	// nothing should wait here either.
	conn := &closeOnly{}
	grace := 2 * time.Second

	started := time.Now()
	leaveGame(t.Context(), conn, grace, quiet())

	if took := time.Since(started); took >= grace {
		t.Errorf("an ordinary reconnect waited %v for the disconnect", took)
	}
	if conn.closes() != 1 {
		t.Errorf("connection closed %d times, want 1", conn.closes())
	}
}

func TestLeavingOnShutdownWaitsForTheServerToBeToldAboutIt(t *testing.T) {
	// Closing the socket is not leaving the game: the disconnect notice goes
	// out from the RakNet layer's own tick after the close, and a process
	// that exits first leaves the server holding the login until it times the
	// session out -- the ten seconds a release used to cost.
	conn := &closeOnly{}
	grace := 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	leaveGame(ctx, conn, grace, quiet())

	if took := time.Since(started); took < grace {
		t.Errorf("the handover waited %v, want at least the %v the disconnect needs", took, grace)
	}
	if conn.closes() != 1 {
		t.Errorf("connection closed %d times, want 1", conn.closes())
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}

// tokenSource is an Xbox Live token source as the warm-up uses it.
type tokenSource struct {
	calls int
	err   error
}

func (t *tokenSource) Token() (*oauth2.Token, error) {
	t.calls++
	if t.err != nil {
		return nil, t.err
	}
	return &oauth2.Token{AccessToken: "live-token"}, nil
}

// The refresh is a round trip to Microsoft, and the one startup cost that
// would otherwise land between taking the lock and joining the game -- the
// single interval this whole design exists to keep short.
func TestTheXboxTokenIsRefreshedBeforeTheAgentIsNeeded(t *testing.T) {
	ts := &tokenSource{}

	warmXboxToken(ts, quiet())

	if ts.calls != 1 {
		t.Errorf("token refreshed %d times during startup, want 1", ts.calls)
	}
}

func TestAFailedTokenRefreshIsNotFatal(t *testing.T) {
	// The connect loop makes the same call through the dialer and already
	// knows how to back off from an account-level rejection. Exiting here
	// would turn that into a crash loop, and a transient failure into an
	// outage.
	ts := &tokenSource{err: errors.New("invalid_grant")}

	warmXboxToken(ts, quiet())
}

// An agent that is live without the lock has given up the one guarantee the
// lock provides, and it cannot be seen in mc_agent_leader: the pod genuinely
// is the live agent and reports 1 there like any other. A separate series is
// what lets an alert say "live, but unprotected".
func TestLeadingWithoutTheLockIsVisibleToAnAlert(t *testing.T) {
	term := newUnlockedFakeTerm(&steps{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		watchForcedLeadership(ctx, term, quiet())
		close(done)
	}()

	waitUntil(t, func() bool {
		return metricstest.Value(t, "mc_agent_leader_unlocked") == 1
	}, "leading without the lock was never reported")

	// And it clears the moment the lock is adopted, because the claim it makes
	// has stopped being true: from here on a conflict is noticed again.
	close(term.adopted)
	waitUntil(t, func() bool {
		return metricstest.Value(t, "mc_agent_leader_unlocked") == 0
	}, "adopting the lock did not clear the unlocked report")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watcher outlived the condition it was watching")
	}
}

func TestAnAgentHoldingTheLockIsNotReportedUnlocked(t *testing.T) {
	term := newFakeTerm(&steps{})

	if got := metricstest.Delta(t, func() {
		watchForcedLeadership(t.Context(), term, quiet())
	}, "mc_agent_leader_unlocked"); got != 0 {
		t.Errorf("mc_agent_leader_unlocked moved by %v for an agent that holds the lock, want 0", got)
	}
}

// The end of a turn ends the claim too: the process is a standby again, or
// gone, and either way it is no longer an unprotected live agent.
func TestTheUnlockedReportClearsWhenTheTurnEnds(t *testing.T) {
	term := newUnlockedFakeTerm(&steps{})
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		watchForcedLeadership(ctx, term, quiet())
		close(done)
	}()
	waitUntil(t, func() bool {
		return metricstest.Value(t, "mc_agent_leader_unlocked") == 1
	}, "leading without the lock was never reported")

	cancel()
	<-done
	if got := metricstest.Value(t, "mc_agent_leader_unlocked"); got != 0 {
		t.Errorf("mc_agent_leader_unlocked = %v after the turn ended, want 0", got)
	}
}

// A lost lock is a lock a successor may already hold, and it refreshes from
// the same row this process would. Microsoft retires a refresh token as it
// issues the replacement, so the claim has to be gone before anything else
// reacts to the turn ending -- not after the connect loop has finished
// draining, which is minutes of a disconnect the successor does not wait for.
func TestLosingTheLockEndsTheClaimOnTheLoginBeforeTheTurnUnwinds(t *testing.T) {
	var gate tokenLiveGate
	term := newFakeTerm(&steps{})

	liveCtx, endTurn := beginTurn(t.Context(), &gate)
	if !gate.isOpen() {
		t.Fatal("the live agent may not refresh the token it is playing on")
	}
	go endTermOnLockLoss(liveCtx, term, endTurn, quiet())

	close(term.lost)

	<-liveCtx.Done()
	// Ordered, not merely eventual: liveCtx is what every live-only worker
	// watches, so anything still open here is open while a successor plays.
	if gate.isOpen() {
		t.Error("the turn ended with this process still entitled to rotate the stored token")
	}
}

func TestAGracefulTurnEndAlsoEndsTheClaim(t *testing.T) {
	var gate tokenLiveGate

	_, endTurn := beginTurn(t.Context(), &gate)
	endTurn()

	if gate.isOpen() {
		t.Error("a process that handed over is still entitled to rotate the stored token")
	}
}

// Each turn is a fresh claim: a process that lost the lock becomes a standby
// and campaigns again, and taking the lock back is what re-entitles it.
func TestTheClaimIsReopenedByTheNextTurn(t *testing.T) {
	var gate tokenLiveGate

	_, endTurn := beginTurn(t.Context(), &gate)
	endTurn()
	_, endSecond := beginTurn(t.Context(), &gate)
	defer endSecond()

	if !gate.isOpen() {
		t.Error("winning the lock back did not restore the right to refresh")
	}
}
