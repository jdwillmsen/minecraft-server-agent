package announce

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// testLogger is a real *logging.Logger writing nowhere anyone asserts on;
// these tests care about Deliverer's return values and what it told the
// fakes to do, not about log lines.
func testLogger() *logging.Logger { return logging.New("error") }

// delivery is one MarkDelivered call a fakeStore recorded.
type delivery struct {
	id   int64
	xuid string
}

// fakeStore is a small in-memory stand-in for Store: PendingFor always
// returns the same fixed slice regardless of xuid/permission, and every
// MarkDelivered call is recorded so a test can assert exactly who was
// marked, rather than just how many. markErr is keyed by xuid rather than a
// single flag, so a test can make one recipient's row fail to record while
// the rest still land — the only way to exercise SendNow's per-row
// continue.
type fakeStore struct {
	mu         sync.Mutex
	enabled    bool
	pending    []Announcement
	pendingErr error
	markErr    map[string]error
	delivered  []delivery
}

var _ Store = (*fakeStore)(nil)

func (s *fakeStore) Insert(context.Context, Announcement) (int64, error) { return 0, nil }

func (s *fakeStore) PendingFor(context.Context, string, string, time.Time) ([]Announcement, error) {
	return s.pending, s.pendingErr
}

func (s *fakeStore) MarkDelivered(_ context.Context, id int64, xuid string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.markErr[xuid]; ok && err != nil {
		return err
	}
	s.delivered = append(s.delivered, delivery{id: id, xuid: xuid})
	return nil
}

func (s *fakeStore) Enabled() bool { return s.enabled }

// fakeVoice records every Tell/Say call and can be told to fail for a
// specific xuid (Tell) or unconditionally (Say).
type fakeVoice struct {
	mu      sync.Mutex
	tells   []delivery // xuid + the announcement id is not known here, so id is unused; message lives separately
	tellMsg []string
	says    []string
	tellErr map[string]error
	sayErr  error
	// onSay runs after the broadcast is recorded, so a test can model the
	// world changing during the bridge round-trip the Say really is.
	onSay func()
}

var _ Voice = (*fakeVoice)(nil)

func (v *fakeVoice) Tell(_ context.Context, xuid, message string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tells = append(v.tells, delivery{xuid: xuid})
	v.tellMsg = append(v.tellMsg, message)
	if err, ok := v.tellErr[xuid]; ok {
		return err
	}
	return nil
}

func (v *fakeVoice) Say(_ context.Context, message string) error {
	v.mu.Lock()
	v.says = append(v.says, message)
	onSay, err := v.onSay, v.sayErr
	v.mu.Unlock()
	if onSay != nil {
		onSay()
	}
	return err
}

// fakeRoster reports a fixed set of online XUIDs.
//
// knows only has to be set for a roster that names nobody. The real one
// learns who is here and that it has been told from the same packet, so a
// roster holding players has necessarily been told; the state worth setting
// by hand is the empty one, which means "nobody is on" when it knows and
// "not told yet" when it does not.
type fakeRoster struct {
	online []string
	knows  bool
}

var _ Roster = fakeRoster{}

func (r fakeRoster) Online() []string { return r.online }

func (r fakeRoster) IsOnline(xuid string) bool {
	for _, x := range r.online {
		if x == xuid {
			return true
		}
	}
	return false
}

func (r fakeRoster) Knows() bool { return r.knows || len(r.online) > 0 }

// fakePermissions resolves each xuid to whatever level the test wired in,
// defaulting to "" for an xuid it wasn't told about.
type fakePermissions struct{ levels map[string]string }

var _ Permissions = fakePermissions{}

func (p fakePermissions) Resolve(_ context.Context, xuid string) string { return p.levels[xuid] }

