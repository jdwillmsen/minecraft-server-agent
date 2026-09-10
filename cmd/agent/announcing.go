package main

import (
	"context"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
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

// playerRecorder is the one thing an outbox needs from the profile store: a
// row for a player, so a foreign key has something to point at.
type playerRecorder interface {
	EnsurePlayer(ctx context.Context, xuid, gamertag string, at time.Time) error
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
func (o *outbox) Insert(ctx context.Context, a announce.Announcement) (int64, error) {
	if a.AuthorXUID == chat.ServerOrigin {
		// The console is not a player. ServerOrigin is a sentinel that will
		// never appear in minecraft.players, and the column is documented
		// as null for an announcement with no human behind it — which is
		// exactly what a `send-command say !announce ...` is.
		a.AuthorXUID = ""
	}
	o.ensure(ctx, a.AuthorXUID)
	return o.store.Insert(ctx, a)
}

func (o *outbox) PendingFor(ctx context.Context, xuid, permission string, now time.Time) ([]announce.Announcement, error) {
	return o.store.PendingFor(ctx, xuid, permission, now)
}

func (o *outbox) MarkDelivered(ctx context.Context, id int64, xuid string, at time.Time) error {
	o.ensure(ctx, xuid)
	return o.store.MarkDelivered(ctx, id, xuid, at)
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
func (o *outbox) ensure(ctx context.Context, xuid string) {
	if xuid == "" || o.players == nil || o.names == nil {
		return
	}
	gamertag, ok := o.names.NameFor(xuid)
	if !ok || gamertag == "" {
		return
	}
	if err := o.players.EnsurePlayer(ctx, xuid, gamertag, time.Now()); err != nil {
		o.log.Error("announce_ensure_player_failed", logging.Fields{"xuid": xuid, "error": err.Error()})
	}
}
