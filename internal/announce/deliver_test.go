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
	defer v.mu.Unlock()
	v.says = append(v.says, message)
	return v.sayErr
}

// fakeRoster reports a fixed set of online XUIDs.
type fakeRoster struct{ online []string }

var _ Roster = fakeRoster{}

func (r fakeRoster) Online() []string { return r.online }

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
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

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
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

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
	if delivered != 3 {
		t.Errorf("delivered = %d, want 3", delivered)
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
	if delivered != 2 {
		t.Errorf("delivered = %d, want 2 (xuid-2's row failed, xuid-1 and xuid-3 still land)", delivered)
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
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
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
	if delivered != 1 || len(voice.tells) != 1 || voice.tells[0].xuid != "xuid-1" {
		t.Errorf("tells = %+v, delivered = %d, want exactly one Tell to xuid-1", voice.tells, delivered)
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
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0", delivered)
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
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

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

	if n, err := d.SendNow(context.Background(), Announcement{TargetKind: TargetEveryone, Delivery: DeliveryBroadcast}, 1); n != 0 || err != nil {
		t.Errorf("SendNow on disabled store = (%d, %v), want (0, nil)", n, err)
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
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

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
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

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
	d := NewDeliverer(store, voice, fakeRoster{}, fakePermissions{}, testLogger())

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
	d := NewDeliverer(store, &fakeVoice{}, fakeRoster{}, fakePermissions{}, testLogger())

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
	if delivered != 0 {
		t.Errorf("delivered = %d, want 0: the joining client cannot render it yet", delivered)
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
	if delivered != 1 || len(voice.tells) != 1 {
		t.Errorf("delivered = %d, tells = %v, want one of each", delivered, voice.tells)
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
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1: the player who just arrived rendered nothing", delivered)
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
	if delivered != 1 {
		t.Errorf("delivered = %d, want 1", delivered)
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
	if sent != 2 {
		t.Errorf("sent = %d, want 2: both heard it, only their delivery rows were withheld", sent)
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
	if sent != 1 {
		t.Errorf("sent = %d, want 1: the snapshot player heard it, the fresh arrival did not", sent)
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
	if sent != 0 {
		t.Errorf("sent = %d, want 0 — it was said, but nobody is known to have heard it", sent)
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
	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
}
