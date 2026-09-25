// Package plugin defines the extension surface every feature of the agent
// is built on: a Plugin registers ! commands (and, via optional interfaces,
// reacts to server events or runs on a schedule); a Registry dispatches
// incoming commands to the right plugin after a permission check.
//
// Plugins never touch the Bedrock connection or the console bridge
// directly - they only ever see the adapters handed to them through
// Context, so a plugin bug can say the wrong thing but cannot reach past
// its sanctioned capabilities.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

// DefaultDispatchTimeout bounds how long a single command's Run may take
// before Dispatch gives up and returns ErrCommandTimedOut. Enforced so a
// plugin blocked on a slow downstream call (the bridge in Stage 2, the LLM
// in Stage 4) can never permanently stall the caller - which today is the
// Bedrock packet read loop.
//
// A var, not a const, so tests can shorten it rather than waiting out the
// real production value.
var DefaultDispatchTimeout = 5 * time.Second

// Permission is the minimum privilege level a command requires, resolved
// from the server's permissions.json via mc-console-bridge's
// GET /permissions (see internal/adapters.PermissionResolver).
type Permission int

const (
	PermissionVisitor Permission = iota
	PermissionMember
	PermissionOperator
)

func (p Permission) String() string {
	switch p {
	case PermissionVisitor:
		return "visitor"
	case PermissionMember:
		return "member"
	case PermissionOperator:
		return "operator"
	default:
		return "unknown"
	}
}

// Voice is how a plugin speaks - always through the console bridge, never
// as a player. Implemented against mc-console-bridge by
// internal/adapters.BridgeVoice; a no-op implementation remains for tests
// that exercise dispatch without a bridge.
type Voice interface {
	// Tell whispers message to the player identified by xuid.
	Tell(ctx context.Context, xuid, message string) error
	// Say broadcasts message to everyone.
	Say(ctx context.Context, message string) error
}

// Facts is how a plugin reads live server state. Grows further in later
// stages as more capabilities (server status, mc-monitor metrics, ...) are
// wired in; Stage 2 earns exactly the one capability a real command needs.
type Facts interface {
	// PlayersOnline returns mc-console-bridge's raw `list` command output,
	// unparsed. Bedrock's real console text format for `list` was not
	// available to verify against in the environment this was built in, so
	// relaying the server's own exact text is safer than a fragile,
	// unverified parse into a name slice — see internal/adapters/facts.go.
	PlayersOnline(ctx context.Context) (string, error)
}

// ServerInfo is how a plugin reads state the game console cannot answer for.
//
// Separate from Facts rather than folded into it because the two are backed
// by different transports and either can be absent: Facts speaks to the
// console through mc-console-bridge, ServerInfo scrapes the mc-monitor and
// backup exporters. Merging them would force the console-backed
// implementation to carry three methods it has no way to answer.
type ServerInfo interface {
	// ServerStatus reports player counts, health and responsiveness in one
	// line -- the "is the server ok" question, as distinct from PlayersOnline's
	// list of who is here.
	ServerStatus(ctx context.Context) (string, error)
	// Version reports the Bedrock build the server is running.
	Version(ctx context.Context) (string, error)
	// BackupStatus reports how recently the world was saved, how large that
	// archive was, and whether it was taken with the world held.
	BackupStatus(ctx context.Context) (string, error)
	// StatusEnabled and BackupEnabled report whether the exporter behind
	// each answer is configured. Split because they are two exporters: an
	// agent can have monitoring without backup metrics or the reverse.
	//
	// A command asked for an unconfigured capability says so, which is the
	// right answer to a player. A caller that chooses what to offer -- the
	// LLM toolset -- needs to know before it asks, because offering a tool
	// whose only possible reply is "not configured" spends a tool round and
	// prompt budget to learn nothing.
	StatusEnabled() bool
	BackupEnabled() bool
}

// Pinger measures the server itself, for !ping. A reply that only proves the
// agent's own process is running answers the wrong question: whoever asks
// wants to know whether the server is keeping up.
type Pinger interface {
	Ping(ctx context.Context) ServerPing
}

