package plugins

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
)

const scheduleUsage = "Usage: !schedule add daily HH:MM [!now] [!urgent] <message>, !schedule add every <N>m|<N>h [!now] [!urgent] <message>, !schedule list, or !schedule del <id>. Times are UTC."

// Kept apart from the announce plugin's replies for the reason those are
// kept apart from each other: the schedules table arrives in its own
// migration, and "the announcement store is off" would send an operator to
// fix the wrong one.
const (
	noScheduleStore  = "I have no schedule store configured."
	noScheduleAccess = "My schedule store is refusing me access."
)

// maxListed caps how many schedules one !schedule list names. The reply is
// one whisper, and a line that scrolls off the chat is a line nobody reads.
const maxListed = 10

// listBodyBytes is how much of each body the list shows: enough to tell
// schedules apart, which is all a list is for.
const listBodyBytes = 40

// Schedule lets an operator set announcements that repeat.
type Schedule struct{}

// NewSchedule builds the schedule plugin.
func NewSchedule() *Schedule { return &Schedule{} }

func (*Schedule) Name() string { return "schedule" }

func (*Schedule) Commands() []plugin.Command {
	return []plugin.Command{{
		Name:        "schedule",
		Description: "Operator: !schedule add daily HH:MM|every <N>m|<N>h <message>, list, del <id>.",
		// Checked again in runSchedule, as !announce does, so a direct call
		// refuses exactly as a dispatched one would.
		Permission: plugin.PermissionOperator,
		Run:        runSchedule,
	}}
}

var _ plugin.Plugin = (*Schedule)(nil)

func runSchedule(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx == nil || pctx.Schedules == nil || !pctx.Schedules.Enabled() {
		return noScheduleStore, nil
	}
	if inv.ActorPermission < plugin.PermissionOperator {
		return "Only an operator can manage schedules.", nil
	}
	if len(inv.Args) == 0 {
		return scheduleUsage, nil
	}
	switch strings.ToLower(inv.Args[0]) {
	case "add":
		return scheduleAdd(ctx, pctx.Schedules, inv, inv.Args[1:], time.Now())
	case "list":
		return scheduleList(ctx, pctx.Schedules, time.Now())
	case "del":
		return scheduleDel(ctx, pctx.Schedules, inv.Args[1:])
	}
	return scheduleUsage, nil
}

// everyPattern is the whole of what "every" accepts: a count and a unit,
// minutes or hours, nothing else. Seconds and days are left out on purpose
// -- one is below the floor by construction and the other is what daily is
// for.
var everyPattern = regexp.MustCompile(`^([0-9]+)([mh])$`)

// parseCadence reads "daily HH:MM" or "every <N>m|<N>h" off the front of
// args and returns what follows it. refusal is the reason to give when the
// cadence cannot be stored.
func parseCadence(args []string) (c announce.Cadence, rest []string, refusal string) {
	if len(args) < 2 {
		return c, nil, scheduleUsage
	}
	kind, value, rest := strings.ToLower(args[0]), strings.ToLower(args[1]), args[2:]
	switch kind {
	case "daily":
		t, err := time.Parse("15:04", value)
		if err != nil {
			return c, nil, "A daily time is HH:MM in UTC, like 18:00."
		}
		c = announce.Cadence{Daily: true, At: time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute}
	case "every":
		m := everyPattern.FindStringSubmatch(value)
		if m == nil {
			return c, nil, "An interval is a number of minutes or hours, like 30m or 2h."
		}
		unit := time.Minute
		if m[2] == "h" {
			unit = time.Hour
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n > int(announce.MaxEvery/unit) {
			// Refused before multiplying, so a number too large for the
			// arithmetic is told the ceiling rather than wrapping past it.
			n = int(announce.MaxEvery/unit) + 1
		}
		c = announce.Cadence{Every: time.Duration(n) * unit}
	default:
		return c, nil, scheduleUsage
	}
	if err := c.Validate(); err != nil {
		return c, nil, sentence(err.Error())
	}
	return c, rest, ""
}

// sentence turns a lower-case error message into a reply.
func sentence(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r) + "."
}

