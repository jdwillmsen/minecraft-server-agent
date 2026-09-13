// Package leader elects the one agent process that may be logged in to the
// game.
//
// The constraint this exists for is outside the agent: one Xbox Live account
// holds one connection, and a second login kicks the first. Two agent
// processes that both dial the server therefore do not share the work, they
// take turns evicting each other -- which is what made a release cost the
// server its agent for half a minute, the old process still holding the
// login while the new one retried into a session the server had not yet let
// go of. So a process that intends to join waits for this lock first, and
// only the holder joins, announces, schedules or moderates.
//
// The lock is a PostgreSQL session-scoped advisory lock rather than a
// Kubernetes Lease. A Lease would need a ServiceAccount, a Role and a
// RoleBinding the agent does not have today, and a lease is held until its
// duration expires: a pod that is SIGKILLed keeps its claim for as long as
// that duration, and shortening the duration buys faster failover at the
// price of a renewal loop that drops leadership on a slow API server. The
// connection pool, on the other hand, is already here, and an advisory lock
// ends when its connection ends -- so the kernel closing a dead pod's socket
// is the release, with no duration to wait out.
//
// The hazard that shape brings is the one this package is built around: a
// session-scoped advisory lock belongs to one connection. Taking it on a
// pooled connection that is later recycled releases it silently, leaving an
// agent that believes it leads while another one takes the lock and the
// login. So an Election takes a dedicated connection out of the pool, takes
// the lock on that connection, and holds it for as long as it leads --
// returning it to the pool is, deliberately, the same act as giving up
// leadership.
package leader

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// DefaultPoll is how often a standby asks whether the lock has come free.
//
// Short because it is the whole length of the handover it is trying to make
// invisible: the old agent releases the lock as it leaves the game, and
// every player is without an agent until a standby notices. Cheap enough to
// be short -- one statement on a connection this process is holding anyway.
const DefaultPoll = 500 * time.Millisecond

// DefaultProbe is how often the holder checks that the connection carrying
// its lock is still there.
//
// This is not a health check for its own sake. The lock lives on that
// connection, so a connection that has gone away has taken the lock with it,
// and the agent is then playing on a claim another process can already have
// taken. The interval bounds how long that can stay unnoticed.
const DefaultProbe = 5 * time.Second

// Session is one dedicated database connection an Election runs on, and the
// lock it holds.
//
// An interface for two reasons. The election's sequencing -- poll, hold,
// probe, release, and what each failure does to the connection -- is worth
// testing without a database. And the production implementation's whole job
// is to never let this connection be shared, which a narrow surface makes
// checkable: everything the election does to the connection is one of these
// four calls.
type Session interface {
	// TryLock takes the lock if it is free, reporting whether it got it. It
	// must not block waiting for the holder: a standby has to remain
	// cancellable, and a statement waiting inside the database is past the
	// point any context can reach it.
	TryLock(ctx context.Context) (bool, error)
	// Unlock releases the lock this session holds. Called only while the
	// connection is known good -- a lost connection has already released it.
	Unlock(ctx context.Context) error
	// Alive reports whether the connection is still usable.
	Alive(ctx context.Context) error
	// Close hands the connection back. Idempotent: the lost-connection path
	// and the release path both end here.
	Close()
}

// Dial opens a Session. Every failure is a retry rather than an error the
// campaign gives up on -- see Campaign.
type Dial func(ctx context.Context) (Session, error)

// Election waits for the agent lock and reports when it is held.
type Election struct {
	dial  Dial
	poll  time.Duration
	probe time.Duration
	log   *logging.Logger
}

// Option configures an Election.
type Option func(*Election)

// WithPoll overrides DefaultPoll.
func WithPoll(d time.Duration) Option {
	return func(e *Election) {
		if d > 0 {
			e.poll = d
		}
	}
}

// WithProbe overrides DefaultProbe.
func WithProbe(d time.Duration) Option {
	return func(e *Election) {
		if d > 0 {
			e.probe = d
		}
	}
}