func TestDrainForJoinCapsNormalButNotExpedited(t *testing.T) {
	// Expedited exists to bypass the cap; normal messages trickle so the
	// welcome message is not buried.
	pending := []Announcement{
		{ID: 1, Body: "urgent one", Priority: PriorityExpedited, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "urgent two", Priority: PriorityExpedited, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 3, Body: "normal one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 4, Body: "normal two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 5, Body: "normal three", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 6, Body: "normal four", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	delivered, remaining, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 5 {
		t.Errorf("delivered = %d, want 5 (2 expedited uncapped + 3 normal capped)", delivered)
	}
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1", remaining)
	}
	if len(store.delivered) != 5 {
		t.Errorf("store recorded %d deliveries, want 5", len(store.delivered))
	}
	// "Oldest first" is inherited from the store's ORDER BY and is
	// load-bearing (a reversal would bury the older normal message behind
	// a newer one); assert the exact sequence Tell was called in, not just
	// how many calls happened.
	wantOrder := []string{"urgent one", "urgent two", "normal one", "normal two", "normal three"}
	if len(voice.tellMsg) != len(wantOrder) {
		t.Fatalf("Tell called with %v, want %v", voice.tellMsg, wantOrder)
	}
	for i, want := range wantOrder {
		if voice.tellMsg[i] != want {
			t.Errorf("Tell[%d] = %q, want %q (order: %v)", i, voice.tellMsg[i], want, voice.tellMsg)
		}
	}
}

func TestAFailedSendIsNotRecordedAsDelivered(t *testing.T) {
	// Otherwise the message is lost: nothing retries it, and the delivery
	// row says the player already has it.
	pending := []Announcement{
		{ID: 1, Body: "hello", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{tellErr: map[string]error{"xuid-1": errors.New("bridge unreachable")}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	delivered, remaining, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
	// remaining is pending minus delivered, not "normal messages never
	// attempted": a message that was attempted and failed is still owed,
	// so it must still count here rather than vanish from the total.
	if remaining != 1 {
		t.Errorf("remaining = %d, want 1 — the one message failed to send and is still queued", remaining)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %d deliveries, want 0 — a failed Tell must not be marked delivered", len(store.delivered))
	}
}

func TestSendNowWritesOneDeliveryRowPerOnlinePlayerForABroadcast(t *testing.T) {
	// The broadcast goes out once, but every player present must be
	// recorded, or they receive it again when they next join.
	a := Announcement{Body: "server restarting soon", TargetKind: TargetEveryone, Delivery: DeliveryBroadcast}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	roster := fakeRoster{online: []string{"xuid-1", "xuid-2", "xuid-3"}}
	d := NewDeliverer(store, voice, roster, fakePermissions{}, testLogger())

	delivered, err := d.SendNow(context.Background(), a, 42)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Players != 3 {
		t.Errorf("delivered.Players = %d, want 3", delivered.Players)
	}
	if len(voice.says) != 1 {
		t.Errorf("Say called %d times, want 1", len(voice.says))
	}
	if len(store.delivered) != 3 {
		t.Fatalf("store recorded %d deliveries, want 3 (one per online player)", len(store.delivered))
	}
	for _, d := range store.delivered {
		if d.id != 42 {
			t.Errorf("delivery recorded for announcement %d, want 42", d.id)
		}
	}
}

func TestSendNowBroadcastStillRecordsOtherRowsWhenOneMarkDeliveredFails(t *testing.T) {
	// The per-row continue after a failed MarkDelivered is the entire
	// substance of "a broadcast writes a row per online player" surviving
	// a partial failure; exercise it with a store that fails exactly one
	// recipient's row.
	a := Announcement{Body: "server restarting soon", TargetKind: TargetEveryone, Delivery: DeliveryBroadcast}
	store := &fakeStore{enabled: true, markErr: map[string]error{"xuid-2": errors.New("db unavailable")}}
	voice := &fakeVoice{}
	roster := fakeRoster{online: []string{"xuid-1", "xuid-2", "xuid-3"}}
	d := NewDeliverer(store, voice, roster, fakePermissions{}, testLogger())

	delivered, err := d.SendNow(context.Background(), a, 42)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Counted {
		t.Errorf("delivered = %+v, want an uncounted reach: xuid-2 heard the Say and their row did not write, so what they got is no longer knowable", delivered)
	}
	if len(voice.says) != 1 {
		t.Errorf("Say called %d times, want 1 — one row failing to record must not trigger a second broadcast", len(voice.says))
	}
	got := map[string]bool{}
	for _, row := range store.delivered {
		got[row.xuid] = true
	}
	if got["xuid-2"] {
		t.Errorf("store.delivered = %+v, want xuid-2 absent since its MarkDelivered failed", store.delivered)
	}
	if !got["xuid-1"] || !got["xuid-3"] {
		t.Errorf("store.delivered = %+v, want xuid-1 and xuid-3 present", store.delivered)
	}
}

func TestSendNowSkipsPlayersWhoseTargetDoesNotIncludeThem(t *testing.T) {
	// A permission-targeted announcement reaches only players resolving to
	// that level.
	a := Announcement{Body: "ops only", TargetKind: TargetPermission, TargetValue: "operator", Delivery: DeliveryWhisper}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	roster := fakeRoster{online: []string{"op-1", "visitor-1"}}
	perms := fakePermissions{levels: map[string]string{"op-1": "operator", "visitor-1": "visitor"}}
	d := NewDeliverer(store, voice, roster, perms, testLogger())

	delivered, err := d.SendNow(context.Background(), a, 7)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Players != 1 {
		t.Fatalf("delivered.Players = %d, want 1", delivered.Players)
	}
	if len(voice.tells) != 1 || voice.tells[0].xuid != "op-1" {
		t.Errorf("tells = %+v, want exactly one Tell to op-1", voice.tells)
	}
	if len(store.delivered) != 1 || store.delivered[0].xuid != "op-1" {
		t.Errorf("store.delivered = %+v, want exactly one row for op-1", store.delivered)
	}
}

func TestSendNowDerivesDeliveryFromTargetNotPersistedField(t *testing.T) {
	// announce.go's DeliveryFor exists precisely so a player-targeted
	// message can never broadcast; SendNow must derive delivery itself
	// rather than trust whatever the row's own Delivery field says. Nothing
	// writes a mismatched Delivery today, but a future source or a bad row
	// PendingFor reads back must not turn into a private message read out
	// to the whole server.
	a := Announcement{
		Body:        "you have received a waypoint",
		TargetKind:  TargetPlayer,
		TargetValue: "xuid-1",
		Delivery:    DeliveryBroadcast, // deliberately disagrees with TargetPlayer
	}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	roster := fakeRoster{online: []string{"xuid-1", "xuid-2"}}
	d := NewDeliverer(store, voice, roster, fakePermissions{}, testLogger())

	delivered, err := d.SendNow(context.Background(), a, 99)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 0 {
		t.Errorf("Say called %d times, want 0 — a player-targeted message must whisper even if its persisted Delivery says broadcast", len(voice.says))
	}
	if delivered.Players != 1 || len(voice.tells) != 1 || voice.tells[0].xuid != "xuid-1" {
		t.Errorf("tells = %+v, delivered.Players = %d, want exactly one Tell to xuid-1", voice.tells, delivered.Players)
	}
	if len(store.delivered) != 1 || store.delivered[0].xuid != "xuid-1" {
		t.Errorf("store.delivered = %+v, want exactly one row for xuid-1", store.delivered)
	}
}

func TestSendNowStopsWhenContextAlreadyCancelled(t *testing.T) {
	// A cancelled context must not turn a multi-recipient send into that
	// many doomed bridge attempts and error lines.
	a := Announcement{Body: "ops only", TargetKind: TargetPermission, TargetValue: "operator", Delivery: DeliveryWhisper}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	roster := fakeRoster{online: []string{"op-1", "op-2"}}
	perms := fakePermissions{levels: map[string]string{"op-1": "operator", "op-2": "operator"}}
	d := NewDeliverer(store, voice, roster, perms, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	delivered, err := d.SendNow(ctx, a, 7)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Players != 0 {
		t.Errorf("delivered.Players = %d, want 0", delivered.Players)
	}
	if len(voice.tells) != 0 {
		t.Errorf("Tell called %d times against a cancelled context, want 0", len(voice.tells))
	}
}

func TestDrainForJoinStopsWhenContextAlreadyCancelled(t *testing.T) {
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	delivered, remaining, err := d.DrainForJoin(ctx, "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
	}
	if remaining != len(pending) {
		t.Errorf("remaining = %d, want %d — nothing was attempted, both are still owed", remaining, len(pending))
	}
	if len(voice.tells) != 0 {
		t.Errorf("Tell called %d times against a cancelled context, want 0", len(voice.tells))
	}
}

func TestDisabledStoreReturnsZeroAndNoError(t *testing.T) {
	// A deployment with no announcements table configured yet must still
	// be able to call every Deliverer method unconditionally.
	store := &fakeStore{enabled: false}
	voice := &fakeVoice{}
	roster := fakeRoster{online: []string{"xuid-1"}}
	d := NewDeliverer(store, voice, roster, fakePermissions{}, testLogger())

	if n, err := d.SendNow(context.Background(), Announcement{TargetKind: TargetEveryone, Delivery: DeliveryBroadcast}, 1); n.Players != 0 || err != nil {
		t.Errorf("SendNow on disabled store = (%+v, %v), want (0, nil)", n, err)
	}
	if delivered, remaining, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now()); delivered != 0 || remaining != 0 || err != nil {
		t.Errorf("DrainForJoin on disabled store = (%d, %d, %v), want (0, 0, nil)", delivered, remaining, err)
	}
	if n, remaining, err := d.DrainAll(context.Background(), "xuid-1", time.Now()); n != 0 || remaining != 0 || err != nil {
		t.Errorf("DrainAll on disabled store = (%d, %d, %v), want (0, 0, nil)", n, remaining, err)
	}
	if len(voice.says) != 0 || len(voice.tells) != 0 {
		t.Errorf("a disabled store must short-circuit before touching Voice at all")
	}
}

// raceStore is a Store whose PendingFor gives a second, genuinely
// concurrent caller a real chance to read the same undelivered snapshot
// before either side records anything -- reproducing the non-atomic
// PendingFor-then-MarkDelivered race a rapid leave-and-rejoin (or a join
// landing alongside that same player's own !inbox) can hit.
//
// The first PendingFor call waits (bounded by rendezvousWindow) for a
// second call to arrive; the second call, on arrival, releases the first
// immediately. Guarded (the fix under test), only one call is ever in
// PendingFor at a time, so the wait simply times out unused and each call
// sees whatever the previous one already marked delivered. Unguarded, both
// calls reach PendingFor back to back, both see the same fully-pending
// snapshot, and both go on to send every message in it.
type raceStore struct {
	mu        sync.Mutex
	items     []Announcement
	delivered map[int64]bool

	calls      int
	rendezvous chan struct{}
}

const rendezvousWindow = 200 * time.Millisecond

func newRaceStore(items []Announcement) *raceStore {
	return &raceStore{items: items, delivered: map[int64]bool{}, rendezvous: make(chan struct{})}
}

var _ Store = (*raceStore)(nil)

func (s *raceStore) Enabled() bool                                       { return true }
func (s *raceStore) Insert(context.Context, Announcement) (int64, error) { return 0, nil }

func (s *raceStore) PendingFor(_ context.Context, _, _ string, _ time.Time) ([]Announcement, error) {
	s.mu.Lock()
	var out []Announcement
	for _, a := range s.items {
		if !s.delivered[a.ID] {
			out = append(out, a)
		}
	}
	s.calls++
	call := s.calls
	s.mu.Unlock()

	switch call {
	case 1:
		select {
		case <-s.rendezvous:
		case <-time.After(rendezvousWindow):
		}
	case 2:
		close(s.rendezvous)
	}
	return out, nil
}

func (s *raceStore) MarkDelivered(_ context.Context, id int64, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered[id] = true
	return nil
}

// countTells reports how many times xuid was told each distinct message
// body -- a value above 1 anywhere means that message was heard twice.
func countTells(v *fakeVoice, xuid string) map[string]int {
	counts := make(map[string]int)
	for i, d := range v.tells {
		if d.xuid == xuid {
			counts[v.tellMsg[i]]++
		}
	}
	return counts
}

func TestConcurrentDrainForJoinCallsForSameXUIDEachSendOnlyOnce(t *testing.T) {
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := newRaceStore(pending)
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if _, _, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now()); err != nil {
				t.Errorf("DrainForJoin: %v", err)
			}
		}()
	}
	wg.Wait()

	counts := countTells(voice, "xuid-1")
	for _, a := range pending {
		if n := counts[a.Body]; n != 1 {
			t.Errorf("xuid-1 was told %q %d times, want exactly 1 -- two concurrent joins must not repeat a message", a.Body, n)
		}
	}
}

func TestConcurrentDrainForJoinAndDrainAllForSameXUIDEachSendOnlyOnce(t *testing.T) {
	// The same race, but between a join drain and that same player's own
	// !inbox -- two different plugins reaching one Deliverer, which is
	// exactly why the guard has to live here rather than in either caller.
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := newRaceStore(pending)
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, _, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now()); err != nil {
			t.Errorf("DrainForJoin: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, _, err := d.DrainAll(context.Background(), "xuid-1", time.Now()); err != nil {
			t.Errorf("DrainAll: %v", err)
		}
	}()
	wg.Wait()

	counts := countTells(voice, "xuid-1")
	for _, a := range pending {
		if n := counts[a.Body]; n != 1 {
			t.Errorf("xuid-1 was told %q %d times, want exactly 1 -- a concurrent join and !inbox must not repeat a message", a.Body, n)
		}
	}
}

