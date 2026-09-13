package main

import (
	"context"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/leader"
	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// leaveGrace is how long the process waits, after closing the Bedrock
// connection, for the disconnect to actually reach the server.
//
// Closing the connection does not tell the server anything on its own: the
// RakNet layer marks the link as closing and sends the disconnect notice from
// its own tick once the packets it still owes are acknowledged -- between
// roughly one and five seconds later. A process that exits before that never
// sends it, and the server keeps the login until it times the session out,
// which is the ten seconds a release used to cost every player. Two seconds
// covers the ordinary case; waiting for the five-second ceiling would spend
// more of the handover than the notice is worth.
const leaveGrace = 2 * time.Second

// handoverTimeout bounds the two database calls a departing agent still owes:
// closing what it was watching, and releasing the lock. Generous relative to
// the work -- two statements -- and far inside the termination grace
// Kubernetes allows, because the alternative to finishing them is a
// successor that joins while this process is still in the game.
const handoverTimeout = 5 * time.Second

// leadership is the lock the live agent holds, as this binary uses it.
//
// An interface rather than *leader.Term because the sequencing around it is
// what this package has to get right -- leave the game, settle the database,
// then release -- and that is only testable if the lock can be faked.
type leadership interface {
	// Lost is closed the moment this process no longer holds the lock.
	Lost() <-chan struct{}
	// Release gives the lock up, which is what lets a standby join.
	Release(ctx context.Context) error
}

// campaigner waits for the agent lock on this process's behalf.
type campaigner interface {
	Campaign(ctx context.Context) (leadership, error)
}

// agentElection adapts the election to the interface above, and is the one
// place a nil term can be turned into a nil interface: a typed nil inside a
// non-nil interface would pass every nil check this file makes.
type agentElection struct {
	election *leader.Election
}

func (a agentElection) Campaign(ctx context.Context) (leadership, error) {
	term, err := a.election.Campaign(ctx)
	if err != nil {
		return nil, err
	}
	return term, nil
}

// newElection builds the election this process campaigns in, on the pool the
// profile store already opened.
//
// Keyed on the Xbox Live account rather than on the deployment: the account is
// the thing two processes cannot share, since a second login kicks the first.
func newElection(cfg config.Config, pool *pgxpool.Pool, log *logging.Logger) campaigner {
	return agentElection{election: leader.New(
		leader.PoolDial(pool, cfg.MCUsername),
		leader.WithPoll(time.Duration(cfg.LeaderPollMs)*time.Millisecond),
		leader.WithLogger(log),
	)}
}

// awaitLeadership holds this process out of the game until it holds the lock,
// reporting the leadership it then holds. ok is false only when the process is
// shutting down, in which case nothing was ever joined.
//
// Everything expensive has already happened by the time this is called -- the
// Xbox token is refreshed, the database is open, the knowledge store is
// loaded, the HTTP server is bound and answering -- which is what makes the
// wait worth anything: a standby that had to do all of that after taking the
// lock would cost the handover exactly what it was meant to save.
//
// With no election (no database, so no lock) the process is live because it is
// the only one, exactly as every release before this one was.
func awaitLeadership(ctx context.Context, election campaigner, setRole func(httpapi.Role), log *logging.Logger) (leadership, bool) {
	if election == nil {
		setRole(httpapi.RoleLive)
		return nil, true
	}

	// Standby before the wait, not after it: the role is what /readyz serves,
	// and a pod that reported itself unready here would be a pod Kubernetes
	// refuses to finish rolling out -- which would stall the very handover
	// this is waiting for.
	setRole(httpapi.RoleStandby)
	term, err := election.Campaign(ctx)
	if err != nil {
		// The only error a campaign returns is the process shutting down; a
		// database that cannot be reached is something it keeps waiting
		// through. Logged by the election itself, so nothing to add here.
		return nil, false
	}
	setRole(httpapi.RoleLive)
	return term, true
}

// endTermOnLockLoss ends this process's turn as the live agent as soon as the
// lock is gone.
//
// Losing the lock is not a database problem to be retried through: the lock is
// what entitles this process to the one login the account allows, so once it is
// gone another process may already hold both. Leaving the game is the only
// honest response -- and after it this process becomes a standby again and
// campaigns for the lock like any other.
func endTermOnLockLoss(ctx context.Context, term leadership, endTerm func(), log *logging.Logger) {
	if term == nil {
		return
	}
	select {
	case <-ctx.Done():
	case <-term.Lost():
		log.Error("lock_lost_standing_down", nil)
		endTerm()
	}
}

// handover settles what a departing agent owes and then lets its successor
// in, in that order.
//
// The order is the whole function. A standby joins the instant it sees the
// lock free, so anything done after the release is done while two agents are
// in the game -- and the sessions this closes are precisely the ones the
// successor is about to reopen from its own roster snapshot.
//
// since is when the departing connection began watching, and carries the same
// meaning it has in RecordLeave: only sessions that began at or after it were
// watched through, so only those are credited.
func handover(ctx context.Context, term leadership, profiles store.Store, since time.Time, log *logging.Logger) {
	// Detached from ctx, which SIGTERM has already cancelled by the time this
	// runs on the path it exists for. A handover that inherited that context
	// would skip both calls below and do nothing at all, silently.
	handoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), handoverTimeout)
	defer cancel()

	if n, err := profiles.CloseForHandover(handoverCtx, since, time.Now()); err != nil {
		// Logged, never fatal, and never a reason to hold the lock: a player's
		// playtime for one visit is worth less than the server's agent, and
		// the next connection closes whatever is left open anyway -- as
		// unwatched, which is the cost this was trying to avoid.
		log.Error("store_handover_close_failed", logging.Fields{"error": err.Error()})
	} else if n > 0 {
		log.Info("store_handover_closed_sessions", logging.Fields{"sessions": n})
	}

	if term == nil {
		return
	}
	if err := term.Release(handoverCtx); err != nil {
		log.Error("leader_release_failed", logging.Fields{"error": err.Error()})
	}
}