// ServerPing is one !ping measurement. Each half can be missing on its own:
// the console and the Bedrock connection are separate paths to the server,
// and whichever one still answers is the useful half of the reply.
type ServerPing struct {
	// TPS is ticks per second read off the server's own game clock over the
	// last minute or so; 20 is full speed. Meaningful only when TPSKnown.
	TPS      float64
	TPSKnown bool
	// TPSErr is set when the console could not be asked at all, as distinct
	// from there being no older reading to measure against yet.
	TPSErr error
	// Link is the round trip over the agent's own Bedrock connection.
	// Meaningful only when LinkKnown: between sessions there is no
	// connection to measure.
	Link      time.Duration
	LinkKnown bool
}

// Invocation is one player's attempt to run a command.
type Invocation struct {
	// ActorXUID identifies who issued the command (never a gamertag - see
	// internal/chat for why).
	ActorXUID string
	// ActorPermission is the actor's resolved permission level.
	ActorPermission Permission
	// Args is the whitespace-split argument list, command name excluded.
	Args []string
}

// Command is one ! command a plugin exposes.
type Command struct {
	// Name is matched case-insensitively against the command a player
	// typed, without the "!" prefix.
	Name string
	// Description is shown by the core plugin's !help.
	Description string
	// Permission is the minimum level required to run this command.
	Permission Permission
	// Run executes the command. ctx carries the process lifetime; pctx
	// carries the adapters this command is allowed to use.
	Run func(ctx context.Context, pctx *Context, inv Invocation) (reply string, err error)
	// RedactReply keeps the reply text out of the log, which then records
	// only its length. Replies are logged to stdout and from there to log
	// storage whose retention nothing in this repo controls. A reply that is
	// whispered to keep it private -- a player's coordinates, other players'
	// moderation records -- would otherwise sit there in full, outside every
	// limit this agent promises about that data.
	RedactReply bool
}

// Plugin is one self-contained unit of agent behaviour.
type Plugin interface {
	// Name identifies the plugin in logs and diagnostics.
	Name() string
	// Commands returns the ! commands this plugin contributes. May be
	// empty for a plugin that only reacts to events.
	Commands() []Command
}

// EventHandler is an optional Plugin capability: react to bus events
// (join/leave/death/server-log events, ...). A Plugin that doesn't need
// this simply doesn't implement it.
type EventHandler interface {
	// HandleEvent is called for every event the plugin's Kinds() selects,
	// with the actual event so the handler can read its payload (e.g. a
	// join event's player XUID) - type-assert ev to the concrete type its
	// Kind() implies. It must not block for long - slow work should be
	// started in a goroutine.
	HandleEvent(ctx context.Context, pctx *Context, ev bus.Event) error
	// Kinds lists the event kinds this plugin wants delivered.
	Kinds() []string
}

// Directory lets a plugin (typically "core", for !help) list every
// registered command. Registry implements this.
type Directory interface {
	Commands() []Command
}

// Context is everything a running command or event handler is allowed to
// touch.
type Context struct {
	Voice     Voice
	Facts     Facts
	Directory Directory
	// ServerInfo may be nil when the exporters are not configured. Commands
	// that need it must say so rather than panic -- see the stats plugin.
	ServerInfo ServerInfo
	// Profiles records presence and reports what is known about a player.
	//
	// May be nil. cmd/agent always supplies one -- store.Nop when no database
	// is configured -- but a plugin must not assume that: an earlier draft
	// documented it as never nil, and the first caller that believed the
	// comment panicked the whole event dispatcher on a nil interface. A
	// greeting is not worth taking the agent down for.
	Profiles PlayerStore
	// Knowledge and Waypoints may be nil. cmd/agent always supplies both --
	// the disabled implementation when no database is configured -- and
	// both are guarded at every use for the same reason Profiles is.
	Knowledge KnowledgeStore
	Waypoints WaypointStore
	// Wiki may be nil, on the same terms as Knowledge and Waypoints: cmd/agent
	// always supplies one -- wiki.Nop when WIKI_ENABLED is off -- and every
	// use asks Enabled first, so no wiki tool exists while it is disabled.
	Wiki Wiki
	// Announcements and Deliverer may be nil, on the same terms: cmd/agent
	// always supplies both, and every use asks AnnouncementsReady rather
	// than assuming it.
	Announcements AnnounceStore
	Deliverer     AnnounceDeliverer
	// Schedules may be nil on the same terms: cmd/agent always supplies
	// one, and !schedule asks Enabled before every use.
	Schedules ScheduleStore
	// Roster resolves a "@player" reference in a command (!announce) to the
	// XUID every other capability keys on. May be nil -- a command that
	// needs it must refuse plainly rather than assume it can resolve one.
	Roster Roster
	// Presence answers whether a player is on the server right now. May be
	// nil; ask through KnownOffline, which says what an absent one means.
	Presence Presence
	// Pinger may be nil; !ping then answers from the agent alone.
	Pinger Pinger
	// Moderation may be nil, on the same terms as Knowledge: cmd/agent
	// always supplies one, and every use checks Enabled first.
	Moderation ModerationStore
}

