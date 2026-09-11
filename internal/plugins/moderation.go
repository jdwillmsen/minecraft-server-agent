package plugins

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

const (
	modlogDefault = 5
	modlogMax     = 10
	// modlogMessageBytes cuts each quoted message in a !modlog reply. Ten
	// entries share one whisper, and the operator needs enough to recognise
	// the line, not all of it.
	modlogMessageBytes = 60
	// moderationQueue is how many flagged messages may wait for the worker.
	// Flags are rare, so a full queue means the database or the bridge has
	// stalled, and dropping the newest is better than holding the event
	// dispatcher that feeds this plugin.
	moderationQueue = 64
)

// moderationWarning is whispered after a term flag. It does not repeat the
// term: the player knows what they typed, and a console command carrying
// the word would put it in the server log a second time.
const moderationWarning = "Please keep chat friendly - that word isn't welcome here."

const (
	noModlog       = "I have no moderation log configured."
	noModlogAccess = "My moderation log is refusing me access."
	modlogUsage    = "Usage: !modlog [player] [n] - the newest flags, n up to 10."
)

// Moderation checks public chat against the moderation rules, records what
// they flag, warns and reports, and serves !modlog.
//
// It reads chat off the bus rather than being called from the packet loop.
// It never runs on the read loop and cannot delay a command or an answer:
// the bus drops rather than blocks, and this handler only evaluates rules in
// memory before handing any database or bridge work to its own worker.
type Moderation struct {
	// rootCtx is the process lifetime. The worker outlives any one dispatch,
	// and HandleEvent's ctx is bounded by the dispatcher's per-call timeout.
	rootCtx context.Context
	terms   *moderation.Terms
	tracker *moderation.Tracker
	jobs    chan moderationJob
	log     *logging.Logger
	// now is replaceable so a test can place messages on a clock of its own.
	now func() time.Time
	// writable is whether the last write to the record succeeded, or, before
	// any write, whether the last probe of it did. Enabled cannot answer
	// that: it is true whenever a pool exists, including while the table is
	// missing or ungranted, which is exactly when a warning would go out with
	// no record behind it. Only the worker touches these; atomic so that
	// stays true if that ever changes.
	writable atomic.Bool
	// written is whether any write has been attempted. From then on only a
	// write can say whether the next one will be kept.
	written atomic.Bool
}

type moderationJob struct {
	pctx     *plugin.Context
	xuid     string
	gamertag string
	message  string
	flags    []moderation.Flag
	at       time.Time
}

// NewModeration builds the moderation plugin and starts its worker, which
// stops when rootCtx ends. The only error is a term list RE2 cannot
// compile, which should stop startup rather than leave the rule off
// silently.
func NewModeration(rootCtx context.Context, terms []string, log *logging.Logger) (*Moderation, error) {
	compiled, err := moderation.NewTerms(terms)
	if err != nil {
		return nil, err
	}
	m := &Moderation{
		rootCtx: rootCtx,
		terms:   compiled,
		tracker: moderation.NewTracker(),
		jobs:    make(chan moderationJob, moderationQueue),
		log:     log,
		now:     time.Now,
	}
	go m.work()
	return m, nil
}

func (*Moderation) Name() string { return "moderation" }

// Kinds satisfies plugin.EventHandler.
func (*Moderation) Kinds() []string { return []string{chat.MessageKind} }

func (*Moderation) Commands() []plugin.Command {
	return []plugin.Command{{
		Name:        "modlog",
		Description: "Operator: !modlog [player] [n] - the newest moderation flags.",
		// Checked again in runModlog, for the reason runAnnounce gives.
		Permission: plugin.PermissionOperator,
		Run:        runModlog,
		// The reply quotes other players' flagged messages, which are kept
		// for 90 days in the table and must not be kept longer in the log.
		RedactReply: true,
	}}
}

var _ plugin.Plugin = (*Moderation)(nil)
var _ plugin.EventHandler = (*Moderation)(nil)

