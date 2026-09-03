// Command agent is minecraft-server-agent: the chat "ear" for the FWB
// Bedrock server. It connects as a headless client, reads chat, and
// dispatches ! commands and @server mentions to registered plugins.
//
// Stage 2 wires a real mc-console-bridge-backed Voice and Facts, live
// permission resolution from permissions.json, and join-triggered
// welcomes; the LLM answer path lands in a later stage.
package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/mcauth"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
)

// welcomeDelay is how long the welcome plugin waits after a join before
// greeting, to stay clear of the documented open upstream crash-on-join
// defect on this server.
const welcomeDelay = 5 * time.Second

// stableSessionThreshold mirrors minecraft-afk-bot: the reconnect backoff
// only resets to its minimum once a session has stayed up at least this
// long, so a server that accepts a connection and immediately drops it
// (e.g. the documented upstream join crash) doesn't cause a reconnect
// storm at full speed.
const stableSessionThreshold = 60 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	log := logging.New(cfg.LogLevel)

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("shutdown_signal", logging.Fields{"signal": sig.String()})
		cancel()
	}()

	// roster is the live XUID<->gamertag mapping, fed from PlayerList
	// packets (see handlePlayerList). It serves two needs: join detection
	// for the welcome plugin, and gamertag resolution for BridgeVoice.Tell
	// (which only ever receives an XUID).
	playerRoster := roster.New()

	bridgeTimeout := time.Duration(cfg.ConsoleBridgeTimeoutMs) * time.Millisecond
	bridgeClient := adapters.NewBridgeClient(cfg.ConsoleBridgeURL, cfg.ConsoleBridgeToken, bridgeTimeout)
	permResolver := adapters.NewPermissionResolver(bridgeClient, adapters.DefaultPermissionsCacheTTL, log)

	registry := plugin.NewRegistry()
	if err := registry.Register(plugins.NewCore()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "core", "error": err.Error()})
		os.Exit(1)
	}
	if err := registry.Register(plugins.NewStats()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "stats", "error": err.Error()})
		os.Exit(1)
	}
	welcomePlugin := plugins.NewWelcome(ctx, welcomeDelay, log)
	if err := registry.Register(welcomePlugin); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "welcome", "error": err.Error()})
		os.Exit(1)
	}

	pctx := &plugin.Context{
		Voice:     adapters.NewBridgeVoice(bridgeClient, playerRoster),
		Facts:     adapters.NewBridgeFacts(bridgeClient),
		Directory: registry,
	}
	// Every answerable chat message and every roster join is published
	// here; event-driven plugins (welcome) subscribe via startEventDispatch
	// rather than touching the connection directly.
	eventBus := bus.New()
	limiter := ratelimit.NewPerActor(cfg.CommandRateLimitPerMinute, time.Minute)
	startEventDispatch(ctx, eventBus, registry, pctx, log)

	httpServer, err := httpapi.New(cfg.HTTPAddr)
	if err != nil {
		// A bind failure here (bad address, port already in use) means the
		// agent would run with no /healthz, /readyz, or /metrics at all -
		// worse than not starting, since nothing external would notice.
		log.Error("http_bind_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			log.Error("http_server_failed", logging.Fields{"error": err.Error()})
			// The listener already succeeded in New, so an error reaching
			// here means Serve itself broke (not merely "someone else had
			// the port") - treat it as fatal rather than silently running
			// on with no health/metrics surface.
			cancel()
		}
	}()

	// Built once, outside the reconnect loop: the refresh token is kept in
	// memory across reconnects instead of being re-derived from disk (and,
	// on a load failure, potentially re-triggering an interactive
	// device-code login) on every single attempt.
	ts, err := mcauth.TokenSource(ctx, cfg.AuthCacheDir, cfg.MCUsername, os.Stdout)
	if err != nil {
		log.Error("auth_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	log.Info("starting", logging.Fields{"mc_host": cfg.MCHost, "mc_port": cfg.MCPort})
	runConnectLoop(ctx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, permResolver)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http_shutdown_failed", logging.Fields{"error": err.Error()})
	}
	log.Info("stopped", nil)
}

