package main

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// onlineRoster returns a roster holding exactly these players, as the
// server's opening PlayerList would leave it: everyone recorded, nobody
// reported as a join.
func onlineRoster(entries ...roster.PlayerListEntry) *roster.Roster {
	r := roster.New()
	r.Apply(entries)
	return r
}

func present(xuid, name string) roster.PlayerListEntry {
	return roster.PlayerListEntry{XUID: xuid, Username: name}
}

// recordingAnnounceStore is a test double for announce.Store that reports
// what it was asked to write, in order.
type recordingAnnounceStore struct {
	mu        sync.Mutex
	inserted  []announce.Announcement
	delivered []string
}

var _ announce.Store = (*recordingAnnounceStore)(nil)

func (s *recordingAnnounceStore) Insert(_ context.Context, a announce.Announcement) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inserted = append(s.inserted, a)
	return int64(len(s.inserted)), nil
}

func (s *recordingAnnounceStore) PendingFor(context.Context, string, string, time.Time) ([]announce.Announcement, error) {
	return nil, nil
}

func (s *recordingAnnounceStore) MarkDelivered(_ context.Context, _ int64, xuid string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, xuid)
	return nil
}

func (s *recordingAnnounceStore) Enabled() bool { return true }

func (s *recordingAnnounceStore) deliveries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivered...)
}

// recordingPlayers is a test double for the profile store's one method the
// outbox uses.
type recordingPlayers struct {
	mu      sync.Mutex
	ensured []string
	err     error
}

var _ playerRecorder = (*recordingPlayers)(nil)

func (p *recordingPlayers) EnsurePlayer(_ context.Context, xuid, gamertag string, _ time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensured = append(p.ensured, xuid+" as "+gamertag)
	return p.err
}

func (p *recordingPlayers) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ensured...)
}

func testOutbox(store announce.Store, players playerRecorder, names *roster.Roster) *outbox {
	return newOutbox(store, players, names, logging.New("error"))
}

// The agent is on the roster like any other player: its own PlayerList entry
// is how Voice resolves gamertags. Announcing to it would whisper the server
// its own messages, and record a delivery against an XUID minecraft.players
// has no row for -- a foreign-key error on every broadcast.
func TestDeliveryAudienceExcludesThisAgent(t *testing.T) {
	audience := newDeliveryAudience(
		onlineRoster(present(selfXUID, "ServerAgent"), present(playerXUID, "Steve")),
		siblingBotXUIDs(),
	)
	audience.beginSession(selfXUID)

	if got := audience.Online(); len(got) != 1 || got[0] != playerXUID {
		t.Errorf("audience = %v, want just the player; the agent announces to itself", got)
	}
}

// The sibling set is empty in production and deliberately so -- see
// siblingBotXUIDs -- but the filter it feeds has to work the moment anything
// does populate it, or wiring it up later would look done and change
// nothing.
func TestDeliveryAudienceExcludesSiblingBots(t *testing.T) {
	audience := newDeliveryAudience(
		onlineRoster(present(siblingBot, "AfkBot"), present(playerXUID, "Steve")),
		map[string]struct{}{siblingBot: {}},
	)
	audience.beginSession(selfXUID)

	if got := audience.Online(); len(got) != 1 || got[0] != playerXUID {
		t.Errorf("audience = %v, want just the player; a sibling bot is being announced to", got)
	}
}

// Before the first login there is no self to exclude. Nothing is connected
// then either, so the filter must not invent an exclusion from a zero value.
func TestDeliveryAudienceWithoutASessionExcludesNobody(t *testing.T) {
	audience := newDeliveryAudience(onlineRoster(present(playerXUID, "Steve")), siblingBotXUIDs())

	if got := audience.Online(); len(got) != 1 || got[0] != playerXUID {
		t.Errorf("audience = %v, want the player; an unset self XUID excluded someone", got)
	}
}

// The Deliverer reads the audience from its own goroutine while the packet
// loop writes the roster and a reconnect writes the session identity. Run
// under -race, this is the test that says so.
func TestDeliveryAudienceSurvivesAConcurrentSession(t *testing.T) {
	playerRoster := roster.New()
	audience := newDeliveryAudience(playerRoster, siblingBotXUIDs())

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			playerRoster.Apply([]roster.PlayerListEntry{present(playerXUID, "Steve"), present(selfXUID, "ServerAgent")})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			audience.beginSession(selfXUID)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			audience.Online()
		}
	}()
	wg.Wait()
}

