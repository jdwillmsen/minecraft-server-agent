package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

func TestBuildToolsetOmitsAbsentCapabilities(t *testing.T) {
	// A context with nothing configured must produce no tools at all: the
	// model cannot call what it was never offered, which is stronger than
	// refusing the call afterwards.
	if got := buildToolset(&plugin.Context{}).Len(); got != 0 {
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
	registry := buildToolset(&plugin.Context{Waypoints: store})

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

// toolNames is what the model would actually be offered.
func toolNames(registry *tools.Registry) []string {
	var names []string
	for _, d := range registry.Definitions() {
		names = append(names, d.Function.Name)
	}
	return names
}

// A tool whose only possible answer is "that isn't configured here" is worse
// than no tool: the model spends one of two tool rounds, and part of a small
// model's prompt budget, learning something the wiring already knew.
// production always constructs a ServerInfo, so nil-checking it is not the
// same question as asking whether its exporters exist.
func TestServerInfoToolsFollowTheExporters(t *testing.T) {
	metrics := func(monitorURL, backupURL string) plugin.ServerInfo {
		return adapters.NewMetricsFacts(adapters.NewMetricsClient(monitorURL, backupURL, time.Second))
	}

	cases := []struct {
		name       string
		serverInfo plugin.ServerInfo
		want       []string
	}{
		{"neither exporter configured", metrics("", ""), nil},
		{"only mc-monitor", metrics("http://monitor.invalid", ""), []string{"server_status", "server_version"}},
		{"only the backup exporter", metrics("", "http://backup.invalid"), []string{"backup_status"}},
		{"both", metrics("http://monitor.invalid", "http://backup.invalid"), []string{"server_status", "server_version", "backup_status"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolNames(buildToolset(&plugin.Context{ServerInfo: tc.serverInfo}))
			if len(got) != len(tc.want) {
				t.Fatalf("tools = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("tools = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
