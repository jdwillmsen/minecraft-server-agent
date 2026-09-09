package waypoints

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists to minecraft.waypoints, sharing the profile store's pool.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

func (p *Postgres) Get(ctx context.Context, xuid, name string) (Waypoint, bool, error) {
	var wp Waypoint
	err := p.pool.QueryRow(ctx, `
		SELECT name, x, y, z, dimension, updated_at
		FROM minecraft.waypoints WHERE xuid = $1 AND name = $2`,
		xuid, NormalizeName(name),
	).Scan(&wp.Name, &wp.X, &wp.Y, &wp.Z, &wp.Dimension, &wp.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Waypoint{}, false, nil
	}
	if err != nil {
		return Waypoint{}, false, fmt.Errorf("waypoints: get: %w", err)
	}
	return wp, true, nil
}

// Set inserts the waypoint, requiring the player row to already exist -- it
// does, because a player must be online to run the command and joining
// records them.
func (p *Postgres) Set(ctx context.Context, xuid string, wp Waypoint) error {
	dimension, err := NormalizeDimension(wp.Dimension)
	if err != nil {
		return err
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.waypoints (xuid, name, x, y, z, dimension)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (xuid, name) DO UPDATE
		SET x = EXCLUDED.x, y = EXCLUDED.y, z = EXCLUDED.z,
		    dimension = EXCLUDED.dimension, updated_at = now()`,
		xuid, NormalizeName(wp.Name), wp.X, wp.Y, wp.Z, dimension,
	); err != nil {
		return fmt.Errorf("waypoints: set: %w", err)
	}
	return nil
}

func (p *Postgres) Delete(ctx context.Context, xuid, name string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM minecraft.waypoints WHERE xuid = $1 AND name = $2`,
		xuid, NormalizeName(name),
	)
	if err != nil {
		return false, fmt.Errorf("waypoints: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) List(ctx context.Context, xuid string) ([]Waypoint, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT name, x, y, z, dimension, updated_at
		FROM minecraft.waypoints WHERE xuid = $1 ORDER BY name`, xuid)
	if err != nil {
		return nil, fmt.Errorf("waypoints: list: %w", err)
	}
	defer rows.Close()

	var out []Waypoint
	for rows.Next() {
		var wp Waypoint
		if err := rows.Scan(&wp.Name, &wp.X, &wp.Y, &wp.Z, &wp.Dimension, &wp.UpdatedAt); err != nil {
			return nil, fmt.Errorf("waypoints: scan: %w", err)
		}
		out = append(out, wp)
	}
	return out, rows.Err()
}