// !inbox is answered inside a command dispatch's timeout, and every message
// on it is a bridge round trip. An uncapped drain of a real backlog spends
// that budget mid-delivery and the player gets a partial trickle with no
// reply at all, so the drain stops at MaxPerInbox and says what is left.
func TestDrainAllCapsOneInboxAndReportsTheRest(t *testing.T) {
	var pending []Announcement
	for i := 1; i <= MaxPerInbox+2; i++ {
		pending = append(pending, Announcement{
			ID: int64(i), Body: "message", Priority: PriorityNormal,
			TargetKind: TargetPlayer, Delivery: DeliveryWhisper,
		})
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	delivered, remaining, err := d.DrainAll(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainAll: %v", err)
	}
	if delivered != MaxPerInbox {
		t.Errorf("delivered = %d, want %d", delivered, MaxPerInbox)
	}
	if remaining != 2 {
		t.Errorf("remaining = %d, want 2 -- the player has to be told to ask again", remaining)
	}
	if len(voice.tellMsg) != MaxPerInbox {
		t.Errorf("Tell called %d times, want %d", len(voice.tellMsg), MaxPerInbox)
	}
}

// A backlog that fits is delivered whole, with nothing owed afterwards.
func TestDrainAllUnderTheCapLeavesNothingOwed(t *testing.T) {
	store := &fakeStore{enabled: true, pending: []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}}
	d := NewDeliverer(store, &fakeVoice{}, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	delivered, remaining, err := d.DrainAll(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainAll: %v", err)
	}
	if delivered != 2 || remaining != 0 {
		t.Errorf("DrainAll = (%d, %d), want (2, 0)", delivered, remaining)
	}
}

// fakeJoins reports a fixed "joined this long ago" per xuid, and for anyone
// it wasn't told about -- a player already online when the agent connected --
// only how long ago that connection began. connected is zero unless a test
// sets it, standing for a clock that has seen no connection at all.
type fakeJoins struct {
	since     map[string]time.Duration
	connected time.Duration
}

var _ JoinClock = fakeJoins{}

func (f fakeJoins) SinceJoin(xuid string) (time.Duration, bool) {
	d, ok := f.since[xuid]
	return d, ok
}

func (f fakeJoins) SinceConnect() (time.Duration, bool) {
	if f.connected == 0 {
		return 0, false
	}
	return f.connected, true
}

// A whisper to someone who joined a second ago is accepted by the server and
// rendered by nobody, and recording it would lose the message for good.
func TestSendNowDefersAWhisperToAFreshArrival(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{since: map[string]time.Duration{"fresh": time.Second}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"fresh", "settled"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	delivered, err := d.SendNow(context.Background(), Announcement{Body: "hello", TargetKind: TargetPlayer, TargetValue: "fresh"}, 9)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Players != 0 {
		t.Errorf("delivered.Players = %d, want 0: the joining client cannot render it yet", delivered.Players)
	}
	if len(voice.tells) != 0 {
		t.Errorf("told %v, want nothing sent to a loading client", voice.tells)
	}
	if len(store.delivered) != 0 {
		t.Errorf("recorded %v, want nothing: the row must stay pending for their join drain", store.delivered)
	}
}

// The same announcement reaches a player who has been on for a while.
func TestSendNowStillWhispersASettledPlayer(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{since: map[string]time.Duration{"settled": time.Hour}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"settled"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	delivered, err := d.SendNow(context.Background(), Announcement{Body: "hello", TargetKind: TargetPlayer, TargetValue: "settled"}, 9)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Players != 1 || len(voice.tells) != 1 {
		t.Errorf("delivered.Players = %d, tells = %v, want one of each", delivered.Players, voice.tells)
	}
}

// A broadcast is heard by everyone whose client is up, so it still goes out --
// but the fresh arrival is not recorded as having heard it.
func TestSendNowBroadcastsButDoesNotRecordAFreshArrival(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{since: map[string]time.Duration{"fresh": time.Second}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"fresh", "settled"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	delivered, err := d.SendNow(context.Background(), Announcement{Body: "everyone hears this", TargetKind: TargetEveryone}, 9)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("says = %v, want the broadcast to go out once", voice.says)
	}
	if delivered.Players != 1 {
		t.Errorf("delivered.Players = %d, want 1: the player who just arrived rendered nothing", delivered.Players)
	}
	if len(store.delivered) != 1 {
		t.Errorf("recorded %v, want only the settled player", store.delivered)
	}
	for _, got := range store.delivered {
		if got.xuid == "fresh" {
			t.Errorf("recorded a delivery for the joining player: %+v", store.delivered)
		}
	}
}

// Without the option nothing defers: every existing caller keeps the old
// behaviour, including a Deliverer built with no join clock at all.
func TestSendNowWithoutAJoinClockDefersNothing(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"fresh"}}, fakePermissions{}, testLogger())

	delivered, err := d.SendNow(context.Background(), Announcement{Body: "hello", TargetKind: TargetPlayer, TargetValue: "fresh"}, 9)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if delivered.Players != 1 {
		t.Errorf("delivered.Players = %d, want 1", delivered.Players)
	}
}

