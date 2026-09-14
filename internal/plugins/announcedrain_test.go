package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
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

func joinEventAt(xuid string, generation uint64) roster.JoinEvent {
	return roster.JoinEvent{Entry: roster.Entry{XUID: xuid, Username: "Steve"}, Generation: generation}
}

func presentEvent(xuid string, generation uint64) roster.PresentEvent {
	return roster.PresentEvent{Entry: roster.Entry{XUID: xuid, Username: "Steve"}, Generation: generation}
}

// fakeConnections is a settable connection counter, so a test can end the
// connection a drain is waiting in.
type fakeConnections struct {
	mu  sync.Mutex
	gen uint64
	// reads reports each answer given, so a test can order itself against
	// the drain goroutine instead of guessing how far along it is. Nil
	// unless a test wants it; the send never blocks either way.
	reads chan struct{}
	// ended stands in for the live connection's channel, closed by end().
	ended chan struct{}
}

var _ Connections = (*fakeConnections)(nil)

func (c *fakeConnections) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.reads <- struct{}{}:
	default:
	}
	return c.gen
}

func (c *fakeConnections) Ended() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended == nil {
		c.ended = make(chan struct{})
	}
	return c.ended
}

// end drops the live connection without opening another, the shape of a
// connection lost mid-delivery.
func (c *fakeConnections) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended == nil {
		c.ended = make(chan struct{})
	}
	select {
	case <-c.ended:
	default:
		close(c.ended)
	}
	c.gen++
}

func (c *fakeConnections) reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended != nil {
		select {
		case <-c.ended:
		default:
			close(c.ended)
		}
	}
	c.ended = make(chan struct{})
	c.gen++
}

