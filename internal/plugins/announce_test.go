package plugins

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
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
	// offline -- the case the queue exists for. uncounted overrides it with
	// the answer a blind broadcast gives: it was said, nobody can be named.
	sentNow   int
	uncounted bool
	// outcome is what became of the line. The zero value is the ordinary
	// spoken one, so a test that cares only about a count says nothing
	// about it.
	outcome        announce.Outcome
	sendErr        error
	drainXUID      string
	drainCount     int
	drainRemaining int
	drainErr       error
}

var _ plugin.AnnounceDeliverer = (*fakeAnnounceDeliverer)(nil)

func (f *fakeAnnounceDeliverer) SendNow(_ context.Context, a announce.Announcement, _ int64) (announce.Reach, error) {
	if f.sendErr != nil {
		return announce.Reach{Counted: true}, f.sendErr
	}
	f.sent = append(f.sent, a)
	if f.uncounted {
		return announce.Reach{}, nil
	}
	return announce.Reach{Players: f.sentNow, Counted: true, Outcome: f.outcome}, nil
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
		Deliverer:     &fakeAnnounceDeliverer{outcome: announce.OutcomeSilent},
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

// "Nobody heard that" is a claim about the server, and the agent can only
// make it when it can see who is on one. Broadcasting while the roster
// cannot answer -- the reconnect gap, or the moments before the first roster
// packet -- reaches whoever is there, so the operator is told what actually
// happened rather than a count the agent never had.
func TestAnnounceNowSaysSoWhenItCannotSeeWhoHeardIt(t *testing.T) {
	cmd := announceCommand(t, "announce")
	pctx := &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{uncounted: true},
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"!now", "restarting", "in", "five"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(reply), "nobody") {
		t.Errorf("reply %q claims nobody heard it, but it was broadcast to a server the agent cannot see", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "couldn't account for who heard it") {
		t.Errorf("reply %q should say the audience could not be counted", reply)
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
	// An enabled store, or the readiness guard answers first and this test
	// passes without the drain ever being reached -- which is how it read
	// while asserting a substring the guard happens to return too. The
	// whole point here is the branch behind the guard, so the reply is
	// pinned exactly.
	pctx := &plugin.Context{Announcements: &fakeAnnounceStore{enabled: true}, Deliverer: deliverer}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "someone", ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("a missing table must not error the command: %v", err)
	}
	if deliverer.drainXUID != "someone" {
		t.Fatal("the drain was never reached; this test is not exercising the branch it names")
	}
	if reply != noStore {
		t.Errorf("reply = %q, want %q", reply, noStore)
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

// The shared parser is what !schedule reuses, so its rules are pinned here
// directly rather than only through !announce's replies: a rule that moved
// would change both commands at once, and only a test of the helper says
// which one it was.
func TestParseAnnounceLine(t *testing.T) {
	const usage = "usage"
	atCap := strings.Repeat("é", announce.MaxBodyChars)
	cases := []struct {
		name        string
		args        []string
		want        announceLine
		wantRefusal string // substring; "" means accepted
	}{
		{"plain body", []string{"hello", "all"}, announceLine{body: "hello all"}, ""},
		{"flags before the body", []string{"!urgent", "!now", "restart"}, announceLine{now: true, urgent: true, body: "restart"}, ""},
		{"a flag inside the body is text", []string{"the", "!urgent", "flag"}, announceLine{body: "the !urgent flag"}, ""},
		{"player target", []string{"@Steve", "hi"}, announceLine{player: "Steve", body: "hi"}, ""},
		{"a bare @ is text", []string{"@", "hi"}, announceLine{body: "@ hi"}, ""},
		{"no args", nil, announceLine{}, usage},
		{"flags only", []string{"!now", "!urgent"}, announceLine{}, usage},
		{"two players", []string{"@A", "@B", "hi"}, announceLine{}, "one player"},
		{"now and a player", []string{"@A", "!now", "hi"}, announceLine{}, "!now and @player"},
		// Counted in characters: a body of multi-byte runes at the cap is
		// twice the cap in bytes and must still be accepted.
		{"exactly at the cap", []string{atCap}, announceLine{body: atCap}, ""},
		{"one past the cap", []string{atCap + "x"}, announceLine{}, "at most 512"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, refusal := parseAnnounceLine(tc.args, usage)
			if tc.wantRefusal == "" {
				if refusal != "" {
					t.Fatalf("refused with %q, want accepted", refusal)
				}
				if got != tc.want {
					t.Errorf("parsed %+v, want %+v", got, tc.want)
				}
				return
			}
			if !strings.Contains(refusal, tc.wantRefusal) {
				t.Errorf("refusal = %q, want it to contain %q", refusal, tc.wantRefusal)
			}
		})
	}
}

// An over-long body is refused before anything is stored: the cap is only a
// cap if nothing gets past it.
func TestAnnounceRefusesAnOverlongBodyBeforeStoringIt(t *testing.T) {
	cmd := announceCommand(t, "announce")
	store := &fakeAnnounceStore{enabled: true}
	pctx := &plugin.Context{Announcements: store, Deliverer: &fakeAnnounceDeliverer{}}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{strings.Repeat("a", announce.MaxBodyChars+1)},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.inserted) != 0 {
		t.Error("an over-long announcement was stored")
	}
	if !strings.Contains(reply, "at most") {
		t.Errorf("reply %q should say what the limit is", reply)
	}
}

// delivered counts only messages whose whisper and delivery row both
// succeeded. A player whose whispers land and whose rows fail to write is
// owed exactly what they have just watched arrive -- and will be sent it
// again, since nothing was recorded -- so "you have nothing new" is the one
// answer that cannot be true.
func TestInboxDoesNotClaimAnEmptyQueueWhenSomethingIsStillOwed(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	pctx := &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{drainCount: 0, drainRemaining: 3},
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(reply), "nothing new") {
		t.Errorf("reply %q denies messages the player is still owed", reply)
	}
	if strings.HasSuffix(reply, "?") {
		t.Errorf("reply %q ends in a question mark", reply)
	}
}