// HandleEvent evaluates one chat message and queues whatever it flagged.
//
// Operators are evaluated like everyone else: a record that exempts the
// people who read it is not a record anyone can rely on.
//
// The console is not: it has no XUID to record or warn, and what it says in
// public is this agent's own voice -- a broadcast LLM answer arrives back as
// console chat, and must not be flagged as shouting.
//
// With no store enabled the rules do not run. The feature is an audit, and
// a warning with no record behind it is enforcement nobody can review.
func (m *Moderation) HandleEvent(_ context.Context, pctx *plugin.Context, ev bus.Event) error {
	msg, ok := ev.(chat.MessageEvent)
	if !ok {
		return fmt.Errorf("moderation: unexpected event type %T for kind %s", ev, ev.Kind())
	}
	if !msg.Public || msg.ActorXUID == "" || msg.ActorXUID == chat.ServerOrigin {
		return nil
	}
	if pctx == nil || pctx.Moderation == nil || !pctx.Moderation.Enabled() {
		return nil
	}

	at := m.now()
	flags := m.evaluate(msg.ActorXUID, msg.Message, at)
	if len(flags) == 0 {
		return nil
	}
	gamertag := msg.Gamertag
	if gamertag == "" {
		gamertag = msg.ActorXUID
	}
	select {
	case m.jobs <- moderationJob{pctx: pctx, xuid: msg.ActorXUID, gamertag: gamertag, message: msg.Message, flags: flags, at: at}:
	default:
		m.log.Error("moderation_dropped", logging.Fields{"xuid": msg.ActorXUID, "queued": cap(m.jobs)})
	}
	return nil
}

// evaluate runs every rule against one message.
//
// A command or an @server question is moderated like any other line. It
// was typed into public chat and every player read it, so a slur after
// "!announce" or a shout at "@server" reaches the same audience as one
// that isn't. The prefix changes who answers, not who saw it. Flood
// counts every message for the same reason: a burst of commands fills the
// chat as surely as a burst of anything else.
func (m *Moderation) evaluate(xuid, message string, at time.Time) []moderation.Flag {
	var flags []moderation.Flag
	if term, ok := m.terms.Match(message); ok {
		flags = append(flags, moderation.Flag{Rule: moderation.RuleTerm, Detail: term})
	}
	if detail, ok := m.tracker.Flood(xuid, at); ok {
		flags = append(flags, moderation.Flag{Rule: moderation.RuleFlood, Detail: detail})
	}
	if detail, ok := moderation.Caps(message); ok {
		flags = append(flags, moderation.Flag{Rule: moderation.RuleCaps, Detail: detail})
	}
	return flags
}

func (m *Moderation) work() {
	for {
		select {
		case <-m.rootCtx.Done():
			return
		case j := <-m.jobs:
			m.act(j)
		}
	}
}

// act warns, records and reports one flagged message, in that order.
//
// The warning comes first so that the record can say what actually
// happened. Recorded first as warned, a whisper that then failed would
// leave a row claiming the player was told something they never saw.
//
// Every call is bounded by the dispatch timeout, like a command's reply: a
// hung bridge or database holds this worker for seconds, never for good.
func (m *Moderation) act(j moderationJob) {
	warned := false
	if hasRule(j.flags, moderation.RuleTerm) && j.pctx.Voice != nil && m.recordable(j.pctx) && m.tracker.ClaimWarning(j.xuid, j.at) {
		ctx, cancel := context.WithTimeout(m.rootCtx, plugin.DefaultDispatchTimeout)
		err := j.pctx.Voice.Tell(ctx, j.xuid, moderationWarning)
		cancel()
		if err != nil {
			m.log.Error("moderation_warn_failed", logging.Fields{"xuid": j.xuid, "error": err.Error()})
		} else {
			warned = true
		}
	}

	recorded := 0
	for _, f := range j.flags {
		action := moderation.ActionLogged
		if f.Rule == moderation.RuleTerm && warned {
			action = moderation.ActionWarned
		}
		ctx, cancel := context.WithTimeout(m.rootCtx, plugin.DefaultDispatchTimeout)
		err := j.pctx.Moderation.Record(ctx, moderation.Event{
			XUID: j.xuid, Gamertag: j.gamertag, Message: j.message,
			Rule: f.Rule, Detail: f.Detail, Action: action, OccurredAt: j.at,
		})
		cancel()
		m.writable.Store(err == nil)
		m.written.Store(true)
		if err != nil {
			// A missing table or grant is said once by the store the binary
			// wires in; per flag it would bury the log for a whole release.
			if !pgerr.Unready(err) {
				m.log.Error("moderation_record_failed", logging.Fields{"xuid": j.xuid, "rule": string(f.Rule), "error": err.Error()})
			}
			continue
		}
		metrics.ModerationFlag(f.Rule, action)
		recorded++
	}

	// Operators are only told about something they can look up: the notice
	// points at !modlog, and a flag that was never written would send them
	// to an empty log.
	if recorded == 0 || !m.tracker.ClaimNotice(j.xuid, j.at) {
		return
	}
	m.notifyOperators(j)
}