func TestAnnounceDrain_Kinds(t *testing.T) {
	d := NewAnnounceDrain(context.Background(), &fakeJoinDeliverer{}, 0, logging.New("info"))
	kinds := d.Kinds()
	if len(kinds) != 2 || kinds[0] != roster.JoinKind || kinds[1] != roster.PresentKind {
		t.Errorf("Kinds() = %v, want [%s %s]", kinds, roster.JoinKind, roster.PresentKind)
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

// captureStdoutUntil is captureStdout for output a background goroutine
// writes: it reads the pipe while fn runs and waits, bounded, for want to
// appear. The drain claims its slot after its wait, inside the goroutine, so
// the decision to drop a join is not made by the time HandleEvent returns.
func captureStdoutUntil(t *testing.T, want string, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	var mu sync.Mutex
	var buf bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		chunk := make([]byte, 4096)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				mu.Lock()
				buf.Write(chunk[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	fn()

	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(seen(), want) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	<-read
	return seen()
}

func TestAnnounceDrain_DroppedWhenAlreadyAtTheConcurrencyCap(t *testing.T) {
	// A reconnect storm (a restart, a network blip) spawns one drain per
	// returning player; without a cap that's one open bridge connection per
	// arrival. A drain waits for a slot rather than giving it up at once,
	// so this holds the only slot for longer than that wait: what is pinned
	// here is that a player really shed is still reported, not lost in
	// silence.
	deliverer := &fakeJoinDeliverer{delivered: 0, remaining: 0}
	voice := newRecordingTellVoice()
	pctx := &plugin.Context{Voice: voice}

	out := captureStdoutUntil(t, `"event":"announce_drain_dropped_busy"`, func() {
		// Built inside the capture, not before it: *logging.Logger resolves
		// os.Stdout at construction, so a logger built before the swap would
		// keep writing to the real stdout regardless of the redirect.
		d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
		d.inFlight = make(chan struct{}, 1)
		d.inFlight <- struct{}{} // the one slot is already taken
		d.slotWait = 50 * time.Millisecond

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

// A drain outliving its own connection must not deliver. The player joined,
// the server blipped, and by the time the wait expired both they and the
// agent had reconnected: whispering the backlog then would hand it to a
// client that is loading all over again and record it, which is the loss the
// wait exists to prevent. The new connection reports them present and owes
// them the same backlog a full wait later, so exactly one delivery happens.
func TestDrainAbandonedByAReconnectIsReplacedByTheNewConnections(t *testing.T) {
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 4)}
	voice := newRecordingTellVoice()
	conns := &fakeConnections{gen: 1}
	d := NewAnnounceDrain(context.Background(), deliverer, 40*time.Millisecond, logging.New("error"), WithConnections(conns))
	pctx := &plugin.Context{Voice: voice}

	if err := d.HandleEvent(context.Background(), pctx, joinEventAt("xuid-1", 1)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	conns.reconnect()

	select {
	case <-deliverer.calls:
		t.Fatal("a drain from the ended connection delivered: its player may be mid-load on a fresh client")
	case <-time.After(250 * time.Millisecond):
	}

	if err := d.HandleEvent(context.Background(), pctx, presentEvent("xuid-1", 2)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.calls:
	case <-time.After(time.Second):
		t.Fatal("the new connection never drained the player it reported present: their backlog is stranded")
	}
	if deliverer.xuid != "xuid-1" {
		t.Errorf("DrainForJoin xuid = %q, want %q", deliverer.xuid, "xuid-1")
	}
}

// The ordinary join, with the connection still current when the wait ends.
func TestDrainDeliversWhenItsConnectionIsStillCurrent(t *testing.T) {
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 4)}
	conns := &fakeConnections{gen: 3}
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("error"), WithConnections(conns))
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	if err := d.HandleEvent(context.Background(), pctx, joinEventAt("xuid-1", 3)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.calls:
	case <-time.After(time.Second):
		t.Fatal("a join on the current connection was never drained")
	}
}

// countingDeliverer records every player it was asked to drain and holds
// each call long enough that more of them are in flight than the cap allows.
type countingDeliverer struct {
	hold time.Duration
	done chan string
}

var _ AnnounceDeliverer = (*countingDeliverer)(nil)

func (d *countingDeliverer) DrainForJoin(ctx context.Context, xuid string, _ time.Time) (int, int, error) {
	select {
	case <-time.After(d.hold):
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
	d.done <- xuid
	return 0, 0, nil
}

// A connection's opening snapshot reports everyone already online at once,
// and the cap is five. It exists to limit how many are served at a time, not
// how many are served at all: every one of them is owed their backlog, and a
// player shed here waits for a join that may not come for days.
func TestEveryPresentPlayerIsDrainedWhenTheCapIsFull(t *testing.T) {
	const players = 10
	deliverer := &countingDeliverer{hold: 20 * time.Millisecond, done: make(chan string, players)}
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("error"))
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	for i := range players {
		if err := d.HandleEvent(context.Background(), pctx, presentEvent(fmt.Sprintf("xuid-%d", i), 0)); err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
	}

	seen := make(map[string]bool, players)
	deadline := time.After(10 * time.Second)
	for len(seen) < players {
		select {
		case xuid := <-deliverer.done:
			seen[xuid] = true
		case <-deadline:
			t.Fatalf("only %d of %d present players were drained: the rest were shed and are owed until they next join", len(seen), players)
		}
	}
}

// Drains scheduled in the same instant must not wake in the same instant.
// The spread is bounded by the wait itself, so a caller that asked for no
// wait still gets none.
func TestDrainSpreadsItsWaitAndNeverExceedsIt(t *testing.T) {
	spreads := make(chan time.Duration, 1)
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 1)}
	delay := 60 * time.Millisecond
	d := NewAnnounceDrain(context.Background(), deliverer, delay, logging.New("error"),
		WithJitter(func(spread time.Duration) time.Duration {
			spreads <- spread
			return spread
		}))
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	started := time.Now()
	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never delivered")
	}

	if elapsed := time.Since(started); elapsed < delay {
		t.Errorf("delivered after %v, want at least the %v wait plus its spread", elapsed, delay)
	}
	select {
	case spread := <-spreads:
		if spread != delay {
			t.Errorf("spread offered = %v, want %v: it may never outrun the wait it is spreading", spread, delay)
		}
	default:
		t.Error("the wait was never spread: a snapshot's worth of players would wake together")
	}
}