// runConnectLoop owns the reconnect/backoff policy. Each iteration runs one
// session to completion (or failure), then waits before trying again.
func runConnectLoop(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver) {
	minDelay := time.Duration(cfg.ReconnectMinMs) * time.Millisecond
	maxDelay := time.Duration(cfg.ReconnectMaxMs) * time.Millisecond
	delay := minDelay
	firstAttempt := true

	for ctx.Err() == nil {
		if !firstAttempt {
			httpapi.IncReconnect()
		}
		firstAttempt = false

		started := time.Now()
		err := session(ctx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, permResolver)
		lasted := time.Since(started)
		httpServer.SetReady(false)

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error("session_error", logging.Fields{"error": err.Error(), "session_lasted_ms": lasted.Milliseconds()})
		} else {
			log.Info("disconnected", logging.Fields{"session_lasted_ms": lasted.Milliseconds()})
		}

		delay = nextDelay(delay, lasted, minDelay, maxDelay)
		wait := jitter(delay)
		log.Info("reconnecting", logging.Fields{"delay_ms": wait.Milliseconds()})

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// nextDelay is the reconnect backoff step: a session that stayed up at
// least stableSessionThreshold is treated as healthy and resets the delay
// to min, while anything shorter doubles the previous delay up to max.
func nextDelay(current, lasted, min, max time.Duration) time.Duration {
	if lasted >= stableSessionThreshold {
		return min
	}
	next := current * 2
	if next < min {
		return min
	}
	if next > max {
		return max
	}
	return next
}

// jitter randomizes d to 50-100% of its value, so multiple agent instances
// reconnecting after a shared server outage don't all retry in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// session runs one connection to the server from login through to
// disconnect, dispatching chat commands as they arrive. It returns nil on a
// clean disconnect and a non-nil error on anything else (including ctx
// cancellation surfaced as a read error, which the caller ignores because
// it checks ctx.Err() itself).
func session(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver) error {
	dialer := minecraft.Dialer{TokenSource: ts}
	addr := net.JoinHostPort(cfg.MCHost, strconv.Itoa(cfg.MCPort))

	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := dialer.DialContext(dialCtx, "raknet", addr)
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// conn.ReadPacket below only unblocks on the connection's own context,
	// which gophertunnel derives from the RakNet link rather than from the
	// ctx passed to DialContext - so closing the connection is the only way
	// a shutdown signal can interrupt a read on an idle server.
	sessionDone := make(chan struct{})
	defer close(sessionDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-sessionDone:
		}
	}()

	log.Info("joined", logging.Fields{"address": addr})

	spawnCtx, spawnCancel := context.WithTimeout(ctx, 30*time.Second)
	err = conn.DoSpawnContext(spawnCtx)
	spawnCancel()
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	// Anything the roster still holds predates this connection and cannot
	// be trusted: leaves that happened while disconnected were never seen.
	// The server's own opening PlayerList repopulates it from scratch.
	playerRoster.BeginSession()

	selfXUID := conn.IdentityData().XUID
	log.Info("spawned", logging.Fields{"self_xuid": selfXUID})
	httpServer.SetReady(true)
	httpapi.SetConnected(true)
	defer httpapi.SetConnected(false)

	// TODO(stage 2+): populate from a real sibling-bot roster (e.g. the
	// AFK bots) once one exists, rather than an empty set.
	siblingXUIDs := map[string]struct{}{}

	for {
		if ctx.Err() != nil {
			return nil
		}
		pk, err := conn.ReadPacket()
		if err != nil {
			return err
		}
		handlePacket(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus, limiter, playerRoster, permResolver)
	}
}

func handlePacket(ctx context.Context, pk packet.Packet, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver) {
	switch pk := pk.(type) {
	case *packet.Text:
		handleText(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus, limiter, permResolver)
	case *packet.PlayerList:
		handlePlayerList(pk, selfXUID, siblingXUIDs, log, eventBus, playerRoster)
	}
}

func handleText(ctx context.Context, text *packet.Text, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, permResolver *adapters.PermissionResolver) {
	if !chat.IsAnswerableType(text.TextType) {
		return
	}

	id, ok := chat.Identity(text)
	if !ok {
		return
	}
	if chat.IsSelfOrSibling(id, selfXUID, siblingXUIDs) {
		return
	}

	trigger := chat.ParseTrigger(text.Message)
	eventBus.Publish(chat.MessageEvent{ActorXUID: id, Message: text.Message, Trigger: trigger})

	switch trigger.Kind {
	case chat.TriggerCommand:
		handleCommand(ctx, id, trigger, log, registry, pctx, limiter, permResolver)
	case chat.TriggerMention:
		// Stage 4 wires this to the LLM tool-calling loop; Stage 1 only
		// proves the detection path end-to-end.
		log.Info("mention_received", logging.Fields{"actor": id, "message": trigger.Message})
	}
}

