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
// marked, rather than just how many.
type fakeStore struct {
	mu         sync.Mutex
	enabled    bool
	pending    []Announcement
	pendingErr error
	markErr    error
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
	if s.markErr != nil {
		return s.markErr
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
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0 (the one message was attempted, just failed)", remaining)
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