func scheduleAdd(ctx context.Context, schedules plugin.ScheduleStore, inv plugin.Invocation, args []string, now time.Time) (string, error) {
	cadence, rest, refusal := parseCadence(args)
	if refusal != "" {
		return refusal, nil
	}
	line, refusal := parseAnnounceLine(rest, scheduleUsage)
	if refusal != "" {
		return refusal, nil
	}
	if line.player != "" {
		return "A schedule can't be aimed at one player: a recurring whisper is a nag. Use !announce @player for a one-off.", nil
	}

	target := announce.TargetEveryone
	if line.now {
		target = announce.TargetOnlineOnly
	}
	priority := announce.PriorityNormal
	if line.urgent {
		priority = announce.PriorityExpedited
	}
	// Blanked for the same reason !announce blanks it: the console is no
	// player, and author_xuid is a foreign key into minecraft.players.
	author := inv.ActorXUID
	if author == chat.ServerOrigin {
		author = ""
	}
	next := cadence.Next(time.Time{}, now)

	id, err := schedules.AddSchedule(ctx, announce.Schedule{
		Body:        line.body,
		AuthorXUID:  author,
		TargetKind:  target,
		Priority:    priority,
		Cadence:     cadence,
		NextFireAt:  next,
		Active:      true,
		TargetValue: "",
	})
	if reply, ok := scheduleStoreState(err); ok {
		return reply, nil
	}
	if err != nil {
		return "", fmt.Errorf("schedule: !schedule add: %w", err)
	}
	return fmt.Sprintf("Schedule #%d set: %s, first at %s.", id, cadence, formatUTC(next)), nil
}

func scheduleList(ctx context.Context, schedules plugin.ScheduleStore, now time.Time) (string, error) {
	all, err := schedules.ListSchedules(ctx)
	if reply, ok := scheduleStoreState(err); ok {
		return reply, nil
	}
	if err != nil {
		return "", fmt.Errorf("schedule: !schedule list: %w", err)
	}
	if len(all) == 0 {
		return "No schedules are set.", nil
	}
	shown := all
	if len(shown) > maxListed {
		shown = shown[:maxListed]
	}
	parts := make([]string, 0, len(shown)+1)
	for _, s := range shown {
		parts = append(parts, fmt.Sprintf("#%d %s to %s: %s (next %s)",
			s.ID, s.Cadence, describeTarget(s), listBody(s.Body), formatUTC(s.NextFireAt)))
	}
	if extra := len(all) - len(shown); extra > 0 {
		parts = append(parts, fmt.Sprintf("and %d more", extra))
	}
	return strings.Join(parts, " | "), nil
}

func describeTarget(s announce.Schedule) string {
	var d string
	switch s.TargetKind {
	case announce.TargetOnlineOnly:
		d = "whoever is online"
	case announce.TargetPermission:
		d = s.TargetValue + "s"
	default:
		d = "everyone"
	}
	if s.Priority == announce.PriorityExpedited {
		d += ", urgent"
	}
	return d
}

func listBody(body string) string {
	cut := text.Truncate(body, listBodyBytes)
	if cut != body {
		return cut + "..."
	}
	return body
}

// formatUTC names the zone every time, so a reader in any timezone knows
// what the number means without asking.
func formatUTC(t time.Time) string {
	return t.UTC().Format("Jan 2 15:04") + " UTC"
}

func scheduleDel(ctx context.Context, schedules plugin.ScheduleStore, args []string) (string, error) {
	if len(args) != 1 {
		return "Say which schedule by its number, like !schedule del 3.", nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil || id <= 0 {
		return "Say which schedule by its number, like !schedule del 3.", nil
	}
	ok, err := schedules.DeactivateSchedule(ctx, id)
	if reply, handled := scheduleStoreState(err); handled {
		return reply, nil
	}
	if err != nil {
		return "", fmt.Errorf("schedule: !schedule del: %w", err)
	}
	if !ok {
		return fmt.Sprintf("There is no active schedule #%d.", id), nil
	}
	// Stopped, not deleted: what it already sent keeps pointing at it.
	return fmt.Sprintf("Schedule #%d stopped.", id), nil
}

// scheduleStoreState answers the two deploy states a configured database
// can be in, the same two !announce answers for.
func scheduleStoreState(err error) (string, bool) {
	switch {
	case pgerr.NotMigrated(err):
		return noScheduleStore, true
	case pgerr.NotGranted(err):
		return noScheduleAccess, true
	}
	return "", false
}
