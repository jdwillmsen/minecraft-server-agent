package plugins

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
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

// A greeting belongs to the connection that reported the arrival. The lock
// can pass to another process inside the delay -- the turn ends, the
// connection with it -- and the console bridge is a separate process that
// speaks either way, so nothing else stops a five-second-old greeting from
// landing in the game the next leader is now playing. The same applies to an
// ordinary reconnect, whose replacement reports the player itself.
func TestWelcome_HandleEvent_GreetingIsDroppedWhenItsConnectionEnded(t *testing.T) {
	voice := newRecordingVoice()
	conns := &fakeConnections{gen: 1}
	w := NewWelcome(context.Background(), 20*time.Millisecond, logging.New("info"), WithGreetConnections(conns))
	pctx := &plugin.Context{Voice: voice}

	if err := w.HandleEvent(context.Background(), pctx, joinEventAt("111", 1)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	conns.end()

	select {
	case msg := <-voice.said:
		t.Errorf("greeted with %q after the connection that reported the join ended — that server is somebody else's to speak into now", msg)
	case <-time.After(250 * time.Millisecond):
	}
}

// The ordinary join, with the same connection still being played when the
// delay expires: the greeting is the whole point of the plugin.
func TestWelcome_HandleEvent_GreetsWhenItsConnectionIsStillCurrent(t *testing.T) {
	voice := newRecordingVoice()
	conns := &fakeConnections{gen: 4}
	w := NewWelcome(context.Background(), testDelay, logging.New("info"), WithGreetConnections(conns))
	pctx := &plugin.Context{Voice: voice}

	if err := w.HandleEvent(context.Background(), pctx, joinEventAt("111", 4)); err != nil {
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
