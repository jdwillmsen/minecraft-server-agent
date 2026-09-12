package plugins

import (
	"context"
	"errors"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// drainToCompletion runs one join drain and returns only once it has
// finished: a semaphore slot is released only when the whole drain returns,
// so taking every one of them is the plugin's own proof it is done.
func drainToCompletion(t *testing.T, voice plugin.Voice) {
	t.Helper()
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{delivered: 3, remaining: 2}, 0, logging.New("error"))
	if err := d.HandleEvent(context.Background(), &plugin.Context{Voice: voice}, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	for i := 0; i < cap(d.inFlight); i++ {
		d.inFlight <- struct{}{}
	}
}

func TestTheDrainSummaryIsCountedAsItsOwnDelivery(t *testing.T) {
	const name = "mc_agent_announce_deliveries_total"

	if got := metricstest.Delta(t, func() {
		drainToCompletion(t, newRecordingTellVoice())
	}, name, "delivery", "summary", "outcome", "sent"); got != 1 {
		t.Errorf("summary/sent moved by %v, want 1", got)
	}

	failing := newRecordingTellVoice()
	failing.err = errors.New("bridge unreachable")
	if got := metricstest.Delta(t, func() {
		drainToCompletion(t, failing)
	}, name, "delivery", "summary", "outcome", "failed"); got != 1 {
		t.Errorf("summary/failed moved by %v, want 1", got)
	}
}
