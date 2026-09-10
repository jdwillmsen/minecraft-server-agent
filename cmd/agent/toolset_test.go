package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

func TestBuildToolsetOmitsAbsentCapabilities(t *testing.T) {
	// A context with nothing configured must produce no tools at all: the
	// model cannot call what it was never offered, which is stronger than
	// refusing the call afterwards.
	registry, _ := buildToolset(&plugin.Context{})
	if got := registry.Len(); got != 0 {
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
	registry, _ := buildToolset(&plugin.Context{Waypoints: store})

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
			registry, _ := buildToolset(&plugin.Context{ServerInfo: tc.serverInfo})
			got := toolNames(registry)
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

// Which tools ran decides whether the answer is whispered, so the flag has
// to follow the data rather than the capability: a context offering both
// surfaces still broadcasts an answer the model built without asking about
// the player.
func TestOnlyCallerScopedToolsMarkAnAnswerAsPersonal(t *testing.T) {
	pctx := &plugin.Context{Waypoints: &recordingWaypoints{}, Knowledge: enabledKnowledge{}}

	registry, scoped := buildToolset(pctx)
	if _, err := registry.Invoke(t.Context(), "knowledge_lookup", json.RawMessage(`{"query":"rules"}`), "2535411111111111"); err != nil {
		t.Fatalf("knowledge_lookup: %v", err)
	}
	if scoped.happened() {
		t.Error("a shared-knowledge answer was marked as the asker's own data")
	}

	if _, err := registry.Invoke(t.Context(), "waypoint_list", noArgs, "2535411111111111"); err != nil {
		t.Fatalf("waypoint_list: %v", err)
	}
	if !scoped.happened() {
		t.Error("reading the asker's own waypoints did not mark the answer personal")
	}
}

// enabledKnowledge is a fact store with something in it, so the tool it
// backs actually runs.
type enabledKnowledge struct{ knowledge.Nop }

func (enabledKnowledge) Enabled() bool { return true }

func (enabledKnowledge) Lookup(context.Context, string, int) ([]knowledge.Entry, error) {
	return []knowledge.Entry{{Topic: "rules", Body: "be nice"}}, nil
}

// fallbackOnlyKnowledge returns a single entry that matched only through
// Lookup's substring fallback, with no full-text overlap with the query at
// all -- exactly the case knowledge_lookup's output has to flag rather than
// hand the model a guess dressed as a confirmed fact.
type fallbackOnlyKnowledge struct{ knowledge.Nop }

func (fallbackOnlyKnowledge) Enabled() bool { return true }

func (fallbackOnlyKnowledge) Lookup(context.Context, string, int) ([]knowledge.Entry, error) {
	return []knowledge.Entry{
		{Topic: "weather", Body: "ask an operator", Matched: knowledge.MatchFallback},
	}, nil
}

func TestKnowledgeLookupToolFlagsAFallbackOnlyMatch(t *testing.T) {
	pctx := &plugin.Context{Knowledge: fallbackOnlyKnowledge{}}
	registry, _ := buildToolset(pctx)

	out, err := registry.Invoke(t.Context(), "knowledge_lookup", json.RawMessage(`{"query":"nether"}`), "2535411111111111")
	if err != nil {
		t.Fatalf("knowledge_lookup: %v", err)
	}
	if !strings.Contains(out, "possible match") {
		t.Errorf("output = %q, want it to flag the fallback-only match", out)
	}
}

func TestKnowledgeLookupToolDoesNotFlagAConfirmedMatch(t *testing.T) {
	pctx := &plugin.Context{Knowledge: enabledKnowledge{}}
	registry, _ := buildToolset(pctx)

	out, err := registry.Invoke(t.Context(), "knowledge_lookup", json.RawMessage(`{"query":"rules"}`), "2535411111111111")
	if err != nil {
		t.Fatalf("knowledge_lookup: %v", err)
	}
	if strings.Contains(out, "possible match") {
		t.Errorf("output = %q, a confirmed match should not be flagged", out)
	}
}