// notifyOperators queues one announcement for the operator permission, so
// operators online now are whispered and one who is offline hears it at
// their next join.
//
// The notice names the player and the rules, never the message or the
// matched term. Announcements are kept indefinitely for their own audit,
// and a copy of what was said there would outlive the 90 days the flag
// itself is kept.
//
// The command it suggests carries an explicit count, so a gamertag ending
// in a number is not read as one. A gamertag that is only the XUID the
// roster fell back to is left out: digits alone would be read as the count.
func (m *Moderation) notifyOperators(j moderationJob) {
	if !j.pctx.AnnouncementsReady() {
		return
	}
	ctx, cancel := context.WithTimeout(m.rootCtx, plugin.DefaultDispatchTimeout)
	defer cancel()

	rules := make([]string, len(j.flags))
	for i, f := range j.flags {
		rules[i] = string(f.Rule)
	}
	body := fmt.Sprintf("Moderation: %s was flagged (%s). Say !modlog %s %d for details.", j.gamertag, strings.Join(rules, ", "), j.gamertag, modlogDefault)
	if j.gamertag == j.xuid {
		body = fmt.Sprintf("Moderation: a player was flagged (%s). Say !modlog %d for details.", strings.Join(rules, ", "), modlogDefault)
	}
	a := announce.Announcement{
		Body:         body,
		Source:       announce.SourceEvent,
		TargetKind:   announce.TargetPermission,
		TargetValue:  plugin.PermissionOperator.String(),
		Priority:     announce.PriorityNormal,
		Delivery:     announce.DeliveryFor(announce.TargetPermission),
		CreatedAt:    j.at,
		DeliverAfter: j.at,
		ExpiresAt:    announce.DefaultExpiry(announce.SourceEvent, announce.TargetPermission, j.at),
	}
	id, err := j.pctx.Announcements.Insert(ctx, a)
	if err != nil {
		if !pgerr.Unready(err) {
			m.log.Error("moderation_notice_failed", logging.Fields{"xuid": j.xuid, "error": err.Error()})
		}
		return
	}
	a.ID = id
	if _, err := j.pctx.Deliverer.SendNow(ctx, a, id); err != nil {
		// Stored, so it still reaches operators at their next join.
		m.log.Error("moderation_notice_send_failed", logging.Fields{"xuid": j.xuid, "announcement_id": id, "error": err.Error()})
	}
}

// recordable reports whether a flag written now would be kept, so that a
// warning is only whispered when its record can follow it.
//
// Before any write it asks with a zero-row read: the same table and the
// same deploy-ordering failures a write would hit, without writing
// anything. Once a write has been attempted its result is the only answer.
// A read can succeed where a write cannot -- a read-only failover, an
// INSERT grant revoked with SELECT left, a CHECK the row breaks -- so after
// a failed write only the next good one reopens the gate. Every flag is
// still written, as logged when no warning went out, so that write comes
// with the next flag. A table that disappears between two flags still
// costs one unrecorded warning, because nothing short of the write can
// know that in advance.
func (m *Moderation) recordable(pctx *plugin.Context) bool {
	if m.writable.Load() {
		return true
	}
	if m.written.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(m.rootCtx, plugin.DefaultDispatchTimeout)
	defer cancel()
	_, err := pctx.Moderation.Recent(ctx, "", 0)
	m.writable.Store(err == nil)
	return err == nil
}