// leaveGame gives up the Bedrock login, waiting for the server to be told
// when something is waiting on it.
//
// Closing the connection is not the same as leaving: see leaveGrace. The wait
// is paid only when this session is ending because the process chose to end
// it -- a shutdown, or a lock this process no longer holds -- since those are
// the only times another agent is waiting to log in as the same account. An
// ordinary reconnect waits for nothing: the server has already dropped the
// session, and this process is the one that will dial again.
func leaveGame(ctx context.Context, conn io.Closer, grace time.Duration, log *logging.Logger) {
	if err := conn.Close(); err != nil {
		// Debug: every way out of a session ends here, including the ones
		// where the connection is already gone and closing it says so.
		log.Debug("connection_close_failed", logging.Fields{"error": err.Error()})
	}
	if ctx.Err() == nil {
		return
	}
	log.Info("leaving_game", logging.Fields{"disconnect_grace_ms": grace.Milliseconds()})
	time.Sleep(grace)
}

// startLiveWork starts everything only the live agent may do, on a context
// that ends with its turn.
//
// Every one of these writes somewhere a second process would be writing too:
// the game's console (the TPS sample is a console command and a line in the
// server log), the announcement outbox (a version change announced twice is
// announced twice to every player), the schedule rows, and the moderation
// record's retention. The event dispatcher is deliberately not here -- its
// subscriptions belong to the process, and it has nothing to dispatch while
// this process is not in the game.
func startLiveWork(
	ctx context.Context,
	cfg config.Config,
	bridgeTimeout time.Duration,
	pinger *adapters.ServerPinger,
	link *linkMeter,
	announceStore announce.Store,
	deliverer *announce.Deliverer,
	scheduleStore announce.ScheduleStore,
	moderationStore moderation.Store,
	log *logging.Logger,
) {
	go sampleGameClock(ctx, pinger, link.roundTrip, bridgeTimeout, log)
	go pruneModerationLog(ctx, moderationStore, moderationPruneInterval, log)
	go runServerWatcher(ctx, cfg, bridgeTimeout, announceStore, deliverer, log)
	go runScheduler(ctx, scheduleStore, deliverer, log)
}

// warmXboxToken spends the Xbox Live token refresh before the campaign rather
// than after it.
//
// The refresh is a network round trip to Microsoft, and it is the one startup
// cost that would otherwise land between taking the lock and joining the game
// -- the single interval this whole design exists to keep short. A failure
// here is not fatal: the connect loop makes the same call through the dialer
// and already knows how to back off from an account-level rejection, which
// exiting here would turn into a crash loop instead.
func warmXboxToken(ts oauth2.TokenSource, log *logging.Logger) {
	if _, err := ts.Token(); err != nil {
		log.Error("auth_token_refresh_failed", logging.Fields{"error": err.Error()})
		return
	}
	log.Info("auth_token_ready", nil)
}
