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
	"strings"
	"syscall"
	"time"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/liveness"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcproto"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/skin"
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

// maxConcurrentAnswers caps @server answers in flight across every player.
//
// Small on purpose: the backend is one self-hosted model, and beyond a
// handful of simultaneous exchanges the same throughput simply arrives
// later, into a chat that has moved on. It also bounds what a coordinated
// group of players can make this agent spend, which the per-player limiter
// alone cannot.
const maxConcurrentAnswers = 3

// authRejectionCode is the OAuth error code Microsoft returns when Xbox
// Live refuses a login because of the account itself -- an abuse-mode hold,
// a ban -- rather than a network or protocol problem. It is the only code
// this agent has ever seen an account get flagged with, so it is the only
// one isAuthRejection treats as a rejection.
const authRejectionCode = "invalid_grant"

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
	if err := registry.Register(plugins.NewKnowledge()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "knowledge", "error": err.Error()})
		os.Exit(1)
	}
	if err := registry.Register(plugins.NewWaypoints()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "waypoints", "error": err.Error()})
		os.Exit(1)
	}
	welcomePlugin := plugins.NewWelcome(ctx, welcomeDelay, log)
	if err := registry.Register(welcomePlugin); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"plugin": "welcome", "error": err.Error()})
		os.Exit(1)
	}

	// Opened before the game connection so a misconfigured database is a
	// startup log line rather than a surprise at the first player join, and
	// before the plugin context because that context needs it. Failure is not
	// fatal: persistence is the personalisation behind greetings, and losing
	// it must not cost the agent its commands.
	playerStore := openStore(ctx, cfg, log)
	defer playerStore.Close()

	// Both share the profile store's pool rather than opening their own: one
	// database, one set of connections, and a store that cannot outlive the
	// pool it borrows.
	var knowledgeStore knowledge.Store = knowledge.Nop{}
	var waypointStore waypoints.Store = waypoints.Nop{}
	if pg, ok := playerStore.(*store.Postgres); ok && pg.Pool() != nil {
		knowledgeStore = knowledge.NewPostgres(pg.Pool())
		waypointStore = waypoints.NewPostgres(pg.Pool())
		log.Info("knowledge_ready", nil)
	}

	pctx := newPluginContext(cfg, bridgeClient, bridgeTimeout, playerRoster, registry, playerStore, knowledgeStore, waypointStore)
	// Every answerable chat message and every roster join is published
	// here; event-driven plugins (welcome) subscribe via startEventDispatch
	// rather than touching the connection directly.
	eventBus := bus.New()
	limiter := ratelimit.NewPerActor(cfg.CommandRateLimitPerMinute, time.Minute)
	ans := newAnswering(
		newLLMClient(cfg, log),
		cfg.AnswerMaxPerMinute,
		time.Duration(cfg.LLMTotalTimeoutMs)*time.Millisecond,
		bridgeTimeout,
	)
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
	// authDelay does not enter the doubling ladder: nextDelay never sees it.
	// A rejected account stays rejected until the account holder clears
	// whatever flag caused it, on a timeline the doubling schedule knows
	// nothing about, so every rejection waits this same floor rather than
	// climbing toward maxDelay or resetting toward minDelay.
	authDelay := time.Duration(cfg.AuthRetryDelayMs) * time.Millisecond
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

		var rejected bool
		delay, rejected = reconnectDelay(err, lasted, delay, minDelay, maxDelay, authDelay)
		wait := jitter(delay)

		switch {
		case rejected:
			log.Error("auth_rejected", logging.Fields{"error": err.Error(), "session_lasted_ms": lasted.Milliseconds(), "delay_ms": wait.Milliseconds()})
		case err != nil:
			log.Error("session_error", logging.Fields{"error": err.Error(), "session_lasted_ms": lasted.Milliseconds()})
		default:
			log.Info("disconnected", logging.Fields{"session_lasted_ms": lasted.Milliseconds()})
		}
		log.Info("reconnecting", logging.Fields{"delay_ms": wait.Milliseconds()})

		if !waitOrShutdown(ctx, wait) {
			return
		}
	}
}

