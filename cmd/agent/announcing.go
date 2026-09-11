package main

import (
	"context"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// announcePermissions adapts the bridge-backed resolver to the plain string
// a Deliverer asks for. A permission-targeted announcement is matched
// against target_value, which holds the schema's own level name, so the
// enum has to become that name somewhere; here, at the wiring, rather than
// by teaching either package about the other's vocabulary.
type announcePermissions struct {
	resolver *adapters.PermissionResolver
}

var _ announce.Permissions = announcePermissions{}

func (p announcePermissions) Resolve(ctx context.Context, xuid string) string {
	return p.resolver.Resolve(ctx, xuid).String()
}

// deliveryAudience is the live roster minus the bots on it: who an
// announcement may actually be sent to.
//
// The filter lives here rather than in roster.Roster because the roster's
// job is to report who is connected, and this agent genuinely is — its own
// entry is how Voice resolves a gamertag, and removing it there would make
// one type answer two different questions depending on the caller. Who
// *should be told* is a delivery decision, and it is the same decision
// handleText and handlePlayerList already make with chat.IsSelfOrSibling
// before publishing an event, so the third caller of that helper belongs
// beside the other two.
//
// Both consequences of getting this wrong are silent: the agent whispers
// permission-targeted announcements to itself, and every broadcast logs a
// foreign-key error, because no bot is ever written to minecraft.players
// and announcement_deliveries.xuid references it.
//
// This covers announcements sent to whoever is online (SendNow). The
// join-time drain is filtered upstream instead — it delivers to the XUID
// from a roster.JoinEvent, and handlePlayerList already refuses to publish
// one for this agent.
type deliveryAudience struct {
	roster   *roster.Roster
	siblings map[string]struct{}

	// self is session-scoped: an XUID is only known once a connection has
	// spawned, while the Deliverer reading it is built once for the
	// process. The delivery goroutines read it while the next session's
	// login writes it, so it is guarded rather than merely assigned.
	mu   sync.RWMutex
	self string
}

// newDeliveryAudience wraps r, excluding siblings and — once beginSession
// has been called — this agent itself. siblings is retained rather than
// copied: main builds it once and never mutates it.
func newDeliveryAudience(r *roster.Roster, siblings map[string]struct{}) *deliveryAudience {
	return &deliveryAudience{roster: r, siblings: siblings}
}

var _ announce.Roster = (*deliveryAudience)(nil)

// beginSession records the identity this agent connected under. Before the
// first call there is no connection, so there is nobody on the roster to
// exclude from either.
func (a *deliveryAudience) beginSession(selfXUID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.self = selfXUID
}

// Online satisfies announce.Roster.
func (a *deliveryAudience) Online() []string {
	a.mu.RLock()
	self := a.self
	a.mu.RUnlock()

	online := a.roster.Online()
	out := make([]string, 0, len(online))
	for _, xuid := range online {
		if chat.IsSelfOrSibling(xuid, self, a.siblings) {
			continue
		}
		out = append(out, xuid)
	}
	return out
}

// nameArchive is the durable half of resolving a gamertag: the names this
// server has recorded, as opposed to the names currently connected.
// Narrowed from store.Store to the single read that needs it.
type nameArchive interface {
	XUIDForName(ctx context.Context, gamertag string) (xuid string, ok bool, err error)
}

// playerLookup resolves the "@player" an operator typed, live roster first
// and the profile store second.
//
// One tier is not enough, and which one is missing decides whether the
// feature works at all. The roster holds only who is connected and is
// emptied at the start of every session, so a lookup that stopped there
// could never name the player an offline announcement is for -- and being
// able to leave a message for someone who is not here is the entire reason
// the queue exists. The store alone would be worse in the other direction:
// it cannot answer for a player this agent has watched arrive but never
// written down.
//
// Ordered roster-first for cost, not correctness. The roster is an in-memory
// map and is by definition current, and the two only ever disagree while a
// rename is still propagating, in which case the connected player is the
// better answer anyway.
type playerLookup struct {
	live    *roster.Roster
	archive nameArchive
}

var _ plugin.Roster = playerLookup{}

// XUIDFor satisfies plugin.Roster. A name neither tier knows is reported as
// not found, never as an error: that is a real answer, and it is the one
// the command refuses on.
func (l playerLookup) XUIDFor(ctx context.Context, name string) (string, bool, error) {
	if l.live != nil {
		if xuid, ok := l.live.XUIDFor(name); ok {
			return xuid, true, nil
		}
	}
	if l.archive == nil {
		return "", false, nil
	}
	return l.archive.XUIDForName(ctx, name)
}

// playerRecorder is the one thing an outbox needs from the profile store: a
// row for a player, so a foreign key has something to point at.
type playerRecorder interface {
	EnsurePlayer(ctx context.Context, xuid, gamertag string, at time.Time) (created bool, err error)
}

// outbox is the announcement store as this binary uses it: before writing a
// row that names a player, it makes sure minecraft.players has one for them.
//
// The case it exists for is genuinely surprising, so it is written down
// rather than left to be rediscovered. A player already connected when the
// agent logs in arrives in the session's opening PlayerList. roster.Apply
// records them but deliberately does not report them as a join — that is
// what stops everyone being welcomed again on every reconnect — so nothing
// ever calls RecordJoin for them. Without a players row the foreign key on
// announcement_deliveries.xuid rejects their delivery, the row is skipped,
// and they hear the same broadcast again at their next real join.
//
// Ensuring the row here, on the write that needs it, keeps "counts as a
// join" and "exists in the table" the separate questions they are: the
// welcome still fires only for a real arrival, and nothing about what
// counts as one had to be relaxed to make this write legal. It also settles
// the race between the two plugins that answer the same join event —
// welcome's RecordJoin and the drain's delivery run on different
// goroutines, so the delivery cannot assume the row is there yet.
type outbox struct {
	store   announce.Store
	players playerRecorder
	names   adapters.NameResolver
	log     *logging.Logger
	// unready fires for the first statement the database refuses for a
	// deploy reason. !announce and !inbox both answer that state to
	// whoever typed them, which leaves it visible to exactly one person
	// and indistinguishable from an agent that was never given a database
	// at all. This is the other half of that: one line, in the log an
	// operator reads when a release looks wrong. Once, because the state
	// cannot change without this process being replaced.
	unready sync.Once
}

func newOutbox(store announce.Store, players playerRecorder, names adapters.NameResolver, log *logging.Logger) *outbox {
	return &outbox{store: store, players: players, names: names, log: log}
}

var _ announce.Store = (*outbox)(nil)

// Insert writes one announcement, having first made its author writable.
//
// announcements.author_xuid is a foreign key into the same table, so an
// operator who was already connected when the agent logged in cannot be
// recorded as the author of their own announcement either.
// noteUnready reports, once, that the announcement tables are missing or
// unreadable to this role. op names the statement that hit it, since which
// one it was is the difference between a partial migration and a total one.
func (o *outbox) noteUnready(op string, err error) {
	if !pgerr.Unready(err) {
		return
	}
	o.unready.Do(func() {
		o.log.Info("announce_store_unready", logging.Fields{"op": op, "error": err.Error()})
	})
}

func (o *outbox) Insert(ctx context.Context, a announce.Announcement) (int64, error) {
	if a.AuthorXUID == chat.ServerOrigin {
		// Belt and braces. The command that builds a console-issued
		// announcement already blanks this, because the console has no
		// player identity and the whole process should agree about that --
		// but this is the last point before a foreign key sees it, and a
		// future source that forgets would fail the write rather than be
		// caught here.
		a.AuthorXUID = ""
	}
	o.ensure(ctx, a.AuthorXUID)
	id, err := o.store.Insert(ctx, a)
	o.noteUnready("insert", err)
	return id, err
}

func (o *outbox) PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]announce.Announcement, error) {
	pending, err := o.store.PendingFor(ctx, xuid, permission, now)
	o.noteUnready("pending", err)
	return pending, err
}

