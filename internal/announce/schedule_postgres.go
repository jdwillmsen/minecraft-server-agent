package announce

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var _ ScheduleStore = (*Postgres)(nil)

// nullable writes an empty string as NULL: author_xuid is a foreign key and
// target_value means nothing for an 'everyone' or 'online_only' target.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// AddSchedule writes a new schedule. Exactly one of every_seconds and
// daily_at is written, matching the table's CHECK.
func (p *Postgres) AddSchedule(ctx context.Context, s Schedule) (int64, error) {
	var every pgtype.Int4
	var daily pgtype.Time
	if s.Cadence.Daily {
		daily = pgtype.Time{Microseconds: s.Cadence.At.Microseconds(), Valid: true}
	} else {
		every = pgtype.Int4{Int32: int32(s.Cadence.Every / time.Second), Valid: true}
	}
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO minecraft.announcement_schedules
		    (body, author_xuid, target_kind, target_value, priority,
		     every_seconds, daily_at, next_fire_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING schedule_id`,
		s.Body, nullable(s.AuthorXUID), string(s.TargetKind), nullable(s.TargetValue),
		string(s.Priority), every, daily, s.NextFireAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("announce: add schedule: %w", err)
	}
	return id, nil
}

const scheduleColumns = `
	schedule_id, body, COALESCE(author_xuid, ''), target_kind,
	COALESCE(target_value, ''), priority, every_seconds, daily_at,
	next_fire_at, active, created_at`

func (p *Postgres) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+scheduleColumns+`
		FROM minecraft.announcement_schedules
		WHERE active
		ORDER BY schedule_id`)
	if err != nil {
		return nil, fmt.Errorf("announce: list schedules: %w", err)
	}
	return scanSchedules(rows)
}

// DueSchedules reads what is due, leaning on the partial index over
// next_fire_at WHERE active. Most overdue first, so a backlog bigger than
// limit is worked through in the order it fell behind.
func (p *Postgres) DueSchedules(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+scheduleColumns+`
		FROM minecraft.announcement_schedules
		WHERE active AND next_fire_at <= $1
		ORDER BY next_fire_at, schedule_id
		LIMIT $2`,
		now, limit)
	if err != nil {
		return nil, fmt.Errorf("announce: due schedules: %w", err)
	}
	return scanSchedules(rows)
}

func scanSchedules(rows pgx.Rows) ([]Schedule, error) {
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		var s Schedule
		var every pgtype.Int4
		var daily pgtype.Time
		if err := rows.Scan(&s.ID, &s.Body, &s.AuthorXUID, &s.TargetKind,
			&s.TargetValue, &s.Priority, &every, &daily,
			&s.NextFireAt, &s.Active, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("announce: scan schedule: %w", err)
		}
		if every.Valid {
			s.Cadence.Every = time.Duration(every.Int32) * time.Second
		}
		if daily.Valid {
			s.Cadence.Daily = true
			s.Cadence.At = time.Duration(daily.Microseconds) * time.Microsecond
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeactivateSchedule sets active = false rather than deleting, so the
// announcements that reference the row keep their provenance.
func (p *Postgres) DeactivateSchedule(ctx context.Context, id int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE minecraft.announcement_schedules
		SET active = false
		WHERE schedule_id = $1 AND active`,
		id)
	if err != nil {
		return false, fmt.Errorf("announce: deactivate schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// FireSchedule claims one occurrence and stores the announcement it
// produces, in one transaction.
//
// The claim is a compare-and-set on next_fire_at: the UPDATE only matches
// while the row still holds the occurrence this caller read, so of any
// number of callers holding the same read, exactly one moves it. Together
// with the insert because apart they fail badly either way round: a claim
// whose insert then failed loses that reminder for good, and an insert
// before the claim can be made twice.
func (p *Postgres) FireSchedule(ctx context.Context, s Schedule, next time.Time, a Announcement) (int64, bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("announce: fire schedule: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE minecraft.announcement_schedules
		SET next_fire_at = $3
		WHERE schedule_id = $1 AND next_fire_at = $2 AND active`,
		s.ID, s.NextFireAt, next)
	if err != nil {
		return 0, false, fmt.Errorf("announce: fire schedule: claim: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return 0, false, nil
	}

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO minecraft.announcements
		    (body, source, author_xuid, target_kind, target_value,
		     priority, delivery, deliver_after, expires_at, schedule_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING announcement_id`,
		a.Body, string(a.Source), nullable(a.AuthorXUID), string(a.TargetKind), nullable(a.TargetValue),
		string(a.Priority), string(DeliveryFor(a.TargetKind)), a.DeliverAfter, a.ExpiresAt, s.ID,
	).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("announce: fire schedule: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("announce: fire schedule: commit: %w", err)
	}
	return id, true, nil
}