// waitOrShutdown pauses for wait, reporting false the moment ctx is
// cancelled instead of letting a long auth-rejection floor or a maxed-out
// reconnect delay hold up shutdown.
func waitOrShutdown(ctx context.Context, wait time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}

// reconnectDelay picks the unjittered wait before the next connection
// attempt. An authentication rejection always waits authDelay flat: it
// skips nextDelay entirely so a run of rejections neither climbs toward max
// nor collapses toward min, because neither bound means anything to an
// account-level hold. Everything else -- success or an ordinary failure --
// keeps exactly nextDelay's ladder.
func reconnectDelay(err error, lasted, current, min, max, authDelay time.Duration) (delay time.Duration, rejected bool) {
	if err != nil && isAuthRejection(err) {
		return authDelay, true
	}
	return nextDelay(current, lasted, min, max), false
}

// answering bundles what @server handling needs, so enabling this feature
// costs one parameter on the chat path rather than two on each of five
// functions. Both live for the process rather than the session: a rate limit
// that reset on every reconnect would be a rate limit a reconnect clears.
type answering struct {
	limiter *ratelimit.PerActor
	llm     *adapters.LLMClient
	// toolsFor is rebuilt per answer rather than cached: it closes over the
	// plugin context's capabilities, and which of those are usable can
	// change while the process runs. The callerScoped it returns belongs to
	// that one answer and reports whether the model read the asker's own
	// data -- see buildToolset.
	toolsFor func(*plugin.Context) (*tools.Registry, *callerScoped)
	// total bounds one whole answering attempt, tool rounds included.
	total time.Duration
	// inFlight is a counting semaphore over answers in progress, capped at
	// maxConcurrentAnswers.
	inFlight chan struct{}
	// broadcast bounds the bridge call that delivers the answer. Carried
	// here because that call is made on a context detached from shutdown and
	// so cannot inherit one; it is the operator's configured bridge timeout,
	// the same value every other bridge call gets.
	broadcast time.Duration
}

// newAnswering assembles the answering dependencies.
//
// A constructor rather than a struct literal at each site because three of
// these fields are silently fatal when left zero, and none of them fail
// where they were forgotten: a nil inFlight channel never accepts a send,
// so every answer is dropped as busy while the log reports a cap of 0; a
// zero total cancels each answer the moment it starts; a zero broadcast
// does the same to the delivery of one already paid for.
//
// The limiter is built here too, on a budget separate from commands: one
// LLM call costs far more than one console command, and sharing a limiter
// would let a burst of questions starve !help for the same player.
func newAnswering(llm *adapters.LLMClient, perMinute int, total, broadcast time.Duration) answering {
	return answering{
		limiter:   ratelimit.NewPerActor(perMinute, time.Minute),
		llm:       llm,
		toolsFor:  buildToolset,
		total:     total,
		inFlight:  make(chan struct{}, maxConcurrentAnswers),
		broadcast: broadcast,
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

// isAuthRejection reports whether err is Xbox Live rejecting the account
// itself, as opposed to a transient dial, protocol, or server-side failure.
// The distinction matters for backoff: retrying a rejected account on the
// normal doubling ladder does nothing to recover it and, per the incident
// that added this check, can extend whatever hold Microsoft has the account
// under -- so a rejection needs a floor long enough to plausibly outlast
// that hold, not a faster retry.
func isAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		return retrieveErr.ErrorCode == authRejectionCode
	}
	// The error crosses gophertunnel, xal and x/oauth2 before it reaches
	// here, and nothing in any of those layers guarantees it goes on
	// wrapping with %w forever -- a version bump anywhere in that chain that
	// starts formatting the error into a plain string instead would quietly
	// turn a real rejection back into an ordinary session_error. Matching
	// the text the account was actually rejected with is the honest
	// belt-and-braces for that gap, not a substitute for the typed check
	// above.
	return strings.Contains(err.Error(), authRejectionCode)
}