func (o *outbox) MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error {
	o.ensure(ctx, xuid)
	err := o.store.MarkDelivered(ctx, id, xuid, at)
	o.noteUnready("mark_delivered", err)
	return err
}

func (o *outbox) Enabled() bool { return o.store.Enabled() }

// ensure creates the players row xuid's foreign keys need, when the roster
// can still say who they are.
//
// An unresolvable name is left alone rather than filled in with the XUID:
// current_gamertag is what every human-facing report and the rename history
// read from, and a placeholder invented here would outlive the moment that
// produced it. The write that follows either succeeds, because the row was
// already there, or fails its foreign key and is logged by its caller —
// which is what happened before this existed, for a case that needs a
// player to have left between being chosen as a recipient and being told.
//
// A failure is logged and not returned for the same reason: the caller's
// own write is worth attempting regardless, and it reports its own error.
//
// Nothing here guards against its own dependencies being nil. An outbox
// assembled without a store, a roster or a logger is a wiring mistake, and
// this file's whole subject is that a wiring mistake must be loud: a guard
// would turn it into an announcement trail that quietly stops recording who
// heard what.
func (o *outbox) ensure(ctx context.Context, xuid string) {
	if xuid == "" {
		return
	}
	gamertag, ok := o.names.NameFor(xuid)
	if !ok || gamertag == "" {
		return
	}
	if _, err := o.players.EnsurePlayer(ctx, xuid, gamertag, time.Now()); err != nil {
		o.log.Error("announce_ensure_player_failed", logging.Fields{"xuid": xuid, "error": err.Error()})
	}
}
