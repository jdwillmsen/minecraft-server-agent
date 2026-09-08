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
	"github.com/jdwillmsen/minecraft-server-agent/internal/liveness"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/mcauth"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
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
		Voice: adapters.NewBridgeVoice(bridgeClient, playerRoster),
		Facts: adapters.NewBridgeFacts(bridgeClient),
		// Shares the bridge's timeout: both are "one HTTP call to something
		// in this namespace", and a second knob for the same property is a
		// knob that drifts.
		ServerInfo: adapters.NewMetricsFacts(
			adapters.NewMetricsClient(cfg.MCMonitorURL, cfg.BackupExporterURL, bridgeTimeout),
		),
		Directory: registry,
	}
	// Every answerable chat message and every roster join is published
	// here; event-driven plugins (welcome) subscribe via startEventDispatch
	// rather than touching the connection directly.
	eventBus := bus.New()
	limiter := ratelimit.NewPerActor(cfg.CommandRateLimitPerMinute, time.Minute)
	// A separate budget from commands on purpose: one LLM call costs far more
	// than one console command, and sharing a limiter would let a burst of
	// questions starve !help for the same player.
	// Opened before the game connection so a misconfigured database is a
	// startup log line rather than a surprise at the first player join.
	// Failure is not fatal: persistence is the personalisation behind
	// greetings, and losing it must not cost the agent its commands.
	playerStore := openStore(ctx, cfg, log)
	defer playerStore.Close()

	ans := answering{
		limiter: ratelimit.NewPerActor(cfg.AnswerMaxPerMinute, time.Minute),
		llm: adapters.NewLLMClient(
			cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey,
			cfg.LLMMaxTokens, time.Duration(cfg.LLMTimeoutMs)*time.Millisecond,
		),
	}
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
	runConnectLoop(ctx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, permResolver, ans, playerStore)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http_shutdown_failed", logging.Fields{"error": err.Error()})
	}
	log.Info("stopped", nil)
}

// runConnectLoop owns the reconnect/backoff policy. Each iteration runs one
// session to completion (or failure), then waits before trying again.
func runConnectLoop(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store) {
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
		err := session(ctx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, permResolver, ans, playerStore)
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
// answering bundles what @server handling needs, so enabling this feature
// costs one parameter on the chat path rather than two on each of five
// functions. Both live for the process rather than the session: a rate limit
// that reset on every reconnect would be a rate limit a reconnect clears.
type answering struct {
	limiter *ratelimit.PerActor
	llm     *adapters.LLMClient
}

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
func session(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store) error {
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
	// Runtime ID rather than XUID: the respawn exchange identifies the player
	// by the id that is unique to this world session, not the account.
	respawner := liveness.New(conn.GameData().EntityRuntimeID)
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
		// Before anything else: a dead agent answers no commands and holds no
		// chunks, and nothing outside this loop can tell that it is dead.
		if handled, err := respawner.Handle(pk, conn); err != nil {
			log.Error("respawn_failed", logging.Fields{"error": err.Error()})
		} else if handled {
			if respawner.Dead() {
				log.Info("died", logging.Fields{"requesting_respawn": true})
			} else {
				log.Info("respawned", nil)
			}
			// Readiness follows aliveness, not just the session. An agent on
			// a death screen is connected and useless; reporting ready would
			// be the lie that made this invisible in the first place.
			httpServer.SetReady(!respawner.Dead())
		}

		handlePacket(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, ans, playerStore)
	}
}

func handlePacket(ctx context.Context, pk packet.Packet, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store) {
	switch pk := pk.(type) {
	case *packet.Text:
		handleText(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus, limiter, permResolver, ans, playerRoster)
	case *packet.PlayerList:
		handlePlayerList(ctx, pk, selfXUID, siblingXUIDs, log, eventBus, playerRoster, playerStore)
	}
}

func handleText(ctx context.Context, text *packet.Text, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, permResolver *adapters.PermissionResolver, ans answering, playerRoster *roster.Roster) {
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
		handleMention(ctx, id, trigger, log, pctx, ans, playerRoster)
	}
}

// openStore connects to Postgres if configured, and degrades to Nop if not.
//
// Deliberately never returns an error. Every failure here -- unset, malformed,
// unreachable -- lands the agent in the same supported state it ran in through
// Stages 1-4: greeting players plainly and answering commands.
func openStore(ctx context.Context, cfg config.Config, log *logging.Logger) store.Store {
	dsn := cfg.PostgresDSN()
	if dsn == "" {
		log.Info("store_disabled", logging.Fields{"reason": "PG_HOST unset"})
		return store.Nop{}
	}
	pg, err := store.Open(ctx, dsn, time.Duration(cfg.PGConnectTimeoutMs)*time.Millisecond)
	if err != nil {
		log.Error("store_open_failed", logging.Fields{"error": err.Error()})
		return store.Nop{}
	}
	// Sessions still open belong to a previous run: the agent learns of a
	// departure by being connected, so anything open at startup ended while
	// it was away.
	if n, err := pg.CloseOrphans(ctx, time.Now()); err != nil {
		log.Error("store_close_orphans_failed", logging.Fields{"error": err.Error()})
	} else if n > 0 {
		log.Info("store_closed_orphans", logging.Fields{"sessions": n})
	}
	log.Info("store_ready", logging.Fields{"database": cfg.PGDatabase})
	return pg
}

