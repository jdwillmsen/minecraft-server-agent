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

// wpListReplyCap mirrors adapters.MaxReplyChars: this reply crosses the
// same Bedrock chat line, but !wp is dispatched by the plugin registry
// rather than the LLM answer path, so nothing else enforces the cap for it.
// Defined locally rather than imported so this package does not have to
// depend on adapters for one number.
const wpListReplyCap = 200

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
		return formatWaypointList(list), nil
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
		if _, err := parseCoord(rest[len(rest)-1]); err == nil {
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
		// A numeric token right where the name ends and the coordinates
		// begin is genuinely ambiguous: it could be the last word of a name
		// like "base 1", or a stray extra number the player meant to
		// remove. Guessing either way risks silently saving the wrong name
		// at the wrong coordinates, so refuse rather than pick one. Uses
		// parseCoord, not a bare strconv.Atoi, so "x=1" is exactly as
		// numeric-shaped here as "1" is -- the boundary check above already
		// treats it that way, and disagreeing between the two would let a
		// coordinate-display prefix slip past the guard it exists to close.
		if _, err := parseCoord(nameTokens[len(nameTokens)-1]); err == nil {
			return "I cannot tell where the name ends and the coordinates begin: drop the extra number and try again.", nil
		}

		x, y, z, reply := resolveCoordinates([3]string{coordTokens[0], coordTokens[1], coordTokens[2]})
		if reply != "" {
			return reply, nil
		}

		name := strings.Join(nameTokens, " ")
		if reservedNames[waypoints.NormalizeName(name)] {
			return "That name is reserved: pick another waypoint name.", nil
		}

		wp := waypoints.Waypoint{
			Name: name, X: x, Y: y, Z: z, Dimension: dimension,
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
		// Every argument, joined, exactly as the read path below builds a
		// name: taking one token instead deletes whichever waypoint shares
		// its first word, so a player holding both "gold" and "gold farm"
		// loses the wrong one and is told the right one is gone.
		name := waypoints.NormalizeName(strings.Join(inv.Args[1:], " "))
		removed, err := pctx.Waypoints.Delete(ctx, inv.ActorXUID, name)
		if err != nil {
			return "", fmt.Errorf("waypoints: !wp del: %w", err)
		}
		if !removed {
			return "You have no waypoint called " + name + ".", nil
		}
		return "Deleted " + name + ".", nil

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

// splitCoordLabel splits a leading "x=", "y=" or "z=" (case-insensitive)
// off a coordinate token, matching the form Minecraft's own F3 coordinate
// display uses. label is 0 when s carries no such prefix.
func splitCoordLabel(s string) (label byte, rest string) {
	if len(s) >= 2 {
		switch s[0] {
		case 'x', 'X':
			if s[1] == '=' {
				return 'x', s[2:]
			}
		case 'y', 'Y':
			if s[1] == '=' {
				return 'y', s[2:]
			}
		case 'z', 'Z':
			if s[1] == '=' {
				return 'z', s[2:]
			}
		}
	}
	return 0, s
}

// parseCoord parses a coordinate token that may carry the "x="/"y="/"z="
// prefix splitCoordLabel understands, without regard for which letter it
// is. Every place that needs to know whether a token is coordinate-shaped
// at all -- the name/coordinate boundary detection and the ambiguity guard
// right below it in the "set" case above -- goes through this one
// function, so a prefixed token reads as numeric consistently everywhere.
// Which axis a label names only matters once resolveCoordinates decides the
// three coordinate tokens are worth reading as labels in the first place.
func parseCoord(s string) (int, error) {
	_, rest := splitCoordLabel(s)
	return strconv.Atoi(rest)
}

// ambiguousCoordLabelsReply is returned by resolveCoordinates when the
// three coordinate tokens carry labels that don't resolve safely -- see
// its doc comment for what "safely" means here.
const ambiguousCoordLabelsReply = "I can't tell which number is x, y, or z: label all three, e.g. x=1 y=2 z=3, or give three plain numbers in x y z order."

// resolveCoordinates turns the three tokens !wp set identified as
// coordinates into x, y and z. reply is empty on success; on failure it is
// the chat line to send back, and x, y, z are meaningless.
//
// Three cases, matched to how confident position alone can be:
//
//   - No token is labelled: read positionally, exactly as this command
//     always has. This is the overwhelming common case.
//   - All three tokens are labelled and the labels are exactly x, y and z
//     in some order: the labels decide, not position. A player reading
//     coordinates off Minecraft's own F3 display can copy them down in
//     whatever order the game shows them, and there is only one way to
//     read three distinct axis labels.
//   - Anything else -- a duplicate label, a label on only some of the
//     tokens -- is refused rather than guessed. Silently assigning a
//     labelled y to the x slot, or guessing which axis an unlabelled token
//     next to a labelled one belongs to, is the same silent wrong-place
//     failure the ambiguity guard above already exists to prevent; a
//     player who has gone to the trouble of labelling anything deserves an
//     exact read, not a best effort.
func resolveCoordinates(tokens [3]string) (x, y, z int, reply string) {
	var labels [3]byte
	labelCount := 0
	for i, t := range tokens {
		if l, _ := splitCoordLabel(t); l != 0 {
			labels[i] = l
			labelCount++
		}
	}

	switch labelCount {
	case 0:
		vals := [3]int{}
		for i, t := range tokens {
			n, err := strconv.Atoi(t)
			if err != nil {
				return 0, 0, 0, "Coordinates must be whole numbers: " + t + " is not a number."
			}
			vals[i] = n
		}
		return vals[0], vals[1], vals[2], ""

	case 3:
		seen := map[byte]bool{}
		for _, l := range labels {
			if seen[l] {
				return 0, 0, 0, ambiguousCoordLabelsReply
			}
			seen[l] = true
		}
		if !seen['x'] || !seen['y'] || !seen['z'] {
			return 0, 0, 0, ambiguousCoordLabelsReply
		}
		byAxis := map[byte]int{}
		for i, t := range tokens {
			_, num := splitCoordLabel(t)
			n, err := strconv.Atoi(num)
			if err != nil {
				return 0, 0, 0, "Coordinates must be whole numbers: " + t + " is not a number."
			}
			byAxis[labels[i]] = n
		}
		return byAxis['x'], byAxis['y'], byAxis['z'], ""

	default:
		return 0, 0, 0, ambiguousCoordLabelsReply
	}
}

// waypointListEntry formats one waypoint for !wp's summary line. The
// dimension is only shown when it isn't the overworld -- NormalizeDimension
// already treats an empty dimension as "the overworld, where almost every
// waypoint is", and spelling that out for every entry would spend most of
// the line's character budget on the common case.
func waypointListEntry(wp waypoints.Waypoint) string {
	if wp.Dimension == "" || wp.Dimension == "overworld" {
		return fmt.Sprintf("%s (%d, %d, %d)", wp.Name, wp.X, wp.Y, wp.Z)
	}
	return fmt.Sprintf("%s (%d, %d, %d, %s)", wp.Name, wp.X, wp.Y, wp.Z, wp.Dimension)
}

// formatWaypointList renders !wp's no-argument reply with coordinates
// inline -- names alone made every lookup a two-step, !wp then !wp <name> --
// bounded to wpListReplyCap.
//
// A player is free to save any number of waypoints, and Bedrock chat lines
// don't tolerate an unbounded one. Rather than truncate mid-entry -- which
// would print a dangling, misleading half-coordinate -- this includes as
// many whole entries as fit and folds the rest into a "+N more" count,
// reserved for up front so it can never itself be the thing that overflows.
func formatWaypointList(list []waypoints.Waypoint) string {
	const prefix = "Your waypoints: "
	budget := wpListReplyCap - len(prefix)

	var shown []string
	length := 0
	for i, wp := range list {
		entry := waypointListEntry(wp)
		sep := 0
		if len(shown) > 0 {
			sep = len(", ")
		}
		remaining := len(list) - i - 1
		suffixLen := 0
		if remaining > 0 {
			suffixLen = len(fmt.Sprintf(", +%d more", remaining))
		}
		if length+sep+len(entry)+suffixLen > budget {
			break
		}
		length += sep + len(entry)
		shown = append(shown, entry)
	}

	out := prefix + strings.Join(shown, ", ")
	if left := len(list) - len(shown); left > 0 {
		if len(shown) > 0 {
			out += ", "
		}
		out += fmt.Sprintf("+%d more", left)
	}
	return out
}