// ModerationStore is the flagged-chat record a plugin may touch: write a flag
// and read the newest back. Pruning is left out on purpose. It is
// housekeeping the process runs on a timer, and no command or event handler
// has a reason to delete a record.
type ModerationStore interface {
	Record(ctx context.Context, e moderation.Event) error
	Recent(ctx context.Context, xuid string, limit int) ([]moderation.Event, error)
	Enabled() bool
}

// AnnouncementsReady reports whether an announcement can actually be stored
// and sent: a store that persists, and a deliverer to send through.
//
// One predicate rather than a check per call site. The two were being asked
// four different ways across this package and the plugins built on it --
// nil-or-Enabled here, a bare nil there -- and the version that only tested
// the deliverer let !inbox answer "you have nothing new" with no database
// behind it at all: a claim about a player's queue made by something that
// had never been able to read one.
func (c *Context) AnnouncementsReady() bool {
	return c != nil && c.Deliverer != nil && c.Announcements != nil && c.Announcements.Enabled()
}

// PlayerStore is the subset of internal/store a plugin may touch.
//
// Narrowed to the read-and-record path a greeting needs: plugins observe
// players, they do not close orphaned sessions or manage a connection pool.
type PlayerStore interface {
	RecordJoin(ctx context.Context, xuid, gamertag string, at time.Time) (store.Profile, error)
	Enabled() bool
}

// KnowledgeStore is the curated-fact surface a plugin may touch. Identical
// to knowledge.Store today; declared here so the plugin package states its
// own dependency rather than inheriting whatever that package grows.
type KnowledgeStore interface {
	Lookup(ctx context.Context, query string, limit int) ([]knowledge.Entry, error)
	Upsert(ctx context.Context, topic, body, authorXUID string) error
	Delete(ctx context.Context, topic string) (removed bool, err error)
	List(ctx context.Context) ([]knowledge.Entry, error)
	Enabled() bool
}

// Wiki answers how the game itself works. Lookup returns text ready for the
// model, or one of wiki.ErrNotFound, wiki.ErrUnavailable, wiki.ErrLimited.
// Enabled reports whether a wiki is configured; when false, no wiki tool
// exists.
type Wiki interface {
	Lookup(ctx context.Context, topic, aspect string) (string, error)
	Enabled() bool
}

// WaypointStore is the per-player coordinate surface a plugin may touch.
// Every method takes the owning XUID: there is no "all waypoints" read,
// because no command and no tool has a reason for one.
type WaypointStore interface {
	Get(ctx context.Context, xuid, name string) (waypoints.Waypoint, bool, error)
	Set(ctx context.Context, xuid string, wp waypoints.Waypoint) error
	Delete(ctx context.Context, xuid, name string) (removed bool, err error)
	List(ctx context.Context, xuid string) ([]waypoints.Waypoint, error)
	Enabled() bool
}

