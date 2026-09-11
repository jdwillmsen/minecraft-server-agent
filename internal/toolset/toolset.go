// Package toolset wires the plugin context's capabilities into the tools
// the @server answer path offers the model.
//
// A package of its own rather than part of cmd/agent so the offline
// evaluation harness builds exactly the toolset production builds. A copy
// there would measure whatever the copy said, not what players get.
package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sync/atomic"

	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
)

// CallerScoped records whether a tool that reads the asking player's own
// data ran while one question was being answered, so the answer can be
// whispered rather than broadcast.
//
// Handed back with the registry it belongs to rather than kept anywhere
// longer-lived: a registry is built fresh per answer, and a flag that
// outlived one would carry a privacy decision into the next player's
// question. Atomic because an answer runs on its own goroutine.
type CallerScoped struct{ used atomic.Bool }

// mark is called on invocation rather than on a successful read: whether
// the sentence the model finally writes contains someone's coordinates is
// not something this side can tell, so the trigger is the model having
// been given them at all.
func (c *CallerScoped) mark() { c.used.Store(true) }

func (c *CallerScoped) Happened() bool { return c != nil && c.used.Load() }

// noArgs is the schema for a tool that takes nothing. Sent rather than
// omitted because some OpenAI-compatible backends reject a function with no
// parameters object at all.
var noArgs = json.RawMessage(`{"type":"object","properties":{}}`)

// Build assembles the read-only tools for one answer.
//
// A capability that is not configured contributes no tool. That is the
// design's central safety property expressed in wiring: the model's
// available actions are exactly what this function registers, and none of
// them write.
//
// There is deliberately no player_playtime tool: plugin.PlayerStore offers
// only RecordJoin and Enabled, so every path to a playtime figure also
// records a join, and a read tool that writes is exactly what the paragraph
// above rules out. Deferred rather than ruled out -- this same branch
// widened plugin.ServerInfo by two methods for the same class of reason.
// Adding it means a read-only lookup on PlayerStore (a profile by XUID that
// writes nothing), implemented on store.Postgres and store.Nop, and a tool
// built on that.
//
// The returned CallerScoped reports whether the model was given the asking
// player's own data, which decides whether the answer is whispered.
func Build(pctx *plugin.Context) (*tools.Registry, *CallerScoped) {
	var list []tools.Tool
	scoped := &CallerScoped{}

	if pctx.Knowledge != nil && pctx.Knowledge.Enabled() {
		list = append(list, tools.Tool{
			Name:        "knowledge_lookup",
			Description: "Look up what this server's operators have recorded about a topic: rules, farm locations, build sites.",
			// A short, topic-like query ("gold farm") matches best, but the
			// search also matches on any word in a longer phrase, so a
			// query that repeats the player's whole question still works.
			Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look up. A short topic works best, e.g. 'gold farm', but a longer phrase also matches."}},"required":["query"]}`),
			Invoke: func(ctx context.Context, args json.RawMessage, _ string) (string, error) {
				var a struct {
					Query string `json:"query"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("knowledge_lookup: bad arguments: %w", err)
				}
				entries, err := pctx.Knowledge.Lookup(ctx, a.Query, 3)
				if err != nil {
					return "", err
				}
				if len(entries) == 0 {
					return "nothing recorded about that", nil
				}
				parts := make([]string, 0, len(entries))
				for _, e := range entries {
					line := e.Topic + ": " + e.Body
					// A fallback-only match has no full-text overlap with
					// the query at all -- flagged here rather than left
					// looking identical to a confirmed hit, so the model
					// doesn't state someone else's fact as a settled answer
					// to this question.
					if e.Matched == knowledge.MatchFallback {
						line = "possible match, " + line
					}
					parts = append(parts, line)
				}
				return strings.Join(parts, " | "), nil
			},
		})
	}

	if pctx.Waypoints != nil && pctx.Waypoints.Enabled() {
		list = append(list,
			tools.Tool{
				Name:        "waypoint_lookup",
				Description: "Get the coordinates the asking player saved under a name, such as their base.",
				Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The waypoint name"}},"required":["name"]}`),
				Invoke: func(ctx context.Context, args json.RawMessage, caller string) (string, error) {
					var a struct {
						Name string `json:"name"`
					}
					if err := json.Unmarshal(args, &a); err != nil {
						return "", fmt.Errorf("waypoint_lookup: bad arguments: %w", err)
					}
					// caller, never an argument: the model names a waypoint,
					// it does not choose whose.
					scoped.mark()
					wp, found, err := pctx.Waypoints.Get(ctx, caller, a.Name)
					if err != nil {
						return "", err
					}
					// Worded as the asker's own. A bare "base is at ..." left
					// the model to attach whichever player the question named,
					// and it reported the asker's base as someone else's.
					if !found {
						return "you have no waypoint by that name", nil
					}
					return fmt.Sprintf("your waypoint %s is at %d %d %d in the %s", wp.Name, wp.X, wp.Y, wp.Z, wp.Dimension), nil
				},
			},
			tools.Tool{
				Name:        "waypoint_list",
				Description: "List the names of the waypoints the asking player has saved.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, caller string) (string, error) {
					scoped.mark()
					saved, err := pctx.Waypoints.List(ctx, caller)
					if err != nil {
						return "", err
					}
					if len(saved) == 0 {
						return "you have no saved waypoints", nil
					}
					names := make([]string, 0, len(saved))
					for _, wp := range saved {
						names = append(names, wp.Name)
					}
					return "your saved waypoints: " + strings.Join(names, ", "), nil
				},
			},
		)
	}

	if pctx.Facts != nil {
		list = append(list, tools.Tool{
			Name:        "players_online",
			Description: "Who is currently connected to the server.",
			Schema:      noArgs,
			Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
				return pctx.Facts.PlayersOnline(ctx)
			},
		})
	}

	// Gated on the exporters, not merely on ServerInfo being present:
	// production always constructs it, and an unconfigured exporter can only
	// answer that it is unconfigured. Offering that tool spends one of two
	// tool rounds and part of a small model's prompt budget to discover an
	// absence the wiring already knows about.
	if pctx.ServerInfo != nil && pctx.ServerInfo.StatusEnabled() {
		list = append(list,
			tools.Tool{
				Name:        "server_status",
				Description: "Server health, player count and responsiveness.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
					return pctx.ServerInfo.ServerStatus(ctx)
				},
			},
			tools.Tool{
				Name:        "server_version",
				Description: "Which Bedrock version this server runs.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
					return pctx.ServerInfo.Version(ctx)
				},
			},
		)
	}

	// The backup exporter is a separate deployment from mc-monitor, so it is
	// separately absent.
	if pctx.ServerInfo != nil && pctx.ServerInfo.BackupEnabled() {
		list = append(list, tools.Tool{
			Name:        "backup_status",
			Description: "How recently the world was backed up and how large that backup was.",
			Schema:      noArgs,
			Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
				return pctx.ServerInfo.BackupStatus(ctx)
			},
		})
	}

	return tools.NewRegistry(list...), scoped
}
