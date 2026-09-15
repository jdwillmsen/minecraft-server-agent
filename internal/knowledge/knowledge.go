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
	// Matched records how this entry answered the query it came from. Its
	// zero value, MatchExact, is correct for Get and List, which never
	// guess; only a Lookup sets anything weaker.
	Matched MatchKind
}

// MatchKind says how confidently an Entry answers the query that returned
// it, so a caller can hedge a weak match rather than state it as settled
// fact.
type MatchKind int

const (
	// MatchExact covers every result the two weaker kinds below do not:
	// Get by its exact topic, every row from List, and a Lookup row
	// Postgres full-text search matched on a word that was not merely the
	// head of someone else's compound. It is the zero value on purpose, so
	// Get and List need not set it.
	MatchExact MatchKind = iota
	// MatchFallback is a Lookup row that satisfied only the substring
	// fallback -- some query token happened to appear inside the topic, or
	// the topic inside some token -- with no full-text overlap at all. A
	// row like this is a plausible guess, not a confirmed answer: ts_rank
	// is 0 for it and a caller with limit=1 has no other way to tell it
	// apart from a real hit.
	MatchFallback
	// MatchPartial is a Lookup row that full-text search did match, on a
	// word the query only used as the head of a different compound: asked
	// "where is the slime farm", the gold farm row matches on "farm" and
	// answers a question nobody asked. Full-text search cannot see this on
	// its own -- to it a matched term is a matched term -- so the row
	// arrives ranked and indistinguishable from a confirmed hit, which is
	// how a player came to be told the gold farm's coordinates as the
	// slime farm's. See MarkPartialMatches for exactly what counts.
	MatchPartial
)

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
	// Delete removes one topic. removed is false when there was no such
	// topic, which is not an error: a caller that confirms a delete it
	// never made teaches an operator the fact is gone while it is still
	// being quoted at players.
	Delete(ctx context.Context, topic string) (removed bool, err error)
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
func (Nop) Delete(context.Context, string) (bool, error)         { return false, nil }
func (Nop) List(context.Context) ([]Entry, error)                { return nil, nil }
func (Nop) Enabled() bool                                        { return false }
