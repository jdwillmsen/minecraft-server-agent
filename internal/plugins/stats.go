package plugins

import (
	"context"
	"fmt"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// Stats provides read-only server information commands. Currently just
// !players; grows in a later stage once mc-monitor metrics are wired in.
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
	}
}
