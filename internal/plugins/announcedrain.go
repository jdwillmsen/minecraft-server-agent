package plugins

import (
	"context"
	"fmt"
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
	// inFlight is a counting semaphore over drains in progress, capped at
	// maxConcurrentDrains. Claimed non-blockingly, after the wait and inside
	// the goroutine: a caller that blocked for it would stall the dispatcher
	// this plugin exists to get off of, and claiming it before the wait
	// would let waiting players crowd out delivering ones.
	inFlight chan struct{}
}

// NewAnnounceDrain builds the announce-drain plugin. rootCtx should be the
// process lifetime context (cancelled on shutdown), not a per-request one.
func NewAnnounceDrain(rootCtx context.Context, deliverer AnnounceDeliverer, delay time.Duration, log *logging.Logger) *AnnounceDrain {
	return &AnnounceDrain{rootCtx: rootCtx, deliverer: deliverer, delay: delay, log: log, inFlight: make(chan struct{}, maxConcurrentDrains)}
}

func (*AnnounceDrain) Name() string { return "announce-drain" }

// Commands is empty: this plugin only reacts to a join, it exposes no !
// commands.
func (*AnnounceDrain) Commands() []plugin.Command { return nil }

// Kinds satisfies plugin.EventHandler.
func (*AnnounceDrain) Kinds() []string { return []string{roster.JoinKind} }

var _ plugin.Plugin = (*AnnounceDrain)(nil)
var _ plugin.EventHandler = (*AnnounceDrain)(nil)

// HandleEvent starts the drain in its own goroutine and returns
// immediately, per EventHandler's documented contract: the dispatcher
// calling this runs on the Bedrock packet read loop, and delivery makes one
// bridge call per queued message.
func (a *AnnounceDrain) HandleEvent(ctx context.Context, pctx *plugin.Context, ev bus.Event) error {
	join, ok := ev.(roster.JoinEvent)
	if !ok {
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
	go func() {
		if a.delay > 0 {
			select {
			case <-time.After(a.delay):
			case <-a.rootCtx.Done():
				return
			}
		}
		// Claimed after the wait, never across it. Held across it, five
		// players rejoining inside one wait would take every slot while
		// doing nothing, and the sixth onwards would be dropped -- which is
		// exactly the shape of the rejoin wave after a restart, when
		// backlogs are likeliest to be owed.
		select {
		case a.inFlight <- struct{}{}:
		default:
			a.log.Info("announce_drain_dropped_busy", logging.Fields{"xuid": join.XUID, "max_concurrent": cap(a.inFlight)})
			return
		}
		defer func() { <-a.inFlight }()
		a.drain(voice, join.XUID)
	}()
	return nil
}

// drain sends xuid everything the per-join cap allows, then — only if
// something was left behind — whispers one line pointing at !inbox. A
// summary is never added when remaining is 0: the welcome message already
// owns this moment, and an empty inbox has nothing to add to it.
func (a *AnnounceDrain) drain(voice plugin.Voice, xuid string) {
	drainCtx, cancel := context.WithTimeout(a.rootCtx, drainTimeout)
	defer cancel()

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
