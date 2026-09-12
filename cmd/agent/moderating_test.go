package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// fakeModerationStore fails every call with err, records every prune
// cutoff, and -- when block is set -- holds every Record until it closes.
type fakeModerationStore struct {
	mu      sync.Mutex
	err     error
	cutoffs []time.Time
	records int
	block   chan struct{}
}

var _ moderation.Store = (*fakeModerationStore)(nil)

func (f *fakeModerationStore) Record(ctx context.Context, _ moderation.Event) error {
	f.mu.Lock()
	f.records++
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return f.err
}

func (f *fakeModerationStore) Recent(context.Context, string, int) ([]moderation.Event, error) {
	return nil, f.err
}

func (f *fakeModerationStore) Prune(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, cutoff)
	return 0, f.err
}

func (f *fakeModerationStore) Enabled() bool { return true }

func (f *fakeModerationStore) pruned() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.cutoffs...)
}

func (f *fakeModerationStore) recordCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records
}

// Released ahead of its migration, the agent hits the same missing table on
// every flag. Said once at INFO, and still returned: the plugin must know a
// flag was not written so it does not send an operator to look for it.
func TestModerationLogReportsAMissingTableOnceAndPassesItThrough(t *testing.T) {
	inner := &fakeModerationStore{err: fmt.Errorf("moderation: record: %w", &pgconn.PgError{Code: "42P01"})}
	out := captureAgentStdout(t, func() {
		m := newModerationLog(inner, logging.New("info"))
		for i := 0; i < 3; i++ {
			if err := m.Record(context.Background(), moderation.Event{}); !pgerr.NotMigrated(err) {
				t.Errorf("Record = %v, want the missing table passed through", err)
			}
		}
		if _, err := m.Recent(context.Background(), "", 5); !pgerr.NotMigrated(err) {
			t.Errorf("Recent = %v, want the missing table passed through for !modlog to answer", err)
		}
		if _, err := m.Prune(context.Background(), time.Now()); !pgerr.NotMigrated(err) {
			t.Errorf("Prune = %v, want the missing table passed through", err)
		}
	})
	if n := strings.Count(out, "moderation_store_unready"); n != 1 {
		t.Errorf("unready reported %d times, want once:\n%s", n, out)
	}
}

func TestModerationLogIsQuietAboutOtherFailures(t *testing.T) {
	out := captureAgentStdout(t, func() {
		m := newModerationLog(&fakeModerationStore{err: errors.New("connection reset")}, logging.New("info"))
		if err := m.Record(context.Background(), moderation.Event{}); err == nil {
			t.Error("a real failure was swallowed")
		}
	})
	if strings.Contains(out, "moderation_store_unready") {
		t.Errorf("an ordinary failure was reported as a deploy-ordering one:\n%s", out)
	}
}

func TestPruneDeletesPastRetentionOnAScheduleUntilShutdown(t *testing.T) {
	inner := &fakeModerationStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pruneModerationLog(ctx, inner, 10*time.Millisecond, logging.New("error"))
	}()

	deadline := time.Now().Add(5 * time.Second)
	for len(inner.pruned()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the prune did not repeat on its interval")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the prune loop outlived shutdown")
	}

	want := time.Now().Add(-moderation.Retention)
	if got := inner.pruned()[0]; got.Before(want.Add(-time.Minute)) || got.After(want) {
		t.Errorf("first cutoff = %v, want 90 days ago (%v)", got, want)
	}
}

func TestPruneDoesNothingWithoutADatabase(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		pruneModerationLog(context.Background(), moderation.Nop{}, time.Millisecond, logging.New("error"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the prune loop ran against a disabled store")
	}
}

// Moderation reads chat off the bus, so the bus must say who spoke by the
// roster's name, and whether the line was public.
func TestChatEventCarriesTheGamertagAndWhetherItWasPublic(t *testing.T) {
	registry, pctx, _, eventBus, events, playerRoster, permResolver := newHarness(t)
	playerRoster.Apply([]roster.PlayerListEntry{{XUID: playerXUID, Username: "Steve"}})
	log := logging.New("error")

	whisper := chatPacket(playerXUID, "Impostor", "hello")
	whisper.TextType = packet.TextTypeWhisper
	for _, pk := range []*packet.Text{chatPacket(playerXUID, "Impostor", "hello"), whisper} {
		handlePacket(context.Background(), pk, selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	}

	want := []bool{true, false}
	for i, public := range want {
		ev := (<-events).(chat.MessageEvent)
		if ev.Gamertag != "Steve" {
			t.Errorf("event %d gamertag = %q, want the roster's name, never the packet's", i, ev.Gamertag)
		}
		if ev.Public != public {
			t.Errorf("event %d public = %v, want %v", i, ev.Public, public)
		}
	}
}

// The one property moderation must never trade away: with its database
// stalled and a flood of flagged lines arriving, commands are still answered
// at the speed of the read loop.
func TestModerationNeverDelaysACommand(t *testing.T) {
	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mod, err := plugins.NewModeration(ctx, []string{"griefer"}, logging.New("error"))
	if err != nil {
		t.Fatalf("NewModeration: %v", err)
	}
	if err := registry.Register(mod); err != nil {
		t.Fatalf("register: %v", err)
	}
	stalled := &fakeModerationStore{block: make(chan struct{})}
	defer close(stalled.block)
	pctx.Moderation = stalled
	log := logging.New("error")
	startEventDispatch(ctx, eventBus, registry, pctx, log)

	start := time.Now()
	for i := 0; i < 200; i++ {
		handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "GRIEFER GRIEFER EVERYONE LOOK AT THIS"), selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	}
	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("201 lines took %s with moderation stalled", elapsed)
	}
	// Searched for rather than read off the end: the moderation worker's own
	// warning whisper can land either side of it.
	answered := false
	for _, line := range voice.output() {
		if line == "tell "+playerXUID+": pong" {
			answered = true
		}
	}
	if !answered {
		t.Errorf("voice output %v, want the !ping answered", voice.output())
	}
	deadline := time.Now().Add(5 * time.Second)
	for stalled.recordCalls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("moderation never reached the store; this test proves nothing about it")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
