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
