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
	"io"
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
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/authcache"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/presence"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/sources"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
	"github.com/jdwillmsen/minecraft-server-agent/internal/toolset"
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

// announceDrainDelay is how long the announce drain waits after a join
// before whispering a player their backlog. Past welcomeDelay so the
// greeting owns the join moment and the backlog follows it, and far enough
// past the join itself that the client is rendering chat: delivered at
// 0.8s, two announcements were accepted by the server, recorded as
// delivered and seen by nobody.
const announceDrainDelay = welcomeDelay + 3*time.Second

// freshJoinGrace is how long after an arrival the deliverer treats a player
// as still loading, and leaves their copy of an announcement pending rather
// than recorded. The join drain reads the same clock when it wakes, so this
// must stay shorter than announceDrainDelay: at grace >= delay every drain
// would defer itself and the backlog would never go out at all.
const freshJoinGrace = announceDrainDelay - time.Second

// stableSessionThreshold mirrors minecraft-afk-bot: the reconnect backoff
// only resets to its minimum once a session has stayed up at least this
// long, so a server that accepts a connection and immediately drops it
// (e.g. the documented upstream join crash) doesn't cause a reconnect
// storm at full speed.
const stableSessionThreshold = 60 * time.Second

// errSessionRecycled ends a session this process chose to end. It is not a
// failure and must not be reported as one -- see SESSION_RECYCLE_MS.
var errSessionRecycled = errors.New("session recycled on schedule")

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
	// packets while the agent is in the world (see handlePlayerList) and from
	// the console bridge while it is deliberately out of it (see
	// bridgeRoster). It serves two needs: join detection for the welcome
	// plugin, and gamertag resolution for BridgeVoice.Tell (which only ever
	// receives an XUID).
	playerRoster := roster.New()

	bridgeTimeout := time.Duration(cfg.ConsoleBridgeTimeoutMs) * time.Millisecond
	bridgeClient := adapters.NewBridgeClient(cfg.ConsoleBridgeURL, cfg.ConsoleBridgeToken, bridgeTimeout)
	permResolver := adapters.NewPermissionResolver(bridgeClient, adapters.DefaultPermissionsCacheTTL, log)
	siblings := siblingBotXUIDs()
	// The pinger lives for the process and the Bedrock connection for one
	// session; the link meter is how the one reaches whichever of the other
	// is live.
	link := &linkMeter{}
	pinger := adapters.NewServerPinger(bridgeClient, link.roundTrip, log)

	// Built first, and serving before anything slow: it is where this
	// process's role is recorded, which the Deliverer below reads to know
	// whether it may speak into the game at all, and /healthz has to answer
	// while openStore waits out an unreachable database, or the liveness
	// probe kills the pod partway through the wait meant to save it. /readyz
	// reports a starting process as unready, so serving early claims nothing.
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

	// Opened before the game connection so a misconfigured database is a
	// startup log line rather than a surprise at the first player join, and
	// before the plugins because two of them are constructed from it. Failure
	// is not fatal to persistence: greetings and commands work without it.
	// It is fatal to authentication when the database is also where the Xbox
	// token lives, which is why openStore retries before giving up.
	playerStore := openStore(ctx, cfg, log, store.Open)
	defer playerStore.Close()
	// A shutdown that lands while openStore is still waiting is a pod being
	// replaced, not a failure: stopping here keeps it from being reported as
	// the authentication failure a missing store would otherwise become.
	if ctx.Err() != nil {
		log.Info("stopped", nil)
		return
	}

	// All four share the profile store's pool rather than opening their own:
	// one database, one set of connections, and a store that cannot outlive
	// the pool it borrows.
	var knowledgeStore knowledge.Store = knowledge.Nop{}
	var waypointStore waypoints.Store = waypoints.Nop{}
	var announceStore announce.Store = announce.Nop{}
	var scheduleStore announce.ScheduleStore = announce.Nop{}
	var auditor audit.Store = audit.Nop{}
	var moderationStore moderation.Store = moderation.Nop{}
	// The lock that makes exactly one of these processes the live agent. It
	// lives on the profile store's pool, so it exists exactly when
	// persistence does -- see newElection and awaitLeadership.
	var election campaigner
	// Nil until there is a database to hold it. Unlike the stores above there
	// is no Nop for it: a token cache that silently forgets would send the
	// agent back to a device-code login on every restart, so the file cache
	// is the degraded case instead -- see openTokenStore.
	var sharedTokens mcauth.Store
	if pg, ok := playerStore.(*store.Postgres); ok && pg.Pool() != nil {
		election = newElection(cfg, pg.Pool(), log)
		sharedTokens = authcache.NewPostgres(pg.Pool(), cfg.MCUsername)
		knowledgeStore = knowledge.NewPostgres(pg.Pool())
		moderationStore = newModerationLog(moderation.NewPostgres(pg.Pool()), log)
		waypointStore = waypoints.NewPostgres(pg.Pool())
		auditor = newAuditTrail(audit.NewPostgres(pg.Pool()), log)
		// Wrapped rather than used directly: every announcement row names a
		// player that minecraft.players must already hold -- see outbox.
		announcePG := announce.NewPostgres(pg.Pool())
		ob := newOutbox(announcePG, pg, playerRoster, log)
		announceStore = ob
		scheduleStore = scheduleBook{ScheduleStore: announcePG, outbox: ob}
		log.Info("knowledge_ready", nil)
	}

	// Built here, before playerStore is wrapped below, for the same reason
	// the stores above are: it borrows the concrete Postgres's pool.
	presenceRT, err := newPresence(cfg, playerStore, auditor, bridgeClient, playerRoster, log)
	if err != nil {
		log.Error("presence_config_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	voice := adapters.NewBridgeVoice(bridgeClient, playerRoster)
	audience := newDeliveryAudience(playerRoster, siblings)

	// One Deliverer for the process, reached two ways: plugin.Context narrows
	// it to what !announce and !inbox need, while the drain plugin needs
	// DrainForJoin, which that interface deliberately does not carry. Two
	// instances would be two views of one outbox with no reason to differ.
	//
	// Every dependency is real. A Deliverer over a disabled store returns
	// before it touches any of them, which made a nil safe here while this
	// was a placeholder -- and would have made it a nil dereference the first
	// time those early returns moved.
	joins := newJoinTimes()
	deliverer := announce.NewDeliverer(
		announceStore,
		voice,
		audience,
		announcePermissions{resolver: permResolver},
		log,
		announce.WithFreshJoinGrace(joins, freshJoinGrace),
		// A publish can reach any replica, because the announcement API is
		// mounted for the process; only the one holding the lock is playing
		// on the server a broadcast would be heard on.
		announce.WithLeadership(httpServer),
	)
	// Wrapped only now: the stores above type-assert the concrete Postgres
	// to borrow its pool, which the wrapper would hide from them.
	playerStore = withPlayerEvents(playerStore, sources.NewEvents(ctx, deliverer, log))

	registry := plugin.NewRegistry()
	if err := registerPlugins(ctx, registry, deliverer, joins, cfg.ModerationTerms, log, presenceRT.plugin); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	pctx := newPluginContext(cfg, bridgeClient, bridgeTimeout, voice, playerRoster, registry, playerStore, knowledgeStore, waypointStore, announceStore, deliverer, pinger, moderationStore, scheduleStore)
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
	// Started for the process rather than for a turn as the live agent: the
	// subscriptions are the registry's, which never changes, and a standby
	// that is in no game has nothing to dispatch anyway. Everything that
	// writes somewhere a second process would also write starts with
	// leadership instead -- see startLiveWork.
	startEventDispatch(ctx, eventBus, registry, pctx, log)

	// Same two-tier lookup !announce @player uses, so a name the API and the
	// command resolve can never mean two different players.
	apiOn := httpServer.MountAnnouncements(cfg.AnnounceAPIToken, deliverer, playerLookup{live: playerRoster, archive: playerStore}, log)
	log.Info("announce_api", logging.Fields{"enabled": apiOn})
	presenceOn := httpServer.MountPresence(presenceRT.api)
	log.Info("presence_api", logging.Fields{"enabled": presenceOn, "actors": len(cfg.PresenceActors)})

	tokenStore, err := openTokenStore(cfg, sharedTokens, log)
	if err != nil {
		log.Error("auth_store_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	// Built once, outside the reconnect loop: the refresh token is kept in
	// memory across reconnects instead of being re-derived from the store
	// (and, on a load failure, potentially re-triggering an interactive
	// device-code login) on every single attempt.
	//
	// The gate is closed until this process wins a turn, so the warm-up
	// below reads what the live agent stored rather than rotating the
	// account's refresh token out from under it.
	tokenGate := &tokenLiveGate{}
	ts, err := newTokenSource(ctx, tokenStore, os.Stdout, log,
		mcauth.WithLiveGate(tokenGate.isOpen),
		mcauth.WithLogger(log),
	)
	if err != nil {
		log.Error("auth_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	// Warmed here, before the wait below rather than after it: every cost
	// paid while this process is still a standby is a cost the handover does
	// not pay.
	warmXboxToken(ts, log)

	log.Info("starting", logging.Fields{"mc_host": cfg.MCHost, "mc_port": cfg.MCPort})

	// The agent's own effective presence when actors are configured, and
	// always present otherwise.
	gate := presenceRT.sessionGate()
	// Built after withPlayerEvents has wrapped playerStore, so a name the
	// follower resolves comes from the same store the session writes.
	bridgeFollower := newBridgeRoster(bridgeClient, playerRoster, joins, playerStore, log)
	bridgeFollower.onJoin = presenceRT.joins.RecordAt

	// One pass of this loop is one turn as the live agent: wait for the lock,
	// do the live agent's work until the process is shutting down or the lock
	// is gone, then hand over. A process that loses the lock becomes a
	// standby again rather than exiting -- the database blinking must not
	// cost the server its agent, which is exactly what it cost before there
	// was a lock at all.
	//
	// The turn and the session are separate lifecycles. The monitoring the
	// lock entitles this process to runs for the whole turn. The session in
	// the world runs only while the gate wants it, and while it does not,
	// the roster is followed from the console bridge instead.
	for ctx.Err() == nil {
		term, live := awaitLeadership(ctx, election, httpServer.SetRole, log)
		if !live {
			break
		}
		// liveCtx ends with this turn, not with the process: the sessions
		// and every live-only writer below run under it, so losing the lock
		// takes the agent out of the game without taking the process down.
		// The claim on the Xbox Live login is opened and closed with it --
		// see beginTurn.
		//
		// Standing down demotes the role ahead of both, so nothing that
		// reads it acts on a game this process no longer has a claim to
		// while the running session mode, the connect loop or the bridge
		// follower, is still unwinding.
		liveCtx, endClaim := beginTurn(ctx, tokenGate)
		endTurn := func() {
			httpServer.SetRole(httpapi.RoleStandby)
			endClaim()
		}
		go endTermOnLockLoss(liveCtx, term, endTurn, log)
		// A turn that began without the lock -- because whoever holds it is
		// gone without having released it -- is a turn worth flagging for as
		// long as it lasts.
		go watchForcedLeadership(liveCtx, term, log)
		startLiveWork(liveCtx, cfg, bridgeTimeout, pinger, link, announceStore, deliverer, scheduleStore, moderationStore, log)
		// Leader-only, like startLiveWork, and before runSessions: its first
		// tick decides the gate that runSessions reads first.
		presenceRT.lead(liveCtx)

		runSessions(liveCtx, gate, presenceModes(sessionModes{
			present: func(sessionCtx context.Context) {
				runConnectLoop(sessionCtx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, audience, siblings, permResolver, ans, playerStore, auditor, link, joins)
			},
			// Leaving on purpose owes what a recycle owes: everyone still
			// here keeps the time they were watched for, instead of the
			// next connection's CloseOrphans rewriting it as unknown. No
			// term, because the lock is not being passed on.
			left: func() {
				handover(liveCtx, nil, playerStore, playerRoster.Since(), log)
			},
			absent: bridgeFollower.run,
		}, httpServer.SetReady, announceRejoin(voice, log)), log)

		endTurn()
		// The agent is out of the game by now -- the connect loop waits for
		// its own disconnect to reach the server -- so the sessions it was
		// watching can be closed at the moment it stopped watching, and only
		// then is the lock passed on.
		handover(ctx, term, playerStore, playerRoster.Since(), log)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http_shutdown_failed", logging.Fields{"error": err.Error()})
	}
	log.Info("stopped", nil)
}

// registerPlugins registers every plugin this binary serves.
//
// A function rather than a run of Register calls inside main so a test can
// hold the finished registry and say what it must contain. Nothing else in
// the process knows this set: a plugin dropped from here takes its commands
// with it, compiles, and leaves every test green -- the same shape as the
// Profiles field that was declared, never assigned, and only noticed in
// production.
//
// The error carries the plugin's own name, because "registration failed" on
// its own does not say which one.
func registerPlugins(ctx context.Context, registry *plugin.Registry, deliverer plugins.AnnounceDeliverer, conns plugins.Connections, moderationTerms []string, log *logging.Logger, extra ...plugin.Plugin) error {
	mod, err := plugins.NewModeration(ctx, moderationTerms, log)
	if err != nil {
		return fmt.Errorf("moderation: %w", err)
	}
	for _, p := range append([]plugin.Plugin{
		plugins.NewCore(),
		plugins.NewStats(),
		plugins.NewKnowledge(),
		plugins.NewWaypoints(),
		plugins.NewWelcome(ctx, welcomeDelay, log, plugins.WithGreetConnections(conns)),
		plugins.NewAnnounce(),
		plugins.NewAnnounceDrain(ctx, deliverer, announceDrainDelay, log, plugins.WithConnections(conns)),
		mod,
		plugins.NewSchedule(),
	}, extra...) {
		if err := registry.Register(p); err != nil {
			return fmt.Errorf("%s: %w", p.Name(), err)
		}
	}
	// Here rather than as a separate line in main: the only moment the
	// command set is known to be complete is the end of this function, and
	// a call main could drop leaves every test green while each command's
	// first use after a restart reads as zero to increase().
	initCommandMetrics(registry)
	return nil
}

// siblingBotXUIDs is the set of bot identities this agent treats as its own
// kind: never answered in chat, never welcomed, never announced to.
//
// It is empty, and nothing populates it. Read that plainly: every filter
// built on it — handleText, handlePlayerList, the delivery audience —
// currently excludes this agent and nobody else, so a sibling AFK bot is
// welcomed, drained and whispered to exactly like a player. It leaves no
// error behind either: the roster can name a bot, so the outbox creates a
// minecraft.players row for one and records its deliveries as if a person
// had heard them. The gap is silent in the database as well as in chat.
//
// Populating it is not a line of code here. The AFK bots are identified by
// gamertag in their own deployment, not by XUID, and this binary is given
// neither: an XUID is only learned by watching a PlayerList entry for that
// name arrive, so the set would have to be rebuilt per session from
// configuration this agent does not yet receive. Stated in one place rather
// than implied at three call sites, so nobody reads a filter that consults
// it and concludes the bots are handled.
func siblingBotXUIDs() map[string]struct{} {
	return map[string]struct{}{}
}

// tpsSampleInterval spaces the background game-clock readings that !ping
// measures TPS against. Each one is a console command and a line in the
// server log, so it is as long as it can be while still leaving every ping
// a baseline inside the pinger's window.
const tpsSampleInterval = time.Minute

// sampleGameClock keeps the pinger's TPS baseline fresh until ctx ends. It
// runs for the process, not the session: the console bridge is a separate
// path to the server from the Bedrock connection and outlives a reconnect.
func sampleGameClock(ctx context.Context, pinger *adapters.ServerPinger, link func() (time.Duration, bool), timeout time.Duration, log *logging.Logger) {
	ticker := time.NewTicker(tpsSampleInterval)
	defer ticker.Stop()
	for {
		sampleOnce(ctx, pinger, link, timeout, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sampleOnce is one tick of sampleGameClock. The pinger records a TPS it
// measures itself; the link round trip is read here because this is the one
// place that runs on a schedule whether or not anyone types !ping.
//
// The link is read independently of the console answering: they are two
// paths to the server, and a bridge outage says nothing about the Bedrock
// connection.
func sampleOnce(ctx context.Context, pinger *adapters.ServerPinger, link func() (time.Duration, bool), timeout time.Duration, log *logging.Logger) {
	if rtt, ok := link(); ok {
		metrics.LinkRTT(rtt)
	}
	sampleCtx, cancel := context.WithTimeout(ctx, timeout)
	err := pinger.Sample(sampleCtx)
	cancel()
	if err != nil && ctx.Err() == nil {
		// Debug rather than Error: !ping already tells whoever asks that
		// the console did not answer, and an outage would otherwise add a
		// line a minute to the bridge's own failures.
		log.Debug("tps_sample_failed", logging.Fields{"error": err.Error()})
	}
}

// connectionEnded retires the session state a dead Bedrock connection left
// behind, in the one place both halves of it are reset together.
//
// The connection is dead here, not merely about to be replaced, and the
// console bridge is a separate process that still answers -- so anything
// reading this state in the gap would speak into a server whose players are
// reconnecting. Anything scheduled under the dead connection must abandon
// rather than whisper to a client that is mid-load, which is what ending the
// join clock's connection says. And nobody is being watched: held onto, the
// roster would answer Online() with whoever was here when the connection
// died, and an announcement published in the gap would be recorded as
// delivered to players who may already have left, which nothing retries.
//
// A function rather than two statements inline so the gap is a state a test
// can reach the way runConnectLoop reaches it.
func connectionEnded(playerRoster *roster.Roster, joinClock *joinTimes) {
	playerRoster.EndSession()
	joinClock.disconnected()
}

// runConnectLoop owns the reconnect/backoff policy. Each iteration runs one
// session to completion (or failure), then waits before trying again.
func runConnectLoop(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, audience *deliveryAudience, siblingXUIDs map[string]struct{}, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store, auditor audit.Store, link *linkMeter, joinClock *joinTimes) {
	minDelay := time.Duration(cfg.ReconnectMinMs) * time.Millisecond
	maxDelay := time.Duration(cfg.ReconnectMaxMs) * time.Millisecond
	// authDelay does not enter the doubling ladder: nextDelay never sees it,
	// and delay (the ladder position) is never overwritten by a rejection --
	// see reconnectDelay. A rejected account stays rejected until the
	// account holder clears whatever flag caused it, on a timeline the
	// doubling schedule knows nothing about, so every rejection waits this
	// same floor rather than climbing toward maxDelay or resetting toward
	// minDelay, and the ordinary failure that eventually follows one still
	// resumes the ladder from wherever it actually was.
	authDelay := time.Duration(cfg.AuthRetryDelayMs) * time.Millisecond
	delay := minDelay
	firstAttempt := true

	for ctx.Err() == nil {
		if !firstAttempt {
			httpapi.IncReconnect()
		}
		firstAttempt = false

		started := time.Now()
		err := session(ctx, cfg, ts, log, registry, pctx, eventBus, limiter, httpServer, playerRoster, audience, siblingXUIDs, permResolver, ans, playerStore, auditor, link, joinClock)
		lasted := time.Since(started)
		httpServer.SetReady(false)
		connectionEnded(playerRoster, joinClock)

		if ctx.Err() != nil {
			return
		}

		var rejected bool
		var rawWait time.Duration
		rawWait, delay, rejected = reconnectDelay(err, lasted, delay, minDelay, maxDelay, authDelay)
		wait := jitter(rawWait)

		reportSessionEnd(log, cfg.MCUsername, err, rejected, lasted, wait)
		log.Info("reconnecting", logging.Fields{"delay_ms": wait.Milliseconds()})

		if !waitOrShutdown(ctx, wait) {
			return
		}
	}
}

// reportSessionEnd says how one session ended. Its own function so the
// rejection count can be tested without dialling a server that rejects us.
func reportSessionEnd(log *logging.Logger, username string, err error, rejected bool, lasted, wait time.Duration) {
	switch {
	case errors.Is(err, errSessionRecycled):
		// The one session end that was this process's own decision. Logged
		// at info with the same shape as a clean disconnect, because that is
		// what it is, and the reconnect that follows is the measurement.
		log.Info("session_recycled", logging.Fields{"session_lasted_ms": lasted.Milliseconds(), "delay_ms": wait.Milliseconds()})
	case rejected:
		metrics.AuthRejection()
		log.Error("auth_rejected", logging.Fields{"username": username, "error": err.Error(), "session_lasted_ms": lasted.Milliseconds(), "delay_ms": wait.Milliseconds()})
	case err != nil:
		log.Error("session_error", logging.Fields{"error": err.Error(), "session_lasted_ms": lasted.Milliseconds()})
	default:
		log.Info("disconnected", logging.Fields{"session_lasted_ms": lasted.Milliseconds()})
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
// attempt, and the ladder position (nextLadder) the caller should carry
// into its next call as current.
//
// An authentication rejection always waits authDelay flat and passes
// current straight back out as nextLadder: it skips nextDelay entirely, so
// a run of rejections neither climbs toward max nor collapses toward min
// (neither bound means anything to an account-level hold), and critically
// it leaves the ladder exactly where it was -- current must stay in
// [min, max] for nextDelay's own invariant to hold, and authDelay is
// chosen independently of both bounds, so feeding it back in as current
// would violate that invariant on the very next ordinary failure.
// Everything else -- success or an ordinary failure -- calls nextDelay and
// returns its result as both wait and nextLadder, exactly today's ladder.
func reconnectDelay(err error, lasted, current, min, max, authDelay time.Duration) (wait, nextLadder time.Duration, rejected bool) {
	if err != nil && isAuthRejection(err) {
		return authDelay, current, true
	}
	next := nextDelay(current, lasted, min, max)
	return next, next, false
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
	// change while the process runs. The CallerScoped it returns belongs to
	// that one answer and reports whether the model read the asker's own
	// data -- see toolset.Build.
	toolsFor func(*plugin.Context) (*tools.Registry, *toolset.CallerScoped)
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
		toolsFor:  toolset.Build,
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
func session(ctx context.Context, cfg config.Config, ts oauth2.TokenSource, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, httpServer *httpapi.Server, playerRoster *roster.Roster, audience *deliveryAudience, siblingXUIDs map[string]struct{}, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store, auditor audit.Store, link *linkMeter, joinClock *joinTimes) error {
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
	// Not a bare Close: when this session is ending because the process is
	// handing the agent over, the server has to be told before the process
	// stops existing, or it holds the login until it times the session out --
	// see leaveGame.
	defer leaveGame(ctx, conn, leaveGrace, log)

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

	selfXUID := conn.IdentityData().XUID
	beginWatching(ctx, playerRoster, playerStore, joinClock, selfXUID, log)

	// Recorded here rather than after the dial: a connection that never
	// spawns is not what a player would call joining, and this timestamp is
	// read as proof that joining works -- see SESSION_RECYCLE_MS.
	metrics.SessionEstablished(time.Now())

	// The scheduled recycle, when it is on. Ending a healthy session on
	// purpose is the point: a session held open proves only that it was
	// established once, and the outage this guards against left exactly that
	// kind of session running while nobody new could join.
	recycled := make(chan struct{})
	if recycle := time.Duration(cfg.SessionRecycleMs) * time.Millisecond; recycle > 0 {
		stopRecycle := afterFuncWaited(recycle, func() {
			close(recycled)
			recycleSession(ctx, conn, playerStore, playerRoster.Since(), recycle, log)
		})
		defer stopRecycle()
	}

	log.Info("spawned", logging.Fields{"self_xuid": selfXUID})
	// The agent is on the roster like any other player, so the announcement
	// audience has to be told which entry is its own before anything is
	// delivered -- see deliveryAudience.
	audience.beginSession(selfXUID)
	// Ended by defer so that no way out of this session leaves !ping reading
	// a closed connection.
	link.beginSession(conn)
	defer link.endSession()
	// Runtime ID rather than XUID: the respawn exchange identifies the player
	// by the id that is unique to this world session, not the account.
	respawner := liveness.New(conn.GameData().EntityRuntimeID)
	httpServer.SetReady(true)
	httpapi.SetConnected(true)
	defer httpapi.SetConnected(false)

	for {
		if ctx.Err() != nil {
			return nil
		}
		pk, err := conn.ReadPacket()
		if err != nil {
			// A read that failed because this session was recycled is not a
			// fault, and reporting it as one would put an error line in the
			// log every cycle and teach whoever reads them to skip it.
			select {
			case <-recycled:
				return errSessionRecycled
			default:
			}
			return err
		}
		// Before anything else: a dead agent answers no commands and holds no
		// chunks, and nothing outside this loop can tell that it is dead.
		handleLiveness(pk, respawner, conn, log, httpServer.SetReady)

		handlePacket(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, ans, playerStore, auditor, joinClock)
	}
}

// afterFuncWaited is time.AfterFunc whose stop also waits out a call that
// has already begun, which Timer.Stop does not. A session recycle that fired
// just before the session ended would otherwise still be closing playtime
// while the caller's own handover closes it again.
func afterFuncWaited(d time.Duration, fn func()) (stop func()) {
	done := make(chan struct{})
	timer := time.AfterFunc(d, func() {
		defer close(done)
		fn()
	})
	return func() {
		if !timer.Stop() {
			<-done
		}
	}
}

// recycleSession ends a healthy session on purpose, so that the reconnect
// which follows measures whether a real account can still join.
//
// The playtime close is the part that is easy to leave out and expensive to
// leave out. This end is deliberate and the agent knows exactly who was online
// at this instant, so everyone still here keeps the hours they have been
// here — exactly as a handover does. Without it the next connection's
// CloseOrphans rewrites those rows to left_at = joined_at as 'unknown', and a
// player who sat through a six-hour cycle loses six hours of playtime and the
// milestones that ride on it.
func recycleSession(ctx context.Context, conn io.Closer, profiles store.Store, since time.Time, after time.Duration, log *logging.Logger) {
	metrics.SessionRecycled()
	log.Info("session_recycling", logging.Fields{"after_ms": after.Milliseconds()})

	// nil term: there is no lock to hand over here. This process is not going
	// anywhere — it is dropping a connection it is about to re-establish.
	handover(ctx, nil, profiles, since, log)

	// Closing the connection is what unblocks the session's read loop, and
	// the close itself is the disconnect the server sees. The deferred
	// leaveGame closes it a second time, which the connection absorbs; its
	// grace sleep is for process shutdown and does not apply here.
	_ = conn.Close()
}

// beginWatching resets everything the agent believes about who is online, at
// the start of a connection and before any of its packets are applied.
//
// Anything the roster still holds predates this connection and cannot be
// trusted: leaves that happened while disconnected were never seen. The
// server's own opening PlayerList repopulates it from scratch. The same goes
// for open sessions, which are closed at zero length rather than left for a
// later departure to close: a player who left and came back unseen would
// otherwise be credited with their whole absence. handlePlayerList reopens a
// session for each player the snapshot shows is still here.
//
// The roster keeps when this began, and every departure passes it to
// RecordLeave. If both this close and the snapshot's ResumeSession fail, a
// session from before the gap is still open when the player leaves, and
// that is what stops it being credited.
func beginWatching(ctx context.Context, playerRoster *roster.Roster, playerStore store.Store, joinClock *joinTimes, selfXUID string, log *logging.Logger) {
	now := time.Now()
	playerRoster.BeginSession(now, selfXUID)
	// Arrivals from the previous connection mean nothing here, and anyone
	// the next snapshot reports may have reconnected moments before the
	// agent did, so this connection's start stands in for their arrival.
	joinClock.connected()
	if n, err := playerStore.CloseOrphans(ctx, now); err != nil {
		log.Error("store_close_orphans_failed", logging.Fields{"error": err.Error()})
	} else if n > 0 {
		log.Info("store_closed_orphans", logging.Fields{"sessions": n})
	}
}

// handleLiveness drives the respawn exchange for one packet. Split out of
// session so a death can be counted in a test, which has no way to die on a
// live server.
func handleLiveness(pk packet.Packet, respawner *liveness.Respawner, w liveness.Writer, log *logging.Logger, setReady func(bool)) {
	handled, err := respawner.Handle(pk, w)
	if err != nil {
		log.Error("respawn_failed", logging.Fields{"error": err.Error()})
		return
	}
	if !handled {
		return
	}
	if respawner.Dead() {
		metrics.Death()
		log.Info("died", logging.Fields{"requesting_respawn": true})
	} else {
		log.Info("respawned", nil)
	}
	// Readiness follows aliveness, not just the session. An agent on a death
	// screen is connected and useless; reporting ready would be the lie that
	// made this invisible in the first place.
	setReady(!respawner.Dead())
}

func handlePacket(ctx context.Context, pk packet.Packet, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, playerRoster *roster.Roster, permResolver *adapters.PermissionResolver, ans answering, playerStore store.Store, auditor audit.Store, joinClock *joinTimes) {
	switch pk := pk.(type) {
	case *packet.Text:
		handleText(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus, limiter, permResolver, ans, playerRoster, auditor)
	case *packet.PlayerList:
		handlePlayerList(ctx, pk, selfXUID, siblingXUIDs, log, eventBus, playerRoster, playerStore, joinClock)
	}
}

func handleText(ctx context.Context, text *packet.Text, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus, limiter *ratelimit.PerActor, permResolver *adapters.PermissionResolver, ans answering, playerRoster *roster.Roster, auditor audit.Store) {
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
	eventBus.Publish(chat.MessageEvent{
		ActorXUID: id,
		Gamertag:  rosterName(playerRoster, id),
		Message:   text.Message,
		Trigger:   trigger,
		Public:    chat.IsPublicType(text.TextType),
	})

	switch trigger.Kind {
	case chat.TriggerCommand:
		handleCommand(ctx, id, trigger, chat.IsPrivateType(text.TextType), log, registry, pctx, limiter, permResolver, auditor, playerRoster)
	case chat.TriggerMention:
		// A command in a mention's clothing: dispatched like !leave, so it
		// gets the same permission check, rate limit and audit row, and never
		// reaches the model.
		if args, ok := presence.LeaveArgs(trigger.Message); ok {
			handleCommand(ctx, id, chat.Trigger{Kind: chat.TriggerCommand, Command: "leave", Args: args}, chat.IsPrivateType(text.TextType), log, registry, pctx, limiter, permResolver, auditor, playerRoster)
			return
		}
		startAnswer(ctx, id, trigger, chat.IsPrivateType(text.TextType), log, pctx, ans, playerRoster)
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
func newPluginContext(cfg config.Config, bridgeClient *adapters.BridgeClient, bridgeTimeout time.Duration, voice plugin.Voice, playerRoster *roster.Roster, registry *plugin.Registry, playerStore store.Store, knowledgeStore knowledge.Store, waypointStore waypoints.Store, announceStore plugin.AnnounceStore, deliverer plugin.AnnounceDeliverer, pinger plugin.Pinger, moderationStore plugin.ModerationStore, scheduleStore plugin.ScheduleStore) *plugin.Context {
	return &plugin.Context{
		// The same Voice the Deliverer speaks through, passed in rather than
		// built here: an announcement and a command reply are the same console
		// bridge saying the same kind of thing, and a second instance would be
		// a second place for that to stop being true.
		Voice: voice,
		Facts: adapters.NewBridgeFacts(bridgeClient),
		// Shares the bridge's timeout: both are "one HTTP call to something
		// in this namespace", and a second knob for the same property is a
		// knob that drifts.
		ServerInfo: adapters.NewMetricsFacts(
			adapters.NewMetricsClient(cfg.MCMonitorURL, cfg.BackupExporterURL, bridgeTimeout),
		),
		Directory: registry,
		// The live roster again, asked the other question: not who a name
		// belongs to, but whether they are still here to see what is sent.
		Presence: playerRoster,
		// store.Nop when no database is configured, never nil -- plugin.Context
		// documents Profiles as possibly nil and the plugins guard for it, but
		// this binary has no reason to hand them one.
		Profiles: playerStore,
		// Same reasoning as Profiles: pool-backed when a database is
		// configured, the disabled implementation when not, never nil.
		Knowledge: knowledgeStore,
		Waypoints: waypointStore,
		// Same reasoning as Profiles again -- pool-backed when a database is
		// configured, the disabled implementation when not, never nil, even
		// though the field is documented as possibly nil for tests that
		// construct a bare Context.
		Announcements: announceStore,
		Deliverer:     deliverer,
		// Same again: the disabled implementation with no database.
		Schedules: scheduleStore,
		// The live roster backed by the profile store, so "@player" resolves
		// for someone who is offline -- which is precisely who a queued
		// announcement is for. With no database configured the second tier
		// answers "never seen", and !announce refuses exactly as it did
		// before there was one.
		Roster: playerLookup{live: playerRoster, archive: playerStore},
		Pinger: pinger,
		// Same reasoning as Profiles: moderation.Nop when no database is
		// configured, never nil.
		Moderation: moderationStore,
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

// storeOpenWindow bounds how long startup keeps asking an unreachable
// Postgres before settling for no store. Long enough to ride out a failover
// or a pod whose network is not yet routable; short enough that a database
// that is really gone becomes a failure an operator sees within minutes.
// Vars so a test need not spend the real ones.
var (
	storeOpenWindow     = 90 * time.Second
	storeRetryFirstWait = time.Second
	storeRetryMaxWait   = 15 * time.Second
)

// storeOpener is store.Open's signature, taken as a parameter so a test can
// stand in a database that fails a given number of times.
type storeOpener func(ctx context.Context, dsn string, connectTimeout time.Duration) (*store.Postgres, error)

// openStore connects to Postgres if configured, and degrades to Nop if not.
//
// Deliberately never returns an error. Every failure here -- unset, malformed,
// unreachable -- lands the agent in the same supported state for greetings
// and commands. It is not the same state for authentication: with a database
// configured the Xbox token lives in it, so an agent that settles for Nop is
// an agent that cannot log in. Hence the retries, with backoff and within
// storeOpenWindow, before an unreachable database is taken as the answer. A
// malformed DSN is not retried: it will read the same every time.
func openStore(ctx context.Context, cfg config.Config, log *logging.Logger, open storeOpener) store.Store {
	dsn := cfg.PostgresDSN()
	if dsn == "" {
		log.Info("store_disabled", logging.Fields{"reason": "PG_HOST unset"})
		return store.Nop{}
	}
	connectTimeout := time.Duration(cfg.PGConnectTimeoutMs) * time.Millisecond
	deadline := time.Now().Add(storeOpenWindow)
	wait := storeRetryFirstWait
	for attempt := 1; ; attempt++ {
		pg, err := open(ctx, dsn, connectTimeout)
		if err == nil {
			// Sessions a previous run left open are closed by beginWatching,
			// at the start of every connection including the first.
			log.Info("store_ready", logging.Fields{"database": cfg.PGDatabase, "attempt": attempt})
			return pg
		}
		if errors.Is(err, store.ErrInvalidDSN) || ctx.Err() != nil || time.Now().Add(wait).After(deadline) {
			log.Error("store_open_failed", logging.Fields{"error": err.Error(), "attempts": attempt})
			return store.Nop{}
		}
		log.Info("store_open_retrying", logging.Fields{"error": err.Error(), "attempt": attempt, "wait_ms": wait.Milliseconds()})
		select {
		case <-ctx.Done():
			log.Error("store_open_failed", logging.Fields{"error": err.Error(), "attempts": attempt})
			return store.Nop{}
		case <-time.After(wait):
		}
		wait = min(wait*2, storeRetryMaxWait)
	}
}

// startAnswer decides whether an @server question is answered, then answers
// it off the read loop.
//
// Every refusal is made here, on the caller's goroutine, and each is a mutex
// or a channel rather than a network call. The rate limiter especially: its
// budget is per rolling minute, so four questions in one second are four
// allowed answers, and spawning each one concurrently would interleave four
// broadcasts. Running inline used to serialise them; nothing else does now.
// asked is how the question reached the agent. whispered carries through to
// where the answer is sent, because a /tell had no audience and so needs no
// public answer.
func startAnswer(ctx context.Context, actorXUID string, trigger chat.Trigger, whispered bool, log *logging.Logger, pctx *plugin.Context, ans answering, playerRoster *roster.Roster) {
	if ans.llm == nil || !ans.llm.Enabled() {
		// Stage 1-3 behaviour, kept as the unconfigured path: detection is
		// proven, nothing is answered.
		metrics.Mention(metrics.MentionDisabled)
		log.Info("mention_received", logging.Fields{"actor": actorXUID, "message": trigger.Message})
		return
	}

	if !ans.limiter.Allow(actorXUID, time.Now()) {
		// Silent on purpose. Telling a player they are rate limited is itself
		// a chat line, so a spammer would still get one message per attempt.
		metrics.Mention(metrics.MentionRateLimited)
		log.Info("mention_rate_limited", logging.Fields{"actor": actorXUID})
		return
	}

	select {
	case ans.inFlight <- struct{}{}:
	default:
		// Dropped, not queued: an answer that waits for a slot arrives after
		// the conversation it belongs to has moved on, and the asker has by
		// then read the silence as the answer.
		metrics.Mention(metrics.MentionBusy)
		log.Info("mention_answer_dropped_busy", logging.Fields{"actor": actorXUID, "max_concurrent": cap(ans.inFlight)})
		return
	}

	// ctx is the process's, not the session's, and deliberately: the reply is
	// delivered through the console bridge, which is a separate service from
	// the Bedrock connection, so a reconnect mid-answer does not invalidate
	// it. ans.total bounds how stale it can be.
	go func() {
		defer func() { <-ans.inFlight }()
		handleMention(ctx, actorXUID, trigger, whispered, log, pctx, ans, playerRoster)
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
func handleMention(ctx context.Context, actorXUID string, trigger chat.Trigger, whispered bool, log *logging.Logger, pctx *plugin.Context, ans answering, playerRoster *roster.Roster) {
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
	started := time.Now()
	reply, err := ans.llm.AnswerWithTools(answerCtx, name, actorXUID, trigger.Message, registry)
	// Timed as answered even when the reply turns out empty: the histogram
	// is how long the model takes to come back, and an empty completion took
	// exactly as long as a usable one. Whether the reply could be used is
	// what the mentions counter below records.
	metrics.Answer(time.Since(started), err)
	if err != nil {
		// Logged, never spoken. A backend timeout is an operator's problem,
		// and narrating it in chat turns one failure into an audience.
		metrics.Mention(metrics.MentionFailed)
		log.Error("mention_answer_failed", logging.Fields{"actor": actorXUID, "error": err.Error()})
		return
	}
	if reply == "" {
		// An empty completion broadcast as a blank line reads to players as
		// the server glitching -- a bug minecraft-afk-bot shipped and fixed.
		metrics.Mention(metrics.MentionEmpty)
		log.Info("mention_answer_empty", logging.Fields{"actor": actorXUID})
		return
	}

	if pctx.Voice == nil {
		metrics.Mention(metrics.MentionUndeliverable)
		log.Error("mention_answer_undeliverable", logging.Fields{"actor": actorXUID})
		return
	}
	// Broadcast by default: an @server question asked in open chat has an
	// audience, and an answer only the asker can see reads as no answer at
	// all to everyone else who watched them ask.
	//
	// Two things make an answer private instead.
	//
	// The question arrived as a whisper. A /tell was seen by nobody, so
	// there is no audience the broadcast default exists to serve, and
	// answering in open chat would publish a question its asker chose not
	// to.
	//
	// Or the model built the answer from the asker's own waypoints. Those
	// coordinates are personal -- !wp whispers them because broadcasting
	// where a player lives is a griefing vector -- and they do not stop
	// being personal because the question reached them through @server.
	//
	// The console has no player to whisper to, and nothing it asks about is
	// its own, so it keeps the broadcast either way.
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

	private := (whispered || personal.Happened()) && actorXUID != chat.ServerOrigin
	var sendErr error
	if private {
		sendErr = pctx.Voice.Tell(sayCtx, actorXUID, reply)
	} else {
		sendErr = pctx.Voice.Say(sayCtx, reply)
	}
	if sendErr != nil {
		metrics.Mention(metrics.MentionSendFailed)
		log.Error("mention_answer_send_failed", logging.Fields{"actor": actorXUID, "error": sendErr.Error()})
		return
	}
	metrics.Mention(metrics.MentionAnswered)
	log.Info("mention_answered", logging.Fields{"actor": actorXUID, "reply_chars": len(reply), "private": private})
}

// handlePlayerList updates the live roster from one PlayerList packet and
// publishes a roster.JoinEvent for each genuinely new, non-self,
// non-sibling arrival — filtered here, before publishing, the same way
// handleText filters chat before publishing chat.MessageEvent, so every
// event-driven plugin downstream can assume it never sees this agent's own
// presence or a sibling bot's.
func handlePlayerList(ctx context.Context, pk *packet.PlayerList, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, eventBus *bus.Bus, playerRoster *roster.Roster, playerStore store.Store, joinClock *joinTimes) {
	entries := make([]roster.PlayerListEntry, len(pk.Entries))
	for i, e := range pk.Entries {
		entries[i] = roster.PlayerListEntry{
			XUID:     e.XUID,
			Username: e.Username,
			UUID:     e.UUID.String(),
			Remove:   e.ActionType == protocol.PlayerListActionRemove,
		}
	}

	joins, leaves, alreadyOnline := playerRoster.Apply(entries)
	// Already here when this connection began, so their session restarts now
	// -- playtime counts only what the agent watched -- and nothing greets
	// them: they did not just arrive. The one exception is a player the agent
	// has never seen before, whom operators are told of once. A snapshot
	// that adds and then removes the same player is read the same way the
	// arrivals below are: what the roster holds at the end of the packet is
	// who is here.
	generation := joinClock.Generation()
	for _, p := range alreadyOnline {
		if chat.IsSelfOrSibling(p.XUID, selfXUID, siblingXUIDs) {
			continue
		}
		if !playerRoster.IsOnline(p.XUID) {
			continue
		}
		if _, err := playerStore.ResumeSession(ctx, p.XUID, p.Username, time.Now()); err != nil {
			log.Error("store_resume_session_failed", logging.Fields{"xuid": p.XUID, "error": err.Error()})
		}
		// No arrival is recorded for them: this is not one, and the clock
		// must keep answering that honestly. What they get is the event,
		// so whoever owes them something delayed can pay it on this
		// connection instead of leaving it for a join that may never come.
		eventBus.Publish(roster.PresentEvent{Entry: p, Generation: generation})
	}
	// One packet can carry both directions for the same player, and Apply
	// reports them in two slices that no longer say which came first --
	// what the roster holds afterwards does. A player added and then
	// removed inside one packet is gone: nothing greets them and nothing
	// schedules them a delivery they cannot receive.
	for _, join := range joins {
		if chat.IsSelfOrSibling(join.XUID, selfXUID, siblingXUIDs) {
			continue
		}
		if !playerRoster.IsOnline(join.XUID) {
			continue
		}
		log.Info("player_joined", logging.Fields{"xuid": join.XUID, "username": join.Username})
		// Recorded before the event is published, so anything the join sets
		// off already sees this player as the fresh arrival they are.
		joinClock.joined(join.XUID)
		eventBus.Publish(roster.JoinEvent{Entry: join, Generation: generation})
	}
	// Departures close a session rather than reaching a plugin. Nothing
	// greets a player for leaving, and publishing an event no handler wants
	// would be scaffolding for its own sake.
	for _, leave := range leaves {
		if chat.IsSelfOrSibling(leave.XUID, selfXUID, siblingXUIDs) {
			continue
		}
		log.Info("player_left", logging.Fields{"xuid": leave.XUID, "username": leave.Username})
		if !playerRoster.IsOnline(leave.XUID) {
			// Still gone at the end of the packet, so this departure is the
			// last word on them. When it isn't -- a removal and a re-add in
			// one packet -- the arrival above is newer than this and must
			// survive, or the returning client reads as settled while it
			// loads.
			joinClock.left(leave.XUID)
		}
		if _, err := playerStore.RecordLeave(ctx, leave.XUID, playerRoster.Since(), time.Now()); err != nil {
			// Logged, never fatal: the session stays open until the next
			// connection closes it at zero length, so this visit's time is
			// lost, and a database problem must not disturb the game.
			log.Error("store_record_leave_failed", logging.Fields{"xuid": leave.XUID, "error": err.Error()})
		}
	}
}

// whispered says the command arrived through /tell rather than open chat. It
// decides only whether an unknown command is answered -- see the
// ErrUnknownCommand case below for why that one distinction exists.
func handleCommand(ctx context.Context, actorXUID string, trigger chat.Trigger, whispered bool, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, limiter *ratelimit.PerActor, permResolver *adapters.PermissionResolver, auditor audit.Store, playerRoster *roster.Roster) {
	// The reply and the audit row are sent on a context the session cannot
	// end, each still under its own bound: a !leave's write wakes the loop
	// that cancels the session before the command has even returned, and
	// the operator must still hear the answer and the dispatch be recorded.
	settle := context.WithoutCancel(ctx)

	// Mirrors handleMention's resolution exactly: the roster is the one place
	// an XUID becomes a name, and a second lookup path here would be a second
	// place for that mapping to drift from the first.
	gamertag := actorXUID
	if playerRoster != nil {
		if resolved, ok := playerRoster.NameFor(actorXUID); ok && resolved != "" {
			gamertag = resolved
		}
	}

	// Filled in once permission resolution below actually runs. A
	// rate-limited dispatch never reaches that point, so its record shows
	// what was known at refusal rather than a resolved level the bridge was
	// never asked for.
	var permission string

	// One record per dispatch, whatever happened. Every exit path below calls
	// this exactly once, and it must never be what makes that path slow: a
	// compliance record that can delay or fail a command is a worse liability
	// than a gap in the record, so a write failure is logged and swallowed
	// rather than surfaced to the caller.
	writeAudit := func(outcome audit.Outcome) {
		// Counted here because this is already the one call every exit path
		// makes, and before the enabled check because a dispatch happened
		// whether or not a database is configured to record it.
		metrics.Command(commandLabel(registry, trigger.Command), string(outcome))
		if auditor == nil || !auditor.Enabled() {
			return
		}
		// Bounded like the permission lookup and the reply below: this call
		// reaches a database from the same read-loop goroutine, and with no
		// deadline of its own a slow or hung one would stall every player's
		// commands behind it -- exactly what "never blocks the command" rules
		// out.
		auditCtx, auditCancel := context.WithTimeout(settle, plugin.DefaultDispatchTimeout)
		defer auditCancel()
		if err := auditor.Write(auditCtx, audit.Record{
			XUID:       actorXUID,
			Gamertag:   gamertag,
			Permission: permission,
			Command:    trigger.Command,
			Args:       strings.Join(trigger.Args, " "),
			Outcome:    outcome,
			At:         time.Now(),
		}); err != nil {
			metrics.AuditWriteFailure()
			log.Error("audit_write_failed", logging.Fields{
				"command": trigger.Command, "actor": actorXUID, "outcome": string(outcome), "error": err.Error(),
			})
		}
	}

	if !limiter.Allow(actorXUID, time.Now()) {
		log.Info("command_rate_limited", logging.Fields{"command": trigger.Command, "actor": actorXUID})
		writeAudit(audit.OutcomeRateLimited)
		return
	}

	// Bounded separately from the command dispatch below: a slow or down
	// bridge must not itself stall the read loop waiting on a permission
	// lookup before Dispatch even gets its own timeout.
	permCtx, permCancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
	actorPermission := permResolver.Resolve(permCtx, actorXUID)
	permCancel()
	permission = actorPermission.String()

	inv := plugin.Invocation{ActorXUID: actorXUID, ActorPermission: actorPermission, Args: trigger.Args}

	reply, err := registry.Dispatch(ctx, pctx, trigger.Command, inv)
	switch {
	case errors.Is(err, plugin.ErrUnknownCommand):
		// Answered only when whispered. ParseTrigger treats any message
		// opening with "!" as a command, so "!!!" and "!nice that was
		// close" reach this branch too -- replying to every one of them
		// would have the agent talking over ordinary conversation. A
		// whisper is the opposite situation: the player addressed the agent
		// directly, nobody else can see it, and silence there is
		// indistinguishable from the agent being down.
		log.Debug("command_unknown", logging.Fields{"command": trigger.Command, "actor": actorXUID, "whispered": whispered})
		if whispered {
			speak(settle, log, pctx, actorXUID, trigger.Command, unknownCommandReply(trigger.Command))
		}
		writeAudit(audit.OutcomeUnknown)
		return
	case errors.Is(err, plugin.ErrPermissionDenied):
		// Always answered, whispered or not, and the stray-"!" argument
		// above does not apply: a denial means the command matched a real
		// registered one, so nothing conversational can land here.
		//
		// This does tell the asker the command exists, which !help does not
		// -- it lists only what the actor may run. Naming it is the
		// deliberate trade: a player who guessed right and got silence
		// cannot tell a refusal from a broken agent, and that costs more
		// than the existence of !shutdown being guessable on a server whose
		// members are known to each other.
		log.Info("command_denied", logging.Fields{"command": trigger.Command, "actor": actorXUID})
		speak(settle, log, pctx, actorXUID, trigger.Command, deniedCommandReply(trigger.Command))
		writeAudit(audit.OutcomeDenied)
		return
	case errors.Is(err, plugin.ErrCommandTimedOut):
		// Checked ahead of the generic err != nil case below: ErrCommandTimedOut
		// wraps into that branch too, and a timeout recorded as a plain error
		// loses exactly the distinction the schema draws between them.
		log.Error("command_timed_out", logging.Fields{"command": trigger.Command, "actor": actorXUID})
		speak(settle, log, pctx, actorXUID, trigger.Command, commandTimedOutReply)
		writeAudit(audit.OutcomeTimeout)
		return
	case err != nil:
		log.Error("command_failed", logging.Fields{"command": trigger.Command, "actor": actorXUID, "error": err.Error()})
		speak(settle, log, pctx, actorXUID, trigger.Command, commandFailedReply)
		writeAudit(audit.OutcomeError)
		return
	}

	replied := logging.Fields{"command": trigger.Command, "actor": actorXUID}
	if cmd, ok := registry.Lookup(trigger.Command); ok && cmd.RedactReply {
		replied["reply_chars"] = len(reply)
	} else {
		replied["reply"] = reply
	}
	log.Info("command_replied", replied)

	speak(settle, log, pctx, actorXUID, trigger.Command, reply)
	// Written after the reply is sent, not before: the record must never be
	// in front of what the player is waiting on.
	writeAudit(audit.OutcomeOK)
}

// commandLabel is the metric label for a typed command: the name it is
// registered under, or metrics.Unregistered for anything else.
//
// Decided by asking the registry rather than by the outcome. An unknown
// command is the obvious case, but a rate-limited dispatch is refused before
// anything resolves what was typed, so "!" followed by any word a spammer
// likes would otherwise reach the label through that path instead.
func commandLabel(registry *plugin.Registry, typed string) string {
	if cmd, ok := registry.Lookup(typed); ok {
		return cmd.Name
	}
	return metrics.Unregistered
}

// initCommandMetrics starts every command the registry holds, and the
// unregistered bucket, at zero for every audit outcome.
func initCommandMetrics(registry *plugin.Registry) {
	names := []string{metrics.Unregistered}
	for _, cmd := range registry.Commands() {
		names = append(names, cmd.Name)
	}
	var outcomes []string
	for _, o := range audit.Outcomes() {
		outcomes = append(outcomes, string(o))
	}
	metrics.InitCommands(names, outcomes)
}

// commandFailedReply and commandTimedOutReply are what a dispatch that
// produced no reply of its own says instead of nothing.
//
// A command that errors is a command whose author never got to write an
// answer for what went wrong, and every one of those used to end in
// silence: the player or operator sees their own typed line and then
// nothing, which reads exactly like a command that worked and had nothing
// to say. The two are separated because they call for different next steps
// -- one is a failure already in the log, the other is something still
// running that outlasted its budget.
const (
	commandFailedReply   = "That didn't work - the failure is in my log."
	commandTimedOutReply = "That took too long, so I stopped waiting on it."
)

// unknownCommandReply and deniedCommandReply both point at !help rather than
// listing anything themselves, because !help already filters to what the
// asker may actually run and duplicating that here would be a second place
// for the two to disagree.
func unknownCommandReply(command string) string {
	return "I don't know !" + command + ". Try !help to see what I can do."
}

func deniedCommandReply(command string) string {
	return "!" + command + " isn't available to you - !help lists what is."
}

// speak sends one reply back the way the command came in: broadcast for the
// console, which has no player to whisper to, and a whisper for anyone else.
//
// Bounded like Dispatch is, and for the same reason: this runs on the
// packet-read goroutine, so a hung Voice implementation must not be able to
// stall the read loop indefinitely.
func speak(ctx context.Context, log *logging.Logger, pctx *plugin.Context, actorXUID, command, reply string) {
	replyCtx, cancel := context.WithTimeout(ctx, plugin.DefaultDispatchTimeout)
	defer cancel()

	if actorXUID == chat.ServerOrigin {
		if err := pctx.Voice.Say(replyCtx, reply); err != nil {
			log.Error("voice_say_failed", logging.Fields{"command": command, "error": err.Error()})
		}
		return
	}
	if err := pctx.Voice.Tell(replyCtx, actorXUID, reply); err != nil {
		log.Error("voice_tell_failed", logging.Fields{"command": command, "actor": actorXUID, "error": err.Error()})
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
