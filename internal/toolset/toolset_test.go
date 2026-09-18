package toolset

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
	registry, _ := Build(&plugin.Context{})
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
	registry, _ := Build(&plugin.Context{Waypoints: store})

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
			registry, _ := Build(&plugin.Context{ServerInfo: tc.serverInfo})
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

	registry, scoped := Build(pctx)
	if _, err := registry.Invoke(t.Context(), "knowledge_lookup", json.RawMessage(`{"query":"rules"}`), "2535411111111111"); err != nil {
		t.Fatalf("knowledge_lookup: %v", err)
	}
	if scoped.Happened() {
		t.Error("a shared-knowledge answer was marked as the asker's own data")
	}

	if _, err := registry.Invoke(t.Context(), "waypoint_list", noArgs, "2535411111111111"); err != nil {
		t.Fatalf("waypoint_list: %v", err)
	}
	if !scoped.Happened() {
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
	registry, _ := Build(pctx)

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
	registry, _ := Build(pctx)

	out, err := registry.Invoke(t.Context(), "knowledge_lookup", json.RawMessage(`{"query":"rules"}`), "2535411111111111")
	if err != nil {
		t.Fatalf("knowledge_lookup: %v", err)
	}
	if strings.Contains(out, "possible match") {
		t.Errorf("output = %q, a confirmed match should not be flagged", out)
	}
}

// stubServerInfo reports a version without an exporter behind it, which the
// metrics adapter cannot do: pointed at a real URL it fails the fetch, and
// pointed at none it disables the tool.
type stubServerInfo struct{ version string }

func (stubServerInfo) ServerStatus(context.Context) (string, error) { return "server healthy.", nil }
func (s stubServerInfo) Version(context.Context) (string, error)    { return s.version, nil }
func (stubServerInfo) BackupStatus(context.Context) (string, error) { return "", nil }
func (stubServerInfo) StatusEnabled() bool                          { return true }
func (stubServerInfo) BackupEnabled() bool                          { return false }

// Asked whether an older client can join, the model answers from a tool
// result or from its training data, and the training data says yes. Every
// tool that reports the build has to displace that: server_status names the
// build as well, so a single status round reaches the same question.
func TestBuildReportingToolsStateThatAnOlderClientMustUpdate(t *testing.T) {
	registry, _ := Build(&plugin.Context{ServerInfo: stubServerInfo{version: "Bedrock 1.21.100.7."}})

	for _, name := range []string{"server_version", "server_status"} {
		out, err := registry.Invoke(t.Context(), name, noArgs, "2535411111111111")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(out, "update") {
			t.Errorf("%s = %q, want the rule that an older client has to update", name, out)
		}
	}
}

// A model holding the compatibility rule and nothing bounding it answered a
// question about Minecraft's release dates with the server's build and the
// rule. The limit belongs on the tool asked for the build, and only there:
// on a status result it cost more in crowded answers than it bought.
func TestServerVersionDisclaimsTheReleaseSchedule(t *testing.T) {
	registry, _ := Build(&plugin.Context{ServerInfo: stubServerInfo{version: "Bedrock 1.21.100.7."}})

	const limit = "when future Minecraft versions are released"

	out, err := registry.Invoke(t.Context(), "server_version", noArgs, "2535411111111111")
	if err != nil {
		t.Fatalf("server_version: %v", err)
	}
	if !strings.Contains(out, limit) {
		t.Errorf("server_version = %q, want the limit that the build says nothing about release dates", out)
	}

	out, err = registry.Invoke(t.Context(), "server_status", noArgs, "2535411111111111")
	if err != nil {
		t.Fatalf("server_status: %v", err)
	}
	if strings.Contains(out, limit) {
		t.Errorf("server_status = %q, want no release-date limit on a health reading", out)
	}
}

// The version the adapter reported has to survive the rule being appended
// to it, or the reply loses the one fact the asker needs to compare against.
func TestServerVersionStillReportsTheBuild(t *testing.T) {
	registry, _ := Build(&plugin.Context{ServerInfo: stubServerInfo{version: "Bedrock 1.21.100.7."}})

	out, err := registry.Invoke(t.Context(), "server_version", noArgs, "2535411111111111")
	if err != nil {
		t.Fatalf("server_version: %v", err)
	}
	if !strings.Contains(out, "1.21.100.7") {
		t.Errorf("output = %q, want the version the adapter reported", out)
	}
}

// The evaluation harness derives the versions a reply is allowed to state
// from the adapter answers alone, never from the rule, so a version number
// added on the way to the model would be shown to it but withheld from the
// scorer -- which would then read an accurate reply as an invented one.
// Asked with adapter answers carrying no digit at all, no tool result may
// carry one either.
func TestVersionReportingToolsAddNoVersionOfTheirOwn(t *testing.T) {
	registry, _ := Build(&plugin.Context{ServerInfo: stubServerInfo{version: "an unreleased build"}})

	for _, name := range []string{"server_version", "server_status"} {
		out, err := registry.Invoke(t.Context(), name, noArgs, "2535411111111111")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.ContainsAny(out, "0123456789") {
			t.Errorf("%s = %q, want no version the adapter did not report", name, out)
		}
	}
}

// partialKnowledge returns a row full-text search did match, on a word the
// query only used as the head of a different compound -- the gold farm
// answering "where is the slime farm". The model has to be told this is an
// answer to another question, not a hit with a soft edge.
type partialKnowledge struct{ knowledge.Nop }

func (partialKnowledge) Enabled() bool { return true }

func (partialKnowledge) Lookup(context.Context, string, int) ([]knowledge.Entry, error) {
	return []knowledge.Entry{
		{Topic: "gold farm", Body: "in the nether at 120 64 -340", Matched: knowledge.MatchPartial},
	}, nil
}

func TestKnowledgeLookupToolFlagsAPartialMatch(t *testing.T) {
	registry, _ := Build(&plugin.Context{Knowledge: partialKnowledge{}})

	out, err := registry.Invoke(t.Context(), "knowledge_lookup", json.RawMessage(`{"query":"where is the slime farm"}`), "2535411111111111")
	if err != nil {
		t.Fatalf("knowledge_lookup: %v", err)
	}
	if !strings.Contains(out, "no entry for what was asked") {
		t.Errorf("output = %q, want it to say the topic asked about is not recorded", out)
	}
	if !strings.Contains(out, "gold farm") {
		t.Errorf("output = %q, want the nearest topic still offered", out)
	}
}
