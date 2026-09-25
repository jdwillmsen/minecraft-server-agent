package metrics

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
)

// Every name here is a contract with dashboards and alerts built elsewhere
// against the exact strings; a rename compiles, passes every other test, and
// silently empties a panel.
func TestEverySeriesIsExportedUnderItsAgreedName(t *testing.T) {
	// The gauges without labels only exist once measured, and command series
	// once main names the commands.
	ServerTPS(20, time.Now())
	LinkRTT(time.Millisecond)
	InitCommands([]string{"ping"}, []string{"ok"})

	for _, name := range []string{
		"mc_agent_commands_total",
		"mc_agent_mentions_total",
		"mc_agent_answer_duration_seconds",
		"mc_agent_announce_deliveries_total",
		"mc_agent_audit_write_failures_total",
		"mc_agent_auth_rejections_total",
		"mc_agent_deaths_total",
		"mc_agent_moderation_flags_total",
		"mc_agent_server_tps",
		"mc_agent_tps_last_success_timestamp_seconds",
		"mc_agent_link_rtt_seconds",
		"mc_agent_wiki_requests_total",
	} {
		if metricstest.Series(t, name) == 0 {
			t.Errorf("%s is not exported", name)
		}
	}
	// Tool calls have no pre-initialised series, so only exist once counted.
	ToolCall("waypoint_list", nil)
	if metricstest.Series(t, "mc_agent_tool_calls_total") == 0 {
		t.Error("mc_agent_tool_calls_total is not exported")
	}
}

// increase() over a series that first appears at 1 reads as 0, so an alert on
// the first failure after a restart only fires if the series was already
// there at zero.
func TestKnownLabelCombinationsStartAtZero(t *testing.T) {
	for _, o := range mentionOutcomes {
		if !metricstest.Exists(t, "mc_agent_mentions_total", "outcome", string(o)) {
			t.Errorf("mentions outcome %q is not pre-initialised", o)
		}
	}
	for _, d := range deliveries {
		for _, o := range []string{"sent", "failed"} {
			if !metricstest.Exists(t, "mc_agent_announce_deliveries_total", "delivery", string(d), "outcome", o) {
				t.Errorf("announce delivery %s/%s is not pre-initialised", d, o)
			}
		}
	}
	for _, name := range []string{"mc_agent_audit_write_failures_total", "mc_agent_auth_rejections_total", "mc_agent_deaths_total"} {
		if !metricstest.Exists(t, name) {
			t.Errorf("%s is not exported from startup", name)
		}
	}
}

func TestInitCommandsStartsEveryPairAtZero(t *testing.T) {
	InitCommands([]string{"initcmd-a", "initcmd-b"}, []string{"ok", "denied"})

	if n := testutil.CollectAndCount(commandsTotal); n < 4 {
		t.Fatalf("commands series = %d, want at least the 4 initialised", n)
	}
	for _, c := range []string{"initcmd-a", "initcmd-b"} {
		for _, o := range []string{"ok", "denied"} {
			if v := testutil.ToFloat64(commandsTotal.WithLabelValues(c, o)); v != 0 {
				t.Errorf("%s/%s = %v, want 0", c, o, v)
			}
		}
	}
}

