package plugins

import (
	"context"
	"fmt"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
)

// Welcome greets a genuinely new arrival with one deterministic message.
// No first-join/returning/long-absence variants yet — that needs player
// history, which doesn't exist until a later stage's database lands; a
// single fixed template is correct and complete for this one.
type Welcome struct {
	// rootCtx is the process-lifetime context, captured at construction
	// rather than taken from HandleEvent's own ctx parameter. HandleEvent's
	// ctx is bounded by the plugin dispatcher's own short per-call timeout
	// and would cancel a multi-second delayed greeting before it ever
	// fires — the greeting's lifetime belongs to the process, not to one
	// dispatch call.
	rootCtx context.Context
	// delay is how long to wait after a join before greeting. There is a
	// known open upstream crash-on-join defect on this server, so the
	// agent must never react in the first moments of a session.
	delay time.Duration
	log   *logging.Logger
}

// NewWelcome builds the welcome plugin. rootCtx should be the process
// lifetime context (cancelled on shutdown), not a per-request one.
func NewWelcome(rootCtx context.Context, delay time.Duration, log *logging.Logger) *Welcome {
	return &Welcome{rootCtx: rootCtx, delay: delay, log: log}
}

func (*Welcome) Name() string { return "welcome" }

// Commands is empty: welcome only reacts to events, it exposes no !
// commands.
func (*Welcome) Commands() []plugin.Command { return nil }

// Kinds satisfies plugin.EventHandler.
func (*Welcome) Kinds() []string { return []string{roster.JoinKind} }

var _ plugin.Plugin = (*Welcome)(nil)
var _ plugin.EventHandler = (*Welcome)(nil)

// HandleEvent starts the delayed greeting in its own goroutine and returns
// immediately, per EventHandler's documented contract that a handler must
// not block for long.
func (w *Welcome) HandleEvent(ctx context.Context, pctx *plugin.Context, ev bus.Event) error {
	join, ok := ev.(roster.JoinEvent)
	if !ok {
		return fmt.Errorf("welcome: unexpected event type %T for kind %s", ev, ev.Kind())
	}

	voice := pctx.Voice
	go w.greetAfterDelay(voice, join)
	return nil
}

func (w *Welcome) greetAfterDelay(voice plugin.Voice, join roster.JoinEvent) {
	select {
	case <-time.After(w.delay):
	case <-w.rootCtx.Done():
		return
	}

	name := join.Username
	if name == "" {
		name = "a new player"
	}

	sayCtx, cancel := context.WithTimeout(w.rootCtx, plugin.DefaultDispatchTimeout)
	defer cancel()
	if err := voice.Say(sayCtx, fmt.Sprintf("Welcome, %s!", name)); err != nil {
		w.log.Error("welcome_say_failed", logging.Fields{"xuid": join.XUID, "error": err.Error()})
	}
}
