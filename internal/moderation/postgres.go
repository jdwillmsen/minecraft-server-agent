package moderation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists to minecraft.moderation_events, sharing the profile
// store's pool.
//
// xuid is not a foreign key there, for the reason command_audit's is not:
// the record must outlive the player it describes, so writing one never
// needs a minecraft.players row first.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

func (p *Postgres) Record(ctx context.Context, e Event) error {
	at := e.OccurredAt
	if at.IsZero() {
		// A zero time is two thousand years old, so the next prune would
		// delete the row before anyone had read it.
		at = time.Now()
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.moderation_events
		    (xuid, gamertag, message, rule, detail, action, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.XUID, e.Gamertag, e.Message, string(e.Rule), e.Detail, string(e.Action), at,
	); err != nil {
		return fmt.Errorf("moderation: record: %w", err)
	}
	return nil
}

// Two statements rather than one with "$1 = ” OR xuid = $1": the filtered
// read is the one moderation_events_xuid_idx exists for, and a predicate the
// planner cannot resolve until it sees the parameter can cost it that index.
// event_id breaks ties because the flags from one message share a timestamp.
const (
	recentAll = `
		SELECT event_id, xuid, gamertag, message, rule, detail, action, occurred_at
		FROM minecraft.moderation_events
		ORDER BY occurred_at DESC, event_id DESC
		LIMIT $1`
	recentFor = `
		SELECT event_id, xuid, gamertag, message, rule, detail, action, occurred_at
		FROM minecraft.moderation_events
		WHERE xuid = $1
		ORDER BY occurred_at DESC, event_id DESC
		LIMIT $2`
)

func (p *Postgres) Recent(ctx context.Context, xuid string, limit int) ([]Event, error) {
	query, args := recentAll, []any{limit}
	if xuid != "" {
		query, args = recentFor, []any{xuid, limit}
	}
	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("moderation: recent: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.XUID, &e.Gamertag, &e.Message,
			&e.Rule, &e.Detail, &e.Action, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("moderation: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("moderation: recent: %w", err)
	}
	return out, nil
}

func (p *Postgres) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM minecraft.moderation_events WHERE occurred_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("moderation: prune: %w", err)
	}
	return tag.RowsAffected(), nil
}
