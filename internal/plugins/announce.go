package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// announceUsage is shown for empty or flag-only input, so a mistyped
// command teaches the syntax rather than silently doing nothing.
const announceUsage = "Usage: !announce [@player] [!now] [!urgent] <message>."

// noStore is what both commands say when the outbox cannot be reached --
// whether because none is configured or because the tables are not there
// yet. One string for both: to the player they are the same fact, and the
// difference between them is an operator's problem, not theirs.
const noStore = "I have no announcement store configured."

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
// multiplePlayers is reported rather than resolved: a second leading
// "@player" token is just as structurally shaped as the first one, so
// nothing here distinguishes "the operator retargeted" from "the operator
// meant to say @A, @B, ..." -- taking the last one silently drops the first
// name from both the target and the body, with no trace it was ever there.
func parseAnnounceFlags(args []string) (now, urgent bool, player string, multiplePlayers bool, body []string) {
	i := 0
loop:
	for ; i < len(args); i++ {
		switch tok := args[i]; {
		case tok == "!now":
			now = true
		case tok == "!urgent":
			urgent = true
		case strings.HasPrefix(tok, "@") && len(tok) > 1:
			if player != "" {
				multiplePlayers = true
				continue
			}
			player = tok[1:]
		default:
			break loop
		}
	}
	return now, urgent, player, multiplePlayers, args[i:]
}

func runAnnounce(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Announcements == nil || !pctx.Announcements.Enabled() {
		return noStore, nil
	}
	if inv.ActorPermission < plugin.PermissionOperator {
		return "Only an operator can send an announcement.", nil
	}
	if len(inv.Args) == 0 {
		return announceUsage, nil
	}

	isNow, isUrgent, playerName, multiplePlayers, body := parseAnnounceFlags(inv.Args)
	if len(body) == 0 {
		return announceUsage, nil
	}
	if multiplePlayers {
		return "An announcement can only go to one player at a time.", nil
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

	// The console has no player identity. chat.ServerOrigin is a sentinel
	// standing in for one, and author_xuid is a foreign key into
	// minecraft.players, so carrying it any further would fail the write --
	// and an announcement with nobody behind it is exactly what the column
	// is documented to be null for. Blanked here, where the announcement is
	// described, rather than on the way to the database: everything
	// downstream, the delivery included, should see the same author the row
	// does.
	authorXUID := inv.ActorXUID
	if authorXUID == chat.ServerOrigin {
		authorXUID = ""
	}

	now := time.Now()
	a := announce.Announcement{
		Body:         strings.Join(body, " "),
		Source:       announce.SourceCommand,
		AuthorXUID:   authorXUID,
		TargetKind:   target,
		TargetValue:  targetValue,
		Priority:     priority,
		Delivery:     announce.DeliveryFor(target),
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    announce.DefaultExpiry(announce.SourceCommand, target, now),
	}

	id, err := pctx.Announcements.Insert(ctx, a)
	if announce.NotMigrated(err) {
		// A store that exists but has no tables behind it yet. Answered like
		// an unconfigured one rather than returned as an error, because an
		// errored command sends no reply at all: the operator would type
		// !announce, see nothing, and have no way to tell that from the
		// message having gone out.
		return noStore, nil
	}
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
		return noStore, nil
	}
	// No argument names another player: a player can only ever drain their
	// own queue, the same restriction !wp places on whose coordinates a
	// command can touch.
	delivered, err := pctx.Deliverer.DrainAll(ctx, inv.ActorXUID, time.Now())
	if announce.NotMigrated(err) {
		// Same reasoning as !announce: silence is the one answer a player
		// cannot interpret.
		return noStore, nil
	}
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
