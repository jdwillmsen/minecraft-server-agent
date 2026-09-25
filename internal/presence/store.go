package presence

import (
	"context"
	"errors"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// ErrDisabled is every Store call with no database configured.
var ErrDisabled = errors.New("presence: no database configured")

// ErrConflict is a versioned write whose expected version was not the
// current one. The Change returned with it carries the current row in Prev.
var ErrConflict = errors.New("presence: override changed since it was read")

// Change is one override write as it landed: the row before it, and the row
// after it (nil once removed).
type Change struct {
	ActorID string
	Prev    *presenceapi.Override
	Now     *presenceapi.Override
}

// Store holds overrides and observed status. In every write, the store
// assigns Version; the caller's value is ignored.
type Store interface {
	// Overrides returns every stored override by actor id.
	Overrides(ctx context.Context) (map[string]presenceapi.Override, error)
	// Set writes one actor's override if its current version is expect,
	// where 0 means no row. On a mismatch it returns ErrConflict with the
	// current row in Change.Prev.
	Set(ctx context.Context, actorID string, ov presenceapi.Override, expect int64) (Change, error)
	// SetMany writes every entry in one transaction, whatever their
	// versions: a group is one operator decision about its members.
	SetMany(ctx context.Context, ovs map[string]presenceapi.Override) ([]Change, error)
	// Clear removes the rows of ids in one statement; an id with no row is
	// not in the result.
	Clear(ctx context.Context, ids []string) ([]Change, error)
	// Remove deletes actorID's row only while it is still the one set at
	// setAt with version, and reports whether it did. The version alone is
	// not enough: a removed row takes its count with it, so a re-park starts
	// again at 1.
	Remove(ctx context.Context, actorID string, version int64, setAt time.Time) (bool, error)
	Statuses(ctx context.Context) (map[string]presenceapi.Status, error)
	PutStatus(ctx context.Context, actorID string, s presenceapi.Status) error
	Enabled() bool
}

// Nop is the Store with no database configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Overrides(context.Context) (map[string]presenceapi.Override, error) {
	return nil, ErrDisabled
}
func (Nop) Set(context.Context, string, presenceapi.Override, int64) (Change, error) {
	return Change{}, ErrDisabled
}
func (Nop) SetMany(context.Context, map[string]presenceapi.Override) ([]Change, error) {
	return nil, ErrDisabled
}
func (Nop) Clear(context.Context, []string) ([]Change, error)               { return nil, ErrDisabled }
func (Nop) Remove(context.Context, string, int64, time.Time) (bool, error)  { return false, ErrDisabled }
func (Nop) Statuses(context.Context) (map[string]presenceapi.Status, error) { return nil, ErrDisabled }
func (Nop) PutStatus(context.Context, string, presenceapi.Status) error     { return ErrDisabled }
func (Nop) Enabled() bool                                                   { return false }
