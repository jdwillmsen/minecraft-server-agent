package presence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Postgres persists to minecraft.presence_overrides and
// minecraft.presence_status, sharing the profile store's pool. Every
// replica reads and writes here directly, which is what lets a standby
// answer the API as well as the leader.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

const overrideColumns = `actor_id, state, until, wake_on, reason, set_by, set_at, version`

type scanner interface {
	Scan(dest ...any) error
}

func scanOverride(row scanner) (string, presenceapi.Override, error) {
	var (
		id, state string
		until     *time.Time
		wake      []byte
		ov        presenceapi.Override
	)
	if err := row.Scan(&id, &state, &until, &wake, &ov.Reason, &ov.SetBy, &ov.SetAt, &ov.Version); err != nil {
		return "", ov, err
	}
	ov.State = presenceapi.State(state)
	ov.SetAt = ov.SetAt.UTC()
	if until != nil {
		u := until.UTC()
		ov.Until = &u
	}
	if wake != nil {
		var w presenceapi.WakeOn
		if err := json.Unmarshal(wake, &w); err != nil {
			return "", ov, fmt.Errorf("presence: wake_on of %s: %w", id, err)
		}
		ov.WakeOn = &w
	}
	return id, ov, nil
}

// wakeParam is the wake_on argument: NULL for none, the JSON text otherwise,
// cast to jsonb by the statement.
func wakeParam(w *presenceapi.WakeOn) (any, error) {
	if w == nil {
		return nil, nil
	}
	b, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("presence: encode wake_on: %w", err)
	}
	return string(b), nil
}

func (p *Postgres) Overrides(ctx context.Context) (map[string]presenceapi.Override, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+overrideColumns+` FROM minecraft.presence_overrides`)
	if err != nil {
		return nil, fmt.Errorf("presence: read overrides: %w", err)
	}
	defer rows.Close()
	out := make(map[string]presenceapi.Override)
	for rows.Next() {
		id, ov, err := scanOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("presence: read overrides: %w", err)
		}
		out[id] = ov
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: read overrides: %w", err)
	}
	return out, nil
}

