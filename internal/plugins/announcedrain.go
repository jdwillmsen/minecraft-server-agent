package plugins

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// AnnounceDeliverer is what AnnounceDrain needs on a join: hand a player
// everything they're owed, capped, and report what's left. Narrowed from
// announce.Deliverer's full method set rather than reusing
// plugin.AnnounceDeliverer, which exists for !announce and !inbox and
// exposes SendNow/DrainAll instead — neither is this plugin's job.
type AnnounceDeliverer interface {
	DrainForJoin(ctx context.Context, xuid string, now time.Time) (delivered, remaining int, err error)
}

// Connections reports which of the agent's connections is live right now,
// as a number that only ever grows. A drain waits before it delivers, and
// the connection it was scheduled in can end inside that wait; the one that
// replaces it re-reports everyone still online, so the waiting drain is a
// duplicate whose player may by then be loading a fresh client.
type Connections interface {
	Generation() uint64
	// Ended is closed when the connection live at the time of the call
	// ends. Captured before a delivery starts, it is what stops a backlog
	// mid-send: the generation is read once and cannot report a drop that
	// happens between one whispered message and the next.
	Ended() <-chan struct{}
}

// drainTimeout bounds one join's delivery. A backlog can be several
// messages, each its own bridge call, so this is generous compared to a
// single command dispatch's DefaultDispatchTimeout — but it is still a
// bound, since nothing else waits on this goroutine and it must not hold a
// bridge connection open forever.
const drainTimeout = 30 * time.Second

// maxConcurrentDrains caps join drains in flight across every player, the
// same shape startAnswer uses to cap @server answers (cmd/agent/main.go).
//
// Kept larger than that budget on purpose, because dropping one costs less.
// An answer that misses its slot is lost outright and the player who asked
// gets nothing, so that cap is held down to what the model budget can bear.
// A dropped drain merely defers those messages to the player's next join or
// their own !inbox -- nothing is lost -- so this can be big enough that an
// ordinary handful of simultaneous arrivals isn't shed, while still giving
// a reconnect storm (a restart, a network blip) somewhere to stop opening
// one bridge connection per returning player.
const maxConcurrentDrains = 5

// drainSpread is how far apart drains scheduled in the same instant are
// pulled. A connection's opening snapshot reports everyone already online
// at once, so without this their waits expire together and the cap turns a
// crowd into a queue of one batch. Never longer than the wait itself, so a
// delay of zero still delivers immediately.
const drainSpread = 3 * time.Second

// slotWait is how long a woken drain will wait for one of the
// maxConcurrentDrains slots before giving up. The cap exists to limit how
// many players are served at once, not how many are served at all: a
// reconnect wave that fills every slot should take longer, not lose people.
// Still bounded, because a drain that waited forever would outlive the
// reason anyone wanted it.
const slotWait = drainTimeout

// AnnounceDrain hands a newly-joined player everything queued for them, then
// — only when the per-join cap left something behind — says one line
// pointing at !inbox. Kept separate from Welcome even though both react to
// the same join: they share an event, not a wording or a dependency —
// Welcome greets from a player profile, this drains an outbox.
type AnnounceDrain struct {
	// rootCtx is the process-lifetime context, captured at construction for
	// the same reason Welcome's is: HandleEvent's own ctx is bounded by the
	// dispatcher's short per-call timeout, which would cancel a
	// several-message drain before it finished.
	rootCtx   context.Context
	deliverer AnnounceDeliverer
	// delay is how long to wait after a join before delivering. A whisper
	// sent the moment the roster reports a join is accepted by the server
	// and shown to nobody: the client is still loading, and the delivery is
	// recorded, so nothing ever retries it. Two announcements were lost
	// that way on 2026-09-11, 0.8s after the join. Long enough that the
	// greeting has already spoken, so the backlog follows it rather than
	// racing it.
	delay time.Duration
	log   *logging.Logger
	// unready fires for the first drain the database refuses because its
	// tables are missing or ungranted. Every join hits the same wall until
	// the deploy that fixes it, so this is said once and at INFO: an
	// error line per arrival would bury the rest of the log for as long as
	// the release and its migration are out of step, and nothing after the
	// first one carries information.
	unready sync.Once
	// conns tells a woken drain whether the connection it was scheduled in
	// is still the current one. Nil unless WithConnections is passed, which
	// leaves every existing caller and test delivering unconditionally.
	conns Connections
	// slotWait bounds how long this drain waits for a free slot, from
	// slotWait unless a test shortens it.
	slotWait time.Duration
	// jitter picks how much to add to delay for one drain, given the widest
	// spread that fits. Injectable so a test gets the same schedule every
	// run; random otherwise, which is the whole point of it.
	jitter func(spread time.Duration) time.Duration
	// inFlight is a counting semaphore over drains in progress, capped at
	// maxConcurrentDrains. Claimed after the wait and inside the goroutine,
	// never across the wait: held across it, players still waiting would
	// occupy every slot while doing nothing and the rest would be shed --
	// exactly the shape of a rejoin wave after a restart.
	inFlight chan struct{}
}

