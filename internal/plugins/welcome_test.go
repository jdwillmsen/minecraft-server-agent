package plugins

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
)

// recordingVoice records every Say/Tell call on a channel, so a test can
// wait for the welcome plugin's background goroutine deterministically
// instead of sleeping and hoping.
type recordingVoice struct {
	said chan string
	err  error
}

func newRecordingVoice() *recordingVoice {
	return &recordingVoice{said: make(chan string, 4)}
}

func (v *recordingVoice) Tell(ctx context.Context, xuid, message string) error {
	return nil
}

func (v *recordingVoice) Say(ctx context.Context, message string) error {
	v.said <- message
	return v.err
}

const testDelay = 5 * time.Millisecond

func TestWelcome_Kinds(t *testing.T) {
	w := NewWelcome(context.Background(), testDelay, logging.New("info"))
	kinds := w.Kinds()
	if len(kinds) != 1 || kinds[0] != roster.JoinKind {
		t.Errorf("Kinds() = %v, want [%s]", kinds, roster.JoinKind)
	}
}

func TestWelcome_HandleEvent_JoinTriggersOneDelayedSay(t *testing.T) {
	voice := newRecordingVoice()
	w := NewWelcome(context.Background(), testDelay, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	join := roster.JoinEvent{Entry: roster.Entry{XUID: "111", Username: "Steve"}}
	if err := w.HandleEvent(context.Background(), pctx, join); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case msg := <-voice.said:
		if msg != "Welcome, Steve!" {
			t.Errorf("greeting = %q, want %q", msg, "Welcome, Steve!")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delayed greeting")
	}
}

func TestWelcome_HandleEvent_ReturnsImmediatelyWithoutWaitingOutTheDelay(t *testing.T) {
	voice := newRecordingVoice()
	w := NewWelcome(context.Background(), time.Hour, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	start := time.Now()
	join := roster.JoinEvent{Entry: roster.Entry{XUID: "111", Username: "Steve"}}
	if err := w.HandleEvent(context.Background(), pctx, join); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("HandleEvent took %v, want it to return immediately regardless of delay", elapsed)
	}
}

func TestWelcome_HandleEvent_WrongEventTypeErrors(t *testing.T) {
	w := NewWelcome(context.Background(), testDelay, logging.New("info"))
	pctx := &plugin.Context{Voice: newRecordingVoice()}

	if err := w.HandleEvent(context.Background(), pctx, notAJoinEvent{}); err == nil {
		t.Fatal("expected an error for a mismatched event type")
	}
}

type notAJoinEvent struct{}

func (notAJoinEvent) Kind() string { return roster.JoinKind }

func TestWelcome_HandleEvent_BlankUsernameFallsBackToGenericGreeting(t *testing.T) {
	voice := newRecordingVoice()
	w := NewWelcome(context.Background(), testDelay, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	join := roster.JoinEvent{Entry: roster.Entry{XUID: "111", Username: ""}}
	if err := w.HandleEvent(context.Background(), pctx, join); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case msg := <-voice.said:
		if msg == "Welcome, !" {
			t.Errorf("greeting = %q, must not be built from an empty name", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delayed greeting")
	}
}

func TestWelcome_HandleEvent_RootCtxCancelledBeforeDelayElapsesSkipsTheGreeting(t *testing.T) {
	voice := newRecordingVoice()
	rootCtx, cancel := context.WithCancel(context.Background())
	w := NewWelcome(rootCtx, time.Hour, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	join := roster.JoinEvent{Entry: roster.Entry{XUID: "111", Username: "Steve"}}
	if err := w.HandleEvent(context.Background(), pctx, join); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cancel()

	select {
	case msg := <-voice.said:
		t.Errorf("Say was called with %q after shutdown was signalled before the delay elapsed", msg)
	case <-time.After(50 * time.Millisecond):
		// expected: no greeting sent
	}
}

func TestWelcome_HandleEvent_SayFailureIsLoggedNotPanicked(t *testing.T) {
	voice := newRecordingVoice()
	voice.err = errors.New("bridge unreachable")
	w := NewWelcome(context.Background(), testDelay, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	join := roster.JoinEvent{Entry: roster.Entry{XUID: "111", Username: "Steve"}}
	if err := w.HandleEvent(context.Background(), pctx, join); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	select {
	case <-voice.said:
		// Say was attempted; its error is logged internally, not returned
		// (HandleEvent already returned before the goroutine even ran).
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delayed Say attempt")
	}
}