func hasRule(flags []moderation.Flag, rule moderation.Rule) bool {
	for _, f := range flags {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

// parseModlogArgs reads "[player] [n]". A trailing integer is the count and
// everything before it is the name, so a gamertag with a space in it needs
// no quoting. A gamertag can end in a number, so runModlog tries the whole
// line as a name before taking this split. A lone number is always the
// count, and a whole-line lookup that fails falls back to the split rather
// than failing the command: the split may well resolve from the live
// roster alone.
func parseModlogArgs(args []string) (player string, n int, ok bool) {
	n = modlogDefault
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[len(args)-1]); err == nil {
			if v < 1 {
				return "", 0, false
			}
			n = min(v, modlogMax)
			args = args[:len(args)-1]
		}
	}
	return strings.TrimPrefix(strings.Join(args, " "), "@"), n, true
}

func runModlog(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if inv.ActorPermission < plugin.PermissionOperator {
		return "Only an operator can read the moderation log.", nil
	}
	if inv.ActorXUID == chat.ServerOrigin {
		// A console reply is broadcast, since it has nobody to whisper to.
		// Here that would read every flagged message out to the server.
		return "The moderation log is whispered, and the console has nobody to whisper to.", nil
	}
	if pctx == nil || pctx.Moderation == nil || !pctx.Moderation.Enabled() {
		return noModlog, nil
	}
	player, n, ok := parseModlogArgs(inv.Args)
	var xuid string
	if whole := strings.TrimPrefix(strings.Join(inv.Args, " "), "@"); len(inv.Args) > 1 && whole != player && pctx.Roster != nil {
		if found, known, err := pctx.Roster.XUIDFor(ctx, whole); err == nil && known {
			player, n, ok, xuid = whole, modlogDefault, true, found
		}
	}
	if !ok {
		return modlogUsage, nil
	}

	if player != "" && xuid == "" {
		if pctx.Roster == nil {
			return "I don't know a player named " + player + ".", nil
		}
		found, known, err := pctx.Roster.XUIDFor(ctx, player)
		if err != nil {
			return "", fmt.Errorf("moderation: !modlog: resolve %s: %w", player, err)
		}
		if !known {
			return "I don't know a player named " + player + ".", nil
		}
		xuid = found
	}

	events, err := pctx.Moderation.Recent(ctx, xuid, n)
	if pgerr.NotMigrated(err) {
		return noModlog, nil
	}
	if pgerr.NotGranted(err) {
		return noModlogAccess, nil
	}
	if err != nil {
		return "", fmt.Errorf("moderation: !modlog: %w", err)
	}
	if len(events) == 0 {
		if player != "" {
			return "Nothing recorded for " + player + ".", nil
		}
		return "Nothing recorded.", nil
	}

	entries := make([]string, len(events))
	for i, e := range events {
		entries[i] = fmt.Sprintf("%s %s %s (%s): \"%s\"",
			e.OccurredAt.UTC().Format("01-02 15:04 UTC"), e.Gamertag, e.Rule, e.Detail, modlogSnippet(e.Message))
	}
	return strings.Join(entries, " | "), nil
}

// modlogSnippet flattens a message onto one line and cuts it. Chat can
// carry control characters a whisper should not, and one line per entry is
// what keeps ten of them readable.
func modlogSnippet(message string) string {
	flat := strings.Join(strings.Fields(message), " ")
	if cut := text.Truncate(flat, modlogMessageBytes); cut != flat {
		return cut + "..."
	}
	return flat
}