func TestOutcomesFollowTheError(t *testing.T) {
	boom := errors.New("boom")

	sent := testutil.ToFloat64(announceDeliveriesTotal.WithLabelValues("whisper", "sent"))
	failed := testutil.ToFloat64(announceDeliveriesTotal.WithLabelValues("whisper", "failed"))
	AnnounceDelivery(DeliveryWhisper, nil)
	AnnounceDelivery(DeliveryWhisper, boom)
	if got := testutil.ToFloat64(announceDeliveriesTotal.WithLabelValues("whisper", "sent")); got != sent+1 {
		t.Errorf("whisper/sent = %v, want %v", got, sent+1)
	}
	if got := testutil.ToFloat64(announceDeliveriesTotal.WithLabelValues("whisper", "failed")); got != failed+1 {
		t.Errorf("whisper/failed = %v, want %v", got, failed+1)
	}

	ok := testutil.ToFloat64(toolCallsTotal.WithLabelValues("t", "ok"))
	bad := testutil.ToFloat64(toolCallsTotal.WithLabelValues("t", "error"))
	ToolCall("t", nil)
	ToolCall("t", boom)
	if got := testutil.ToFloat64(toolCallsTotal.WithLabelValues("t", "ok")); got != ok+1 {
		t.Errorf("tool ok = %v, want %v", got, ok+1)
	}
	if got := testutil.ToFloat64(toolCallsTotal.WithLabelValues("t", "error")); got != bad+1 {
		t.Errorf("tool error = %v, want %v", got, bad+1)
	}

	answered := metricstest.Value(t, "mc_agent_answer_duration_seconds", "outcome", "answered")
	answerFailed := metricstest.Value(t, "mc_agent_answer_duration_seconds", "outcome", "failed")
	Answer(time.Second, nil)
	Answer(time.Second, boom)
	if got := metricstest.Value(t, "mc_agent_answer_duration_seconds", "outcome", "answered"); got != answered+1 {
		t.Errorf("answered observations = %v, want %v", got, answered+1)
	}
	if got := metricstest.Value(t, "mc_agent_answer_duration_seconds", "outcome", "failed"); got != answerFailed+1 {
		t.Errorf("failed observations = %v, want %v", got, answerFailed+1)
	}
}

func TestServerTPSStampsTheMomentItWasMeasured(t *testing.T) {
	at := time.Date(2026, 9, 10, 8, 0, 0, 500_000_000, time.UTC)
	ServerTPS(19.5, at)

	if got := testutil.ToFloat64(serverTPS.WithLabelValues()); got != 19.5 {
		t.Errorf("tps = %v, want 19.5", got)
	}
	want := float64(at.UnixNano()) / 1e9
	if got := testutil.ToFloat64(tpsLastSuccess); math.Abs(got-want) > 1e-3 {
		t.Errorf("last success = %v, want %v", got, want)
	}
}

func TestLinkRTTIsInSeconds(t *testing.T) {
	LinkRTT(6 * time.Millisecond)
	if got := testutil.ToFloat64(linkRTT.WithLabelValues()); math.Abs(got-0.006) > 1e-9 {
		t.Errorf("link rtt = %v, want 0.006", got)
	}
}

// The rule and action sets are fixed by the schema, so every pair can start
// at zero and an alert on the first flag after a restart still fires.
func TestEveryModerationFlagPairStartsAtZero(t *testing.T) {
	for _, r := range moderationRules {
		for _, a := range moderationActions {
			if !metricstest.Exists(t, "mc_agent_moderation_flags_total", "rule", string(r), "action", string(a)) {
				t.Errorf("moderation flag %s/%s is not pre-initialised", r, a)
			}
		}
	}
	if n := metricstest.Series(t, "mc_agent_moderation_flags_total"); n != len(moderationRules)*len(moderationActions) {
		t.Errorf("mc_agent_moderation_flags_total exports %d series, want exactly %d", n, len(moderationRules)*len(moderationActions))
	}
}

func TestWikiLookupOutcomesStartAtZero(t *testing.T) {
	// Present at zero from startup: increase() over a series that first
	// appears at 1 reads as 0, so an alert on the first failure would miss it.
	for _, o := range wikiOutcomes {
		if !metricstest.Exists(t, "mc_agent_wiki_requests_total", "outcome", o) {
			t.Errorf("wiki outcome %q is not pre-initialised", o)
		}
	}
	if d := metricstest.Delta(t, func() { WikiLookup("hit") }, "mc_agent_wiki_requests_total", "outcome", "hit"); d != 1 {
		t.Errorf("WikiLookup(hit) moved the counter by %v, want 1", d)
	}
}
