package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
	cfg.MaxConns = 4
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

	prior := Profile{XUID: xuid, Gamertag: gamertag}
	err = tx.QueryRow(ctx, `
		SELECT p.first_seen_at, p.last_seen_at, p.join_count,
		       COALESCE(SUM(s.duration_seconds), 0)::BIGINT,
		       COUNT(s.session_id) FILTER (WHERE s.ended_reason = 'unknown')
		FROM minecraft.players p
		LEFT JOIN minecraft.sessions s ON s.xuid = p.xuid
		WHERE p.xuid = $1
		GROUP BY p.first_seen_at, p.last_seen_at, p.join_count`,
		xuid,
	).Scan(&prior.FirstSeen, &prior.LastSeen, &prior.JoinCount, &prior.TotalSeconds, &prior.UncleanSessions)
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

	// Any session still open for this player belongs to a visit whose end was
	// never observed -- the agent was away for it. Closing it here rather
	// than leaving two open sessions keeps playtime arithmetic honest.
	if _, err := tx.Exec(ctx, `
		UPDATE minecraft.sessions
		SET left_at = $2, ended_reason = 'unknown'
		WHERE xuid = $1 AND ended_reason = 'open'`,
		xuid, at,
	); err != nil {
		return Profile{}, fmt.Errorf("store: close stale sessions: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO minecraft.sessions (xuid, gamertag, joined_at)
		VALUES ($1, $2, $3)`,
		xuid, gamertag, at,
	); err != nil {
		return Profile{}, fmt.Errorf("store: open session: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Profile{}, fmt.Errorf("store: commit: %w", err)
	}

	// JoinCount describes the arrival just recorded, so a first-ever join
	// reads as 1 rather than 0 -- see Profile.JoinCount.
	prior.JoinCount++
	return prior, nil
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
	if _, err := p.pool.Exec(ctx, `
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

func (p *Postgres) RecordLeave(ctx context.Context, xuid string, at time.Time) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE minecraft.sessions
		SET left_at = $2, ended_reason = 'left'
		WHERE xuid = $1 AND ended_reason = 'open'`,
		xuid, at,
	)
	if err != nil {
		return fmt.Errorf("store: close session: %w", err)
	}
	return nil
}

// CloseOrphans ends sessions left open by a previous run.
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
