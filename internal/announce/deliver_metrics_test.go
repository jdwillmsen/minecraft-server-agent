package announce

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
)

const deliveriesMetric = "mc_agent_announce_deliveries_total"

func deliveryDelta(t *testing.T, delivery, outcome string, fn func()) float64 {
	t.Helper()
	return metricstest.Delta(t, fn, deliveriesMetric, "delivery", delivery, "outcome", outcome)
}

func TestABroadcastIsCountedOncePerSendNotPerListener(t *testing.T) {
	a := Announcement{Body: "restart soon", TargetKind: TargetEveryone}
	roster := fakeRoster{online: []string{"xuid-1", "xuid-2", "xuid-3"}}

	sent := deliveryDelta(t, "broadcast", "sent", func() {
		d := NewDeliverer(&fakeStore{enabled: true}, &fakeVoice{}, roster, fakePermissions{}, testLogger())
		if _, err := d.SendNow(context.Background(), a, 1); err != nil {
			t.Fatalf("SendNow: %v", err)
		}
	})
	if sent != 1 {
		t.Errorf("broadcast/sent moved by %v, want 1: one Say, however many heard it", sent)
	}

	failed := deliveryDelta(t, "broadcast", "failed", func() {
		voice := &fakeVoice{sayErr: errors.New("bridge unreachable")}
		d := NewDeliverer(&fakeStore{enabled: true}, voice, roster, fakePermissions{}, testLogger())
		if _, err := d.SendNow(context.Background(), a, 1); err != nil {
			t.Fatalf("SendNow: %v", err)
		}
	})
	if failed != 1 {
		t.Errorf("broadcast/failed moved by %v, want 1", failed)
	}
}

func TestAWhisperSentNowIsCounted(t *testing.T) {
	a := Announcement{Body: "psst", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	roster := fakeRoster{online: []string{"xuid-1", "xuid-2"}}

	sent := deliveryDelta(t, "whisper", "sent", func() {
		d := NewDeliverer(&fakeStore{enabled: true}, &fakeVoice{}, roster, fakePermissions{}, testLogger())
		if _, err := d.SendNow(context.Background(), a, 1); err != nil {
			t.Fatalf("SendNow: %v", err)
		}
	})
	if sent != 1 {
		t.Errorf("whisper/sent moved by %v, want 1", sent)
	}

	failed := deliveryDelta(t, "whisper", "failed", func() {
		voice := &fakeVoice{tellErr: map[string]error{"xuid-1": errors.New("bridge unreachable")}}
		d := NewDeliverer(&fakeStore{enabled: true}, voice, roster, fakePermissions{}, testLogger())
		if _, err := d.SendNow(context.Background(), a, 1); err != nil {
			t.Fatalf("SendNow: %v", err)
		}
	})
	if failed != 1 {
		t.Errorf("whisper/failed moved by %v, want 1", failed)
	}
}

func TestEveryWhisperInAJoinDrainIsCounted(t *testing.T) {
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer},
		{ID: 2, Body: "two", Priority: PriorityNormal, TargetKind: TargetPlayer},
	}

	sent := deliveryDelta(t, "whisper", "sent", func() {
		d := NewDeliverer(&fakeStore{enabled: true, pending: pending}, &fakeVoice{}, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())
		if _, _, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now()); err != nil {
			t.Fatalf("DrainForJoin: %v", err)
		}
	})
	if sent != 2 {
		t.Errorf("whisper/sent moved by %v, want 2", sent)
	}

	failed := deliveryDelta(t, "whisper", "failed", func() {
		voice := &fakeVoice{tellErr: map[string]error{"xuid-1": errors.New("bridge unreachable")}}
		d := NewDeliverer(&fakeStore{enabled: true, pending: pending}, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())
		if _, _, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now()); err != nil {
			t.Fatalf("DrainForJoin: %v", err)
		}
	})
	if failed != 2 {
		t.Errorf("whisper/failed moved by %v, want 2", failed)
	}
}