// DrainOption configures an AnnounceDrain at construction.
type DrainOption func(*AnnounceDrain)

// WithConnections makes a drain abandon itself when the connection it was
// scheduled in ends before its wait does.
func WithConnections(c Connections) DrainOption {
	return func(a *AnnounceDrain) { a.conns = c }
}

// WithJitter replaces the random spread between simultaneous drains, so a
// test can schedule them deterministically.
func WithJitter(f func(spread time.Duration) time.Duration) DrainOption {
	return func(a *AnnounceDrain) { a.jitter = f }
}

// NewAnnounceDrain builds the announce-drain plugin. rootCtx should be the
// process lifetime context (cancelled on shutdown), not a per-request one.
func NewAnnounceDrain(rootCtx context.Context, deliverer AnnounceDeliverer, delay time.Duration, log *logging.Logger, opts ...DrainOption) *AnnounceDrain {
	a := &AnnounceDrain{rootCtx: rootCtx, deliverer: deliverer, delay: delay, log: log, slotWait: slotWait, jitter: randomJitter, inFlight: make(chan struct{}, maxConcurrentDrains)}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

func randomJitter(spread time.Duration) time.Duration {
	if spread <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(spread)))
}

// wait is how long this drain sleeps before delivering: the shared delay,
// plus a spread that keeps a snapshot's worth of players from waking as one.
// A zero delay stays zero, so a test that asked for no wait gets none.
func (a *AnnounceDrain) wait() time.Duration {
	if a.delay <= 0 {
		return 0
	}
	spread := drainSpread
	if a.delay < spread {
		spread = a.delay
	}
	return a.delay + a.jitter(spread)
}

// stale reports whether the connection this drain was scheduled in has
// ended. Its replacement re-reports everyone still online and owes them
// their own delivery a full wait from now, so speaking here would whisper a
// backlog to a client that may be loading all over again -- and record it,
// which is the loss the wait exists to prevent. The bridge is a separate
// process and answers either way, so nothing else stops it.
func (a *AnnounceDrain) stale(generation uint64) bool {
	return a.conns != nil && a.conns.Generation() != generation
}

func (*AnnounceDrain) Name() string { return "announce-drain" }

// Commands is empty: this plugin only reacts to a join, it exposes no !
// commands.
func (*AnnounceDrain) Commands() []plugin.Command { return nil }

// Kinds satisfies plugin.EventHandler. A player already online when a
// connection opens gets the same delayed delivery as an arrival: they are
// not greeted and they are not new, but they may have reconnected moments
// before the agent did, and nothing else will ever owe them their backlog
// for this connection.
func (*AnnounceDrain) Kinds() []string { return []string{roster.JoinKind, roster.PresentKind} }

var _ plugin.Plugin = (*AnnounceDrain)(nil)
var _ plugin.EventHandler = (*AnnounceDrain)(nil)