// The drain of an arrival the player has already replaced must deliver
// nothing: they crashed on join and came back inside the first drain's wait,
// so when it wakes it is looking at a client that is loading all over again.
// Whispering then would record the backlog against a player who never saw
// it, and the second drain would find nothing left to send.
func TestDrainForJoinDefersToTheDrainOfANewerArrival(t *testing.T) {
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
		{ID: 2, Body: "two", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{}
	// The player joined at T=0 and rejoined at T=5; this first drain fires
	// at T=8, three seconds into the new arrival.
	joins := fakeJoins{since: map[string]time.Duration{"rejoiner": 3 * time.Second}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"rejoiner"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	delivered, remaining, err := d.DrainForJoin(context.Background(), "rejoiner", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0: the client cannot render it yet", delivered)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0: a summary line is as unrenderable as the backlog", remaining)
	}
	if len(voice.tells) != 0 {
		t.Errorf("told %v, want nothing sent to a loading client", voice.tells)
	}
	if len(store.delivered) != 0 {
		t.Errorf("recorded %v, want nothing: the newer arrival's drain owes it", store.delivered)
	}

	// The second drain, a full wait after the rejoin, is the one that pays.
	joins.since["rejoiner"] = 8 * time.Second
	delivered, remaining, err = d.DrainForJoin(context.Background(), "rejoiner", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 2 || remaining != 0 {
		t.Errorf("delivered = %d, remaining = %d, want 2 and 0", delivered, remaining)
	}
	if len(store.delivered) != 2 {
		t.Errorf("store recorded %v, want both announcements", store.delivered)
	}
}

// An agent reconnect must not cancel a drain a real join scheduled. The
// player joined eight seconds ago and their client is long since loaded, but
// the agent dropped and came back four seconds in, taking its record of
// their arrival with it -- all that is left is a connection younger than the
// grace. A guess about who might be loading may withhold a delivery row; it
// may not cancel the drain a real join scheduled, which nothing replaces.
func TestDrainForJoinDeliversAcrossAnAgentReconnect(t *testing.T) {
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer, Delivery: DeliveryWhisper},
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{}
	joins := fakeJoins{connected: 4 * time.Second}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"steve"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	delivered, remaining, err := d.DrainForJoin(context.Background(), "steve", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 1 || remaining != 0 {
		t.Errorf("delivered = %d, remaining = %d, want 1 and 0: the reconnect is not their arrival", delivered, remaining)
	}
	if len(store.delivered) != 1 {
		t.Errorf("store recorded %v, want the one announcement", store.delivered)
	}
}

// A player with no arrival of their own is measured from the connection --
// they may have reconnected moments before the agent did -- so their copy is
// withheld from the books even though the drain would have served them.
func TestSendNowDefersInsideTheConnectWindow(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{connected: 3 * time.Second}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"unknown"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	if _, err := d.SendNow(context.Background(), Announcement{Body: "hello", TargetKind: TargetPlayer, TargetValue: "unknown"}, 9); err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 0 || len(store.delivered) != 0 {
		t.Errorf("told %v and recorded %v, want neither inside the connect window", voice.tells, store.delivered)
	}
}

// A broadcast is one Say heard by every client that is up, so a recipient
// whose bookkeeping was deferred still heard it. Reporting zero here is what
// makes !announce !now tell an operator nobody was online moments after they
// watched their own line go out.
func TestSendNowCountsDeferredBroadcastRecipientsAsReached(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{connected: 3 * time.Second}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"operator", "builder"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	sent, err := d.SendNow(context.Background(), Announcement{Body: "server restarting", TargetKind: TargetOnlineOnly}, 9)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("says = %v, want the broadcast to go out once", voice.says)
	}
	if sent.Players != 2 {
		t.Errorf("sent.Players = %d, want 2: both heard it, only their delivery rows were withheld", sent.Players)
	}
	if len(store.delivered) != 0 {
		t.Errorf("recorded %v, want nothing recorded inside the connect window", store.delivered)
	}
}

// Reached counts hearing, not bookkeeping, and the two deferrals are not the
// same fact. A player whose arrival the agent saw inside the grace demonstrably
// rendered nothing and is owed the text again by their own drain; a player
// withheld only because the agent itself just connected almost certainly heard
// the one Say, and saying otherwise would tell an operator nobody was online
// moments after they watched their own line go out.
func TestSendNowCountsOnlyGuessedDeferralsAsReached(t *testing.T) {
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{
		since:     map[string]time.Duration{"arrival": 2 * time.Second},
		connected: 3 * time.Second,
	}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"arrival", "snapshot"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	sent, err := d.SendNow(context.Background(), Announcement{Body: "server restarting", TargetKind: TargetEveryone}, 9)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("says = %v, want the broadcast to go out once", voice.says)
	}
	if sent.Players != 1 {
		t.Errorf("sent.Players = %d, want 1: the snapshot player heard it, the fresh arrival did not", sent.Players)
	}
	if len(store.delivered) != 0 {
		t.Errorf("recorded %v, want nothing: neither copy may be marked", store.delivered)
	}
}

