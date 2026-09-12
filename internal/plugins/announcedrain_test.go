package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, 0, logging.New("info"))
	kinds := d.Kinds()
	if len(kinds) != 1 || kinds[0] != roster.JoinKind {
		t.Errorf("Kinds() = %v, want [%s]", kinds, roster.JoinKind)
	}
}

func TestAnnounceDrain_NoCommands(t *testing.T) {
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, 0, logging.New("info"))
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
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
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
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
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
	// the dispatcher that calls it is on the packet read loop. The delay is
	// well past the assertion threshold -- a wide gap so this doesn't flake
	// under -race on a loaded machine, while still failing hard if
	// HandleEvent ever starts waiting on the drain itself.
	deliverer := &fakeJoinDeliverer{delivered: 1, remaining: 1, delay: 500 * time.Millisecond}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
	pctx := &plugin.Context{Voice: voice}

	start := time.Now()
	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("HandleEvent took %v, want it to return immediately regardless of delivery time", elapsed)
	}

	select {
	case <-voice.told:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the delayed summary")
	}
}

func TestAnnounceDrain_WrongEventTypeErrors(t *testing.T) {
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, 0, logging.New("info"))
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	if err := d.HandleEvent(context.Background(), pctx, notAJoinEvent{}); err == nil {
		t.Fatal("expected an error for a mismatched event type")
	}
}

func TestAnnounceDrain_NilDelivererIsSilent(t *testing.T) {
	// No database configured: DrainForJoin has nothing to report on, so
	// HandleEvent has nothing to say either, rather than panicking on a nil
	// dependency.
	d := NewAnnounceDrain(context.Background(), nil, 0, logging.New("info"))
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
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
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
	d := NewAnnounceDrain(rootCtx, deliverer, 0, logging.New("info"))
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

// captureStdout redirects the process's real stdout for the duration of fn,
// so a test can assert on a *logging.Logger's line without a writer seam --
// the same technique cmd/agent's chat-flow tests use for the same reason.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestAnnounceDrain_DroppedWhenAlreadyAtTheConcurrencyCap(t *testing.T) {
	// A reconnect storm (a restart, a network blip) spawns one drain per
	// returning player; without a cap that's one open bridge connection per
	// arrival. One slot, pre-occupied here exactly like startAnswer's own
	// busy-agent test occupies its inFlight channel, so the next join meets
	// a full drain plugin rather than a real held goroutine.
	deliverer := &fakeJoinDeliverer{delivered: 0, remaining: 0}
	voice := newRecordingTellVoice()
	pctx := &plugin.Context{Voice: voice}

	out := captureStdout(t, func() {
		// Built inside the capture, not before it: *logging.Logger resolves
		// os.Stdout at construction, so a logger built before the swap would
		// keep writing to the real stdout regardless of the redirect.
		d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
		d.inFlight = make(chan struct{}, 1)
		d.inFlight <- struct{}{} // the one slot is already taken

		if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
	})

	if !strings.Contains(out, `"event":"announce_drain_dropped_busy"`) {
		t.Errorf("stdout = %q, want an announce_drain_dropped_busy event", out)
	}
	if deliverer.xuid != "" {
		t.Errorf("DrainForJoin was called with xuid %q, want the dropped join never to reach the deliverer", deliverer.xuid)
	}
}

// missingGrantErr is what pgx returns when the tables exist but the role the
// agent connects as was never granted access to them.
func missingGrantErr() error {
	return fmt.Errorf("announce: pending: %w", &pgconn.PgError{
		Code:    "42501",
		Message: "permission denied for table announcements",
	})
}

// Every join hits the same missing tables, and an error line per arrival
// buries the log for as long as the release and its migration are out of
// step. Said once, at INFO, it is a deploy-ordering notice.
func TestAnnounceDrain_MissingTablesAreOneNoticeNotAnErrorPerJoin(t *testing.T) {
	for name, drainErr := range map[string]error{
		"tables not migrated": missingTableErr(),
		"tables not granted":  missingGrantErr(),
	} {
		t.Run(name, func(t *testing.T) {
			deliverer := &fakeJoinDeliverer{err: drainErr, calls: make(chan struct{}, 1)}
			pctx := &plugin.Context{Voice: newRecordingTellVoice()}

			out := captureStdout(t, func() {
				d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
				// One join at a time: two drains sharing this fake would
				// race on what it records, which is a property of the fake
				// and not of the plugin under test.
				for _, xuid := range []string{"xuid-1", "xuid-2"} {
					if err := d.HandleEvent(context.Background(), pctx, joinEvent(xuid)); err != nil {
						t.Fatalf("HandleEvent: %v", err)
					}
					select {
					case <-deliverer.calls:
					case <-time.After(time.Second):
						t.Fatal("timed out waiting for the drain to run")
					}
				}
				// The drain logs after it has answered the deliverer, so
				// waiting on the call alone would race the capture against
				// the line it is capturing. A slot in the semaphore is only
				// released once the whole drain has returned, so taking
				// every one of them is the plugin's own proof that both
				// goroutines are done.
				for i := 0; i < cap(d.inFlight); i++ {
					d.inFlight <- struct{}{}
				}
			})

			if strings.Contains(out, `"event":"announce_drain_failed"`) {
				t.Errorf("stdout = %q, want no error line for a state a deploy fixes", out)
			}
			if got := strings.Count(out, `"event":"announce_drain_unready"`); got != 1 {
				t.Errorf("stdout carried %d announce_drain_unready events, want exactly 1:\n%s", got, out)
			}
		})
	}
}

// A whisper sent the instant the roster reports a join is accepted by the
// server and shown to nobody, because the client is still loading -- and the
// delivery is recorded, so nothing retries it. The drain must wait.
func TestAnnounceDrain_WaitsBeforeDelivering(t *testing.T) {
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 1)}
	d := NewAnnounceDrain(context.Background(), deliverer, 75*time.Millisecond, logging.New("info"))

	if err := d.HandleEvent(t.Context(), &plugin.Context{Voice: newRecordingTellVoice()}, joinEvent("111")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.calls:
		t.Fatal("drained immediately: the joining client is still loading and would never see the messages")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-deliverer.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("never drained after the delay")
	}
}

// Shutdown during the wait must deliver nothing: the process is going away
// and the backlog is still owed, so the next join retries it.
func TestAnnounceDrain_ShutdownDuringTheWaitDeliversNothing(t *testing.T) {
	rootCtx, cancel := context.WithCancel(context.Background())
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 1)}
	d := NewAnnounceDrain(rootCtx, deliverer, time.Hour, logging.New("info"))

	if err := d.HandleEvent(t.Context(), &plugin.Context{Voice: newRecordingTellVoice()}, joinEvent("111")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	cancel()
	select {
	case <-deliverer.calls:
		t.Error("drained after shutdown, want nothing delivered")
	case <-time.After(100 * time.Millisecond):
	}
}