// An uncounted reach has two causes and they call for different answers. A
// whisper that reached its player and lost its delivery row must not be
// reported as the agent being unable to see who is online: the roster was
// never in doubt, and the operator would go looking for a connection problem
// that is not there.
func TestAnnounceNowDoesNotBlameTheRosterForAWhisperItCouldNotRecord(t *testing.T) {
	cmd := announceCommand(t, "announce")
	pctx := &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{uncounted: true},
		Roster:        fakeAnnounceRoster{online: map[string]string{"LightKing0221": "xuid-1"}},
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"@LightKing0221", "your", "waypoint", "is", "at", "spawn"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(reply), "who is online") {
		t.Errorf("reply %q blames the roster for a whisper whose record failed", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "record") {
		t.Errorf("reply %q should say the record is what failed", reply)
	}
}

// A publish that lands on a process which is not the live agent says
// nothing: the row is stored for whoever is. Answering "Announced." there
// claims a broadcast that was never spoken, and the operator watching chat
// waits for a line this process was never going to say.
func TestAnnounceDoesNotClaimABroadcastItOnlyQueued(t *testing.T) {
	cmd := announceCommand(t, "announce")
	pctx := &plugin.Context{
		Announcements: &fakeAnnounceStore{enabled: true},
		Deliverer:     &fakeAnnounceDeliverer{outcome: announce.OutcomeQueued},
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            []string{"server", "restarting", "in", "5"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply == "Announced." {
		t.Errorf("reply %q claims a broadcast nothing spoke", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "queued") {
		t.Errorf("reply %q should say the announcement is queued for the live agent", reply)
	}
}

// outbox is an announce.Store that holds what it is given, so a test can
// put a real Deliverer behind the command and read the reply the operator
// would actually see -- the fake deliverer above can only report a Reach a
// test made up, which is the thing under test here.
type outbox struct {
	nextID    int64
	pending   []announce.Announcement
	delivered []int64
}

var _ announce.Store = (*outbox)(nil)

func (o *outbox) Enabled() bool { return true }

func (o *outbox) Insert(context.Context, announce.Announcement) (int64, error) {
	o.nextID++
	return o.nextID, nil
}

// PendingFor reports only what has no delivery row yet, which is what makes
// a row here the suppression it is in Postgres.
func (o *outbox) PendingFor(context.Context, string, string, time.Time) ([]announce.Announcement, error) {
	var out []announce.Announcement
	for _, a := range o.pending {
		if !slices.Contains(o.delivered, a.ID) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (o *outbox) MarkDelivered(_ context.Context, id int64, _ string, _ time.Time) error {
	o.delivered = append(o.delivered, id)
	return nil
}

// bridge is the console bridge as a Deliverer sees it, able to refuse a
// broadcast the way an allowlist rejection, a 5xx or a timeout does.
type bridge struct{ sayErr error }

var _ announce.Voice = bridge{}

func (b bridge) Say(context.Context, string) error { return b.sayErr }

func (bridge) Tell(context.Context, string, string) error { return nil }

// watchedRoster is a roster that has been told who is here and names them.
type watchedRoster []string

var _ announce.Roster = watchedRoster{}

func (r watchedRoster) Online() []string { return r }
func (r watchedRoster) Knows() bool      { return true }
func (r watchedRoster) IsOnline(xuid string) bool {
	for _, x := range r {
		if x == xuid {
			return true
		}
	}
	return false
}

// openingRoster is the roster in the moments after a connection opens: it
// has not been told who is here, so it names nobody and says so.
type openingRoster struct{}

var _ announce.Roster = openingRoster{}

func (openingRoster) Online() []string     { return nil }
func (openingRoster) Knows() bool          { return false }
func (openingRoster) IsOnline(string) bool { return false }

// flatPermissions resolves everyone to the same level, which is all a
// broadcast target ever asks.
type flatPermissions struct{}

var _ announce.Permissions = flatPermissions{}

func (flatPermissions) Resolve(context.Context, string) string { return "member" }

type standby struct{ live bool }

var _ announce.Leadership = standby{}

func (s standby) Live() bool { return s.live }

// announceThrough runs !announce against a real Deliverer and returns what
// the operator was told.
func announceThrough(t *testing.T, d *announce.Deliverer, args ...string) string {
	t.Helper()
	cmd := announceCommand(t, "announce")
	pctx := &plugin.Context{Announcements: &fakeAnnounceStore{enabled: true}, Deliverer: d}
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "op",
		ActorPermission: plugin.PermissionOperator,
		Args:            args,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return reply
}

// A Say the bridge refused reached nobody, and the operator has to hear
// that: "Announced." sends them away believing a restart warning is in
// chat, and the server was never told.
func TestAnnounceReportsABroadcastTheBridgeRefused(t *testing.T) {
	d := announce.NewDeliverer(&outbox{}, bridge{sayErr: errors.New("bridge refused it")},
		watchedRoster{"xuid-1"}, flatPermissions{}, logging.New("error"))

	reply := announceThrough(t, d, "server", "restarting", "in", "5", "minutes")

	if reply == "Announced." {
		t.Errorf("reply %q claims a broadcast the bridge refused", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "couldn't send") {
		t.Errorf("reply %q should say the send failed", reply)
	}
}

// The same failure under !now, where the zero reaches the arm that reports
// an empty server: the operator is told nobody was on to hear a countdown
// that was never spoken, with players standing on the server.
func TestAnnounceNowDoesNotReportAFailedSendAsAnEmptyServer(t *testing.T) {
	d := announce.NewDeliverer(&outbox{}, bridge{sayErr: errors.New("bridge refused it")},
		watchedRoster{"xuid-1"}, flatPermissions{}, logging.New("error"))

	reply := announceThrough(t, d, "!now", "restarting", "in", "five")

	if strings.Contains(strings.ToLower(reply), "nobody") {
		t.Errorf("reply %q reports an empty server for a send that failed", reply)
	}
	if !strings.Contains(strings.ToLower(reply), "couldn't send") {
		t.Errorf("reply %q should say the send failed", reply)
	}
}

// !now on a process that is not the live agent speaks nothing and stores
// nothing anybody will pick up -- online-only is excluded from PendingFor
// by construction. Reported as an empty server, the operator goes on
// believing the countdown simply had no audience.
func TestAnnounceNowOffTheLeaderDoesNotReportAnEmptyServer(t *testing.T) {
	d := announce.NewDeliverer(&outbox{}, bridge{}, watchedRoster{"xuid-1"}, flatPermissions{}, logging.New("error"),
		announce.WithLeadership(standby{live: false}))

	reply := announceThrough(t, d, "!now", "restarting", "in", "five")

	if strings.Contains(strings.ToLower(reply), "nobody") {
		t.Errorf("reply %q reports an empty server for an announcement this process declined to say", reply)
	}
	if reply == "Announced." {
		t.Errorf("reply %q claims a broadcast nothing spoke", reply)
	}
}

// The reply that must stay: a watched server with nobody on it really did
// hear nothing, and !now has no queue to keep it in.
func TestAnnounceNowStillReportsAGenuinelyEmptyServer(t *testing.T) {
	d := announce.NewDeliverer(&outbox{}, bridge{}, watchedRoster{}, flatPermissions{}, logging.New("error"))

	reply := announceThrough(t, d, "!now", "restarting", "in", "five")

	if !strings.Contains(strings.ToLower(reply), "nobody") {
		t.Errorf("reply %q should say nobody was online to hear it", reply)
	}
}

// !inbox is typed by a player standing in the world, and their message is
// itself evidence of it. Answered in the moments before the opening roster
// packet lands, the drain used to read a roster that had not been told as
// the player having left: nothing was sent, and they were told something
// had gone wrong with messages that were never attempted.
func TestInboxDeliversWhileTheRosterHasNotBeenToldWhoIsHere(t *testing.T) {
	cmd := announceCommand(t, "inbox")
	store := &outbox{pending: []announce.Announcement{
		{ID: 1, Body: "the nether hub is open", TargetKind: announce.TargetPlayer, TargetValue: "xuid-1"},
		{ID: 2, Body: "back up your builds", TargetKind: announce.TargetPlayer, TargetValue: "xuid-1"},
	}}
	d := announce.NewDeliverer(store, bridge{}, openingRoster{}, flatPermissions{}, logging.New("error"))
	pctx := &plugin.Context{Announcements: &fakeAnnounceStore{enabled: true}, Deliverer: d}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID:       "xuid-1",
		ActorPermission: plugin.PermissionMember,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(reply), "went wrong") {
		t.Errorf("reply %q reports a failure for messages nothing failed to send", reply)
	}
	if !strings.Contains(reply, "2") {
		t.Errorf("reply %q should report both messages delivered", reply)
	}
}
