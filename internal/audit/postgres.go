package audit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres writes to minecraft.command_audit, sharing the profile store's
// pool.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

func (p *Postgres) Write(ctx context.Context, r Record) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.command_audit
		    (xuid, gamertag, permission, command, args, outcome, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		r.XUID, r.Gamertag, r.Permission, r.Command, r.Args, string(r.Outcome), r.At,
	); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	return nil
}
