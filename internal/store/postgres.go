package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists to the minecraft schema migrated by jdwlabs/platform's
// jdwillmsen-schemas service.
//
// It connects as the app role, which owns the tables but is deliberately not
// the role migrations run as: the agent stores rows, it does not reshape its
// own schema.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// Open connects and verifies the connection before returning. A pool that
// fails on first use rather than on open turns a configuration mistake into a
// mystery at the first player join, hours later.
func Open(ctx context.Context, dsn string, connectTimeout time.Duration) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	// Small on purpose. This workload writes two rows per player visit; a
	// large pool would reserve connections on a shared cluster to sit idle.
	//
	// One more than the work needs, because one is not available to it: the
	// leader lock holds a connection of its own for as long as the process
	// leads (see internal/leader), and a pool sized for the work alone would
	// be a pool the agent is permanently one connection short of.
	cfg.MaxConns = 5
	cfg.MinConns = 0

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

func (p *Postgres) Enabled() bool { return true }

func (p *Postgres) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}

// Pool exposes the connection pool so the knowledge and waypoint stores can
// share it. One pool, not three: this whole workload is a few rows per
// player visit, and extra pools would reserve connections on a shared
// cluster to sit idle. Returns nil for a zero-valued Postgres.
func (p *Postgres) Pool() *pgxpool.Pool {
	if p == nil {
		return nil
	}
	return p.pool
}

// RecordJoin reads the prior profile and opens a session in one transaction.
//
// TotalSeconds counts the two endings the agent watched through: a departure
// it saw ('left') and a session it closed itself while handing the game over
// to a successor ('agent_restart'). A handover splits one visit into two
// rows, and leaving its first half out would mean a release silently deleting
// playtime from every profile that was online for it. 'unknown' stays
// excluded: those are the visits nobody watched end.
//
// One transaction because the two halves contradict each other otherwise: the
// upsert advances last_seen_at and join_count, so a read afterwards would
// report the player as having just been seen, and every greeting would say
// "welcome back" to someone who arrived seconds ago for the first time.
func (p *Postgres) RecordJoin(ctx context.Context, xuid, gamertag string, at time.Time) (Profile, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Profile{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockPlayer(ctx, tx, xuid); err != nil {
		return Profile{}, err
	}
	prior := Profile{XUID: xuid, Gamertag: gamertag}
	err = tx.QueryRow(ctx, `
		SELECT p.first_seen_at, p.last_seen_at, p.join_count,
		       COALESCE(SUM(s.duration_seconds) FILTER (WHERE s.ended_reason IN ('left', 'agent_restart')), 0)::BIGINT,
		       COUNT(s.session_id) FILTER (WHERE s.ended_reason = 'unknown'),
		       COUNT(s.session_id)
		FROM minecraft.players p
		LEFT JOIN minecraft.sessions s ON s.xuid = p.xuid
		WHERE p.xuid = $1
		GROUP BY p.first_seen_at, p.last_seen_at, p.join_count`,
		xuid,
	).Scan(&prior.FirstSeen, &prior.LastSeen, &prior.JoinCount, &prior.TotalSeconds, &prior.UncleanSessions, &prior.Sessions)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, fmt.Errorf("store: read profile: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO minecraft.players (xuid, current_gamertag, first_seen_at, last_seen_at, join_count)
		VALUES ($1, $2, $3, $3, 1)
		ON CONFLICT (xuid) DO UPDATE
		SET current_gamertag = EXCLUDED.current_gamertag,
		    last_seen_at     = EXCLUDED.last_seen_at,
		    join_count       = minecraft.players.join_count + 1,
		    updated_at       = now()`,
		xuid, gamertag, at,
	); err != nil {
		return Profile{}, fmt.Errorf("store: upsert player: %w", err)
	}

	// Gamertag history: a rename must not erase who they used to be.
	if _, err := tx.Exec(ctx, `
		INSERT INTO minecraft.player_names (xuid, gamertag, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (xuid, gamertag) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at`,
		xuid, gamertag, at,
	); err != nil {
		return Profile{}, fmt.Errorf("store: record name: %w", err)
	}

	if err := openSession(ctx, tx, xuid, gamertag, at); err != nil {
		return Profile{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Profile{}, fmt.Errorf("store: commit: %w", err)
	}

	// JoinCount describes the arrival just recorded, so a first-ever join
	// reads as 1 rather than 0 -- see Profile.JoinCount.
	prior.JoinCount++
	return prior, nil
}

// lockPlayer serializes the transactions that decide whether a player has
// been seen before. RecordJoin runs on a dispatch goroutine and ResumeSession
// on the read loop, so both can reach a brand-new player at once; under READ
// COMMITTED neither would see the other's session, and operators would be
// told twice. Held until the transaction ends.
func lockPlayer(ctx context.Context, tx pgx.Tx, xuid string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('minecraft.player:' || $1))`, xuid); err != nil {
		return fmt.Errorf("store: lock player: %w", err)
	}
	return nil
}

// openSession starts the player's one open session, closing any other first.
//
// A session still open here belongs to a visit whose end was never observed.
// Every connection closes those as it begins, so one surviving to this point
// means that close failed. Closed at its own joined_at, the way CloseOrphans
// closes one, and never at this new start: that would credit the player with
// their whole absence, days of it after a long break, and a milestone read
// off that total would be announced to the server as fact.
func openSession(ctx context.Context, tx pgx.Tx, xuid, gamertag string, at time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE minecraft.sessions
		SET left_at = joined_at, ended_reason = 'unknown'
		WHERE xuid = $1 AND ended_reason = 'open'`,
		xuid,
	); err != nil {
		return fmt.Errorf("store: close stale sessions: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO minecraft.sessions (xuid, gamertag, joined_at)
		VALUES ($1, $2, $3)`,
		xuid, gamertag, at,
	); err != nil {
		return fmt.Errorf("store: open session: %w", err)
	}
	return nil
}

// ResumeSession opens a session for a player already connected, leaving
// their profile as it was: no join counted, last_seen_at not advanced, the
// player row created bare if this is the first the agent has heard of them,
// exactly as EnsurePlayer would.
func (p *Postgres) ResumeSession(ctx context.Context, xuid, gamertag string, at time.Time) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockPlayer(ctx, tx, xuid); err != nil {
		return false, err
	}
	if err := ensurePlayer(ctx, tx, xuid, gamertag, at); err != nil {
		return false, err
	}
	var firstSeen bool
	if err := tx.QueryRow(ctx,
		`SELECT NOT EXISTS (SELECT 1 FROM minecraft.sessions WHERE xuid = $1)`, xuid,
	).Scan(&firstSeen); err != nil {
		return false, fmt.Errorf("store: read sessions: %w", err)
	}
	if err := openSession(ctx, tx, xuid, gamertag, at); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit: %w", err)
	}
	return firstSeen, nil
}

