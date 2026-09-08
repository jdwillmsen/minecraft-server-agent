package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

func TestBuildToolsetOmitsAbsentCapabilities(t *testing.T) {
	// A context with nothing configured must produce no tools at all: the
	// model cannot call what it was never offered, which is stronger than
	// refusing the call afterwards.
	if got := buildToolset(&plugin.Context{}, nil).Len(); got != 0 {
		t.Errorf("empty context produced %d tools, want 0", got)
	}
}

// recordingWaypoints is an enabled store that captures the XUID it was
// asked about.
type recordingWaypoints struct {
	waypoints.Nop
	askedFor string
}

func (w *recordingWaypoints) Enabled() bool { return true }

func (w *recordingWaypoints) Get(_ context.Context, xuid, name string) (waypoints.Waypoint, bool, error) {
	w.askedFor = xuid
	return waypoints.Waypoint{Name: name, X: 1, Y: 2, Z: 3, Dimension: "overworld"}, true, nil
}

func (w *recordingWaypoints) List(_ context.Context, xuid string) ([]waypoints.Waypoint, error) {
	w.askedFor = xuid
	return []waypoints.Waypoint{{Name: "base"}}, nil
}

// The model names a waypoint; it never chooses whose. A tool that took an
// owner from its arguments would let a crafted chat line read another
// player's coordinates, so both waypoint tools must ignore anything but the
// injected caller.
func TestWaypointToolsReadOnlyTheInjectedCaller(t *testing.T) {
	store := &recordingWaypoints{}
	registry := buildToolset(&plugin.Context{Waypoints: store}, nil)

	args := json.RawMessage(`{"name":"base","xuid":"2535499999999999","caller":"2535499999999999"}`)
	if _, err := registry.Invoke(t.Context(), "waypoint_lookup", args, "2535411111111111"); err != nil {
		t.Fatalf("waypoint_lookup: %v", err)
	}
	if store.askedFor != "2535411111111111" {
		t.Errorf("waypoint_lookup read waypoints for %q, want the injected caller", store.askedFor)
	}

	store.askedFor = ""
	if _, err := registry.Invoke(t.Context(), "waypoint_list", args, "2535411111111111"); err != nil {
		t.Fatalf("waypoint_list: %v", err)
	}
	if store.askedFor != "2535411111111111" {
		t.Errorf("waypoint_list read waypoints for %q, want the injected caller", store.askedFor)
	}
}
