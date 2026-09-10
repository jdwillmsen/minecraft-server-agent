package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// fakeAnnounceStore is a small in-memory stand-in for plugin.AnnounceStore:
// every Insert is recorded so a test can assert exactly what was stored,
// rather than just how many rows exist.
type fakeAnnounceStore struct {
	enabled   bool
	inserted  []announce.Announcement
	insertErr error
	nextID    int64
}

var _ plugin.AnnounceStore = (*fakeAnnounceStore)(nil)

func (f *fakeAnnounceStore) Insert(_ context.Context, a announce.Announcement) (int64, error) {
	if f.insertErr != nil {
		return 0, f.insertErr
	}
	f.nextID++
	a.ID = f.nextID
	f.inserted = append(f.inserted, a)
	return f.nextID, nil
}

func (f *fakeAnnounceStore) Enabled() bool { return f.enabled }

// fakeAnnounceDeliverer is a small stand-in for plugin.AnnounceDeliverer:
// SendNow records what it was asked to send, DrainAll reports a
// test-configured count and remembers which xuid it was asked to drain.
type fakeAnnounceDeliverer struct {
	sent []announce.Announcement
	// sentNow is what SendNow reports as actually delivered. Zero by
	// default, which is what a real deliverer reports for a target who is
	// offline -- the case the queue exists for.
	sentNow        int
	sendErr        error
	drainXUID      string
	drainCount     int
	drainRemaining int
	drainErr       error
}

var _ plugin.AnnounceDeliverer = (*fakeAnnounceDeliverer)(nil)

func (f *fakeAnnounceDeliverer) SendNow(_ context.Context, a announce.Announcement, _ int64) (int, error) {
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sent = append(f.sent, a)
	return f.sentNow, nil
}

func (f *fakeAnnounceDeliverer) DrainAll(_ context.Context, xuid string, _ time.Time) (int, int, error) {
	f.drainXUID = xuid
	return f.drainCount, f.drainRemaining, f.drainErr
}

// fakeAnnounceRoster stands in for the two-tier lookup a command is handed:
// online names resolve from the live roster, and names the server has
// merely recorded before resolve from the profile store. The split is the
// point -- a fake with only one map cannot tell an offline player from one
// who has never existed, which is exactly the distinction this command has
// to make.
type fakeAnnounceRoster struct {
	online  map[string]string
	offline map[string]string
	err     error
}

var _ plugin.Roster = fakeAnnounceRoster{}

func (f fakeAnnounceRoster) XUIDFor(_ context.Context, name string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	if xuid, ok := f.online[name]; ok {
		return xuid, true, nil
	}
	xuid, ok := f.offline[name]
	return xuid, ok, nil
}

func announceCommand(t *testing.T, name string) plugin.Command {
	t.Helper()
	for _, c := range NewAnnounce().Commands() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s command not registered", name)
	return plugin.Command{}
}

func TestAnnounceRequiresOperator(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "member",
		ActorPermission: plugin.PermissionMember,
		Args:            []string{"server", "restarting"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 0 {
		t.Fatal("a member's announcement was stored")
	}
	if !strings.Contains(strings.ToLower(reply), "operator") {
		t.Errorf("reply %q should say why the announcement was refused", reply)
	}
}

func TestAnnounceFlagsParseOnlyBeforeTheBody(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}}
	op := plugin.Invocation{ActorXUID: "op", ActorPermission: plugin.PermissionOperator}

	expedited := op
	expedited.Args = []string{"!urgent", "server", "restarting"}
	if _, err := cmd.Run(context.Background(), pctx, expedited); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("got %d announcements, want 1", len(store.inserted))
	}
	if store.inserted[0].Priority != announce.PriorityExpedited {
		t.Errorf("priority = %q, want expedited -- !urgent led the line", store.inserted[0].Priority)
	}
	if store.inserted[0].Body != "server restarting" {
		t.Errorf("body = %q, want %q", store.inserted[0].Body, "server restarting")
	}

	ambiguous := op
	ambiguous.Args = []string{"the", "!urgent", "flag", "goes", "first"}
	if _, err := cmd.Run(context.Background(), pctx, ambiguous); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 2 {
		t.Fatalf("got %d announcements, want 2", len(store.inserted))
	}
	if store.inserted[1].Priority != announce.PriorityNormal {
		t.Errorf("priority = %q, want normal -- !urgent appeared inside the body, not before it", store.inserted[1].Priority)
	}
	if want := "the !urgent flag goes first"; store.inserted[1].Body != want {
		t.Errorf("body = %q, want %q (the whole phrase preserved)", store.inserted[1].Body, want)
	}
}

