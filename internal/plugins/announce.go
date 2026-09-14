package plugins

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
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

// noAccess is what both commands say when the tables exist but the role the
// agent connects as cannot touch them. Kept distinct from noStore because
// the two need different things done to them: one is a migration that has
// not run, the other is a grant that was never made -- and the grant is the
// failure this project has actually had in production, where it read as the
// agent having gone quiet for no stated reason.
const noAccess = "My announcement store is refusing me access."

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

// announceLine is what the front of an announcement-shaped command says:
// its flags, its "@player" target if it has one, and the body after them.
type announceLine struct {
	now, urgent bool
	player      string
	body        string
}

// parseAnnounceLine reads !now, !urgent and an "@player" target from the
// front of args, stopping at the first token that is not one of them, and
// applies the rules every command that describes an announcement shares.
// refusal is non-empty when the line must not be sent, and is the reason to
// give; usage is what an empty body is answered with, since only the
// command knows its own syntax.
//
// Stopping at the first non-flag -- rather than scanning the whole line for
// flag-shaped words -- is what keeps "!announce the !urgent flag goes first"
// a normal announcement whose body happens to contain those words, rather
// than a silent reinterpretation of it as urgent: the same ambiguity !wp set
// refuses to guess at for its own trailing numeric token.
//
// One helper for !announce and !schedule rather than a copy each: a flag
// that parsed one way in a one-off and another way in a recurring reminder
// would be a difference nobody could see until it fired.
func parseAnnounceLine(args []string, usage string) (line announceLine, refusal string) {
	i := 0
	multiplePlayers := false
loop:
	for ; i < len(args); i++ {
		switch tok := args[i]; {
		case tok == "!now":
			line.now = true
		case tok == "!urgent":
			line.urgent = true
		case strings.HasPrefix(tok, "@") && len(tok) > 1:
			// Reported rather than resolved: a second leading "@player" is
			// just as structurally shaped as the first, so nothing here
			// distinguishes "the operator retargeted" from "the operator meant
			// to say @A, @B, ..." -- taking the last one silently drops the
			// first name from both the target and the body.
			if line.player != "" {
				multiplePlayers = true
				continue
			}
			line.player = tok[1:]
		default:
			break loop
		}
	}
	if i == len(args) {
		return line, usage
	}
	line.body = strings.Join(args[i:], " ")

	if multiplePlayers {
		return line, "An announcement can only go to one player at a time."
	}
	if line.now && line.player != "" {
		// online_only means "everyone connected right now"; a player target
		// means one specific person, queued if they are not. Those are two
		// different targets, and guessing which one was meant risks either
		// broadcasting a message meant for one player or silently dropping
		// a countdown nobody but that player was supposed to see.
		return line, "!now and @player can't both be what this is aimed at: send it as one or the other."
	}
	if n := utf8.RuneCountInString(line.body); n > announce.MaxBodyChars {
		return line, fmt.Sprintf("That message is %d characters; an announcement can be at most %d.", n, announce.MaxBodyChars)
	}
	return line, ""
}

