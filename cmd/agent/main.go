// Command agent is minecraft-server-agent: the chat "ear" for the FWB
// Bedrock server. It connects as a headless client, reads chat, and
// dispatches ! commands and @server mentions to registered plugins.
//
// Stage 1 wires the connect loop, plugin dispatch, and the core plugin
// end-to-end with a logging stand-in for server-voice output; a real
// mc-console-bridge-backed Voice, permission resolution from
// permissions.json, and the LLM answer path land in later stages.
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
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/mcauth"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
)

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

	registry := plugin.NewRegistry()
	if err := registry.Register(plugins.NewCore()); err != nil {
		log.Error("plugin_register_failed", logging.Fields{"error": err.Error()})
		os.Exit(1)
	}

	pctx := &plugin.Context{
		Voice:     adapters.NewNoopVoice(log),
		Directory: registry,
	}
	eventBus := bus.New() // wired for future event-driven plugins (join/leave/welcome, Stage 2+)

	httpServer := httpapi.New(cfg.HTTPAddr)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			log.Error("http_server_failed", logging.Fields{"error": err.Error()})
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("shutdown_signal", logging.Fields{"signal": sig.String()})
		cancel()
	}()

	log.Info("starting", logging.Fields{"mc_host": cfg.MCHost, "mc_port": cfg.MCPort})
	runConnectLoop(ctx, cfg, log, registry, pctx, eventBus)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http_shutdown_failed", logging.Fields{"error": err.Error()})
	}
	log.Info("stopped", nil)
}

// runConnectLoop owns the reconnect/backoff policy. Each iteration runs one
// session to completion (or failure), then waits before trying again.
func runConnectLoop(ctx context.Context, cfg config.Config, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus) {
	minDelay := time.Duration(cfg.ReconnectMinMs) * time.Millisecond
	maxDelay := time.Duration(cfg.ReconnectMaxMs) * time.Millisecond
	delay := minDelay

	for ctx.Err() == nil {
		started := time.Now()
		err := session(ctx, cfg, log, registry, pctx, eventBus)
		lasted := time.Since(started)

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error("session_error", logging.Fields{"error": err.Error(), "session_lasted_ms": lasted.Milliseconds()})
		} else {
			log.Info("disconnected", logging.Fields{"session_lasted_ms": lasted.Milliseconds()})
		}

		if lasted >= stableSessionThreshold {
			delay = minDelay
		} else {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
		wait := jitter(delay)
		log.Info("reconnecting", logging.Fields{"delay_ms": wait.Milliseconds()})

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
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
func session(ctx context.Context, cfg config.Config, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus) error {
	ts, err := mcauth.TokenSource(ctx, cfg.AuthCacheDir, os.Stdout)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	dialer := minecraft.Dialer{TokenSource: ts}
	addr := net.JoinHostPort(cfg.MCHost, strconv.Itoa(cfg.MCPort))

	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := dialer.DialContext(dialCtx, "raknet", addr)
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	log.Info("joined", logging.Fields{"address": addr})

	spawnCtx, spawnCancel := context.WithTimeout(ctx, 30*time.Second)
	err = conn.DoSpawnContext(spawnCtx)
	spawnCancel()
	if err != nil {
		return fmt.Errorf("spawn: %w", err)
	}

	selfXUID := conn.IdentityData().XUID
	log.Info("spawned", logging.Fields{"self_xuid": selfXUID})

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
		handlePacket(ctx, pk, selfXUID, siblingXUIDs, log, registry, pctx, eventBus)
	}
}

func handlePacket(ctx context.Context, pk packet.Packet, selfXUID string, siblingXUIDs map[string]struct{}, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context, eventBus *bus.Bus) {
	text, ok := pk.(*packet.Text)
	if !ok || !chat.IsAnswerableType(text.TextType) {
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
	switch trigger.Kind {
	case chat.TriggerCommand:
		handleCommand(ctx, id, trigger, log, registry, pctx)
	case chat.TriggerMention:
		// Stage 4 wires this to the LLM tool-calling loop; Stage 1 only
		// proves the detection path end-to-end.
		log.Info("mention_received", logging.Fields{"actor": id, "message": trigger.Message})
	}
}

func handleCommand(ctx context.Context, actorXUID string, trigger chat.Trigger, log *logging.Logger, registry *plugin.Registry, pctx *plugin.Context) {
	// TODO(stage 2): resolve real permission from permissions.json via
	// mc-console-bridge. Every actor is treated as a visitor until then, so
	// no operator-only command can be reached before that wiring exists.
	inv := plugin.Invocation{ActorXUID: actorXUID, ActorPermission: plugin.PermissionVisitor, Args: trigger.Args}

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
	if actorXUID == chat.ServerOrigin {
		_ = pctx.Voice.Say(ctx, reply)
		return
	}
	_ = pctx.Voice.Tell(ctx, actorXUID, reply)
}