func TestSendNowBroadcastsWithNobodyOnTheRoster(t *testing.T) {
	// An announcement published while the agent is between connections has
	// no roster to aim at, but the console bridge is a separate process that
	// stays up, so the server can still speak to whoever is on it. Online-
	// only never queues, so suppressing Say here would not delay the message
	// -- it would lose it.
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 7)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 || voice.says[0] != a.Body {
		t.Errorf("Say calls = %v, want exactly one carrying %q — an online-only broadcast has no second chance", voice.says, a.Body)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing: there is no roster snapshot to record from, and a row for a player who may have left loses the message for good", store.delivered)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — it was said, and the roster could name nobody to count", sent)
	}
}

func TestSendNowStaysSilentForAWhisperWithNobodyOnTheRoster(t *testing.T) {
	// The other half of the asymmetry: a whisper needs an XUID to go to, so
	// an empty roster leaves it with nothing to send. It stays pending for
	// the player's own join instead.
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 8)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 0 || len(voice.says) != 0 {
		t.Errorf("Tell = %v and Say = %v, want both empty — a private message must not be broadcast just because its recipient is unreachable", voice.tells, voice.says)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing", store.delivered)
	}
	if sent.Players != 0 || !sent.Counted {
		t.Errorf("sent = %+v, want a counted zero — nothing was said, so nobody heard it, and that is an answer", sent)
	}
}

// fakeLeadership is a Leadership whose answer the test fixes.
type fakeLeadership struct{ live bool }

var _ Leadership = fakeLeadership{}

func (l fakeLeadership) Live() bool { return l.live }

func TestSendNowStaysSilentOnAStandby(t *testing.T) {
	// A standby's roster is empty for its whole life, which from the roster
	// alone is indistinguishable from the live agent's reconnect gap. The
	// announcement API is mounted for the process, so a publish can land
	// here; the console bridge this process holds is up regardless, so Say
	// would be heard by every player on the server the other process is
	// playing on.
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 11)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 0 {
		t.Errorf("Say calls = %v, want none — a process that is in no game must not speak into the one the leader is playing", voice.says)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing", store.delivered)
	}
	if sent.Players != 0 || !sent.Counted {
		t.Errorf("sent = %+v, want a counted zero — nothing was said, so nobody heard it, and that is an answer", sent)
	}
}

func TestSendNowBroadcastsFromTheLeaderInTheGap(t *testing.T) {
	// The same empty roster, from the process that holds the lock: it is
	// between connections, the bridge is still up, and online-only never
	// queues, so suppressing this would lose the message rather than delay
	// it. Nothing is recorded either way — there is no roster to record from.
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	sent, err := d.SendNow(context.Background(), a, 12)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 || voice.says[0] != a.Body {
		t.Errorf("Say calls = %v, want exactly one carrying %q", voice.says, a.Body)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing", store.delivered)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — it was said, and the roster could name nobody to count", sent)
	}
}

func TestSendNowWhispersOnAStandbyIsAlreadyNothing(t *testing.T) {
	// Leadership gates only the broadcast a roster cannot back. A whisper on
	// a standby was already silent, because it has nobody to whisper to, and
	// must stay pending rather than be recorded.
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	if _, err := d.SendNow(context.Background(), a, 13); err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 0 || len(voice.says) != 0 {
		t.Errorf("Tell = %v and Say = %v, want both empty", voice.tells, voice.says)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing", store.delivered)
	}
}

// partedRoster names everyone a send was aimed at while reporting only some
// of them as still online: a player who quit between the roster naming them
// and their turn in the send loop that follows.
type partedRoster struct {
	named []string
	still map[string]bool
	// forgot is the connection dying while the send was in flight: the
	// roster stops knowing anyone at all, not merely these players.
	forgot bool
}

var _ Roster = partedRoster{}

func (r partedRoster) Online() []string          { return r.named }
func (r partedRoster) IsOnline(xuid string) bool { return !r.forgot && r.still[xuid] }
func (r partedRoster) Knows() bool               { return !r.forgot }

