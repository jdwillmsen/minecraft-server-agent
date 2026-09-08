package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
)

// noArgs is the schema for a tool that takes nothing. Sent rather than
// omitted because some OpenAI-compatible backends reject a function with no
// parameters object at all.
var noArgs = json.RawMessage(`{"type":"object","properties":{}}`)

// buildToolset assembles the read-only tools for one answer.
//
// A capability that is not configured contributes no tool. That is the
// design's central safety property expressed in wiring: the model's
// available actions are exactly what this function registers, and none of
// them write.
//
// The playtime parameter is accepted and unused: plugin.PlayerStore offers
// only RecordJoin and Enabled, so every path to a playtime figure also
// records a join, and a read tool that writes is exactly what the paragraph
// above rules out. Widening the store interface is a separate change.
func buildToolset(pctx *plugin.Context, _ plugin.PlayerStore) *tools.Registry {
	var list []tools.Tool

	if pctx.Knowledge != nil && pctx.Knowledge.Enabled() {
		list = append(list, tools.Tool{
			Name:        "knowledge_lookup",
			Description: "Look up what this server's operators have recorded about a topic: rules, farm locations, build sites.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look up"}},"required":["query"]}`),
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
					parts = append(parts, e.Topic+": "+e.Body)
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
					wp, found, err := pctx.Waypoints.Get(ctx, caller, a.Name)
					if err != nil {
						return "", err
					}
					if !found {
						return "no waypoint by that name", nil
					}
					return fmt.Sprintf("%s is at %d %d %d in the %s", wp.Name, wp.X, wp.Y, wp.Z, wp.Dimension), nil
				},
			},
			tools.Tool{
				Name:        "waypoint_list",
				Description: "List the names of the waypoints the asking player has saved.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, caller string) (string, error) {
					list, err := pctx.Waypoints.List(ctx, caller)
					if err != nil {
						return "", err
					}
					if len(list) == 0 {
						return "no saved waypoints", nil
					}
					names := make([]string, 0, len(list))
					for _, wp := range list {
						names = append(names, wp.Name)
					}
					return strings.Join(names, ", "), nil
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

	if pctx.ServerInfo != nil {
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
			tools.Tool{
				Name:        "backup_status",
				Description: "How recently the world was backed up and how large that backup was.",
				Schema:      noArgs,
				Invoke: func(ctx context.Context, _ json.RawMessage, _ string) (string, error) {
					return pctx.ServerInfo.BackupStatus(ctx)
				},
			},
		)
	}

	return tools.NewRegistry(list...)
}