// handleMention answers an @server question.
//
// Everything that makes this safe is upstream or in the prompt rather than
// here: handleText has already dropped this agent's own messages and its
// sibling bots' (chat.IsSelfOrSibling), and the system prompt refuses to end a
// reply with a question. Both matter because the answering AFK bot is still
// running alongside this agent, and two automated speakers in one chat is the
// shape of a loop.
func handleMention(ctx context.Context, actorXUID string, trigger chat.Trigger, log *logging.Logger, pctx *plugin.Context, ans answering, playerRoster *roster.Roster) {
	if ans.llm == nil || !ans.llm.Enabled() {
		// Stage 1-3 behaviour, kept as the unconfigured path: detection is
		// proven, nothing is answered.
		log.Info("mention_received", logging.Fields{"actor": actorXUID, "message": trigger.Message})
		return
	}

	if !ans.limiter.Allow(actorXUID, time.Now()) {
		// Silent on purpose. Telling a player they are rate limited is itself
		// a chat line, so a spammer would still get one message per attempt.
		log.Info("mention_rate_limited", logging.Fields{"actor": actorXUID})
		return
	}

	name := actorXUID
	if playerRoster != nil {
		if resolved, ok := playerRoster.NameFor(actorXUID); ok && resolved != "" {
			name = resolved
		}
	}

	reply, err := ans.llm.Answer(ctx, name, trigger.Message)
	if err != nil {
		// Logged, never spoken. A backend timeout is an operator's problem,
		// and narrating it in chat turns one failure into an audience.
		log.Error("mention_answer_failed", logging.Fields{"actor": actorXUID, "error": err.Error()})
		return
	}
	if reply == "" {
		// An empty completion broadcast as a blank line reads to players as
		// the server glitching -- a bug minecraft-afk-bot shipped and fixed.
		log.Info("mention_answer_empty", logging.Fields{"actor": actorXUID})
		return
	}

	if pctx.Voice == nil {
		log.Error("mention_answer_undeliverable", logging.Fields{"actor": actorXUID})
		return
	}
	// Broadcast rather than whispered: an @server question is asked in public
	// chat, and an answer only the asker can see reads as no answer at all to
	// everyone else who watched them ask.
	if err := pctx.Voice.Say(ctx, reply); err != nil {
		log.Error("mention_answer_send_failed", logging.Fields{"actor": actorXUID, "error": err.Error()})
		return
	}
	log.Info("mention_answered", logging.Fields{"actor": actorXUID, "reply_chars": len(reply)})
}

// handlePlayerList updates the live roster from one PlayerList packet and
// publishes a roster.JoinEvent for each genuinely new, non-self,
// non-sibling arrival — filtered here, before publishing, the same way
// handleText filters chat before publishing chat.MessageEvent, so every
// event-driven plugin downstream can assume it never sees this agent's own
// presence or a sibling bot's.
func handlePlayerList(ctx context.Context, pk *packet.PlayerList, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, eventBus *bus.Bus, playerRoster *roster.Roster, playerStore store.Store) {
	entries := make([]roster.PlayerListEntry, len(pk.Entries))
	for i, e := range pk.Entries {
		entries[i] = roster.PlayerListEntry{
			XUID:     e.XUID,
			Username: e.Username,
			Remove:   e.ActionType == protocol.PlayerListActionRemove,
		}
	}

	joins, leaves := playerRoster.Apply(entries)
	for _, join := range joins {
		if chat.IsSelfOrSibling(join.XUID, selfXUID, siblingXUIDs) {
			continue
		}
		log.Info("player_joined", logging.Fields{"xuid": join.XUID, "username": join.Username})
		eventBus.Publish(roster.JoinEvent{Entry: join})
	}
	// Departures close a session rather than reaching a plugin. Nothing
	// greets a player for leaving, and publishing an event no handler wants
	// would be scaffolding for its own sake.
	for _, leave := range leaves {
		if chat.IsSelfOrSibling(leave.XUID, selfXUID, siblingXUIDs) {
			continue
		}
		log.Info("player_left", logging.Fields{"xuid": leave.XUID, "username": leave.Username})
		if err := playerStore.RecordLeave(ctx, leave.XUID, time.Now()); err != nil {
			// Logged, never fatal: an unclosed session is recoverable at the
			// next startup, and a database problem must not disturb the game.
			log.Error("store_record_leave_failed", logging.Fields{"xuid": leave.XUID, "error": err.Error()})
		}
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
