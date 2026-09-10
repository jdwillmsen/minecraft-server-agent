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
	if n, err := d.DrainAll(context.Background(), "xuid-1", time.Now()); n != 0 || err != nil {
		t.Errorf("DrainAll on disabled store = (%d, %v), want (0, nil)", n, err)
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
		if _, err := d.DrainAll(context.Background(), "xuid-1", time.Now()); err != nil {
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