// WithLogger attaches a logger. Without one the election is silent, which is
// only ever right in a test: in production the standby's wait and the
// holder's loss are the two lines that explain a handover afterwards.
func WithLogger(l *logging.Logger) Option {
	return func(e *Election) { e.log = l }
}

// New builds an Election over dial.
func New(dial Dial, opts ...Option) *Election {
	e := &Election{dial: dial, poll: DefaultPoll, probe: DefaultProbe}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Campaign blocks until this process holds the lock, and returns the Term
// that holds it. The only error it returns is ctx's own: a database that is
// unreachable, a pool with nothing to spare and a lock somebody else holds
// are all the same situation from here -- not yet -- and the standby's job
// in all three is to keep waiting while the live agent carries on playing.
func (e *Election) Campaign(ctx context.Context) (*Term, error) {
	var sess Session
	// Closed on every path out that is not a term: a standby that is
	// shutting down must not leave a connection checked out of a pool its
	// process is about to stop using.
	defer func() {
		if sess != nil {
			sess.Close()
		}
	}()

	waiting := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if sess == nil {
			opened, err := e.dial(ctx)
			if err != nil {
				e.note("leader_connection_unavailable", logging.Fields{"error": err.Error()})
			} else {
				sess = opened
			}
		}

		if sess != nil {
			held, err := sess.TryLock(ctx)
			switch {
			case err != nil:
				// The connection's state is no longer something this can
				// reason about, so it goes back rather than being polled on
				// for the rest of the wait.
				e.note("leader_lock_attempt_failed", logging.Fields{"error": err.Error()})
				sess.Close()
				sess = nil
			case held:
				term := newTerm(sess, e.probe, e.log)
				sess = nil // The term owns the connection now.
				return term, nil
			case !waiting:
				// Once, not per attempt: a handover that takes a minute
				// would otherwise be a hundred identical lines.
				waiting = true
				e.note("leader_standby", logging.Fields{"poll_ms": e.poll.Milliseconds()})
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(e.poll):
		}
	}
}

func (e *Election) note(event string, fields logging.Fields) {
	if e.log != nil {
		e.log.Info(event, fields)
	}
}

// Term is one period of leadership: the dedicated connection, the lock on
// it, and the probe that watches both.
//
// Every use of the connection goes through mu. The probe runs on its own
// goroutine while the caller is free to release at any moment, and a pgx
// connection used from two goroutines at once is a data race, not merely a
// serialisation question.
type Term struct {
	mu   sync.Mutex
	sess Session

	lost   chan struct{}
	closed sync.Once

	stop     chan struct{}
	stopOnce sync.Once

	log *logging.Logger
}

func newTerm(sess Session, probe time.Duration, log *logging.Logger) *Term {
	t := &Term{
		sess: sess,
		lost: make(chan struct{}),
		stop: make(chan struct{}),
		log:  log,
	}
	go t.watch(probe)
	if log != nil {
		log.Info("leader_acquired", nil)
	}
	return t
}

// Lost is closed the moment this process stops holding the lock, whether
// because the connection carrying it died or because Release gave it up.
//
// A caller that is in the game must treat this as its cue to leave: the lock
// is what entitles it to the login, and something else may already hold
// both.
func (t *Term) Lost() <-chan struct{} {
	return t.lost
}

// Release gives up the lock and hands the connection back.
//
// Call it last in a shutdown, after leaving the game and after everything
// the departing agent still owes the database: a successor joins the instant
// it sees the lock free, so work left until after this happens with two
// agents live.
//
// Safe to call on a term that has already been lost or released. A lost term
// unlocks nothing -- the connection that held the lock is gone, and the lock
// with it -- but still hands back what it was holding.
func (t *Term) Release(ctx context.Context) error {
	t.stopOnce.Do(func() { close(t.stop) })

	t.mu.Lock()
	defer t.mu.Unlock()
	sess := t.sess
	t.sess = nil
	if sess == nil {
		return nil
	}

	var err error
	select {
	case <-t.lost:
		// Connection already gone: unlocking on it would only produce a
		// second error describing the first.
	default:
		if err = sess.Unlock(ctx); err != nil {
			err = fmt.Errorf("leader: release lock: %w", err)
		}
	}
	sess.Close()
	t.markLost()
	if t.log != nil {
		t.log.Info("leader_released", nil)
	}
	return err
}

// watch ends the term as soon as the connection carrying the lock stops
// answering.
func (t *Term) watch(probe time.Duration) {
	ticker := time.NewTicker(probe)
	defer ticker.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
		}

		// Bounded by the probe interval itself: a probe that hangs for
		// longer than the gap to the next one is already telling us what a
		// failure would.
		ctx, cancel := context.WithTimeout(context.Background(), probe)
		err := t.check(ctx)
		cancel()
		if err == nil {
			continue
		}
		if t.log != nil {
			t.log.Error("leader_lock_lost", logging.Fields{"error": err.Error()})
		}
		t.drop()
		return
	}
}

