package main

import (
	"context"
	"runtime/debug"
	"sync/atomic"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/presence"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// rejoinLine is what the agent says as it comes back into the world after
// being parked, so the players who saw it leave know it is back.
const rejoinLine = "I'm back in the world."

// presenceRuntime is actor presence as this binary runs it: built once for
// the process, led once per turn as the live agent.
type presenceRuntime struct {
	svc    *presence.Service
	gate   *presence.Gate
	joins  *presence.JoinLog
	loop   *presence.Loop
	api    *presence.API
	plugin *presence.ChatPlugin
}

// newPresence builds every presence part from configuration. It is called
// before the profile store is wrapped, because, like the other stores, it
// borrows the concrete Postgres's pool.
func newPresence(cfg config.Config, profiles store.Store, auditor audit.Store, console presence.Console, playerRoster *roster.Roster, log *logging.Logger) (*presenceRuntime, error) {
	var st presence.Store = presence.Nop{}
	if pg, ok := profiles.(*store.Postgres); ok && pg.Pool() != nil {
		st = presence.NewPostgres(pg.Pool())
	}
	return buildPresence(cfg, st, auditor, console, playerRoster, log)
}

// buildPresence is newPresence once the database is decided.
func buildPresence(cfg config.Config, st presence.Store, auditor audit.Store, console presence.Console, playerRoster *roster.Roster, log *logging.Logger) (*presenceRuntime, error) {
	actors := make([]presence.Actor, 0, len(cfg.PresenceActors))
	for _, a := range cfg.PresenceActors {
		actors = append(actors, presence.Actor{ID: a.ID, Gamertag: a.Gamertag, Kind: a.Kind, Groups: a.Groups, Default: presenceapi.State(a.DefaultState)})
	}
	reg, err := presence.NewRegistry(actors, cfg.PresenceSelfID)
	if err != nil {
		return nil, err
	}
	svc := presence.NewService(reg, st, auditor, log)

	// The configured default until the loop's first tick reads the stored
	// answer, so a leader parked by git never joins on its way to finding
	// out, and one whose table is not migrated yet keeps to git.
	present := true
	if self, ok := reg.Actor(reg.SelfID()); ok {
		present = self.Default == presenceapi.StatePresent
	}
	gate := presence.NewGate(present)
	joins := presence.NewJoinLog()
	// NewLoop takes the service's one change callback for its nudge.
	loop := presence.NewLoop(presence.LoopConfig{
		Service: svc, Console: console, Roster: playerRoster, Joins: joins, Gate: gate,
		SessionUp: httpapi.Connected, Version: processVersion(), Log: log,
	})

	tokens := make([]presence.Token, 0, len(cfg.PresenceTokens))
	for _, t := range cfg.PresenceTokens {
		tokens = append(tokens, presence.Token{Name: t.Name, Secret: t.Token, Scopes: t.Scopes, Actor: t.Actor})
	}
	return &presenceRuntime{
		svc: svc, gate: gate, joins: joins, loop: loop,
		api:    presence.NewAPI(svc, tokens, log),
		plugin: presence.NewChatPlugin(svc, playerRoster, joins),
	}, nil
}

// sessionGate decides whether the live agent is in the world: by its own
// actor's effective presence when presence is configured, and always
// otherwise, exactly as before presence existed.
func (p *presenceRuntime) sessionGate() sessionGate {
	if !p.svc.Enabled() {
		return alwaysPresent{}
	}
	return p.gate
}

// lead starts presence's share of a turn as the live agent. The first tick
// runs before it returns, so the gate holds the stored answer by the time
// the session lifecycle first asks it.
func (p *presenceRuntime) lead(ctx context.Context) {
	if !p.svc.Enabled() {
		return
	}
	p.loop.Prime(ctx)
	go p.loop.Run(ctx)
}

// presenceModes adds what presence needs around the session modes.
//
// A parked leader reports ready. It is doing its job -- monitoring, the
// policy loop, the API -- and an unready one would fall out of the Service
// the bots poll. The session sets readiness again when it reaches spawn,
// so readiness is cleared as the absence ends.
//
// runSessions never runs two modes at once, but it runs each on its own
// goroutine, so the flag between them is atomic.
func presenceModes(modes sessionModes, setReady func(bool), rejoined func(context.Context)) sessionModes {
	var away atomic.Bool
	return sessionModes{
		present: func(ctx context.Context) {
			if away.Swap(false) {
				rejoined(ctx)
			}
			modes.present(ctx)
		},
		left: modes.left,
		absent: func(ctx context.Context) {
			away.Store(true)
			setReady(true)
			defer setReady(false)
			modes.absent(ctx)
		},
	}
}

// announceRejoin says the agent is back, through the bridge. It does not
// need the session that is only now being dialled.
func announceRejoin(voice plugin.Voice, log *logging.Logger) func(context.Context) {
	return func(ctx context.Context) {
		sayCtx, cancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
		defer cancel()
		if err := voice.Say(sayCtx, rejoinLine); err != nil {
			log.Error("presence_rejoin_say_failed", logging.Fields{"error": err.Error()})
		}
	}
}

// processVersion is this binary's module version, as reported in the
// agent's own status row. An image built without VCS metadata reports
// "(devel)", which is still the truth about what the build knew.
func processVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}