// session runs one connection to the server from login through to
// disconnect, dispatching chat commands as they arrive. It returns nil on a
// clean disconnect and a non-nil error on anything else (including ctx
// cancellation surfaced as a read error, which the caller ignores because
// it checks ctx.Err() itself).
func session(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store) error {
	// Without this the agent joins as a solid black silhouette under a
	// SkinID regenerated every connect: Bedrock skins are uploaded by the
	// client from its own installation, and a headless client has none, so
	// gophertunnel substitutes a placeholder. Geometry and the resource patch
	// are left to the dialer, which supplies working humanoid defaults.
	agentSkin := skin.For(cfg.MCUsername)
	addr := net.JoinHostPort(cfg.MCHost, strconv.Itoa(cfg.MCPort))

	// The server upgrades itself to Mojang's latest on restart, and a point
	// release that only bumps the protocol number still gets this client
	// kicked before login. Asking the server what it speaks costs one ping per
	// session and keeps the agent connectable until the library catches up.
	proto := mcproto.Negotiate(ctx, addr, func(ad mcproto.Advertisement) {
		log.Warn("protocol_spoofed", logging.Fields{
			"compiled_protocol":   minecraft.DefaultProtocol.ID(),
			"advertised_protocol": ad.Protocol,
			"server_version":      ad.Version,
		})
	})

	dialer := minecraft.Dialer{
		TokenSource: ts,
		Protocol:    proto,
		ClientData: login.ClientData{
			SkinID:          agentSkin.ID,
			SkinData:        agentSkin.Data,
			SkinImageWidth:  agentSkin.Width,
			SkinImageHeight: agentSkin.Height,
		},
	}

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
		startAnswer(ctx, id, trigger, log, pctx, ans, playerRoster)
	}
}

// newPluginContext assembles what every plugin is allowed to reach.
//
// A function rather than a struct literal inline in main so the wiring can be
// tested. Profiles was declared on plugin.Context and never assigned in an
// earlier version: plugins saw a nil interface, the welcome plugin skipped
// RecordJoin without reporting anything, and no player arrival was ever
// persisted. Nothing failed loudly, because the one branch that would have
// logged is the error path of the call that was not being made.
func newPluginContext(cfg config.Config, bridgeClient *adapters.BridgeClient, bridgeTimeout time.Duration, playerRoster *roster.Roster, registry *plugin.Registry, playerStore store.Store, knowledgeStore knowledge.Store, waypointStore waypoints.Store) *plugin.Context {
	return &plugin.Context{
		Voice: adapters.NewBridgeVoice(bridgeClient, playerRoster),
		Facts: adapters.NewBridgeFacts(bridgeClient),
		// Shares the bridge's timeout: both are "one HTTP call to something
		// in this namespace", and a second knob for the same property is a
		// knob that drifts.
		ServerInfo: adapters.NewMetricsFacts(
			adapters.NewMetricsClient(cfg.MCMonitorURL, cfg.BackupExporterURL, bridgeTimeout),
		),
		Directory: registry,
		// store.Nop when no database is configured, never nil -- plugin.Context
		// documents Profiles as possibly nil and the plugins guard for it, but
		// this binary has no reason to hand them one.
		Profiles: playerStore,
		// Same reasoning as Profiles: pool-backed when a database is
		// configured, the disabled implementation when not, never nil.
		Knowledge: knowledgeStore,
		Waypoints: waypointStore,
	}
}

