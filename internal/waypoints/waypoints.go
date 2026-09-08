// Package waypoints stores each player's own named coordinates.
//
// Per-player by design: a waypoint is a base location, and a shared
// namespace would both collide on names and hand every player everyone
// else's coordinates.
package waypoints

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Waypoint is one named coordinate belonging to one player.
type Waypoint struct {
	Name      string
	X, Y, Z   int
	Dimension string
	UpdatedAt time.Time
}

// Store holds waypoints. Every method must tolerate being called on a
// disabled implementation.
type Store interface {
	Get(ctx context.Context, xuid, name string) (wp Waypoint, found bool, err error)
	Set(ctx context.Context, xuid string, wp Waypoint) error
	// Delete removes the named waypoint and reports whether one existed to
	// remove. A caller that only sees an error would have no way to tell a
	// delete of nothing from a delete of something, and the chat reply for
	// those two cases must differ.
	Delete(ctx context.Context, xuid, name string) (removed bool, err error)
	List(ctx context.Context, xuid string) ([]Waypoint, error)
	Enabled() bool
}

// ErrUnknownDimension is returned for a dimension the schema's CHECK
// constraint would reject. Caught here so a typo is a chat reply rather
// than a database error.
var ErrUnknownDimension = errors.New("unknown dimension")

// NormalizeName is the single definition of what makes two waypoint names
// the same one. Applied on every write and every read, so "Home Base" typed
// once and "home base" typed later reach the same row.
func NormalizeName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NormalizeDimension maps what players actually type onto the three values
// the schema allows. An empty dimension means the overworld, which is where
// almost every waypoint is.
func NormalizeDimension(s string) (string, error) {
	switch NormalizeName(s) {
	case "", "overworld", "the overworld":
		return "overworld", nil
	case "nether", "the nether", "the_nether":
		return "nether", nil
	case "end", "the end", "the_end":
		return "end", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownDimension, s)
	}
}

// Nop is the Store used when no database is configured.
type Nop struct{}

var _ Store = Nop{}

func (Nop) Get(context.Context, string, string) (Waypoint, bool, error) {
	return Waypoint{}, false, nil
}

func (Nop) Set(context.Context, string, Waypoint) error { return nil }

func (Nop) Delete(context.Context, string, string) (bool, error) { return false, nil }

func (Nop) List(context.Context, string) ([]Waypoint, error) { return nil, nil }

func (Nop) Enabled() bool { return false }