// A caller that asked for no wait gets no spread either.
func TestDrainWithNoDelayIsNotSpread(t *testing.T) {
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 1)}
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("error"),
		WithJitter(func(time.Duration) time.Duration {
			t.Error("spread a drain that was asked to wait for nothing")
			return time.Hour
		}))
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.calls:
	case <-time.After(time.Second):
		t.Fatal("the drain never delivered")
	}
}

// Queueing for a slot is a second wait, and a connection can die inside it.
// A drain that held for a slot across a disconnect must abandon like one that
// slept through the delay: the bridge is a separate process and would accept
// the whisper, recording it against a player who is mid-reconnect.
//
// What this catches is a drain that reads the connection only before it
// queues. The connection is ended strictly after that first read and
// strictly before the slot frees, so the read before the queue cannot be
// what abandons this drain -- only a second read, taken once the slot is
// held, sees the connection it waited through end.
func TestDrainHoldingForASlotAbandonsWhenItsConnectionEnds(t *testing.T) {
	deliverer := &fakeJoinDeliverer{calls: make(chan struct{}, 1)}
	conns := &fakeConnections{gen: 1, reads: make(chan struct{}, 8)}
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("error"), WithConnections(conns))
	d.inFlight = make(chan struct{}, 1)
	d.inFlight <- struct{}{} // the only slot is taken, so the drain queues for it
	d.slotWait = 5 * time.Second
	pctx := &plugin.Context{Voice: newRecordingTellVoice()}

	if err := d.HandleEvent(context.Background(), pctx, joinEventAt("xuid-1", 1)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-conns.reads:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never read the connection it was scheduled in")
	}

	// From here the drain has already been told its connection is current,
	// so everything below happens behind that answer.
	conns.reconnect()
	<-d.inFlight // release the slot it has been queueing for

	select {
	case <-deliverer.calls:
		t.Fatal("a drain that queued for a slot across a disconnect delivered: its player may be mid-reconnect")
	case <-time.After(250 * time.Millisecond):
	}
}

// blockingDeliverer parks inside DrainForJoin until its context ends, the
// shape of a backlog part-way through its per-message bridge calls.
type blockingDeliverer struct {
	inside chan struct{}
	err    chan error
}

var _ AnnounceDeliverer = (*blockingDeliverer)(nil)

func (b *blockingDeliverer) DrainForJoin(ctx context.Context, _ string, _ time.Time) (int, int, error) {
	select {
	case b.inside <- struct{}{}:
	default:
	}
	<-ctx.Done()
	b.err <- ctx.Err()
	return 0, 0, ctx.Err()
}

// A connection that dies part-way through a backlog must stop it. The
// bridge is a separate process and stays up, so every remaining message
// would otherwise be whispered and recorded against a player this agent is
// no longer watching -- the same permanent loss, one message later.
func TestAnnounceDrain_ConnectionEndingMidSendStopsTheBacklog(t *testing.T) {
	deliverer := &blockingDeliverer{inside: make(chan struct{}, 1), err: make(chan error, 1)}
	conns := &fakeConnections{gen: 1}
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("error"), WithConnections(conns))

	if err := d.HandleEvent(t.Context(), &plugin.Context{Voice: newRecordingTellVoice()}, joinEventAt("111", 1)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.inside:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain never reached the deliverer")
	}

	conns.end()

	select {
	case err := <-deliverer.err:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("send ended with %v, want it cancelled when the connection did", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the send was never cancelled: the rest of the backlog would reach a player this connection no longer watches")
	}
}