// check probes the held connection, or reports nothing to probe once the
// term has been released.
func (t *Term) check(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sess == nil {
		return nil
	}
	return t.sess.Alive(ctx)
}

// drop abandons a connection that has stopped answering. The lock went with
// it, so there is nothing to unlock.
func (t *Term) drop() {
	t.mu.Lock()
	sess := t.sess
	t.sess = nil
	t.mu.Unlock()

	t.markLost()
	if sess != nil {
		sess.Close()
	}
}

func (t *Term) markLost() {
	t.closed.Do(func() { close(t.lost) })
}

// PoolDial takes the dedicated connection this package needs out of an
// existing pool, and locks on the key derived from account.
//
// The key is the account rather than the deployment: the resource two
// processes contend for is one Xbox Live login, so two agents for two
// different accounts are not in conflict and must not wait on each other.
//
// hashtext is how this schema already derives an advisory key (see the
// player lock in internal/store), and the two-argument lock functions are
// used deliberately: PostgreSQL keeps the one-argument and two-argument
// advisory key spaces separate, so this lock cannot collide with a
// per-player key however hashtext hashes.
func PoolDial(pool *pgxpool.Pool, account string) Dial {
	return func(ctx context.Context) (Session, error) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("leader: acquire connection: %w", err)
		}
		return &poolSession{conn: conn, account: account}, nil
	}
}

type poolSession struct {
	conn    *pgxpool.Conn
	account string
	once    sync.Once
}

const (
	tryLockSQL = `SELECT pg_try_advisory_lock(hashtext('minecraft.agent'), hashtext($1))`
	unlockSQL  = `SELECT pg_advisory_unlock(hashtext('minecraft.agent'), hashtext($1))`
)

func (s *poolSession) TryLock(ctx context.Context) (bool, error) {
	var held bool
	if err := s.conn.QueryRow(ctx, tryLockSQL, s.account).Scan(&held); err != nil {
		return false, fmt.Errorf("leader: try lock: %w", err)
	}
	return held, nil
}

func (s *poolSession) Unlock(ctx context.Context) error {
	var released bool
	if err := s.conn.QueryRow(ctx, unlockSQL, s.account).Scan(&released); err != nil {
		return fmt.Errorf("leader: unlock: %w", err)
	}
	if !released {
		// PostgreSQL reports false when this connection did not hold the
		// lock. Worth saying out loud: it means something released it
		// underneath us, and whatever this process did while it believed it
		// led may have overlapped a successor.
		return fmt.Errorf("leader: unlock: lock was not held by this connection")
	}
	return nil
}

// Alive asks the connection itself rather than asking PostgreSQL whether the
// lock is still recorded.
//
// A session-scoped advisory lock cannot be lost while its connection lives:
// only an explicit unlock on the same connection releases it, and this
// process makes exactly one of those, in Term.Release. So "is the connection
// still there" and "do we still hold the lock" are the same question, and
// the cheaper one is the honest one to ask.
func (s *poolSession) Alive(ctx context.Context) error {
	if err := s.conn.Conn().Ping(ctx); err != nil {
		return fmt.Errorf("leader: ping: %w", err)
	}
	return nil
}

func (s *poolSession) Close() {
	s.once.Do(s.conn.Release)
}