// newLLMClient builds the one production answering client.
//
// A function rather than a literal inline so the logger wiring is testable.
// Passed a nil logger the client's tool_invocation_failed events go nowhere:
// a lookup that fails on every question would be invisible to an operator,
// and the answer path degrades silently by design -- the model just answers
// around the missing fact.
func newLLMClient(cfg config.Config, log *logging.Logger) *adapters.LLMClient {
	return adapters.NewLLMClient(
		cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey,
		cfg.LLMMaxTokens, time.Duration(cfg.LLMTimeoutMs)*time.Millisecond,
		log,
	)
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

// startAnswer decides whether an @server question is answered, then answers
// it off the read loop.
//
// Every refusal is made here, on the caller's goroutine, and each is a mutex
// or a channel rather than a network call. The rate limiter especially: its
// budget is per rolling minute, so four questions in one second are four
// allowed answers, and spawning each one concurrently would interleave four
// broadcasts. Running inline used to serialise them; nothing else does now.
func startAnswer(ctx context.Context, actorXUID string, trigger chat.Trigger, log *logging.Logger, pctx *plugin.Context, ans answering, playerRoster *roster.Roster) {
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

	select {
	case ans.inFlight <- struct{}{}:
	default:
		// Dropped, not queued: an answer that waits for a slot arrives after
		// the conversation it belongs to has moved on, and the asker has by
		// then read the silence as the answer.
		log.Info("mention_answer_dropped_busy", logging.Fields{"actor": actorXUID, "max_concurrent": cap(ans.inFlight)})
		return
	}

	// ctx is the process's, not the session's, and deliberately: the reply is
	// delivered through the console bridge, which is a separate service from
	// the Bedrock connection, so a reconnect mid-answer does not invalidate
	// it. ans.total bounds how stale it can be.
	go func() {
		defer func() { <-ans.inFlight }()
		handleMention(ctx, actorXUID, trigger, log, pctx, ans, playerRoster)
	}()
}

// handleMention answers one @server question. Its caller has already decided
// that this question gets answered -- see startAnswer.
//
// Everything that makes this safe is upstream or in the prompt rather than
// here: handleText has already dropped this agent's own messages and its
// sibling bots' (chat.IsSelfOrSibling), and the system prompt refuses to end a
// reply with a question. Both matter because the answering AFK bot is still
// running alongside this agent, and two automated speakers in one chat is the
// shape of a loop.
func handleMention(ctx context.Context, actorXUID string, trigger chat.Trigger, log *logging.Logger, pctx *plugin.Context, ans answering, playerRoster *roster.Roster) {
	name := actorXUID
	if playerRoster != nil {
		if resolved, ok := playerRoster.NameFor(actorXUID); ok && resolved != "" {
			name = resolved
		}
	}

	// Bounds the attempt end to end. The per-call timeout inside the client
	// bounds each request, which a model that keeps calling tools can spend
	// several of.
	answerCtx, cancel := context.WithTimeout(ctx, ans.total)
	defer cancel()

	registry, personal := ans.toolsFor(pctx)
	reply, err := ans.llm.AnswerWithTools(answerCtx, name, actorXUID, trigger.Message, registry)
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
	//
	// The exception is an answer the model built from the asker's own
	// waypoints. Those coordinates are personal -- !wp whispers them because
	// broadcasting where a player lives is a griefing vector -- and they do
	// not stop being personal because the question reached them through
	// @server. The console has no player to whisper to, and nothing it asks
	// about is its own, so it keeps the broadcast.
	//
	// Detached from ctx, and bounded by the operator's bridge timeout rather
	// than a number chosen here: the client applies that same value to every
	// other call it makes, and a shorter deadline on this one path would be a
	// configuration knob that silently stops working where it matters most.
	//
	// Detached because the answer is already computed and paid for, and
	// SIGTERM landing in the gap between the backend replying and this line
	// would otherwise throw it away. This does not outrun the process exit
	// that follows a signal -- nothing waits for these goroutines -- it only
	// stops a cancelled context discarding a reply the bridge could still
	// have delivered.
	sayCtx, sayCancel := context.WithTimeout(context.WithoutCancel(ctx), ans.broadcast)
	defer sayCancel()

	private := personal.happened() && actorXUID != chat.ServerOrigin
	var sendErr error
	if private {
		sendErr = pctx.Voice.Tell(sayCtx, actorXUID, reply)
	} else {
		sendErr = pctx.Voice.Say(sayCtx, reply)
	}
	if sendErr != nil {
		log.Error("mention_answer_send_failed", logging.Fields{"actor": actorXUID, "error": sendErr.Error()})
		return
	}
	log.Info("mention_answered", logging.Fields{"actor": actorXUID, "reply_chars": len(reply), "private": private})
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