// The headline case: the player is not here. An announcement aimed at
// someone offline has to be stored and left pending, because a message that
// waits for them is the entire promise of the feature -- a lookup that only
// knew who was connected would answer "I don't know a player named X" to
// the one target this command exists for.
func TestAnnounceToAPlayerWhispersAndQueues(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	deliverer := &fakeAnnounceDeliverer{}
	roster := fakeAnnounceRoster{offline: map[string]string{"Dotablaze": "xuid-dota"}}
	pctx := &plugin.Context{Announcements: store, Deliverer: deliverer, Roster: roster}

	before := time.Now()
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@Dotablaze", "the", "farm", "moved"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("got %d announcements, want 1", len(store.inserted))
	}
	a := store.inserted[0]
	if a.TargetKind != announce.TargetPlayer || a.TargetValue != "xuid-dota" {
		t.Errorf("target = %s/%s, want player/xuid-dota", a.TargetKind, a.TargetValue)
	}
	if a.Delivery != announce.DeliveryWhisper {
		t.Errorf("delivery = %s, want whisper", a.Delivery)
	}
	if a.Body != "the farm moved" {
		t.Errorf("body = %q, want %q", a.Body, "the farm moved")
	}
	if a.ExpiresAt == nil {
		t.Fatal("expiry = nil, want about 7 days out")
	}
	if d := a.ExpiresAt.Sub(before); d < 6*24*time.Hour || d > 8*24*time.Hour {
		t.Errorf("expiry = %v out, want about 7 days", d)
	}
	if !strings.Contains(reply, "Dotablaze") {
		t.Errorf("reply %q should name the whispered player", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "queued") {
		t.Errorf("reply %q should say the message is queued; nothing was delivered to an offline player", reply)
	}
	// Nothing was delivered -- they are offline -- so the row has to be the
	// thing that survives, ready for their next join to drain.
	if len(deliverer.sent) != 1 {
		t.Fatalf("the deliverer was asked to send %d times, want 1", len(deliverer.sent))
	}
	if deliverer.sent[0].TargetValue != "xuid-dota" {
		t.Errorf("delivered target = %q, want the resolved xuid", deliverer.sent[0].TargetValue)
	}
}

// An online target resolves through the live roster without the durable
// half being consulted at all.
func TestAnnounceToAnOnlinePlayerResolvesFromTheLiveRoster(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	deliverer := &fakeAnnounceDeliverer{sentNow: 1}
	roster := fakeAnnounceRoster{
		online:  map[string]string{"Dotablaze": "xuid-live"},
		offline: map[string]string{"Dotablaze": "xuid-stale"},
	}
	pctx := &plugin.Context{Announcements: store, Deliverer: deliverer, Roster: roster}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@Dotablaze", "the", "farm", "moved"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("got %d announcements, want 1", len(store.inserted))
	}
	if got := store.inserted[0].TargetValue; got != "xuid-live" {
		t.Errorf("target = %q, want the connected player rather than the recorded one", got)
	}
	if reply != "Told Dotablaze." {
		t.Errorf("reply = %q, want it to report the delivery that happened", reply)
	}
}

// !now has no queue behind it, so nobody hearing it means nobody ever will
// -- "Announced." would report something that did not happen and cannot
// happen later.
func TestAnnounceNowWithNobodyOnlineSaysNobodyHeardIt(t *testing.T) {
	cmd := announceCommand(t, "announce")
	pctx := &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{sentNow: 0},
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"!now", "restarting", "in", "five"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "nobody") {
		t.Errorf("reply %q should say nobody heard it", reply)
	}
}

// A lookup that could not be made is not an answer of "no". Refusing on one
// would deny a player who does exist, and storing on one would aim a
// private message at nobody.
func TestAnnounceFailsRatherThanGuessWhenTheLookupBreaks(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	roster := fakeAnnounceRoster{err: errors.New("connection refused")}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}, Roster: roster}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@Dotablaze", "hello"},
	}); err == nil {
		t.Error("a broken lookup was treated as a player who does not exist")
	}
	if len(store.inserted) != 0 {
		t.Error("an announcement was stored for a name that was never resolved")
	}
}

func TestAnnounceNowNeverQueues(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"!now", "restarting", "in", "five"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("got %d announcements, want 1", len(store.inserted))
	}
	a := store.inserted[0]
	if a.TargetKind != announce.TargetOnlineOnly {
		t.Errorf("target = %s, want online_only", a.TargetKind)
	}
	if a.ExpiresAt != nil {
		t.Errorf("expiry = %v, want nil -- online_only never queues", a.ExpiresAt)
	}
}

func TestInboxDrainsOnlyTheCallersOwnQueue(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	deliverer := &fakeAnnounceDeliverer{drainCount: 2}
	pctx := &plugin.Context{Announcements: &fakeAnnounceStore{enabled: true}, Deliverer: deliverer}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "player-a",
		ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deliverer.drainXUID != "player-a" {
		t.Errorf("drained %q, want the caller's own xuid", deliverer.drainXUID)
	}
	if !strings.Contains(reply, "2") {
		t.Errorf("reply %q should say how many were delivered", reply)
	}
}