// lockRow reads actorID's row and holds it for the transaction, or reports
// nil when there is none -- in which case nothing is locked, and an insert
// racing this one is caught by the primary key instead.
func lockRow(ctx context.Context, tx pgx.Tx, actorID string) (*presenceapi.Override, error) {
	_, ov, err := scanOverride(tx.QueryRow(ctx, `SELECT `+overrideColumns+` FROM minecraft.presence_overrides WHERE actor_id = $1 FOR UPDATE`, actorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("presence: lock %s: %w", actorID, err)
	}
	return &ov, nil
}

func (p *Postgres) Set(ctx context.Context, actorID string, ov presenceapi.Override, expect int64) (Change, error) {
	wake, err := wakeParam(ov.WakeOn)
	if err != nil {
		return Change{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Change{}, fmt.Errorf("presence: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prev, err := lockRow(ctx, tx, actorID)
	if err != nil {
		return Change{}, err
	}
	var have int64
	if prev != nil {
		have = prev.Version
	}
	if have != expect {
		return Change{ActorID: actorID, Prev: prev}, ErrConflict
	}

	var row pgx.Row
	if prev == nil {
		// DO NOTHING rather than an upsert: two writers that both read "no
		// row" must not both succeed, and the second one finds out here.
		row = tx.QueryRow(ctx, `
			INSERT INTO minecraft.presence_overrides (actor_id, state, until, wake_on, reason, set_by, set_at, version)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, 1)
			ON CONFLICT (actor_id) DO NOTHING
			RETURNING `+overrideColumns,
			actorID, string(ov.State), ov.Until, wake, ov.Reason, ov.SetBy, ov.SetAt)
	} else {
		row = tx.QueryRow(ctx, `
			UPDATE minecraft.presence_overrides
			SET state = $2, until = $3, wake_on = $4::jsonb, reason = $5, set_by = $6, set_at = $7, version = version + 1
			WHERE actor_id = $1
			RETURNING `+overrideColumns,
			actorID, string(ov.State), ov.Until, wake, ov.Reason, ov.SetBy, ov.SetAt)
	}
	_, now, err := scanOverride(row)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		current, cerr := p.Overrides(ctx)
		if cerr != nil {
			return Change{}, cerr
		}
		var cur *presenceapi.Override
		if c, ok := current[actorID]; ok {
			cur = &c
		}
		return Change{ActorID: actorID, Prev: cur}, ErrConflict
	}
	if err != nil {
		return Change{}, fmt.Errorf("presence: write %s: %w", actorID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Change{}, fmt.Errorf("presence: commit %s: %w", actorID, err)
	}
	return Change{ActorID: actorID, Prev: prev, Now: &now}, nil
}

func (p *Postgres) SetMany(ctx context.Context, ovs map[string]presenceapi.Override) ([]Change, error) {
	// Locked in id order, so two group writes over overlapping members wait
	// for each other instead of deadlocking.
	ids := make([]string, 0, len(ovs))
	for id := range ovs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("presence: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	changes := make([]Change, 0, len(ids))
	for _, id := range ids {
		ov := ovs[id]
		wake, err := wakeParam(ov.WakeOn)
		if err != nil {
			return nil, err
		}
		prev, err := lockRow(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		_, now, err := scanOverride(tx.QueryRow(ctx, `
			INSERT INTO minecraft.presence_overrides (actor_id, state, until, wake_on, reason, set_by, set_at, version)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, 1)
			ON CONFLICT (actor_id) DO UPDATE
			SET state = EXCLUDED.state, until = EXCLUDED.until, wake_on = EXCLUDED.wake_on,
			    reason = EXCLUDED.reason, set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at,
			    version = minecraft.presence_overrides.version + 1
			RETURNING `+overrideColumns,
			id, string(ov.State), ov.Until, wake, ov.Reason, ov.SetBy, ov.SetAt))
		if err != nil {
			return nil, fmt.Errorf("presence: write %s: %w", id, err)
		}
		changes = append(changes, Change{ActorID: id, Prev: prev, Now: &now})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("presence: commit: %w", err)
	}
	return changes, nil
}

func (p *Postgres) Clear(ctx context.Context, ids []string) ([]Change, error) {
	rows, err := p.pool.Query(ctx, `DELETE FROM minecraft.presence_overrides WHERE actor_id = ANY($1) RETURNING `+overrideColumns, ids)
	if err != nil {
		return nil, fmt.Errorf("presence: clear: %w", err)
	}
	defer rows.Close()
	var changes []Change
	for rows.Next() {
		id, ov, err := scanOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("presence: clear: %w", err)
		}
		changes = append(changes, Change{ActorID: id, Prev: &ov})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: clear: %w", err)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].ActorID < changes[j].ActorID })
	return changes, nil
}

func (p *Postgres) Remove(ctx context.Context, actorID string, version int64, setAt time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM minecraft.presence_overrides WHERE actor_id = $1 AND version = $2 AND set_at = $3`, actorID, version, setAt)
	if err != nil {
		return false, fmt.Errorf("presence: remove %s: %w", actorID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (p *Postgres) Statuses(ctx context.Context) (map[string]presenceapi.Status, error) {
	rows, err := p.pool.Query(ctx, `SELECT actor_id, connected, observed_state, last_seen, process_version FROM minecraft.presence_status`)
	if err != nil {
		return nil, fmt.Errorf("presence: read status: %w", err)
	}
	defer rows.Close()
	out := make(map[string]presenceapi.Status)
	for rows.Next() {
		var id, observed string
		var st presenceapi.Status
		if err := rows.Scan(&id, &st.Connected, &observed, &st.LastSeen, &st.ProcessVersion); err != nil {
			return nil, fmt.Errorf("presence: read status: %w", err)
		}
		st.ObservedState = presenceapi.State(observed)
		st.LastSeen = st.LastSeen.UTC()
		out[id] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("presence: read status: %w", err)
	}
	return out, nil
}

func (p *Postgres) PutStatus(ctx context.Context, actorID string, s presenceapi.Status) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.presence_status (actor_id, connected, observed_state, last_seen, process_version)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (actor_id) DO UPDATE
		SET connected = EXCLUDED.connected, observed_state = EXCLUDED.observed_state,
		    last_seen = EXCLUDED.last_seen, process_version = EXCLUDED.process_version`,
		actorID, s.Connected, string(s.ObservedState), s.LastSeen, s.ProcessVersion,
	); err != nil {
		return fmt.Errorf("presence: write status of %s: %w", actorID, err)
	}
	return nil
}
