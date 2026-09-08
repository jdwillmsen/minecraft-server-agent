// Package knowledge stores the curated facts the agent is allowed to state
// as the server's own word.
//
// Separate from internal/store, which owns presence: these rows are written
// by operators through a command and read by the LLM answer path, and
// keeping them apart means the answer path cannot reach player sessions.
package knowledge

import (
	"context"
	"strings"
	"time"
)

// Entry is one curated fact.
type Entry struct {
	Topic string
	Body  string
	// AuthorXUID is empty when the author's player row has since been
	// deleted -- the fact outlives the account that wrote it.
	AuthorXUID string
	UpdatedAt  time.Time
}

// Store reads and writes curated facts. Every method must tolerate being
// called on a disabled implementation.
type Store interface {
	// Lookup returns the entries best matching a free-text query, most
	// relevant first, capped at limit.
	Lookup(ctx context.Context, query string, limit int) ([]Entry, error)
	// Get returns one entry by exact topic. found is false when there is no
	// such topic, which is not an error.
	Get(ctx context.Context, topic string) (entry Entry, found bool, err error)
	Upsert(ctx context.Context, topic, body, authorXUID string) error
	Delete(ctx context.Context, topic string) error
	List(ctx context.Context) ([]Entry, error)
	Enabled() bool
}

// NormalizeTopic is the single definition of what makes two topics the same
// one. Applied on every write and every read, so "Gold Farm" typed by an
// operator and "gold farm" asked by a player reach the same row.
func NormalizeTopic(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Lookup(context.Context, string, int) ([]Entry, error) { return nil, nil }
func (Nop) Get(context.Context, string) (Entry, bool, error)     { return Entry{}, false, nil }
func (Nop) Upsert(context.Context, string, string, string) error { return nil }
func (Nop) Delete(context.Context, string) error                 { return nil }
func (Nop) List(context.Context) ([]Entry, error)                { return nil, nil }
func (Nop) Enabled() bool                                        { return false }
