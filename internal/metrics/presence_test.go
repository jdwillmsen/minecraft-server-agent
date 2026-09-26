package metrics

import (
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
)

func TestPresenceGauges(t *testing.T) {
	t.Cleanup(ResetPresence)
	PresenceDesired("afk-bot-1", false)
	PresenceObserved("afk-bot-1", true)
	if got := metricstest.Value(t, "mc_presence_desired", "actor", "afk-bot-1"); got != 0 || !metricstest.Exists(t, "mc_presence_desired", "actor", "afk-bot-1") {
		t.Errorf("desired = %v, want an exported 0", got)
	}
	if got := metricstest.Value(t, "mc_presence_observed", "actor", "afk-bot-1"); got != 1 {
		t.Errorf("observed = %v, want 1", got)
	}
}

// An override with an expiry cannot be forgotten, so it has no age series:
// the parked-long alert is about the ones that can.
func TestPresenceOverrideAgeExistsOnlyForOpenOverrides(t *testing.T) {
	t.Cleanup(ResetPresence)
	PresenceOverrideAge("afk-bot-2", 90*time.Minute, true)
	if got := metricstest.Value(t, "mc_presence_override_age_seconds", "actor", "afk-bot-2"); got != 5400 {
		t.Errorf("age = %v, want 5400", got)
	}
	PresenceOverrideAge("afk-bot-2", 0, false)
	if metricstest.Exists(t, "mc_presence_override_age_seconds", "actor", "afk-bot-2") {
		t.Error("age series survived its override gaining an expiry")
	}
}

func TestPresenceKicksStartAtZero(t *testing.T) {
	InitPresence([]string{"afk-bot-1"})
	if !metricstest.Exists(t, "mc_presence_kicks_total", "actor", "afk-bot-1") {
		t.Fatal("kick counter not initialised; increase() over its first kick would read 0")
	}
	if d := metricstest.Delta(t, func() { PresenceKick("afk-bot-1") }, "mc_presence_kicks_total", "actor", "afk-bot-1"); d != 1 {
		t.Errorf("kick moved the counter by %v, want 1", d)
	}
}

// A standby must not keep exporting what it last saw as leader beside what
// the new leader sees.
func TestResetPresenceWithdrawsTheGauges(t *testing.T) {
	PresenceDesired("agent", true)
	PresenceObserved("agent", true)
	PresenceOverrideAge("agent", time.Minute, true)
	PresenceTickSuccess(time.Unix(1700000000, 0))
	ResetPresence()
	if metricstest.Exists(t, "mc_presence_tick_success_timestamp_seconds") {
		t.Error("mc_presence_tick_success_timestamp_seconds still exported after ResetPresence")
	}
	for _, name := range []string{"mc_presence_desired", "mc_presence_observed", "mc_presence_override_age_seconds"} {
		if metricstest.Exists(t, name, "actor", "agent") {
			t.Errorf("%s still exported after ResetPresence", name)
		}
	}
}