func TestInboxWithNothingPendingSaysSo(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	pctx := &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{drainCount: 0},
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "nothing new") {
		t.Errorf("reply %q should say there was nothing new", reply)
	}
}

func TestAnnounceWithoutAStoreSaysSo(t *testing.T) {
	cmd := announceCommand(t, "announce")
	const wantSubstring = "store configured"

	reply, err := cmd.Run(context.Background(), &plugin.Context{}, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting"},
	})
	if err != nil {
		t.Fatalf("a nil Announcements store must not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), wantSubstring) {
		t.Errorf("reply %q should explain the feature is unconfigured", reply)
	}

	disabled := &fakeAnnounceStore{enabled: false}
	reply, err = cmd.Run(context.Background(), &plugin.Context{
		Announcements: disabled,
		Deliverer:     &fakeAnnounceDeliverer{},
	}, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting"},
	})
	if err != nil {
		t.Fatalf("a disabled store must not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), wantSubstring) {
		t.Errorf("reply %q should explain the feature is unconfigured", reply)
	}
	if len(disabled.inserted) != 0 {
		t.Error("a disabled store should not receive an insert")
	}
}

func TestInboxWithoutAStoreSaysSo(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	inv := plugin.Invocation{ActorXUID: "someone", ActorPermission: plugin.PermissionMember}

	reply, err := cmd.Run(context.Background(), &plugin.Context{}, inv)
	if err != nil {
		t.Fatalf("a nil Deliverer must not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "store configured") {
		t.Errorf("reply %q should explain the feature is unconfigured", reply)
	}

	// The regression this predicate exists for: a deliverer over a store
	// that persists nothing drains zero, which reads as an empty queue.
	// Answering "you have nothing new" there asserts a fact about the
	// player's messages that nothing here can know, while !announce in the
	// identical state correctly says the store is unconfigured.
	deliverer := &fakeAnnounceDeliverer{}
	reply, err = cmd.Run(context.Background(), &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: false},
		Deliverer:     deliverer,
	}, inv)
	if err != nil {
		t.Fatalf("a disabled store must not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "store configured") {
		t.Errorf("reply %q should explain the feature is unconfigured, not claim the queue is empty", reply)
	}
	if deliverer.drainXUID != "" {
		t.Error("a disabled store was drained anyway")
	}
}

// The console has no player identity, so nothing can ever be queued for it
// and every whisper the drain attempted would fail to resolve a gamertag --
// one logged failure per pending message, ending in "you have nothing new".
func TestInboxFromTheConsoleIsRefusedPlainly(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	deliverer := &fakeAnnounceDeliverer{drainCount: 3}

	reply, err := cmd.Run(context.Background(), &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     deliverer,
	}, plugin.Invocation{ActorXUID: chat.ServerOrigin, ActorPermission: plugin.PermissionOperator})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if deliverer.drainXUID != "" {
		t.Errorf("the console drained %q; it has no queue to drain", deliverer.drainXUID)
	}
	if !strings.Contains(strings.ToLower(reply), "console") {
		t.Errorf("reply %q should say the console has no inbox", reply)
	}
}

func TestAnnounceRefusesAnUnknownPlayerRatherThanStoreIt(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}, Roster: fakeAnnounceRoster{}}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@NoSuchPlayer", "hello"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 0 {
		t.Error("an announcement was stored for a player nobody can ever resolve")
	}
	if !strings.Contains(reply, "NoSuchPlayer") {
		t.Errorf("reply %q should name the player it does not know", reply)
	}
	if reply == "" {
		t.Error("reply should say the player is unknown")
	}
}

func TestAnnounceNowAndPlayerTargetIsRefused(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	roster := fakeAnnounceRoster{online: map[string]string{"Dotablaze": "xuid-dota"}}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}, Roster: roster}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@Dotablaze", "!now", "hello"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 0 {
		t.Error("a self-contradictory target/flag combination was stored")
	}
	if reply == "" {
		t.Error("reply should explain the conflict rather than guess")
	}
}

func TestAnnounceRefusesASecondPlayerToken(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	roster := fakeAnnounceRoster{online: map[string]string{"A": "xuid-a", "B": "xuid-b"}}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}, Roster: roster}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@A", "@B", "the", "farm", "moved"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 0 {
		t.Error("an announcement with two @player tokens was stored -- one target must have been silently dropped")
	}
	if !strings.Contains(strings.ToLower(reply), "one player") {
		t.Errorf("reply %q should say an announcement goes to one player at a time", reply)
	}
}