// EnsurePlayer inserts the minimum row a foreign key needs and leaves an
// existing one untouched.
//
// join_count keeps its schema default of zero: this is not an arrival, and
// RecordJoin increments from whatever is already there, so a player recorded
// here and greeted later still reads as the first-time arrival they are.
// first_seen_at is when this agent first had to write them down, which is
// all it has ever meant -- the server saw them earlier, and nothing here can
// know when.
func (p *Postgres) EnsurePlayer(ctx context.Context, xuid, gamertag string, at time.Time) error {
	return ensurePlayer(ctx, p.pool, xuid, gamertag, at)
}

func ensurePlayer(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, xuid, gamertag string, at time.Time) error {
	if _, err := db.Exec(ctx, `
		INSERT INTO minecraft.players (xuid, current_gamertag, first_seen_at, last_seen_at)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (xuid) DO NOTHING`,
		xuid, gamertag, at,
	); err != nil {
		return fmt.Errorf("store: ensure player: %w", err)
	}
	return nil
}

// XUIDForName resolves a gamertag through the two places one is recorded:
// the name a player answers to now, and every name they have answered to
// before.
//
// The current holder wins outright over anyone who merely used to hold the
// name. A gamertag freed by a rename can be taken by a different Xbox
// account, so a historical match is a guess about which person was meant
// while a current match is not: aiming a private message at the previous
// holder of a name someone else answers to today is the one outcome worth
// designing against. Among historical holders only -- which needs two
// renames in opposite directions to even arise -- the most recent to answer
// to the name wins, because there is nothing better to go on and refusing
// outright would make a queued message impossible for a player who has
// merely changed their name once.
func (p *Postgres) XUIDForName(ctx context.Context, gamertag string) (string, bool, error) {
	var xuid string
	err := p.pool.QueryRow(ctx, `
		SELECT xuid FROM (
		    SELECT p.xuid, 0 AS tier, p.last_seen_at
		    FROM minecraft.players p
		    WHERE p.current_gamertag = $1
		    UNION ALL
		    SELECT n.xuid, 1 AS tier, n.last_seen_at
		    FROM minecraft.player_names n
		    WHERE n.gamertag = $1
		) held
		ORDER BY tier, last_seen_at DESC
		LIMIT 1`,
		gamertag,
	).Scan(&xuid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: resolve gamertag: %w", err)
	}
	return xuid, true, nil
}