// A drain is scheduled by an arrival and fires seconds later. A player who
// quits inside that wait is gone, but the console accepts a tellraw that
// matches nobody and reports success — so whispering their backlog anyway
// would record every message of it and lose the lot, which is the permanent
// loss this package exists to avoid.
func TestDrainForJoinSendsNothingToAPlayerWhoLeftBeforeItFired(t *testing.T) {
	pending := []Announcement{
		{ID: 1, Body: "one", Priority: PriorityNormal, TargetKind: TargetPlayer},
		{ID: 2, Body: "two", Priority: PriorityExpedited, TargetKind: TargetPlayer},
	}
	store := &fakeStore{enabled: true, pending: pending}
	voice := &fakeVoice{}
	// Nobody on the roster: the player this drain belongs to has quit.
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

	delivered, remaining, err := d.DrainForJoin(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if len(voice.tells) != 0 {
		t.Errorf("whispered %v to a player who is not on the server", voice.tells)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing — a recorded delivery is never retried", store.delivered)
	}
	if delivered != 0 || remaining != len(pending) {
		t.Errorf("DrainForJoin = (%d, %d), want (0, %d) — the whole backlog is still owed", delivered, remaining, len(pending))
	}
}

func TestDrainAllSendsNothingToAPlayerWhoLeft(t *testing.T) {
	// The !inbox path reaches the same sender, and a player can quit between
	// typing it and the store answering.
	store := &fakeStore{enabled: true, pending: []Announcement{{ID: 1, Body: "one", TargetKind: TargetPlayer}}}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

	delivered, remaining, err := d.DrainAll(context.Background(), "xuid-1", time.Now())
	if err != nil {
		t.Fatalf("DrainAll: %v", err)
	}
	if len(voice.tells) != 0 || len(store.delivered) != 0 {
		t.Errorf("tells = %v, rows = %v, want both empty", voice.tells, store.delivered)
	}
	if delivered != 0 || remaining != 1 {
		t.Errorf("DrainAll = (%d, %d), want (0, 1)", delivered, remaining)
	}
}

func TestSendNowSkipsAWhisperRecipientWhoLeftMidLoop(t *testing.T) {
	// recipients() reads the roster once. A permission-targeted send then
	// whispers one player at a time, and the last of them may have quit
	// while the first was still being told.
	a := Announcement{Body: "the nether hub is open", TargetKind: TargetPermission, TargetValue: "operator"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		partedRoster{named: []string{"op-1", "op-2"}, still: map[string]bool{"op-1": true}},
		fakePermissions{levels: map[string]string{"op-1": "operator", "op-2": "operator"}},
		testLogger())

	sent, err := d.SendNow(context.Background(), a, 21)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Players != 1 || len(voice.tells) != 1 || voice.tells[0].xuid != "op-1" {
		t.Errorf("sent.Players = %d, tells = %v; want just op-1, who was still there", sent.Players, voice.tells)
	}
	if len(store.delivered) != 1 || store.delivered[0].xuid != "op-1" {
		t.Errorf("rows = %v, want one for op-1 — op-2 left, so what they are owed must survive", store.delivered)
	}
}

func TestSendNowStaysSilentOnAnIdleConnectedServer(t *testing.T) {
	// A roster that has been told who is here and names nobody is right:
	// there is genuinely nobody to hear it, and a console line sent anyway
	// is noise counted as a broadcast.
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{knows: true}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	sent, err := d.SendNow(context.Background(), a, 22)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 0 {
		t.Errorf("Say calls = %v, want none on an idle server the agent is connected to", voice.says)
	}
	if sent.Players != 0 || len(store.delivered) != 0 {
		t.Errorf("sent.Players = %d, rows = %v, want nothing", sent.Players, store.delivered)
	}
}

func TestSendNowBroadcastsWhenTheSameRosterMeansAGap(t *testing.T) {
	// The identical empty roster, from a leader whose roster has not been
	// told who is here: the console bridge is a separate process and still
	// reaches whoever is on the server, and online-only never queues, so
	// this is its one chance.
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	if _, err := d.SendNow(context.Background(), a, 23); err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 || voice.says[0] != a.Body {
		t.Errorf("Say calls = %v, want exactly one carrying %q", voice.says, a.Body)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing — there is no roster snapshot to record from", store.delivered)
	}
}

// Say is one bridge round-trip, bounded by the operator's bridge timeout, so
// a player can quit while it is in flight. The recording loop runs against
// the roster snapshot taken before it, and a row for someone who has gone
// permanently suppresses the redelivery their next join would otherwise pay
// -- the same loss the whisper loop refuses.
func TestSendNowDoesNotRecordABroadcastForAPlayerWhoLeftDuringTheSay(t *testing.T) {
	a := Announcement{Body: "the nether hub is open", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		partedRoster{named: []string{"stayed", "left"}, still: map[string]bool{"stayed": true}},
		fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 31)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("Say calls = %v, want exactly one — everyone on the server hears a broadcast", voice.says)
	}
	if len(store.delivered) != 1 || store.delivered[0].xuid != "stayed" {
		t.Errorf("rows = %v, want one for stayed — a row for a player who quit loses the message for good", store.delivered)
	}
	if sent.Players != 1 || !sent.Counted {
		t.Errorf("sent = %+v, want one counted player", sent)
	}
}

// Say is one bridge round-trip, and the Bedrock connection can die inside
// it: connectionEnded empties the roster, so every recipient chosen a moment
// earlier now reads as departed and nothing is recorded. The announcement
// still went out -- the console bridge is a separate process -- so reporting
// a counted zero would say the opposite of what happened. A caller that
// retries on zero, which the API documents as safe, would then publish again
// into a pod that is now in the gap and broadcast the same line twice.
func TestSendNowReportsAnUncountedBroadcastWhenTheConnectionDiesDuringTheSay(t *testing.T) {
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	roster := &forgettingRoster{named: []string{"steve"}}
	d := NewDeliverer(store, voice, roster, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 41)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("Say calls = %v, want exactly one — it was broadcast before the connection died", voice.says)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — a counted zero reads as \"nobody heard it\" and invites a second broadcast", sent)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing — there is no roster left to record from", store.delivered)
	}
}

// forgettingRoster names its players until the send begins, then answers as a
// roster whose connection has died: it knows nothing and nobody.
type forgettingRoster struct {
	named []string
	dead  bool
}

var _ Roster = (*forgettingRoster)(nil)

func (r *forgettingRoster) Online() []string {
	names := r.named
	// The Say that follows this read is when the connection drops.
	r.dead = true
	return names
}

func (r *forgettingRoster) IsOnline(string) bool { return false }
func (r *forgettingRoster) Knows() bool          { return !r.dead }

// The connection surviving the Say is the ordinary case, and it must still
// report a real count: everyone who left is skipped, and what is left is an
// answer rather than an absence of one.
func TestSendNowStillCountsWhenTheRosterOutlivesTheSay(t *testing.T) {
	a := Announcement{Body: "the nether hub is open", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		partedRoster{named: []string{"stayed", "left"}, still: map[string]bool{"stayed": true}},
		fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 42)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if !sent.Counted || sent.Players != 1 {
		t.Errorf("sent = %+v, want one counted player — the roster is still watching, so this is a real answer", sent)
	}
}

// Once a Say has gone out the line is in chat, and everything after it is
// bookkeeping. A row that will not write leaves that recipient neither
// delivered nor honestly excluded, so the count stops being an answer --
// reporting zero would say nobody heard a line the whole server just read,
// and the API documents zero as safe to publish again.
func TestSendNowDoesNotCountABroadcastWhoseOnlyRowFailed(t *testing.T) {
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true, markErr: map[string]error{"steve": errors.New("db unavailable")}}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"steve"}}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 51)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("Say calls = %v, want exactly one — Steve read it before the row failed", voice.says)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — a counted zero invites a second broadcast of a line already said", sent)
	}
}

// The same rule for a loop cut short: the recipients it never reached heard
// the Say like everyone else, and nothing decided what they got.
func TestSendNowDoesNotCountABroadcastWhoseRecordingWasCancelled(t *testing.T) {
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancelled by the Say itself, so the recording loop that follows it is
	// the part that is cut short.
	voice.onSay = cancel
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"steve", "alex"}}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(ctx, a, 52)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("Say calls = %v, want exactly one — it went out before the cancel took hold", voice.says)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — nobody decided what the recipients left in the list received", sent)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing once the context was gone", store.delivered)
	}
}