// handlePlayerList updates the live roster from one PlayerList packet and
// publishes a roster.JoinEvent for each genuinely new, non-self,
// non-sibling arrival — filtered here, before publishing, the same way
// handleText filters chat before publishing chat.MessageEvent, so every
// event-driven plugin downstream can assume it never sees this agent's own
// presence or a sibling bot's.
func handlePlayerList(pk *packet.PlayerList, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, eventBus *bus.Bus, playerRoster *roster.Roster) {
	entries := make([]roster.PlayerListEntry, len(pk.Entries))
	for i, e := range pk.Entries {
		entries[i] = roster.PlayerListEntry{
			XUID:     e.XUID,
			Username: e.Username,
			Remove:   e.ActionType == protocol.PlayerListActionRemove,
		}
	}

	for _, join := range playerRoster.Apply(entries) {
		if chat.IsSelfOrSibling(join.XUID, selfXUID, siblingXUIDs) {
			continue
		}
		log.Info("player_joined", logging.Fields{"xuid": join.XUID, "username": join.Username})
		eventBus.Publish(roster.JoinEvent{Entry: join})
	}
}

func handleCommand(ctx context.Context, actorXUID string, trigger chat.Trigger, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, limiter *ratelimit.PerActor, permResolver *adapters.PermissionResolver) {
	if !limiter.Allow(actorXUID, time.Now()) {
		log.Info("command_rate_limited", logging.Fields{"command": trigger.Command, "actor": actorXUID})
		return
	}

	// Bounded separately from the command dispatch below: a slow or down
	// bridge must not itself stall the read loop waiting on a permission
	// lookup before Dispatch even gets its own timeout.
	permCtx, permCancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
	actorPermission := permResolver.Resolve(permCtx, actorXUID)
	permCancel()

	inv := plugin.Invocation{ActorXUID: actorXUID, ActorPermission: actorPermission, Args: trigger.Args}

	reply, err := registry.Dispatch(ctx, pctx, trigger.Command, inv)
	switch {
	case errors.Is(err, plugin.ErrUnknownCommand):
		log.Debug("command_unknown", logging.Fields{"command": trigger.Command, "actor": actorXUID})
		return
	case errors.Is(err, plugin.ErrPermissionDenied):
		log.Info("command_denied", logging.Fields{"command": trigger.Command, "actor": actorXUID})
		return
	case err != nil:
		log.Error("command_failed", logging.Fields{"command": trigger.Command, "actor": actorXUID, "error": err.Error()})
		return
	}

	log.Info("command_replied", logging.Fields{"command": trigger.Command, "actor": actorXUID, "reply": reply})

	// The reply runs on the packet-read goroutine just like Dispatch does,
	// so it needs the same bound: a hung Voice implementation must not be
	// able to stall the read loop indefinitely.
	replyCtx, cancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
	defer cancel()

	if actorXUID == chat.ServerOrigin {
		if err := pctx.Voice.Say(replyCtx, reply); err != nil {
			log.Error("voice_say_failed", logging.Fields{"command": trigger.Command, "error": err.Error()})
		}
		return
	}
	if err := pctx.Voice.Tell(replyCtx, actorXUID, reply); err != nil {
		log.Error("voice_tell_failed", logging.Fields{"command": trigger.Command, "actor": actorXUID, "error": err.Error()})
	}
}

// startEventDispatch subscribes every registered plugin.EventHandler to
// each bus kind it declared interest in via Kinds(), and runs one goroutine
// per (plugin, kind) subscription pumping events to HandleEvent until ctx
// is cancelled. Called once at startup, after every plugin is registered —
// registry.Plugins() is a fixed set for the life of the process, so there
// is nothing to re-subscribe on a reconnect; join/leave and chat events
// keep flowing to the same subscriptions across sessions.
func startEventDispatch(ctx context.Context, b *bus.Bus, registry *plugin.Registry, pctx *plugin.Context, log *logging.Logger) {
	for _, p := range registry.Plugins() {
		eh, ok := p.(plugin.EventHandler)
		if !ok {
			continue
		}
		for _, kind := range eh.Kinds() {
			ch, unsub := b.Subscribe(kind, 16)
			go dispatchEvents(ctx, p.Name(), kind, ch, unsub, eh, pctx, log)
		}
	}
}

func dispatchEvents(ctx context.Context, pluginName, kind string, ch <-chan bus.Event, unsub func(), eh plugin.EventHandler, pctx *plugin.Context, log *logging.Logger) {
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			handleCtx, cancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
			err := eh.HandleEvent(handleCtx, pctx, ev)
			cancel()
			if err != nil {
				log.Error("event_handler_failed", logging.Fields{"plugin": pluginName, "kind": kind, "error": err.Error()})
			}
		}
	}
}