// AnnounceStore is the outbox surface a plugin may touch: enough to create
// an announcement, and nothing else. Reading what is pending and marking it
// delivered both belong to the deliverer, which is the only thing that
// actually sends a message, so it is the only thing allowed to decide what
// is owed or record that one arrived.
type AnnounceStore interface {
	Insert(ctx context.Context, a announce.Announcement) (int64, error)
	Enabled() bool
}

// AnnounceDeliverer is how a command sends an announcement immediately and
// drains a player's own queue on request. Narrowed to what !announce and
// !inbox need -- not announce.Deliverer's full method set, since join-time
// draining is the announce-drain event handler's job, not a chat command's.
type AnnounceDeliverer interface {
	SendNow(ctx context.Context, a announce.Announcement, id int64) (announce.Reach, error)
	DrainAll(ctx context.Context, xuid string, now time.Time) (delivered, remaining int, err error)
}

// ScheduleStore is what a command may do with recurring announcements:
// create, list and stop them. Firing one belongs to the schedule loop, the
// only thing that claims an occurrence, so no command can make a reminder
// fire twice.
type ScheduleStore interface {
	AddSchedule(ctx context.Context, s announce.Schedule) (int64, error)
	ListSchedules(ctx context.Context) ([]announce.Schedule, error)
	DeactivateSchedule(ctx context.Context, id int64) (ok bool, err error)
	Enabled() bool
}

// Roster resolves a player's XUID from a gamertag typed into a command,
// turning an "@player" reference into the identity every other capability
// keys on.
//
// It carries a context and an error because the answer is not always in
// memory: a player who is offline is exactly who a queued announcement is
// for, and only a durable record can name them. ok false means nobody by
// that name has ever been seen -- the only case in which a command should
// refuse. An error means the lookup itself could not be made, which is not
// the same as an answer of no.
type Roster interface {
	XUIDFor(ctx context.Context, name string) (xuid string, ok bool, err error)
}

// Presence answers whether a player is on the server at this moment.
//
// Deliberately not part of Roster, which resolves a name to an XUID and is
// meant to answer for players who are offline -- a queued announcement is
// written for exactly those. This is the opposite question, and conflating
// the two is how a whisper to someone who has left comes to look like one
// they saw: a gamertag outlives the session that taught it, because a reply
// already in flight still has to be addressable, so resolving one is no
// evidence at all that the player is still there.
type Presence interface {
	// IsOnline reports whether xuid is on the roster the live connection is
	// watching. Meaningful only while Knows is true.
	IsOnline(xuid string) bool
	// Knows reports whether the roster can answer who is on the server at
	// all: false between connections, and false again after one opens until
	// its first roster packet arrives. IsOnline says "no" about everyone in
	// both, which is absence of knowledge rather than knowledge of absence.
	Knows() bool
}

// KnownOffline reports whether this process can say for certain that xuid
// has left. A Context with no Presence wired keeps doing what it did before
// rather than silently withholding everything it would otherwise send;
// cmd/agent always supplies one.
//
// The predicate itself lives in the roster package, because a deliverer
// deciding whether to whisper asks exactly the same question of its own
// audience filter. Two spellings of it went one step apart -- a player was
// owed their inbox by one and written off as gone by the other -- and the
// answer had to be the same in both.
func (c *Context) KnownOffline(xuid string) bool {
	return c != nil && roster.KnownOffline(c.Presence, xuid)
}

// Registry holds every registered plugin and routes commands to them.
type Registry struct {
	mu       sync.RWMutex
	plugins  []Plugin
	commands map[string]Command
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{commands: make(map[string]Command)}
}