// A standby says nothing because it is in no game, but the row it stored is
// the live agent's to deliver. Reporting that identically to a watched,
// empty server would have a caller retry a publish already waiting in the
// queue, and the next player to join would be whispered it twice.
func TestSendNowReportsAStandbysPublishAsQueued(t *testing.T) {
	a := Announcement{Body: "deploying v2", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 53)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 0 {
		t.Errorf("Say calls = %v, want none from a process that is in no game", voice.says)
	}
	if sent.Outcome != OutcomeQueued {
		t.Errorf("sent = %+v, want it reported as queued for the live agent", sent)
	}
	if !sent.Counted || sent.Players != 0 {
		t.Errorf("sent = %+v, want a counted zero beside it — nothing was spoken here", sent)
	}
}

// Queued is a promise somebody owes. Online-only queues for nobody --
// PendingFor excludes it -- so a standby reporting one would tell a caller
// the live agent will deliver a row nothing will ever pick up. The zero is
// the honest answer: nothing was spoken here, and nothing will be.
func TestSendNowDoesNotReportAnOnlineOnlyPublishOffTheLeaderAsQueued(t *testing.T) {
	a := Announcement{Body: "restarting in five", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 55)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 0 {
		t.Errorf("Say calls = %v, want none from a process that is in no game", voice.says)
	}
	if sent.Outcome == OutcomeQueued {
		t.Errorf("sent = %+v, want it not reported as queued: an online-only row is delivered by nobody", sent)
	}
	if !sent.Counted || sent.Players != 0 {
		t.Errorf("sent = %+v, want a counted zero", sent)
	}
}

// The live agent watching an empty server is the zero this one must stay
// distinct from: nothing queued on anyone else's behalf, and nobody heard it
// because nobody was there.
func TestSendNowDoesNotReportAnIdleServerAsQueued(t *testing.T) {
	a := Announcement{Body: "deploying v2", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{knows: true}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	sent, err := d.SendNow(context.Background(), a, 54)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Outcome == OutcomeQueued {
		t.Errorf("sent = %+v, want it not reported as queued: this process is the one that speaks", sent)
	}
	if !sent.Counted || sent.Players != 0 {
		t.Errorf("sent = %+v, want a counted zero — the roster is watching and names nobody", sent)
	}
}

// A player can quit inside the bridge round-trip the Say really is. They
// read the line before they went, and nobody else was on to be counted, so
// there is nothing left to name -- and the zero that would otherwise be
// returned is the one value the API tells a caller is safe to publish again.
func TestSendNowDoesNotCountABroadcastWhoseOnlyRecipientLeftDuringTheSay(t *testing.T) {
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		partedRoster{named: []string{"steve"}, still: map[string]bool{}},
		fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 61)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("Say calls = %v, want exactly one — Steve read it before he quit", voice.says)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — a counted zero says the line was never spoken", sent)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing for a player who has left", store.delivered)
	}
}

// The other way a spoken broadcast can name nobody: everyone online arrived
// moments ago, so each is demonstrably still loading and none of them is
// counted as having heard it. The Say still went out to whatever clients are
// up, so the result is unknown rather than zero.
func TestSendNowDoesNotCountABroadcastHeardOnlyByJustArrivedPlayers(t *testing.T) {
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	joins := fakeJoins{since: map[string]time.Duration{"fresh": time.Second, "alsofresh": 2 * time.Second}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"fresh", "alsofresh"}}, fakePermissions{}, testLogger(),
		WithFreshJoinGrace(joins, 7*time.Second))

	sent, err := d.SendNow(context.Background(), a, 62)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Fatalf("Say calls = %v, want exactly one", voice.says)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — it was said, and nobody on the roster can be counted as having seen it", sent)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing: a just-arrived client rendered none of it", store.delivered)
	}
}

// A standby accepts a whisper-target publish and stores it, exactly as it
// does a broadcast one, so it must report the same queued result. A caller
// reading a bare zero here would take it for the live agent's empty server,
// retry, and have the player whispered the same line twice on their join.
func TestSendNowReportsAStandbysWhisperPublishAsQueued(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 63)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 0 || len(voice.says) != 0 {
		t.Errorf("Tell = %v and Say = %v, want both empty from a process in no game", voice.tells, voice.says)
	}
	if sent.Outcome != OutcomeQueued {
		t.Errorf("sent = %+v, want it reported as queued for the live agent, the same as a broadcast target", sent)
	}
}

// The live agent's own whisper to nobody stays a plain zero: nothing is
// waiting on another process, and the caller is looking at a real answer.
func TestSendNowDoesNotReportTheLiveAgentsWhisperToNobodyAsQueued(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{knows: true}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	sent, err := d.SendNow(context.Background(), a, 64)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Outcome == OutcomeQueued {
		t.Errorf("sent = %+v, want it not reported as queued: this process is the one that speaks", sent)
	}
	if !sent.Counted || sent.Players != 0 {
		t.Errorf("sent = %+v, want a counted zero", sent)
	}
}

// The whisper half of the rule the broadcast branch already holds to: the
// player has read the line, and the row that would have said so did not
// write. Reporting zero says nothing was sent, which the API documents as
// safe to publish again -- so the caller retries, the player is whispered
// the same line a second time, and the still-pending original makes it a
// third on their next join.
func TestSendNowDoesNotCountAWhisperWhoseOnlyRowFailed(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true, markErr: map[string]error{"xuid-1": errors.New("db unavailable")}}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 71)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 1 {
		t.Fatalf("Tell calls = %v, want exactly one — the player read it before the row failed", voice.tells)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — a counted zero says the whisper was never sent", sent)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing: the row is what failed, so it stays pending", store.delivered)
	}
}

// A whisper nobody was told is a real zero: the Tell failed, nothing reached
// anyone, and publishing again repeats nothing in chat.
func TestSendNowStillCountsAWhisperNobodyWasTold(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{tellErr: map[string]error{"xuid-1": errors.New("bridge unreachable")}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 72)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if !sent.Counted || sent.Players != 0 {
		t.Errorf("sent = %+v, want a counted zero — the send failed, so nothing is in chat", sent)
	}
}

// One row failing among several is not a count short by one: op-2 read the
// whisper, so a reported 1 of 2 understates what the server saw while giving
// the caller no sign anything is missing. The same rule the broadcast loop
// holds to -- a recipient sent to and not recorded is not accounted for.
func TestSendNowDoesNotCountWhispersWhenOneRowFailed(t *testing.T) {
	a := Announcement{Body: "the nether hub is open", TargetKind: TargetPermission, TargetValue: "operator"}
	store := &fakeStore{enabled: true, markErr: map[string]error{"op-2": errors.New("db unavailable")}}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		fakeRoster{online: []string{"op-1", "op-2"}},
		fakePermissions{levels: map[string]string{"op-1": "operator", "op-2": "operator"}},
		testLogger())

	sent, err := d.SendNow(context.Background(), a, 73)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 2 {
		t.Fatalf("Tell calls = %v, want both operators told", voice.tells)
	}
	if sent.Counted {
		t.Errorf("sent = %+v, want an uncounted reach — op-2 read it and no row says so", sent)
	}
	// op-1's row still wrote, so they are not whispered it again.
	if len(store.delivered) != 1 || store.delivered[0].xuid != "op-1" {
		t.Errorf("rows = %v, want one for op-1", store.delivered)
	}
}

