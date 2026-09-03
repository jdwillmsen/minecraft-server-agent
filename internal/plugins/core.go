// Package plugins holds every concrete Plugin implementation: core
// (!help/!ping), stats (!players), and welcome (event-driven, no
// commands).
package plugins

import (
	"context"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// Core provides command discovery (!help) and a liveness check (!ping).
// Every other plugin should stay silent about its own existence and let
// Core's !help be the single place players learn what's available.
type Core struct{}

// NewCore builds the core plugin.
func NewCore() Core { return Core{} }

func (Core) Name() string { return "core" }

func (Core) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "ping",
			Description: "Check that the agent is alive.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
				return "pong", nil
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
