package plugins

import (
	"context"
	"fmt"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
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
// Kept smaller than that budget on purpose: an answer that misses its slot
// is lost outright, so that cap is sized for throughput. A dropped drain
// merely defers those messages to the player's next join or their own
// !inbox -- nothing is lost -- so this only needs to be big enough that an
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
	log       *logging.Logger
	// inFlight is a counting semaphore over drains in progress, capped at
	// maxConcurrentDrains. Acquired non-blockingly in HandleEvent, before
	// the goroutine is even spawned: a caller that blocked here would stall
	// the dispatcher this plugin exists to get off of.
	inFlight chan struct{}
}

// NewAnnounceDrain builds the announce-drain plugin. rootCtx should be the
// process lifetime context (cancelled on shutdown), not a per-request one.
func NewAnnounceDrain(rootCtx context.Context, deliverer AnnounceDeliverer, log *logging.Logger) *AnnounceDrain {
	return &AnnounceDrain{rootCtx: rootCtx, deliverer: deliverer, log: log, inFlight: make(chan struct{}, maxConcurrentDrains)}
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
		// No store configured: there is nothing to drain and nothing to
		// say, the same silence a disabled Deliverer would itself return.
		return nil
	}

	select {
	case a.inFlight <- struct{}{}:
	default:
		// Dropped, not queued: queued announcements aren't urgent, so
		// deferring them to this player's next join or their own !inbox
		// costs nothing that a wait would preserve -- unlike startAnswer's
		// drop, there is no reply being discarded here, only a delay.
		a.log.Info("announce_drain_dropped_busy", logging.Fields{"xuid": join.XUID, "max_concurrent": cap(a.inFlight)})
		return nil
	}

	voice := pctx.Voice
	go func() {
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
	if err := voice.Tell(tellCtx, xuid, drainSummary(remaining)); err != nil {
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