// Every row writing is still a real count: nothing is unaccounted for, and
// the caller gets a number it can act on.
func TestSendNowCountsWhispersWhenEveryRowWrote(t *testing.T) {
	a := Announcement{Body: "the nether hub is open", TargetKind: TargetPermission, TargetValue: "operator"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		fakeRoster{online: []string{"op-1", "op-2"}},
		fakePermissions{levels: map[string]string{"op-1": "operator", "op-2": "operator"}},
		testLogger())

	sent, err := d.SendNow(context.Background(), a, 74)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if !sent.Counted || sent.Players != 2 {
		t.Errorf("sent = %+v, want two counted players", sent)
	}
}

// A leader that has just lost the lock keeps its roster for as long as the
// connect loop takes to unwind, so the empty-roster guard never fires for it.
// Its console bridge is up like any other, and the server it would speak into
// now belongs to whoever took the lock -- so the role, not the roster, has to
// be what decides whether it speaks.
func TestSendNowDoesNotBroadcastFromADemotedLeaderThatStillNamesPlayers(t *testing.T) {
	a := Announcement{Body: "deploying v2", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		fakeRoster{online: []string{"steve", "alex"}},
		fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 81)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 0 {
		t.Errorf("Say calls = %v, want none — this process no longer holds the lock", voice.says)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing: it said nothing", store.delivered)
	}
	if sent.Outcome != OutcomeQueued {
		t.Errorf("sent = %+v, want it reported as queued for whoever holds the lock", sent)
	}
}

// The same populated roster on the process that does hold the lock still
// broadcasts and still records, so the guard is the role and not the players.
func TestSendNowStillBroadcastsFromTheLeaderWithPlayersOnTheRoster(t *testing.T) {
	a := Announcement{Body: "deploying v2", TargetKind: TargetEveryone}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		fakeRoster{online: []string{"steve", "alex"}},
		fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	sent, err := d.SendNow(context.Background(), a, 82)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.says) != 1 {
		t.Errorf("Say calls = %v, want exactly one", voice.says)
	}
	if !sent.Counted || sent.Players != 2 {
		t.Errorf("sent = %+v, want two counted players", sent)
	}
}

// The whisper twin of the broadcast guard above. A demoted leader still names
// the players its roster learned, and a Tell is addressed off that roster
// rather than off the role -- so without the same guard, the process on its
// way out of the game whispers into the one the new leader now owns. The row
// it would write is the real damage: a delivery nothing retries, marking a
// whisper as read by a player who never saw it.
func TestSendNowDoesNotWhisperFromADemotedLeaderThatStillNamesPlayers(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "steve"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		fakeRoster{online: []string{"steve", "alex"}},
		fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 83)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 0 {
		t.Errorf("Tell calls = %v, want none — this process no longer holds the lock", voice.tells)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing: nothing was whispered", store.delivered)
	}
	if sent.Outcome != OutcomeQueued {
		t.Errorf("sent = %+v, want it queued for whoever holds the lock", sent)
	}
}

// The same roster on the process that does hold the lock still whispers and
// still records, so the guard is the role and not the players.
func TestSendNowStillWhispersFromTheLeaderWithPlayersOnTheRoster(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "steve"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice,
		fakeRoster{online: []string{"steve", "alex"}},
		fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: true}))

	sent, err := d.SendNow(context.Background(), a, 84)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if len(voice.tells) != 1 || voice.tells[0].xuid != "steve" {
		t.Errorf("tells = %+v, want exactly one Tell to steve", voice.tells)
	}
	if !sent.Counted || sent.Players != 1 {
		t.Errorf("sent = %+v, want one counted player", sent)
	}
}

// A Say that never went out is not an empty server, and the two are the
// same zero. An operator warning of a restart against a bridge rejection, a
// 5xx or a timeout is told the message was handled, and the server is never
// warned at all.
func TestSendNowReportsABroadcastThatWasNeverSaid(t *testing.T) {
	a := Announcement{Body: "server restarting in 5 minutes", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{sayErr: errors.New("bridge rejected the command")}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 90)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Outcome != OutcomeFailed {
		t.Errorf("sent = %+v, want a failed outcome — nothing was said, which a bare zero reads as an empty server", sent)
	}
	if len(store.delivered) != 0 {
		t.Errorf("store recorded %v, want nothing — the line never reached the server", store.delivered)
	}
}

// The same distinction for a whisper: every Tell failed, so nobody read it,
// and that is not the same fact as nobody having been there to read it.
func TestSendNowReportsAWhisperThatWasNeverTold(t *testing.T) {
	a := Announcement{Body: "your waypoint is at 100 64 -200", TargetKind: TargetPlayer, TargetValue: "xuid-1"}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{tellErr: map[string]error{"xuid-1": errors.New("bridge unreachable")}}
	d := NewDeliverer(store, voice, fakeRoster{online: []string{"xuid-1"}}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 91)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Outcome != OutcomeFailed {
		t.Errorf("sent = %+v, want a failed outcome — the whisper was attempted and did not go out", sent)
	}
}

// Nothing spoken and nobody owed it: PendingFor excludes online-only, so
// the row this standby stored is picked up by nothing. Reported as the zero
// a watched server gives, the operator is told nobody was on to hear a
// countdown that was never said.
func TestSendNowReportsAnOnlineOnlyPublishOffTheLeaderAsUnsaid(t *testing.T) {
	a := Announcement{Body: "restarting in five", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger(),
		WithLeadership(fakeLeadership{live: false}))

	sent, err := d.SendNow(context.Background(), a, 92)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Outcome != OutcomeFailed {
		t.Errorf("sent = %+v, want a failed outcome — nothing was said here and nothing will say it", sent)
	}
}

// The empty server the two above must stay distinct from: the roster is
// watching, it names nobody, and the line was rightly never spoken.
func TestSendNowReportsAWatchedEmptyServerAsSilent(t *testing.T) {
	a := Announcement{Body: "restarting in five", TargetKind: TargetOnlineOnly}
	store := &fakeStore{enabled: true}
	voice := &fakeVoice{}
	d := NewDeliverer(store, voice, fakeRoster{knows: true}, fakePermissions{}, testLogger())

	sent, err := d.SendNow(context.Background(), a, 93)
	if err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if sent.Outcome != OutcomeSilent {
		t.Errorf("sent = %+v, want a silent outcome — nobody was there to hear it", sent)
	}
}
