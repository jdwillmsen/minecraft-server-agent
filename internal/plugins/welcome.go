package plugins

import (
	"context"
	"fmt"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
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

	// The profile is read here rather than in the goroutine, and the arrival
	// is recorded in the same call: RecordJoin returns the player's state as
	// it stood *before* this join, which is the only moment "how long since I
	// last saw you" is still answerable.
	var profile store.Profile
	enabled := pctx.Profiles != nil && pctx.Profiles.Enabled()

	var err error
	if pctx.Profiles != nil {
		profile, err = pctx.Profiles.RecordJoin(ctx, join.XUID, join.Username, time.Now())
	}
	if err != nil {
		// Greet anyway. A database problem should cost the personalisation,
		// never the welcome.
		w.log.Error("welcome_profile_failed", logging.Fields{"xuid": join.XUID, "error": err.Error()})
		profile = store.Profile{}
	}

	voice := pctx.Voice
	go w.greetAfterDelay(voice, join, profile, enabled)
	return nil
}

func (w *Welcome) greetAfterDelay(voice plugin.Voice, join roster.JoinEvent, profile store.Profile, enabled bool) {
	select {
	case <-time.After(w.delay):
	case <-w.rootCtx.Done():
		return
	}

	sayCtx, cancel := context.WithTimeout(w.rootCtx, plugin.DefaultDispatchTimeout)
	defer cancel()
	if err := voice.Say(sayCtx, greeting(join.Username, profile, enabled, time.Now())); err != nil {
		w.log.Error("welcome_say_failed", logging.Fields{"xuid": join.XUID, "error": err.Error()})
	}
}

// greeting picks what to say to an arriving player.
//
// Pure and separate from the sending so the wording can be tested without a
// server, a database or a two-second delay. profile is the player's state
// *before* this arrival: store.RecordJoin returns it that way precisely so
// "welcome back" is never said to someone arriving for the first time.
//
// An empty store yields the plain greeting, which is what the agent said
// through Stages 1-4 and what it must keep saying when Postgres is down.
// Losing the database should cost personalisation, never the greeting.
func greeting(name string, profile store.Profile, enabled bool, now time.Time) string {
	if name == "" {
		name = "a new player"
	}
	if !enabled {
		return fmt.Sprintf("Welcome, %s!", name)
	}
	if profile.New() {
		return fmt.Sprintf("Welcome to the server, %s! First time here - say !help to see what I can do.", name)
	}

	away := profile.AwayFor(now)
	switch {
	case away >= 30*24*time.Hour:
		return fmt.Sprintf("Welcome back, %s! It has been %d days.", name, int(away.Hours()/24))
	case away >= 7*24*time.Hour:
		return fmt.Sprintf("Welcome back, %s! Long time no see - %d days.", name, int(away.Hours()/24))
	case profile.JoinCount >= 100:
		// Milestones are worth noticing out loud; a hundred visits is a
		// regular, not a visitor.
		return fmt.Sprintf("Welcome back, %s! That is visit number %d.", name, profile.JoinCount)
	default:
		return fmt.Sprintf("Welcome back, %s!", name)
	}
}