// HandleEvent starts the drain in its own goroutine and returns
// immediately, per EventHandler's documented contract: the dispatcher
// calling this runs on the Bedrock packet read loop, and delivery makes one
// bridge call per queued message.
func (a *AnnounceDrain) HandleEvent(ctx context.Context, pctx *plugin.Context, ev bus.Event) error {
	var xuid string
	var generation uint64
	switch e := ev.(type) {
	case roster.JoinEvent:
		xuid, generation = e.XUID, e.Generation
	case roster.PresentEvent:
		xuid, generation = e.XUID, e.Generation
	default:
		return fmt.Errorf("announcedrain: unexpected event type %T for kind %s", ev, ev.Kind())
	}
	if a.deliverer == nil {
		// Nothing to drain through. Whether the store behind a real
		// deliverer persists anything is that deliverer's question to
		// answer -- it reports zero drained and nothing owed for a
		// disabled one, which this handles as the quiet join it is.
		return nil
	}

	voice := pctx.Voice
	// Captured now, with the generation: both describe the connection this
	// delivery belongs to, and the channel is what a send already in
	// progress watches.
	var ended <-chan struct{}
	if a.conns != nil {
		ended = a.conns.Ended()
	}
	go func() {
		if wait := a.wait(); wait > 0 {
			select {
			case <-time.After(wait):
			case <-a.rootCtx.Done():
				return
			}
		}
		if a.stale(generation) {
			return
		}
		// Claimed after the wait, never across it. Held across it, five
		// players rejoining inside one wait would take every slot while
		// doing nothing, and the sixth onwards would be shed -- which is
		// exactly the shape of the rejoin wave after a restart, when
		// backlogs are likeliest to be owed.
		waited, cancelWait := context.WithTimeout(a.rootCtx, a.slotWait)
		select {
		case a.inFlight <- struct{}{}:
			cancelWait()
		case <-waited.Done():
			cancelWait()
			if a.rootCtx.Err() != nil {
				// Shutting down, not shedding: nothing is owed a log line
				// for a wait the process itself ended.
				return
			}
			a.log.Info("announce_drain_dropped_busy", logging.Fields{"xuid": xuid, "max_concurrent": cap(a.inFlight), "waited_ms": a.slotWait.Milliseconds()})
			return
		}
		defer func() { <-a.inFlight }()
		// Read again: queueing for a slot is a second wait, and a drain
		// that spent it while the connection died would deliver into the
		// gap exactly as one that slept through the first would.
		if a.stale(generation) {
			return
		}
		a.drain(voice, xuid, ended)
	}()
	return nil
}

// drain sends xuid everything the per-join cap allows, then — only if
// something was left behind — whispers one line pointing at !inbox. A
// summary is never added when remaining is 0: the welcome message already
// owns this moment, and an empty inbox has nothing to add to it.
func (a *AnnounceDrain) drain(voice plugin.Voice, xuid string, ended <-chan struct{}) {
	drainCtx, cancel := context.WithTimeout(a.rootCtx, drainTimeout)
	defer cancel()

	// Cancelled the moment the connection ends, so a backlog stops between
	// messages instead of whispering the rest of itself at a player this
	// agent is no longer watching -- and recording each one as delivered.
	// The bridge is a separate process and stays up, so nothing else fails.
	if ended != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ended:
				cancel()
			case <-stop:
			}
		}()
	}

	_, remaining, err := a.deliverer.DrainForJoin(drainCtx, xuid, time.Now())
	if pgerr.Unready(err) {
		a.unready.Do(func() {
			a.log.Info("announce_drain_unready", logging.Fields{"error": err.Error()})
		})
		return
	}
	if err != nil {
		a.log.Error("announce_drain_failed", logging.Fields{"xuid": xuid, "error": err.Error()})
		return
	}
	if remaining == 0 {
		return
	}
	if voice == nil {
		a.log.Error("announce_drain_summary_undeliverable", logging.Fields{"xuid": xuid})
		return
	}

	// A fresh bound rather than whatever's left of drainCtx: the summary is
	// one short whisper, no reason for it to inherit a budget that a long
	// backlog may have already spent down to nothing.
	tellCtx, tellCancel := context.WithTimeout(a.rootCtx, plugin.DefaultDispatchTimeout)
	defer tellCancel()
	err = voice.Tell(tellCtx, xuid, drainSummary(remaining))
	metrics.AnnounceDelivery(metrics.DeliverySummary, err)
	if err != nil {
		a.log.Error("announce_drain_summary_failed", logging.Fields{"xuid": xuid, "error": err.Error()})
	}
}

// drainSummary names how many announcements are still owed after the
// per-join cap. It points at !inbox rather than repeating them here — that
// command already owns draining the rest, and never ends in a question
// mark, matching every other reply this plugin family sends.
func drainSummary(remaining int) string {
	if remaining == 1 {
		return "1 more message is waiting - say !inbox to see it."
	}
	return fmt.Sprintf("%d more messages are waiting - say !inbox to see them.", remaining)
}
