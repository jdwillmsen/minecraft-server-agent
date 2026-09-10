package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// announceUsage is shown for empty or flag-only input, so a mistyped
// command teaches the syntax rather than silently doing nothing.
const announceUsage = "Usage: !announce [@player] [!now] [!urgent] <message>."

// Announce lets an operator say something to the server, right now or
// queued for whoever is offline, and lets any member collect what has been
// queued for them. One plugin for both: they share the same outbox, and a
// future change to how a message is aimed only has one file to touch.
type Announce struct{}

// NewAnnounce builds the announce plugin.
func NewAnnounce() *Announce { return &Announce{} }

func (*Announce) Name() string { return "announce" }

func (*Announce) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "announce",
			Description: "Operator: !announce [@player] [!now] [!urgent] <message>.",
			// The registry's own floor is Operator so !help never lists this
			// to a player who cannot use it; runAnnounce checks the same
			// thing again because a direct call (tests, and any future
			// caller that skips the registry) must refuse exactly the same
			// way rather than trust a gate it did not pass through.
			Permission: plugin.PermissionOperator,
			Run:        runAnnounce,
		},
		{
			Name:        "inbox",
			Description: "Collect the announcements waiting for you.",
			Permission:  plugin.PermissionMember,
			Run:         runInbox,
		},
	}
}

var _ plugin.Plugin = (*Announce)(nil)

// parseAnnounceFlags reads !now, !urgent and an "@player" target from the
// front of args, stopping at the first token that is not one of them.
// Stopping there -- rather than scanning the whole line for flag-shaped
// words -- is what keeps "!announce the !urgent flag goes first" a normal
// announcement whose body happens to contain those words, rather than a
// silent reinterpretation of it as urgent: the same ambiguity !wp set
// refuses to guess at for its own trailing numeric token.
func parseAnnounceFlags(args []string) (now, urgent bool, player string, body []string) {
	i := 0
loop:
	for ; i < len(args); i++ {
		switch tok := args[i]; {
		case tok == "!now":
			now = true
		case tok == "!urgent":
			urgent = true
		case strings.HasPrefix(tok, "@") && len(tok) > 1:
			player = tok[1:]
		default:
			break loop
		}
	}
	return now, urgent, player, args[i:]
}

func runAnnounce(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Announcements == nil || !pctx.Announcements.Enabled() {
		return "I have no announcement store configured.", nil
	}
	if inv.ActorPermission < plugin.PermissionOperator {
		return "Only an operator can send an announcement.", nil
	}
	if len(inv.Args) == 0 {
		return announceUsage, nil
	}

	isNow, isUrgent, playerName, body := parseAnnounceFlags(inv.Args)
	if len(body) == 0 {
		return announceUsage, nil
	}
	if isNow && playerName != "" {
		// online_only means "everyone connected right now"; a player target
		// means one specific person, queued if they are not. Those are two
		// different targets, and guessing which one was meant risks either
		// broadcasting a message meant for one player or silently dropping
		// a countdown nobody but that player was supposed to see.
		return "!now and @player can't both be what this is aimed at: send it as one or the other.", nil
	}

	target := announce.TargetEveryone
	var targetValue, displayName string
	switch {
	case isNow:
		target = announce.TargetOnlineOnly
	case playerName != "":
		if pctx.Roster == nil {
			return "I don't know a player named " + playerName + ".", nil
		}
		xuid, ok := pctx.Roster.XUIDFor(playerName)
		if !ok {
			// Refuse rather than store: an announcement targeted at an XUID
			// nobody holds can never be delivered or drained, so it would
			// sit in the outbox forever looking like a message in flight.
			return "I don't know a player named " + playerName + ".", nil
		}
		target = announce.TargetPlayer
		targetValue = xuid
		displayName = playerName
	}

	priority := announce.PriorityNormal
	if isUrgent {
		priority = announce.PriorityExpedited
	}

	now := time.Now()
	a := announce.Announcement{
		Body:         strings.Join(body, " "),
		Source:       announce.SourceCommand,
		AuthorXUID:   inv.ActorXUID,
		TargetKind:   target,
		TargetValue:  targetValue,
		Priority:     priority,
		Delivery:     announce.DeliveryFor(target),
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    announce.DefaultExpiry(announce.SourceCommand, target, now),
	}

	id, err := pctx.Announcements.Insert(ctx, a)
	if err != nil {
		return "", fmt.Errorf("announce: !announce: %w", err)
	}
	a.ID = id

	if pctx.Deliverer != nil {
		if _, err := pctx.Deliverer.SendNow(ctx, a, id); err != nil {
			return "", fmt.Errorf("announce: !announce: send: %w", err)
		}
	}

	if target == announce.TargetPlayer {
		return "Told " + displayName + ".", nil
	}
	return "Announced.", nil
}

func runInbox(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Deliverer == nil {
		return "I have no announcement store configured.", nil
	}
	// No argument names another player: a player can only ever drain their
	// own queue, the same restriction !wp places on whose coordinates a
	// command can touch.
	delivered, err := pctx.Deliverer.DrainAll(ctx, inv.ActorXUID, time.Now())
	if err != nil {
		return "", fmt.Errorf("announce: !inbox: %w", err)
	}
	if delivered == 0 {
		return "You have nothing new.", nil
	}
	if delivered == 1 {
		return "Delivered 1 message.", nil
	}
	return fmt.Sprintf("Delivered %d messages.", delivered), nil
}