// A player already connected when the agent logged in is on the roster but
// was never a join, so nothing recorded them and the delivery's foreign key
// has nothing to point at. Without this they hear the same broadcast again
// on every future join.
func TestOutboxRecordsAPlayerBeforeRecordingTheirDelivery(t *testing.T) {
	store := &recordingAnnounceStore{}
	players := &recordingPlayers{}
	o := testOutbox(store, players, onlineRoster(present(playerXUID, "Steve")))

	if err := o.MarkDelivered(t.Context(), 7, playerXUID, time.Now()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	if got := players.all(); len(got) != 1 || got[0] != playerXUID+" as Steve" {
		t.Errorf("ensured %v, want the recipient recorded under their roster name", got)
	}
	if got := store.deliveries(); len(got) != 1 || got[0] != playerXUID {
		t.Errorf("deliveries = %v, want the delivery still written", got)
	}
}

// The roster is the only thing that knows a gamertag, and a player who left
// between being chosen as a recipient and being told is no longer in it.
// Inventing a name would put a placeholder in the column every human-facing
// report reads from, and it would outlive the moment that produced it.
func TestOutboxWritesNoPlayerItCannotName(t *testing.T) {
	store := &recordingAnnounceStore{}
	players := &recordingPlayers{}
	o := testOutbox(store, players, roster.New())

	if err := o.MarkDelivered(t.Context(), 7, playerXUID, time.Now()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	if got := players.all(); len(got) != 0 {
		t.Errorf("ensured %v for a player the roster cannot name", got)
	}
	if got := store.deliveries(); len(got) != 1 {
		t.Errorf("deliveries = %v, want the write attempted anyway -- the row may already exist", got)
	}
}

// A failed ensure is the deliverer's problem to report, not a reason to skip
// the write: the row it needs may well already be there.
func TestOutboxStillWritesWhenEnsuringFails(t *testing.T) {
	store := &recordingAnnounceStore{}
	players := &recordingPlayers{err: errors.New("database down")}
	o := testOutbox(store, players, onlineRoster(present(playerXUID, "Steve")))

	if err := o.MarkDelivered(t.Context(), 7, playerXUID, time.Now()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if got := store.deliveries(); len(got) != 1 {
		t.Errorf("deliveries = %v, want the write attempted despite the failed ensure", got)
	}
}

// author_xuid is the same foreign key: an operator who was already connected
// when the agent logged in cannot be recorded as the author of their own
// announcement either.
func TestOutboxRecordsTheAuthorOfAnAnnouncement(t *testing.T) {
	store := &recordingAnnounceStore{}
	players := &recordingPlayers{}
	o := testOutbox(store, players, onlineRoster(present(playerXUID, "Steve")))

	if _, err := o.Insert(t.Context(), announce.Announcement{Body: "hello", AuthorXUID: playerXUID}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if got := players.all(); len(got) != 1 || got[0] != playerXUID+" as Steve" {
		t.Errorf("ensured %v, want the author recorded", got)
	}
}

// The console is not a player. Left as the sentinel it would fail the same
// foreign key on every console-issued !announce, and the column is
// documented as null for an announcement with no human behind it.
func TestOutboxDropsTheConsoleAsAnAuthor(t *testing.T) {
	store := &recordingAnnounceStore{}
	players := &recordingPlayers{}
	o := testOutbox(store, players, roster.New())

	if _, err := o.Insert(t.Context(), announce.Announcement{Body: "restarting", AuthorXUID: chat.ServerOrigin}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.inserted) != 1 {
		t.Fatalf("inserted %d announcements, want 1", len(store.inserted))
	}
	if author := store.inserted[0].AuthorXUID; author != "" {
		t.Errorf("author_xuid = %q, want empty so the column is written NULL", author)
	}
	if got := players.all(); len(got) != 0 {
		t.Errorf("ensured %v; the console sentinel is not a player and must never become a row", got)
	}
}

// The permission a Deliverer matches against target_value is the schema's
// own level name, not the plugin package's enum.
func TestAnnouncePermissionsSpeakTheSchemasLevelNames(t *testing.T) {
	perms := announcePermissions{resolver: fakePermResolver(t, map[string]string{playerXUID: "member"})}

	if got := perms.Resolve(t.Context(), playerXUID); got != "member" {
		t.Errorf("Resolve = %q, want %q", got, "member")
	}
	if got := perms.Resolve(t.Context(), chat.ServerOrigin); got != "operator" {
		t.Errorf("console Resolve = %q, want %q", got, "operator")
	}
}

// The whole delivery path as main assembles it, against the roster a real
// session leaves behind: the agent's own entry is on it, and a broadcast
// must reach the players without ever recording one for the agent.
func TestBroadcastRecordsEveryPlayerAndOnlyPlayers(t *testing.T) {
	store := &recordingAnnounceStore{}
	players := &recordingPlayers{}
	playerRoster := onlineRoster(
		present(selfXUID, "ServerAgent"),
		present(playerXUID, "Steve"),
		present(otherPlayerXUID, "Alex"),
	)
	audience := newDeliveryAudience(playerRoster, siblingBotXUIDs())
	audience.beginSession(selfXUID)
	voice := &recordingVoice{}

	deliverer := announce.NewDeliverer(
		testOutbox(store, players, playerRoster),
		voice,
		audience,
		announcePermissions{resolver: fakePermResolver(t, nil)},
		logging.New("error"),
	)

	sent, err := deliverer.SendNow(t.Context(), announce.Announcement{
		Body:       "the server restarts in ten minutes",
		TargetKind: announce.TargetEveryone,
	}, 42)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent != 2 {
		t.Errorf("delivered to %d, want 2 -- the agent counts itself an audience", sent)
	}

	got := store.deliveries()
	sort.Strings(got)
	want := []string{otherPlayerXUID, playerXUID}
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("deliveries = %v, want %v", got, want)
	}
	if said := voice.output(); len(said) != 1 || said[0] != "say: the server restarts in ten minutes" {
		t.Errorf("said %v, want one broadcast", said)
	}
	if len(players.all()) != 2 {
		t.Errorf("ensured %v, want a row for each real recipient", players.all())
	}
}
