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

// reservedNames collides with the subcommand names runWP switches on, for
// the same reason knowledge.go refuses a reserved topic: a waypoint saved
// under one of these would be unreachable through !wp <name> even though it
// still sits in the table.
var reservedNames = map[string]bool{"set": true, "del": true}

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

	const setUsage = "Usage: !wp set <name> <x> <y> <z> [overworld|nether|end]."

	switch strings.ToLower(inv.Args[0]) {
	case "set":
		// The trailing arguments are positional (three coordinates, or
		// three coordinates plus a dimension) and everything before them is
		// the name -- parsing from the right, rather than taking a single
		// token after "set", is what makes a multi-word name agree with the
		// read path below, which already joins every argument.
		rest := inv.Args[1:]
		if len(rest) < 4 {
			return setUsage, nil
		}
		var nameTokens, coordTokens []string
		var dimension string
		if _, err := strconv.Atoi(rest[len(rest)-1]); err == nil {
			nameTokens, coordTokens = rest[:len(rest)-3], rest[len(rest)-3:]
		} else {
			if len(rest) < 5 {
				return setUsage, nil
			}
			dimension = rest[len(rest)-1]
			nameTokens, coordTokens = rest[:len(rest)-4], rest[len(rest)-4:len(rest)-1]
		}
		if len(nameTokens) == 0 {
			return setUsage, nil
		}

		coords := make([]int, 3)
		for i, raw := range coordTokens {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return "Coordinates must be whole numbers: " + raw + " is not a number.", nil
			}
			coords[i] = n
		}

		name := strings.Join(nameTokens, " ")
		if reservedNames[waypoints.NormalizeName(name)] {
			return "That name is reserved: pick another waypoint name.", nil
		}

		wp := waypoints.Waypoint{
			Name: name, X: coords[0], Y: coords[1], Z: coords[2], Dimension: dimension,
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
		removed, err := pctx.Waypoints.Delete(ctx, inv.ActorXUID, inv.Args[1])
		if err != nil {
			return "", fmt.Errorf("waypoints: !wp del: %w", err)
		}
		if !removed {
			return "You have no waypoint called " + waypoints.NormalizeName(inv.Args[1]) + ".", nil
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
