//go:build livedb

// Exercises the real advisory-lock SQL, and the hazard the dedicated
// connection exists to avoid, against a real PostgreSQL.
//
// The unit tests above describe the election's sequencing against a fake
// session. What they cannot describe is the thing that makes this design
// subtle: an advisory lock belongs to the connection that took it, and a
// pooled connection is not the caller's to keep. That is only true or false
// against a real server, so it is asserted here.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/leader/
package leader

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// livePool builds a pool of its own per test, because two of these tests
// destroy connections and a shared one would be destroying the other test's
// lock out from under it.
//
// Every pool names itself, and label must be unique to the test using it: one
// of these tests terminates the connection carrying a lock, and the only
// honest way to aim that at its own connection -- in a database this repo's
// other live tests are running against at the same time -- is to ask
// PostgreSQL for the backend that said it was this pool.
func livePool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("MC_TEST_DSN")
	if dsn == "" {
		t.Skip("MC_TEST_DSN unset")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName(label)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// appName is short on purpose: PostgreSQL truncates application_name past 63
// bytes, and a truncated name would make the terminate below miss silently.
func appName(label string) string {
	return "leader-livedb-" + label
}

// account keys the lock, so each test contends only with itself however many
// times the suite is run against one database.
func account(t *testing.T) string {
	t.Helper()
	return "agent-" + t.Name()
}

// campaignWithin is Campaign with a deadline, for the cases that assert the
// lock is takeable now.
func campaignWithin(t *testing.T, e *Election, d time.Duration) (*Term, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	return e.Campaign(ctx)
}

func TestOnlyOneElectionHoldsTheLockAtATime(t *testing.T) {
	pool := livePool(t, "one-at-a-time")
	acct := account(t)
	poll := 20 * time.Millisecond

	first, err := campaignWithin(t, New(PoolDial(pool, acct), WithPoll(poll)), 5*time.Second)
	if err != nil {
		t.Fatalf("first campaign: %v", err)
	}

	// The second process is a warm standby: started, connected, and
	// deliberately not in the game.
	standby := New(PoolDial(pool, acct), WithPoll(poll))
	if _, err := campaignWithin(t, standby, 300*time.Millisecond); err == nil {
		t.Fatal("a second election took a lock the first one holds")
	}

	if err := first.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := campaignWithin(t, standby, 5*time.Second)
	if err != nil {
		t.Fatalf("the standby never took the released lock: %v", err)
	}
	if err := second.Release(t.Context()); err != nil {
		t.Fatalf("release second: %v", err)
	}
}

func TestTwoAccountsDoNotWaitOnEachOther(t *testing.T) {
	// The lock keys on the Xbox Live account because that is the resource
	// being contended. Two agents for two accounts can both play.
	pool := livePool(t, "two-accounts")

	one, err := campaignWithin(t, New(PoolDial(pool, account(t)+"-a")), 5*time.Second)
	if err != nil {
		t.Fatalf("first account: %v", err)
	}
	defer func() { _ = one.Release(t.Context()) }()

	two, err := campaignWithin(t, New(PoolDial(pool, account(t)+"-b")), 2*time.Second)
	if err != nil {
		t.Fatalf("second account had to wait on the first: %v", err)
	}
	defer func() { _ = two.Release(t.Context()) }()
}

func TestALockTakenOnAPooledConnectionIsSilentlyLost(t *testing.T) {
	// This is the bug the package's dedicated connection exists to avoid,
	// written down as a test so nobody "simplifies" PoolDial into a
	// pool.Exec later. The lock is taken through the pool, so the connection
	// carrying it goes back to the pool afterwards; when the pool retires
	// that connection -- on its idle timeout, its lifetime, or a reset -- the
	// lock goes with it, and nothing tells the holder.
	holder := livePool(t, "pooled-lock-holder")
	contender := livePool(t, "pooled-lock-contender")
	acct := account(t)

	var held bool
	if err := holder.QueryRow(t.Context(), tryLockSQL, acct).Scan(&held); err != nil {
		t.Fatalf("take lock through the pool: %v", err)
	}
	if !held {
		t.Fatal("the lock was not free at the start of the test")
	}

	// Genuinely held, for now: a separate connection cannot take it.
	e := New(PoolDial(contender, acct), WithPoll(20*time.Millisecond))
	if _, err := campaignWithin(t, e, 300*time.Millisecond); err == nil {
		t.Fatal("the lock was never held at all")
	}

	// The pool retires the connection. Nothing above learns anything.
	holder.Reset()

	term, err := campaignWithin(t, e, 5*time.Second)
	if err != nil {
		t.Fatalf("expected the pooled lock to have been lost with its connection, got: %v", err)
	}
	if err := term.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestAHeldTermSurvivesThePoolRetiringEveryOtherConnection(t *testing.T) {
	// The same event as the test above, against the design that handles it:
	// the connection carrying the lock is checked out, so the pool cannot
	// retire it, and the term outlives a reset that would have taken a
	// pooled lock with it.
	leading := livePool(t, "surviving-leader")
	contender := livePool(t, "surviving-contender")
	acct := account(t)

	term, err := campaignWithin(t, New(PoolDial(leading, acct), WithProbe(50*time.Millisecond)), 5*time.Second)
	if err != nil {
		t.Fatalf("campaign: %v", err)
	}

	// Ordinary work through the same pool, of the kind the agent does all
	// day, plus the reset that destroys what it leaves idle.
	for range 10 {
		var one int
		if err := leading.QueryRow(t.Context(), `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("pool work: %v", err)
		}
	}
	leading.Reset()

	if _, err := campaignWithin(t, New(PoolDial(contender, acct), WithPoll(20*time.Millisecond)), 500*time.Millisecond); err == nil {
		t.Fatal("another process took the lock while this term still held it")
	}
	select {
	case <-term.Lost():
		t.Fatal("the term reported itself lost while its connection was healthy")
	default:
	}

	if err := term.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestLeadershipEndsWhenTheServerTerminatesTheConnection(t *testing.T) {
	// What a database failover or an administrative disconnect looks like
	// from here. The lock died with the backend, so the term must stop
	// claiming it rather than leave an agent playing on a claim it no longer
	// has.
	pool := livePool(t, "terminated-leader")
	admin := livePool(t, "terminated-admin")
	acct := account(t)

	term, err := campaignWithin(t, New(PoolDial(pool, acct), WithProbe(50*time.Millisecond)), 5*time.Second)
	if err != nil {
		t.Fatalf("campaign: %v", err)
	}

	// Aimed by application_name at this test's own leading pool: the other
	// live suites in this repo run against the same database, and killing
	// every backend would fail their tests instead of this one's.
	if _, err := admin.Exec(t.Context(), `
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = $1`,
		appName("terminated-leader"),
	); err != nil {
		t.Fatalf("terminate backends: %v", err)
	}

	select {
	case <-term.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("the term never noticed its connection had been terminated")
	}

	// And the lock really is free, so a standby elsewhere can take over.
	taken, err := campaignWithin(t, New(PoolDial(admin, acct), WithPoll(20*time.Millisecond)), 5*time.Second)
	if err != nil {
		t.Fatalf("the lock was not released by the terminated connection: %v", err)
	}
	if err := taken.Release(t.Context()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := term.Release(t.Context()); err != nil {
		t.Errorf("releasing a lost term: %v", err)
	}
}

func TestAnAbandonedLockIsOutwaitedAndThenAdopted(t *testing.T) {
	// The failure the bound exists for, against a real server: a lock held by
	// a connection that never releases it. In production that connection
	// belongs to a pod that died without closing its socket, and PostgreSQL
	// keeps it -- and the lock -- until TCP keepalive reaps the backend, which
	// on this cluster's inherited defaults is over two hours away.
	abandoned := livePool(t, "abandoned-holder")
	taker := livePool(t, "abandoned-taker")
	acct := account(t)

	dead, err := abandoned.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var held bool
	if err := dead.QueryRow(t.Context(), tryLockSQL, acct).Scan(&held); err != nil {
		t.Fatalf("take lock: %v", err)
	}
	if !held {
		t.Fatal("the lock was not free at the start of the test")
	}

	term, err := campaignWithin(t,
		New(PoolDial(taker, acct),
			WithPoll(20*time.Millisecond),
			WithMaxWait(100*time.Millisecond),
			WithProbe(50*time.Millisecond)),
		5*time.Second)
	if err != nil {
		t.Fatalf("the campaign never gave up on an abandoned lock: %v", err)
	}
	defer func() { _ = term.Release(t.Context()) }()
	if term.Held() {
		t.Fatal("the term claims a lock the abandoned connection is still holding")
	}

	// Whenever that connection does finally go -- keepalive, a failover, an
	// operator -- the live agent takes the lock and is protected again.
	if _, err := dead.Exec(t.Context(), `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("release the abandoned lock: %v", err)
	}
	dead.Release()

	select {
	case <-term.Adopted():
	case <-time.After(5 * time.Second):
		t.Fatal("the lock came free and the unlocked agent never adopted it")
	}
	if !term.Held() {
		t.Error("the term adopted the lock and still reports it holds none")
	}
}

func TestAStandbyWaitsOutTheBoundWhileTheHolderAnnouncesItself(t *testing.T) {
	// The failure the heartbeat exists for, against a real server. Two pods
	// coexisting for longer than the bound -- a rolling update, a paused
	// rollout, an old pod hanging past its termination grace -- used to end
	// with the standby forcing itself live and kicking an agent that was
	// perfectly healthy, which its connect loop answers by kicking back. Here
	// the standby hears the holder and stays out of the game instead, however
	// long the bound says it has waited.
	leading := livePool(t, "beating-holder")
	standby := livePool(t, "beating-standby")
	acct := account(t)
	beat := 25 * time.Millisecond

	held, err := campaignWithin(t,
		New(PoolDial(leading, acct), WithHeartbeat(beat), WithProbe(50*time.Millisecond)),
		5*time.Second)
	if err != nil {
		t.Fatalf("the holder never took a free lock: %v", err)
	}
	if !held.Held() {
		t.Fatal("the holder does not hold the lock it was given")
	}

	// A bound of 100ms against a staleness window of 75ms: a standby acting on
	// the clock alone would be live almost at once.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	waiting := New(PoolDial(standby, acct),
		WithPoll(20*time.Millisecond),
		WithMaxWait(100*time.Millisecond),
		WithHeartbeat(beat))
	done := make(chan *Term, 1)
	go func() {
		term, err := waiting.Campaign(ctx)
		if err != nil {
			t.Errorf("standby campaign: %v", err)
		}
		done <- term
	}()

	select {
	case <-done:
		t.Fatal("the standby went live while the holder was still announcing itself")
	case <-time.After(2 * time.Second):
	}

	// An ordinary handover from here, which also says something the unit tests
	// cannot: that connection has had its read time out on every one of those
	// hundred polls and is still the connection that takes the lock.
	if err := held.Release(t.Context()); err != nil {
		t.Fatalf("release the held lock: %v", err)
	}
	select {
	case term := <-done:
		defer func() { _ = term.Release(t.Context()) }()
		if !term.Held() {
			t.Error("the standby went live without the lock although the holder released it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the standby never took the lock the holder gave up")
	}
}

func TestAHolderThatStopsAnnouncingItselfIsOutwaitedAndTakenOver(t *testing.T) {
	// The other half of the same mechanism: waiting on a live holder must not
	// become waiting forever. This lock is held by a connection that announces
	// itself and then stops, which is what a pod killed between two
	// announcements leaves behind -- the backend, and the lock, still there for
	// as long as TCP keepalive takes to notice.
	holding := livePool(t, "stopped-holder")
	standby := livePool(t, "stopped-standby")
	acct := account(t)
	beat := 25 * time.Millisecond
	announceFor := 500 * time.Millisecond

	dead, err := holding.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer dead.Release()
	var taken bool
	if err := dead.QueryRow(t.Context(), tryLockSQL, acct).Scan(&taken); err != nil {
		t.Fatalf("take lock: %v", err)
	}
	if !taken {
		t.Fatal("the lock was not free at the start of the test")
	}

	// Joined before the connection is handed back, because it is the same
	// connection: a pgx connection used from two goroutines at once is a data
	// race, and this test would be reporting one of its own making.
	announcing, stopAnnouncing := context.WithTimeout(t.Context(), announceFor)
	announced := make(chan struct{})
	defer func() {
		stopAnnouncing()
		<-announced
	}()
	go func() {
		defer close(announced)
		for announcing.Err() == nil {
			if _, err := dead.Exec(announcing, heartbeatSQL, heartbeatChannel, acct); err != nil && announcing.Err() == nil {
				t.Errorf("announce: %v", err)
			}
			select {
			case <-announcing.Done():
			case <-time.After(beat):
			}
		}
	}()

	started := time.Now()
	term, err := campaignWithin(t,
		New(PoolDial(standby, acct),
			WithPoll(20*time.Millisecond),
			WithMaxWait(100*time.Millisecond),
			WithHeartbeat(beat),
			WithProbe(50*time.Millisecond)),
		30*time.Second)
	if err != nil {
		t.Fatalf("the standby never gave up on a holder that had gone quiet: %v", err)
	}
	defer func() { _ = term.Release(t.Context()) }()

	if took := time.Since(started); took < announceFor {
		t.Errorf("the standby went live after %v, before the holder stopped announcing itself at %v", took, announceFor)
	}
	if term.Held() {
		t.Error("the term claims a lock the quiet connection is still holding")
	}
}