// stoppedDeliverer parks inside DrainForJoin until its context ends, then
// reports what a real one does when a send is cancelled part-way: the
// messages it managed, the rest still owed, and no error -- sendPending
// treats cancellation as a clean stop.
type stoppedDeliverer struct {
	inside    chan struct{}
	returned  chan struct{}
	remaining int
}

var _ AnnounceDeliverer = (*stoppedDeliverer)(nil)

func (s *stoppedDeliverer) DrainForJoin(ctx context.Context, _ string, _ time.Time) (int, int, error) {
	select {
	case s.inside <- struct{}{}:
	default:
	}
	<-ctx.Done()
	defer close(s.returned)
	return 1, s.remaining, nil
}

// The summary line has to stop with the backlog it summarises. A cancelled
// send returns no error, so nothing downstream of the deliverer can tell
// this apart from a capped drain unless the connection is consulted again:
// what this catches is a trailer whispered on a context the connection-end
// cancel cannot reach, pointing a player who is mid-reconnect at an !inbox
// they are not there to read.
func TestAnnounceDrain_ConnectionEndingMidSendSendsNoSummary(t *testing.T) {
	deliverer := &stoppedDeliverer{inside: make(chan struct{}, 1), returned: make(chan struct{}), remaining: 4}
	conns := &fakeConnections{gen: 1}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("error"), WithConnections(conns))

	if err := d.HandleEvent(t.Context(), &plugin.Context{Voice: voice}, joinEventAt("111", 1)); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	select {
	case <-deliverer.inside:
	case <-time.After(2 * time.Second):
		t.Fatal("the drain never reached the deliverer")
	}

	conns.end()

	select {
	case <-deliverer.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the send was never cancelled by the connection ending")
	}
	select {
	case msg := <-voice.told:
		t.Fatalf("summary %q was whispered after the connection ended: its player may be mid-reconnect", msg)
	case <-time.After(250 * time.Millisecond):
	}
}

// drainPresence answers presence from a fixed set.
type drainPresence struct{ online map[string]bool }

var _ plugin.Presence = drainPresence{}

func (p drainPresence) IsOnline(xuid string) bool { return p.online[xuid] }

// A drain that delivered nothing because the player left reports the whole
// backlog as still owed, which reads identically to the cap holding it back.
// The trailer must not follow: the player is not there to read it, so it is
// a console line to nobody counted as a summary they saw. Their backlog stays
// pending either way, which is the point of the deliverer's own guard.
func TestNoTrailerWhenTheBacklogIsOwedBecauseThePlayerLeft(t *testing.T) {
	deliverer := &fakeJoinDeliverer{delivered: 0, remaining: 3, calls: make(chan struct{}, 1)}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
	pctx := &plugin.Context{Voice: voice, Presence: drainPresence{online: map[string]bool{}}}

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
		t.Errorf("Tell was called with %q, want silence for a player who is not on the server", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// The same backlog, for a player who is still here: the cap held it back, so
// the trailer is exactly what they need.
func TestTrailerStillFollowsForAPlayerWhoIsStillOnline(t *testing.T) {
	deliverer := &fakeJoinDeliverer{delivered: 1, remaining: 3, calls: make(chan struct{}, 1)}
	voice := newRecordingTellVoice()
	d := NewAnnounceDrain(context.Background(), deliverer, 0, logging.New("info"))
	pctx := &plugin.Context{Voice: voice, Presence: drainPresence{online: map[string]bool{"xuid-1": true}}}

	if err := d.HandleEvent(context.Background(), pctx, joinEvent("xuid-1")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case msg := <-voice.told:
		if !strings.Contains(msg, "!inbox") {
			t.Errorf("Tell = %q, want the trailer pointing at !inbox", msg)
		}
	case <-time.After(time.Second):
		t.Error("no trailer for a player who is still online and still owed messages")
	}
}
