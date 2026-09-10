package announce

import (
	"testing"
	"time"
)

func TestDeliveryFollowsTheTarget(t *testing.T) {
	// A player-targeted announcement that broadcast would leak exactly what
	// whispering a waypoint protects, so this is derived, never chosen.
	cases := map[Target]Delivery{
		TargetEveryone:   DeliveryBroadcast,
		TargetOnlineOnly: DeliveryBroadcast,
		TargetPlayer:     DeliveryWhisper,
		TargetPermission: DeliveryWhisper,
	}
	for target, want := range cases {
		if got := DeliveryFor(target); got != want {
			t.Errorf("DeliveryFor(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestOnlineOnlyNeverQueues(t *testing.T) {
	if Queues(TargetOnlineOnly) {
		t.Error("online_only queued; a countdown delivered later is noise")
	}
	for _, target := range []Target{TargetEveryone, TargetPlayer, TargetPermission} {
		if !Queues(target) {
			t.Errorf("%q does not queue, so an offline player never hears it", target)
		}
	}
}

// TestConstantsMatchTheSchema looks tautological — every comparison is a
// constant against a literal that was copied from its own declaration — but
// it exists to catch a rename the rest of this file cannot: every other
// test compares a constant to itself under a different name, so a typo'd
// constant value would still pass every one of them. These strings are also
// written verbatim into Postgres columns guarded by CHECK constraints in
// another repository's migration, so a drift here is invisible until the
// first write of that kind, in production.
func TestConstantsMatchTheSchema(t *testing.T) {
	targets := map[Target]string{
		TargetEveryone:   "everyone",
		TargetPlayer:     "player",
		TargetPermission: "permission",
		TargetOnlineOnly: "online_only",
	}
	for constant, want := range targets {
		if got := string(constant); got != want {
			t.Errorf("Target = %q, want %q", got, want)
		}
	}

	sources := map[Source]string{
		SourceCommand:  "command",
		SourceSchedule: "schedule",
		SourceEvent:    "event",
		SourceAPI:      "api",
	}
	for constant, want := range sources {
		if got := string(constant); got != want {
			t.Errorf("Source = %q, want %q", got, want)
		}
	}

	priorities := map[Priority]string{
		PriorityNormal:    "normal",
		PriorityExpedited: "expedited",
	}
	for constant, want := range priorities {
		if got := string(constant); got != want {
			t.Errorf("Priority = %q, want %q", got, want)
		}
	}

	deliveries := map[Delivery]string{
		DeliveryBroadcast: "broadcast",
		DeliveryWhisper:   "whisper",
	}
	for constant, want := range deliveries {
		if got := string(constant); got != want {
			t.Errorf("Delivery = %q, want %q", got, want)
		}
	}
}

func TestDefaultExpiryVariesByWhatTheMessageIsFor(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	if got := DefaultExpiry(SourceCommand, TargetOnlineOnly, now); got != nil {
		t.Errorf("online_only got an expiry of %v; it never queues, so it needs none", got)
	}

	whisper := DefaultExpiry(SourceCommand, TargetPlayer, now)
	if whisper == nil || !whisper.Equal(now.Add(7*24*time.Hour)) {
		t.Errorf("player whisper expiry = %v, want 7 days out", whisper)
	}

	broadcast := DefaultExpiry(SourceCommand, TargetEveryone, now)
	if broadcast == nil || !broadcast.Equal(now.Add(24*time.Hour)) {
		t.Errorf("broadcast expiry = %v, want 24 hours out", broadcast)
	}
}