func runAnnounce(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if !pctx.AnnouncementsReady() {
		return noStore, nil
	}
	if inv.ActorPermission < plugin.PermissionOperator {
		return "Only an operator can send an announcement.", nil
	}

	line, refusal := parseAnnounceLine(inv.Args, announceUsage)
	if refusal != "" {
		return refusal, nil
	}
	isNow, isUrgent, playerName := line.now, line.urgent, line.player

	target := announce.TargetEveryone
	var targetValue, displayName string
	switch {
	case isNow:
		target = announce.TargetOnlineOnly
	case playerName != "":
		if pctx.Roster == nil {
			return "I don't know a player named " + playerName + ".", nil
		}
		xuid, ok, err := pctx.Roster.XUIDFor(ctx, playerName)
		if err != nil {
			// Not an answer of "no": the lookup itself could not be made,
			// and storing on a guess would aim a private message at nobody
			// while refusing would deny a player who does exist.
			return "", fmt.Errorf("announce: !announce: resolve @%s: %w", playerName, err)
		}
		if !ok {
			// Nobody by that name has ever been seen -- neither connected
			// now nor recorded before. Refuse rather than store: an
			// announcement targeted at an XUID nobody holds can never be
			// delivered or drained, so it would sit in the outbox forever
			// looking like a message in flight. Being offline is not this
			// case, which is why the lookup outlives the session.
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
		Body:         line.body,
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
	// A store that exists but cannot serve this statement yet: no tables
	// behind it, or no grant on them. Answered rather than returned as an
	// error so the operator is told which one to go and fix, instead of
	// reading the generic failure line every other broken command gets.
	if pgerr.NotMigrated(err) {
		return noStore, nil
	}
	if pgerr.NotGranted(err) {
		return noAccess, nil
	}
	if err != nil {
		return "", fmt.Errorf("announce: !announce: %w", err)
	}
	a.ID = id

	sent, err := pctx.Deliverer.SendNow(ctx, a, id)
	if err != nil {
		return "", fmt.Errorf("announce: !announce: send: %w", err)
	}

	// What the operator is told is what actually happened, not what was
	// attempted. Nobody may have heard this: the target can be offline,
	// their whisper can have failed, and a target that resolves to this
	// agent or a sibling bot is filtered out of every audience. "Told X"
	// in any of those cases is a report of a delivery that did not occur.
	switch {
	case !sent.Counted:
		// Broadcast while the roster could not say who is here, so it went
		// out and nobody can be named. Claiming a number would be a count
		// this agent did not have.
		return "Announced, but I can't see who is online right now.", nil
	case target == announce.TargetPlayer && sent.Players == 0:
		// The row is stored and unexpired, so this is a promise the queue
		// can keep: their next join or their own !inbox drains it.
		return "Queued for " + displayName + ".", nil
	case target == announce.TargetPlayer:
		return "Told " + displayName + ".", nil
	case target == announce.TargetOnlineOnly && sent.Players == 0:
		// online_only is the one target with no queue behind it, so nobody
		// hearing it now means nobody ever will.
		return "Nobody was online to hear that.", nil
	}
	return "Announced.", nil
}

func runInbox(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if inv.ActorXUID == chat.ServerOrigin {
		// The console is not a player: nothing was ever queued for it, and
		// every whisper the drain attempted would fail to resolve a
		// gamertag it does not have -- one logged failure per pending
		// message, ending in "you have nothing new".
		return "The console has no inbox.", nil
	}
	// The store, not just the deliverer: a deliverer over a store that
	// persists nothing reports zero drained, which is indistinguishable
	// from an empty queue and would have this command assert a fact about
	// the player's messages that it has no way to know.
	if !pctx.AnnouncementsReady() {
		return noStore, nil
	}
	// No argument names another player: a player can only ever drain their
	// own queue, the same restriction !wp places on whose coordinates a
	// command can touch.
	delivered, remaining, err := pctx.Deliverer.DrainAll(ctx, inv.ActorXUID, time.Now())
	// Same two states !announce answers for, same reasoning.
	if pgerr.NotMigrated(err) {
		return noStore, nil
	}
	if pgerr.NotGranted(err) {
		return noAccess, nil
	}
	if err != nil {
		return "", fmt.Errorf("announce: !inbox: %w", err)
	}
	if delivered == 0 {
		// delivered counts only messages whose whisper and delivery row
		// both succeeded, so zero is not the same fact as an empty queue:
		// three whispers that landed and three rows that failed to write
		// look identical to nothing having been owed. Saying "nothing new"
		// there denies messages the player has just watched arrive, and
		// they will arrive again, since nothing was recorded.
		if remaining > 0 {
			return "Something went wrong sending those - anything that did arrive may come again.", nil
		}
		return "You have nothing new.", nil
	}
	// Saying so is the whole reason the drain is capped: a player left
	// holding an unexplained partial delivery has no way to know there is
	// more, and !inbox is the same gesture they already made.
	if remaining > 0 {
		return fmt.Sprintf("Delivered %d, with %d still waiting - say !inbox again for the rest.", delivered, remaining), nil
	}
	if delivered == 1 {
		return "Delivered 1 message.", nil
	}
	return fmt.Sprintf("Delivered %d messages.", delivered), nil
}
