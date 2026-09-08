package plugins

import (
	"context"
	"fmt"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// Stats provides read-only server information commands.
//
// Two transports back these. !players asks the game console through
// mc-console-bridge; !online, !version and !backup cannot be answered that way
// at all -- Bedrock has no uptime or version console command and knows nothing
// about backups -- so they read the mc-monitor and backup exporters instead.
type Stats struct{}

// NewStats builds the stats plugin.
func NewStats() Stats { return Stats{} }

func (Stats) Name() string { return "stats" }

func (Stats) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "players",
			Description: "List who's currently online.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				if pctx.Facts == nil {
					return "", fmt.Errorf("stats: no facts source available")
				}
				out, err := pctx.Facts.PlayersOnline(ctx)
				if err != nil {
					return "", fmt.Errorf("stats: !players: %w", err)
				}
				return out, nil
			},
		},
		{
			Name:        "online",
			Description: "Server health, player count and responsiveness.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				if pctx.ServerInfo == nil {
					return "", fmt.Errorf("stats: !online: no server info source available")
				}
				out, err := pctx.ServerInfo.ServerStatus(ctx)
				if err != nil {
					return "", fmt.Errorf("stats: !online: %w", err)
				}
				return out, nil
			},
		},
		{
			Name:        "version",
			Description: "Which Bedrock version this server runs.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				if pctx.ServerInfo == nil {
					return "", fmt.Errorf("stats: !version: no server info source available")
				}
				out, err := pctx.ServerInfo.Version(ctx)
				if err != nil {
					return "", fmt.Errorf("stats: !version: %w", err)
				}
				return out, nil
			},
		},
		{
			Name:        "backup",
			Description: "When the world was last backed up.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				if pctx.ServerInfo == nil {
					return "", fmt.Errorf("stats: !backup: no server info source available")
				}
				out, err := pctx.ServerInfo.BackupStatus(ctx)
				if err != nil {
					return "", fmt.Errorf("stats: !backup: %w", err)
				}
				return out, nil
			},
		},
	}
}