// RecordLeave closes the open session and reports the totals around it in
// one statement.
//
// One statement is what makes the two totals comparable. Every part of a
// statement reads the same snapshot, taken before its own UPDATE applies, so
// prior sums the sessions as they stood -- the open one contributing nothing,
// since its generated duration is NULL until left_at is set -- and closed
// returns the duration the UPDATE just gave it. After is their sum, not a
// second read that a concurrent join could have moved.
//
// prior counts the sessions whose end was watched -- a departure ('left') or
// a handover this agent made itself ('agent_restart') -- the same rule
// RecordJoin's TotalSeconds uses so a greeting and a milestone never
// disagree. An 'unknown' session's duration is a guess: this agent now
// closes one at zero length, but rows written before that carry the player's
// entire absence, and a milestone read off them would announce hours nobody
// played. closed needs no filter; everything it returns has just been set to
// 'left'.
//
// Only a session that began at or after since is closed as 'left'. One
// older than that was open when the current connection began, which means
// the close every connection starts with failed and nothing replaced it:
// the player may have left and returned unseen, so stale closes it at its
// own joined_at like CloseOrphans would, and it adds nothing.
//
// No open session is not an error: after equals before and the gamertag is
// blank, which reads to the caller as a departure that changed nothing.
func (p *Postgres) RecordLeave(ctx context.Context, xuid string, since, at time.Time) (Playtime, error) {
	var gamertag string
	var before, after int64
	err := p.pool.QueryRow(ctx, `
		WITH stale AS (
		    UPDATE minecraft.sessions
		    SET left_at = joined_at, ended_reason = 'unknown'
		    WHERE xuid = $1 AND ended_reason = 'open' AND joined_at < $3
		), closed AS (
		    UPDATE minecraft.sessions
		    SET left_at = $2, ended_reason = 'left'
		    WHERE xuid = $1 AND ended_reason = 'open' AND joined_at >= $3
		    RETURNING gamertag, duration_seconds
		), prior AS (
		    SELECT COALESCE(SUM(duration_seconds) FILTER (WHERE ended_reason IN ('left', 'agent_restart')), 0)::BIGINT AS total
		    FROM minecraft.sessions
		    WHERE xuid = $1
		)
		SELECT COALESCE((SELECT MAX(gamertag) FROM closed), ''),
		       prior.total,
		       prior.total + COALESCE((SELECT SUM(duration_seconds) FROM closed), 0)::BIGINT
		FROM prior`,
		xuid, at, since,
	).Scan(&gamertag, &before, &after)
	if err != nil {
		return Playtime{}, fmt.Errorf("store: close session: %w", err)
	}
	return Playtime{
		Gamertag: gamertag,
		Before:   time.Duration(before) * time.Second,
		After:    time.Duration(after) * time.Second,
	}, nil
}

// CloseForHandover closes what this agent was watching, at the moment it
// stopped watching, so a release costs nobody their playtime.
//
// One statement, two readings of "open", exactly as RecordLeave draws them. A
// session that began at or after the current connection was watched through
// to here, so it is closed at `at` and counted: the successor finds the
// player in its own opening roster snapshot and resumes watching them, and
// the two halves sum to the visit. One that began earlier survived a close
// this connection already attempted, which means the agent was away while it
// was open, so it is closed at its own joined_at and credits nothing -- the
// same honesty CloseOrphans applies, for the same reason.
//
// Only the rows this call credits are counted in the return value. The stale
// ones are a fault being cleaned up, not playtime being handed over, and
// reporting them together would make the log line say a release preserved
// time it actually discarded.
func (p *Postgres) CloseForHandover(ctx context.Context, since, at time.Time) (int, error) {
	var closed int
	err := p.pool.QueryRow(ctx, `
		WITH stale AS (
		    UPDATE minecraft.sessions
		    SET left_at = joined_at, ended_reason = 'unknown'
		    WHERE ended_reason = 'open' AND joined_at < $2
		), handed_over AS (
		    UPDATE minecraft.sessions
		    SET left_at = $1, ended_reason = 'agent_restart'
		    WHERE ended_reason = 'open' AND joined_at >= $2
		    RETURNING session_id
		)
		SELECT COUNT(*)::INT FROM handed_over`,
		at, since,
	).Scan(&closed)
	if err != nil {
		return 0, fmt.Errorf("store: close sessions for handover: %w", err)
	}
	return closed, nil
}

// CloseOrphans ends sessions left open by a previous run or a previous
// connection.
//
// left_at is the session's own joined_at, not now: the agent has no idea when
// those players actually left, and crediting them with the entire downtime
// would inflate playtime by exactly the length of the outage -- which, on this
// server, has been 40 hours.
func (p *Postgres) CloseOrphans(ctx context.Context, at time.Time) (int, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE minecraft.sessions
		SET left_at = joined_at, ended_reason = 'unknown'
		WHERE ended_reason = 'open'`,
	)
	if err != nil {
		return 0, fmt.Errorf("store: close orphans: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
