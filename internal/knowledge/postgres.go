package knowledge

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres persists to minecraft.knowledge.
//
// Takes the pool the profile store already opened rather than opening its
// own: this workload is a handful of rows read per question, and a second
// pool would reserve connections on a shared cluster to sit idle.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

func NewPostgres(pool *pgxpool.Pool) *Postgres { return &Postgres{pool: pool} }

func (p *Postgres) Enabled() bool { return p != nil && p.pool != nil }

// Lookup ranks by Postgres full-text relevance against ANY of the query's
// terms (see searchQuery), and falls back to a plain substring test against
// the topic for compound words full-text search cannot split on its own --
// English stemming has no reason to know "goldfarm" is "gold" plus "farm".
// Both sides are driven from the same tokens, so "goldfarm", "gold farm",
// and "gold farm location" all find a fact stored under the topic
// "goldfarm".
func (p *Postgres) Lookup(ctx context.Context, query string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 3
	}
	normalized := NormalizeTopic(query)
	if normalized == "" {
		return nil, nil
	}
	tokens := queryTokens(normalized)
	if len(tokens) == 0 {
		return nil, nil
	}
	tsq := searchQuery(tokens)
	fallback := fallbackTokens(tokens)

	rows, err := p.pool.Query(ctx, `
		SELECT topic, body, COALESCE(author_xuid, ''), updated_at
		FROM minecraft.knowledge
		WHERE search @@ websearch_to_tsquery('english', $1)
		   OR EXISTS (
		        SELECT 1 FROM unnest($2::text[]) AS tok
		        WHERE position(tok IN topic) > 0 OR position(topic IN tok) > 0
		      )
		ORDER BY ts_rank(search, websearch_to_tsquery('english', $1)) DESC, updated_at DESC
		LIMIT $3`,
		tsq, fallback, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("knowledge: lookup: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Topic, &e.Body, &e.AuthorXUID, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("knowledge: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *Postgres) Get(ctx context.Context, topic string) (Entry, bool, error) {
	var e Entry
	err := p.pool.QueryRow(ctx, `
		SELECT topic, body, COALESCE(author_xuid, ''), updated_at
		FROM minecraft.knowledge WHERE topic = $1`,
		NormalizeTopic(topic),
	).Scan(&e.Topic, &e.Body, &e.AuthorXUID, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("knowledge: get: %w", err)
	}
	return e, true, nil
}

func (p *Postgres) Upsert(ctx context.Context, topic, body, authorXUID string) error {
	// author_xuid is written as NULL rather than '' when unknown: the column
	// is a foreign key, and '' is not a player.
	var author any
	if authorXUID != "" {
		author = authorXUID
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO minecraft.knowledge (topic, body, author_xuid)
		VALUES ($1, $2, $3)
		ON CONFLICT (topic) DO UPDATE
		SET body = EXCLUDED.body, author_xuid = EXCLUDED.author_xuid, updated_at = now()`,
		NormalizeTopic(topic), body, author,
	)
	if err != nil {
		return fmt.Errorf("knowledge: upsert: %w", err)
	}
	return nil
}

func (p *Postgres) Delete(ctx context.Context, topic string) (bool, error) {
	tag, err := p.pool.Exec(ctx,
		`DELETE FROM minecraft.knowledge WHERE topic = $1`, NormalizeTopic(topic),
	)
	if err != nil {
		return false, fmt.Errorf("knowledge: delete: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) List(ctx context.Context) ([]Entry, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT topic, body, COALESCE(author_xuid, ''), updated_at
		FROM minecraft.knowledge ORDER BY topic`)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Topic, &e.Body, &e.AuthorXUID, &e.UpdatedAt); err != nil {
			return nil, fmt.Errorf("knowledge: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
