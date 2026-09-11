package toolset

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

// emptyWaypoints is an enabled store with nothing saved.
type emptyWaypoints struct{ waypoints.Nop }

func (emptyWaypoints) Enabled() bool { return true }

// Given "base is at ...", the model attached whichever player the question
// named and reported the asker's base as another player's. Every waypoint
// result has to say whose it is, found or not.
func TestWaypointResultsSayTheyAreTheAskersOwn(t *testing.T) {
	const caller = "2535411111111111"
	name := json.RawMessage(`{"name":"base"}`)

	saved, _ := Build(&plugin.Context{Waypoints: &recordingWaypoints{}})
	empty, _ := Build(&plugin.Context{Waypoints: emptyWaypoints{}})

	for _, tc := range []struct {
		label string
		out   func() (string, error)
		want  string
	}{
		{"lookup, found", func() (string, error) { return saved.Invoke(t.Context(), "waypoint_lookup", name, caller) }, "your waypoint base is at 1 2 3"},
		{"lookup, missing", func() (string, error) { return empty.Invoke(t.Context(), "waypoint_lookup", name, caller) }, "you have no waypoint"},
		{"list, some", func() (string, error) { return saved.Invoke(t.Context(), "waypoint_list", noArgs, caller) }, "your saved waypoints: base"},
		{"list, none", func() (string, error) { return empty.Invoke(t.Context(), "waypoint_list", noArgs, caller) }, "you have no saved waypoints"},
	} {
		got, err := tc.out()
		if err != nil {
			t.Fatalf("%s: %v", tc.label, err)
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: result %q, want it to start %q", tc.label, got, tc.want)
		}
	}
}