// missingTableErr is what pgx returns for a statement against a table that
// does not exist, wrapped the way the store wraps it.
func missingTableErr() error {
	return fmt.Errorf("announce: insert: %w", &pgconn.PgError{
		Code:    "42P01",
		Message: `relation "minecraft.announcements" does not exist`,
	})
}

// A pool with no announcement tables behind it passes every configuration
// check this command makes and then fails on the first statement. Answered
// like an unconfigured store, because an errored command sends no reply at
// all and the operator would be left unable to tell a delivered
// announcement from a broken one.
func TestAnnounceBeforeTheMigrationSaysSoRatherThanNothing(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true, insertErr: missingTableErr()}
	deliverer := &fakeAnnounceDeliverer{}
	pctx := &plugin.Context{Announcements: store, Deliverer: deliverer}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting"},
	})
	if err != nil {
		t.Fatalf("a missing table must not error the command: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "store configured") {
		t.Errorf("reply %q should explain the feature is unconfigured", reply)
	}
	if len(deliverer.sent) != 0 {
		t.Error("an announcement that was never stored was delivered anyway")
	}
}

func TestInboxBeforeTheMigrationSaysSoRatherThanNothing(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	deliverer := &fakeAnnounceDeliverer{drainErr: missingTableErr()}

	reply, err := cmd.Run(context.Background(), &plugin.Context{Deliverer: deliverer}, plugin.Invocation{
		ActorXUID: "someone", ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("a missing table must not error the command: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "store configured") {
		t.Errorf("reply %q should explain the feature is unconfigured", reply)
	}
}

// Any other database failure is still a failure: translating them all would
// tell an operator the feature is unconfigured when it is merely broken.
func TestAnnounceStillFailsOnAnOrdinaryStoreError(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true, insertErr: errors.New("connection refused")}

	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}}
	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting"},
	}); err == nil {
		t.Error("a store that is broken rather than unmigrated was reported as unconfigured")
	}
}

// The console is not a player, and author_xuid is a foreign key into
// minecraft.players. Blanked where the announcement is described, so the
// delivery sees the same author the row does.
func TestAnnounceFromTheConsoleHasNoAuthor(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	deliverer := &fakeAnnounceDeliverer{}
	pctx := &plugin.Context{Announcements: store, Deliverer: deliverer}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       chat.ServerOrigin,
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(store.inserted) != 1 {
		t.Fatalf("stored %d announcements, want 1", len(store.inserted))
	}
	if got := store.inserted[0].AuthorXUID; got != "" {
		t.Errorf("stored author = %q, want empty so the column is written NULL", got)
	}
	if len(deliverer.sent) != 1 {
		t.Fatalf("delivered %d announcements, want 1", len(deliverer.sent))
	}
	if got := deliverer.sent[0].AuthorXUID; got != "" {
		t.Errorf("delivered author = %q; the delivery must see the same author the row does", got)
	}
}

// The tables exist but the role the agent connects as cannot touch them --
// the failure this project has actually had. Answered, and answered
// differently from an unconfigured store, because the two need different
// things done to them.
func TestCommandsSayWhenTheStoreRefusesAccess(t *testing.T) {
	grantErr := fmt.Errorf("announce: insert: %w", &pgconn.PgError{
		Code:    "42501",
		Message: "permission denied for table announcements",
	})

	announceCmd := announceCommand(t, "announce")
	reply, err := announceCmd.Run(context.Background(), &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true, insertErr: grantErr},
		Deliverer:     &fakeAnnounceDeliverer{},
	}, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting"},
	})
	if err != nil {
		t.Fatalf("a refused grant must not error the command: %v", err)
	}
	if reply != noAccess {
		t.Errorf("!announce replied %q, want %q -- a missing grant is not a missing store", reply, noAccess)
	}

	inboxCmd := announceCommand(t, "inbox")
	reply, err = inboxCmd.Run(context.Background(), &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{drainErr: grantErr},
	}, plugin.Invocation{ActorXUID: "player-a", ActorPermission: plugin.PermissionMember})
	if err != nil {
		t.Fatalf("a refused grant must not error the command: %v", err)
	}
	if reply != noAccess {
		t.Errorf("!inbox replied %q, want %q", reply, noAccess)
	}
}

// A capped drain that said only how many arrived would leave the player
// holding a partial delivery with no way to know there is more.
func TestInboxSaysWhenSomethingIsStillWaiting(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	deliverer := &fakeAnnounceDeliverer{drainCount: announce.MaxPerInbox, drainRemaining: 3}
	pctx := &plugin.Context{Announcements: &fakeAnnounceStore{enabled: true}, Deliverer: deliverer}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(reply, "3") || !strings.Contains(reply, "!inbox") {
		t.Errorf("reply %q should say how many are still waiting and how to get them", reply)
	}
	if strings.HasSuffix(reply, "?") {
		t.Errorf("reply %q ends in a question mark", reply)
	}
}
