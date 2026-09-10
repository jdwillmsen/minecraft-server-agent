package announce

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// undefinedTable is Postgres's SQLSTATE for "relation does not exist".
const undefinedTable = "42P01"

// NotMigrated reports whether err is this store talking to a database whose
// announcement tables have not been created yet.
//
// Enabled() answers "is there a pool", which is not the same question: an
// agent pointed at a database that predates the announcements migration
// passes every configuration check and then fails on the first statement.
// Without this the operator who typed !announce is told nothing at all --
// the command errors, and an errored command has no reply -- which is the
// same silent shape as the permissions incident this project keeps
// designing against.
//
// This becomes dead code the moment the migration is applied everywhere, and
// is meant to: it costs one comparison on a path that is already failing.
func NotMigrated(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedTable
}

// Store is what a source writes to and a deliverer reads from: the outbox
// and the record of who has already heard what.
type Store interface {
	Insert(ctx context.Context, a Announcement) (int64, error)
	PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]Announcement, error)
	MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error
	Enabled() bool
}

// Nop is the store for a deployment with no announcements table configured
// yet: every call is a no-op rather than an error, so a caller can wire an
// announcer in before the database is ready for it.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Insert(context.Context, Announcement) (int64, error) { return 0, nil }
func (Nop) PendingFor(context.Context, string, string, time.Time) ([]Announcement, error) {
	return nil, nil
}
func (Nop) MarkDelivered(context.Context, int64, string, time.Time) error { return nil }
func (Nop) Enabled() bool                                                 { return false }

// Postgres persists to minecraft.announcements and minecraft.announcement_deliveries.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

// Insert writes one row to the outbox. author_xuid and target_value are
// written as NULL rather than "" when unset: author_xuid is a foreign key,
// and target_value has no meaning for an 'everyone' or 'online_only' target.
func (p *Postgres) Insert(ctx context.Context, a Announcement) (int64, error) {
	var author any
	if a.AuthorXUID != "" {
		author = a.AuthorXUID
	}
	var target any
	if a.TargetValue != "" {
		target = a.TargetValue
	}
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO minecraft.announcements
		    (body, source, author_xuid, target_kind, target_value,
		     priority, delivery, deliver_after, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING announcement_id`,
		a.Body, string(a.Source), author, string(a.TargetKind), target,
		string(a.Priority), string(a.Delivery), a.DeliverAfter, a.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("announce: insert: %w", err)
	}
	return id, nil
}

// PendingFor returns what this player has not yet heard: not expired, its
// deliver_after has passed, and its target includes this xuid either
// directly or through permission. Expedited first, then oldest first, so a
// caller enforcing MaxNormalPerJoin can take the head of the list without
// re-sorting.
//
// online_only is excluded by construction, matching the table's partial
// index, rather than by an expiry check: it has no queue at all, so a
// countdown can't resurface hours later just because nobody set one.
func (p *Postgres) PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]Announcement, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT a.announcement_id, a.body, a.source, COALESCE(a.author_xuid, ''),
		       a.target_kind, COALESCE(a.target_value, ''), a.priority,
		       a.delivery, a.created_at, a.deliver_after, a.expires_at
		FROM minecraft.announcements a
		WHERE a.target_kind <> 'online_only'
		  AND a.deliver_after <= $3
		  AND (a.expires_at IS NULL OR a.expires_at > $3)
		  AND (a.target_kind = 'everyone'
		       OR (a.target_kind = 'player' AND a.target_value = $1)
		       OR (a.target_kind = 'permission' AND a.target_value = $2))
		  AND NOT EXISTS (
		        SELECT 1 FROM minecraft.announcement_deliveries d
		        WHERE d.announcement_id = a.announcement_id AND d.xuid = $1)
		ORDER BY (a.priority = 'expedited') DESC, a.created_at ASC`,
		xuid, permission, now,
	)
	if err != nil {
		return nil, fmt.Errorf("announce: pending: %w", err)
	}
	defer rows.Close()

	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Body, &a.Source, &a.AuthorXUID,
			&a.TargetKind, &a.TargetValue, &a.Priority, &a.Delivery,
			&a.CreatedAt, &a.DeliverAfter, &a.ExpiresAt); err != nil {
			return nil, fmt.Errorf("announce: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkDelivered records that xuid has received id. Idempotent by design: two
// deliverers racing the same join must not make the second one fail, and a
// conflict on the primary key means the player already has it, not an error.
func (p *Postgres) MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error {
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.announcement_deliveries (announcement_id, xuid, delivered_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (announcement_id, xuid) DO NOTHING`,
		id, xuid, at,
	); err != nil {
		return fmt.Errorf("announce: mark delivered: %w", err)
	}
	return nil
}
