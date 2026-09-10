package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// fakeJoinDeliverer is a small stand-in for AnnounceDeliverer: it returns
// test-configured (delivered, remaining) and can be told to take a while,
// so a test can assert HandleEvent does not wait on it.
type fakeJoinDeliverer struct {
	delivered int
	remaining int
	err       error
	delay     time.Duration

	xuid  string
	calls chan struct{}
}

var _ AnnounceDeliverer = (*fakeJoinDeliverer)(nil)

func (f *fakeJoinDeliverer) DrainForJoin(ctx context.Context, xuid string, _ time.Time) (int, int, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	f.xuid = xuid
	if f.calls != nil {
		f.calls <- struct{}{}
	}
	return f.delivered, f.remaining, f.err
}

// recordingTellVoice records every Tell call on a channel, so a test can
// wait for the drain plugin's background goroutine deterministically
// instead of sleeping and hoping.
type recordingTellVoice struct {
	told chan string
	err  error
}

func newRecordingTellVoice() *recordingTellVoice {
	return &recordingTellVoice{told: make(chan string, 4)}
}

func (v *recordingTellVoice) Tell(_ context.Context, _ string, message string) error {
	v.told <- message
	return v.err
}

func (v *recordingTellVoice) Say(_ context.Context, _ string) error { return nil }

func joinEvent(xuid string) roster.JoinEvent {
	return roster.JoinEvent{Entry: roster.Entry{XUID: xuid, Username: "Steve"}}
}

func TestAnnounceDrain_Kinds(t *testing.T) {
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, logging.New("info"))
	kinds := d.Kinds()
	if len(kinds) != 1 || kinds[0] != roster.JoinKind {
		t.Errorf("Kinds() = %v, want [%s]", kinds, roster.JoinKind)
	}
}

func TestAnnounceDrain_NoCommands(t *testing.T) {
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, logging.New("info"))
	if cmds := d.Commands(); len(cmds) != 0 {
		t.Errorf("Commands() = %v, want none", cmds)
	}
}

func TestJoinDeliversAndThenSummarises(t *testing.T) {
	// Six pending: two expedited, four normal. DrainForJoin already applied
	// the cap (this plugin doesn't re-implement it) and reports the two
	// expedited plus three normal as delivered, one normal left over.
	deliverer := &fakeJoinDeliverer{delivered: 5, remaining: 1}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case msg := <-voice.told:
		if !strings.Contains(msg, "1") || !strings.Contains(msg, "!inbox") {
			t.Errorf("summary = %q, want it to name the remaining count and point at !inbox", msg)
		}
		if strings.HasSuffix(strings.TrimSpace(msg), "?") {
			t.Errorf("summary = %q, must not end in a question mark", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the summary line")
	}
	if deliverer.xuid != "xuid-1" {
		t.Errorf("DrainForJoin xuid = %q, want %q", deliverer.xuid, "xuid-1")
	}
}

func TestJoinWithNothingPendingSaysNothing(t *testing.T) {
	// The welcome message already owns this moment; an empty inbox must not
	// add a line to it.
	deliverer := &fakeJoinDeliverer{delivered: 0, remaining: 0, calls: make(chan struct{}, 1)}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case <-deliverer.calls:
		// DrainForJoin ran; now make sure it produced no reply.
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DrainForJoin to be called")
	}
	select {
	case msg := <-voice.told:
		t.Errorf("Tell was called with %q, want silence for an empty inbox", msg)
	case <-time.After(50 * time.Millisecond):
		// expected: no summary sent
	}
}

func TestDrainRunsOffTheReadLoop(t *testing.T) {
	// HandleEvent must return promptly even when delivery is slow, because
	// the dispatcher that calls it is on the packet read loop.
	deliverer := &fakeJoinDeliverer{delivered: 1, remaining: 1, delay: 200 * time.Millisecond}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	start := time.Now()
	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("HandleEvent took %v, want it to return immediately regardless of delivery time", elapsed)
	}

	select {
	case <-voice.told:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delayed summary")
	}
}

func TestAnnounceDrain_WrongEventTypeErrors(t *testing.T) {
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, logging.New("info"))
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	if err := d.HandleEvent(context.Background(), pctx, notAJoinEvent{}); err == nil {
		t.Fatal("expected an error for a mismatched event type")
	}
}

func TestAnnounceDrain_NilDelivererIsSilent(t *testing.T) {
	// No database configured: DrainForJoin has nothing to report on, so
	// HandleEvent has nothing to say either, rather than panicking on a nil
	// dependency.
	d := NewAnnounceDrain(context.Background(), nil, logging.New("info"))
	voice := newRecordingTellVoice()
	pctx := &plugin.Context{Voice: voice}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case msg := <-voice.told:
		t.Errorf("Tell was called with %q, want silence with no deliverer configured", msg)
	case <-time.After(50 * time.Millisecond):
		// expected: no summary sent
	}
}

func TestAnnounceDrain_DrainErrorIsLoggedNotPanicked(t *testing.T) {
	deliverer := &fakeJoinDeliverer{err: errors.New("db unreachable"), calls: make(chan struct{}, 1)}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case <-deliverer.calls:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DrainForJoin to be called")
	}
	select {
	case msg := <-voice.told:
		t.Errorf("Tell was called with %q, want silence when the drain itself failed", msg)
	case <-time.After(50 * time.Millisecond):
		// expected: no summary sent
	}
}

func TestAnnounceDrain_RootCtxCancelledStopsTheDrain(t *testing.T) {
	deliverer := &fakeJoinDeliverer{delivered: 1, remaining: 1, delay: time.Hour}
	voice := newRecordingTellVoice()
	rootCtx, cancel := context.WithCancel(context.Background())
	d := NewAnnounceDrain(rootCtx, deliverer, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	cancel()

	select {
	case msg := <-voice.told:
		t.Errorf("Tell was called with %q after shutdown was signalled mid-drain", msg)
	case <-time.After(50 * time.Millisecond):
		// expected: no summary sent, the drain's context died with rootCtx
	}
}