// commandKey normalises a command name into the form the registry keys on,
// so the case-insensitive matching Command.Name documents is enforced here
// rather than assumed of every caller.
func commandKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Register adds a plugin's commands to the registry. It rejects a plugin
// whose commands are unusable (empty name, nil Run) or whose names collide
// with each other or with one already registered, so two plugins can never
// silently shadow each other and no unrunnable command can ever be
// dispatched. Registration is all-or-nothing.
func (r *Registry) Register(p Plugin) error {
	cmds := p.Commands()
	keys := make([]string, len(cmds))
	for i, cmd := range cmds {
		key := commandKey(cmd.Name)
		if key == "" {
			return fmt.Errorf("plugin %s: command %d has an empty name", p.Name(), i)
		}
		if cmd.Run == nil {
			return fmt.Errorf("plugin %s: command %q has a nil Run function", p.Name(), cmd.Name)
		}
		for j := 0; j < i; j++ {
			if keys[j] == key {
				return fmt.Errorf("plugin %s: command %q is declared twice", p.Name(), cmd.Name)
			}
		}
		keys[i] = key
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for i, cmd := range cmds {
		if _, exists := r.commands[keys[i]]; exists {
			return fmt.Errorf("plugin %s: command %q is already registered", p.Name(), cmd.Name)
		}
	}
	for i, cmd := range cmds {
		r.commands[keys[i]] = cmd
	}
	r.plugins = append(r.plugins, p)
	return nil
}

// ErrUnknownCommand is returned by Dispatch when no plugin registered the
// requested command.
var ErrUnknownCommand = errors.New("unknown command")

// ErrPermissionDenied is returned by Dispatch when the actor's permission
// is below the command's required level.
var ErrPermissionDenied = errors.New("permission denied")

// ErrCommandPanicked is returned by Dispatch when a plugin's Run panicked.
var ErrCommandPanicked = errors.New("command panicked")

// ErrCommandTimedOut is returned by Dispatch when a plugin's Run did not
// return within DefaultDispatchTimeout.
var ErrCommandTimedOut = errors.New("command timed out")

// Dispatch finds the command named name and runs it, after checking that
// inv.ActorPermission meets the command's requirement. name is matched
// case-insensitively.
//
// Run executes with a bounded timeout (DefaultDispatchTimeout) derived from
// ctx: a plugin that never returns cannot block the caller forever - today
// that caller is the Bedrock packet read loop, so an unbounded call here
// would deafen the whole agent. A panic inside Run is likewise recovered
// and returned as an error, so a plugin bug can say the wrong thing but
// cannot take down the connect loop or the HTTP endpoints.
//
// Note: on timeout, Dispatch returns without waiting for the still-running
// Run to finish - Go has no way to force-cancel a goroutine that ignores
// ctx, so a plugin that both blocks and ignores its context leaks a
// goroutine until it eventually returns. Well-behaved plugins (including
// every one shipped in this repo) respect ctx cancellation in any I/O they
// perform, which avoids that leak in practice.
func (r *Registry) Dispatch(ctx context.Context, pctx *Context, name string, inv Invocation) (reply string, err error) {
	r.mu.RLock()
	cmd, ok := r.commands[commandKey(name)]
	r.mu.RUnlock()

	if !ok {
		return "", ErrUnknownCommand
	}
	if inv.ActorPermission < cmd.Permission {
		return "", ErrPermissionDenied
	}

	runCtx, cancel := context.WithTimeout(ctx, DefaultDispatchTimeout)
	defer cancel()

	type outcome struct {
		reply string
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		var out outcome
		defer func() {
			if p := recover(); p != nil {
				out = outcome{"", fmt.Errorf("%w: command %q: %v", ErrCommandPanicked, cmd.Name, p)}
			}
			done <- out
		}()
		reply, err := cmd.Run(runCtx, pctx, inv)
		out = outcome{reply, err}
	}()

	select {
	case out := <-done:
		return out.reply, out.err
	case <-runCtx.Done():
		return "", fmt.Errorf("%w: command %q", ErrCommandTimedOut, cmd.Name)
	}
}

// Lookup returns the command name resolves to, matched the same
// case-insensitive way Dispatch matches it.
func (r *Registry) Lookup(name string) (Command, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cmd, ok := r.commands[commandKey(name)]
	return cmd, ok
}

// Commands returns every registered command, sorted by name. Satisfies
// Directory.
func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Command, 0, len(r.commands))
	for _, cmd := range r.commands {
		out = append(out, cmd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Plugins returns every registered plugin, in registration order.
// Primarily useful for wiring event subscriptions in main.
func (r *Registry) Plugins() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Plugin, len(r.plugins))
	copy(out, r.plugins)
	return out
}
