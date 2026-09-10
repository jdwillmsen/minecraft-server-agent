// Package plugins holds every concrete Plugin implementation: core
// (!help/!ping), stats (!players), and welcome (event-driven, no
// commands).
package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// Core provides command discovery (!help) and a server check (!ping).
// Every other plugin should stay silent about its own existence and let
// Core's !help be the single place players learn what's available.
type Core struct{}

// formatPing never fails. A ping that cannot reach part of the server is
// reporting exactly what it was asked about, so each missing half is said
// in place of its number rather than turned into an error.
func formatPing(p plugin.ServerPing) string {
	var tps string
	switch {
	case p.TPSErr != nil:
		tps = "TPS unavailable (console didn't answer)"
	case !p.TPSKnown:
		tps = "TPS still measuring, try again in a minute"
	default:
		tps = fmt.Sprintf("TPS %.1f", p.TPS)
	}
	link := "link unavailable"
	if p.LinkKnown {
		link = "link " + formatRoundTrip(p.Link)
	}
	return "pong - " + tps + ", " + link
}

func formatRoundTrip(d time.Duration) string {
	if d < time.Millisecond {
		return "under 1ms"
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// NewCore builds the core plugin.
func NewCore() Core { return Core{} }

func (Core) Name() string { return "core" }

func (Core) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "ping",
			Description: "Check the server is keeping up: TPS and link latency.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				if pctx.Pinger == nil {
					return "pong", nil
				}
				return formatPing(pctx.Pinger.Ping(ctx)), nil
			},
		},
		{
			Name:        "help",
			Description: "List available commands.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				if pctx.Directory == nil {
					return "", fmt.Errorf("core: no command directory available")
				}
				cmds := pctx.Directory.Commands()
				if len(cmds) == 0 {
					return "No commands registered.", nil
				}
				names := make([]string, 0, len(cmds))
				for _, cmd := range cmds {
					if inv.ActorPermission < cmd.Permission {
						continue
					}
					names = append(names, "!"+cmd.Name)
				}
				if len(names) == 0 {
					return "No commands available to you.", nil
				}
				return "Available: " + strings.Join(names, ", "), nil
			},
		},
	}
}
