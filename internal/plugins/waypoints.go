package plugins

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

// Waypoints lets a player keep their own named coordinates.
//
// Member level, not visitor: this writes rows keyed to the actor, and a
// server that lets anyone who joined once fill the table has a spam problem
// rather than a feature.
type Waypoints struct{}

// NewWaypoints builds the waypoints plugin.
func NewWaypoints() *Waypoints { return &Waypoints{} }

func (*Waypoints) Name() string { return "waypoints" }

func (*Waypoints) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "wp",
			Description: "Your saved coordinates: !wp, !wp <name>, !wp set <name> <x> <y> <z>.",
			Permission:  plugin.PermissionMember,
			Run:         runWP,
		},
	}
}

func runWP(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Waypoints == nil || !pctx.Waypoints.Enabled() {
		return "I have no waypoint store configured.", nil
	}

	if len(inv.Args) == 0 {
		list, err := pctx.Waypoints.List(ctx, inv.ActorXUID)
		if err != nil {
			return "", fmt.Errorf("waypoints: !wp: %w", err)
		}
		if len(list) == 0 {
			return "You have no waypoints. Save one with !wp set <name> <x> <y> <z>.", nil
		}
		names := make([]string, 0, len(list))
		for _, wp := range list {
			names = append(names, wp.Name)
		}
		return "Your waypoints: " + strings.Join(names, ", "), nil
	}

	switch strings.ToLower(inv.Args[0]) {
	case "set":
		if len(inv.Args) < 5 {
			return "Usage: !wp set <name> <x> <y> <z> [overworld|nether|end].", nil
		}
		coords := make([]int, 3)
		for i, raw := range inv.Args[2:5] {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return "Coordinates must be whole numbers: " + raw + " is not a number.", nil
			}
			coords[i] = n
		}
		dimension := ""
		if len(inv.Args) >= 6 {
			dimension = inv.Args[5]
		}
		wp := waypoints.Waypoint{
			Name: inv.Args[1], X: coords[0], Y: coords[1], Z: coords[2], Dimension: dimension,
		}
		if err := pctx.Waypoints.Set(ctx, inv.ActorXUID, wp); err != nil {
			if errors.Is(err, waypoints.ErrUnknownDimension) {
				return "Dimension must be overworld, nether or end.", nil
			}
			return "", fmt.Errorf("waypoints: !wp set: %w", err)
		}
		return fmt.Sprintf("Saved %s at %d %d %d.", waypoints.NormalizeName(wp.Name), wp.X, wp.Y, wp.Z), nil

	case "del":
		if len(inv.Args) < 2 {
			return "Usage: !wp del <name>.", nil
		}
		if err := pctx.Waypoints.Delete(ctx, inv.ActorXUID, inv.Args[1]); err != nil {
			return "", fmt.Errorf("waypoints: !wp del: %w", err)
		}
		return "Deleted " + waypoints.NormalizeName(inv.Args[1]) + ".", nil

	default:
		name := strings.Join(inv.Args, " ")
		wp, found, err := pctx.Waypoints.Get(ctx, inv.ActorXUID, name)
		if err != nil {
			return "", fmt.Errorf("waypoints: !wp: %w", err)
		}
		if !found {
			return "You have no waypoint called " + waypoints.NormalizeName(name) + ".", nil
		}
		return fmt.Sprintf("%s: %d %d %d (%s).", wp.Name, wp.X, wp.Y, wp.Z, wp.Dimension), nil
	}
}
