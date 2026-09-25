package presence

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

func parkReq(version int64) Request {
	return Request{State: presenceapi.StateParked, Reason: "chunk budget", Version: version}
}

func TestSetWritesAuditsAndNotifies(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	notified := 0
	svc.OnChange(func() { notified++ })

	p, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops"))
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if p.Effective != presenceapi.StateParked || p.Override.Version != 1 || p.Override.SetBy != "api:ops" || !p.Override.SetAt.Equal(t0) {
		t.Errorf("presence = %+v", p)
	}
	if notified != 1 {
		t.Errorf("notified %d times, want once", notified)
	}
	recs := aud.all()
	if len(recs) != 1 {
		t.Fatalf("audited %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.Command != "presence" || r.XUID != "api:ops" || r.Permission != "api" ||
		r.Args != `actor=afk-bot-1 from=present to=parked cause=set source=api:ops reason="chunk budget"` {
		t.Errorf("audit record = %+v", r)
	}
}

func TestSetRefusesAStaleVersionWithTheCurrentRow(t *testing.T) {
	clock := t0
	svc, aud := newTestService(t, newFakeStore(), &clock)
	if _, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops")); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops"))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	if conflict.Current.Override == nil || conflict.Current.Override.Version != 1 || conflict.Current.ActorID != "afk-bot-1" {
		t.Errorf("current = %+v, want the version 1 row", conflict.Current)
	}
	if _, err := svc.Set(t.Context(), "afk-bot-1", Request{State: presenceapi.StatePresent, Reason: "r", Version: 1}, APISource("ops")); err != nil {
		t.Errorf("Set at the current version: %v", err)
	}
	if n := len(aud.all()); n != 2 {
		t.Errorf("audited %d records, want 2: a refused write is not a change", n)
	}
}

func TestSetValidates(t *testing.T) {
	clock := t0
	svc, _ := newTestService(t, newFakeStore(), &clock)
	past, future := t0.Add(-time.Minute), t0.Add(time.Hour)
	cases := map[string]Request{
		"unknown state":       {State: "gone", Reason: "r"},
		"no reason":           {State: presenceapi.StateParked, Reason: "  "},
		"long reason":         {State: presenceapi.StateParked, Reason: strings.Repeat("x", maxReasonChars+1)},
		"expiry in the past":  {State: presenceapi.StateParked, Reason: "r", Until: &past},
		"wake on present":     {State: presenceapi.StatePresent, Reason: "r", WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true}},
		"wake on both":        {State: presenceapi.StateParked, Reason: "r", Until: &future, WakeOn: &presenceapi.WakeOn{AnyPlayerJoin: true, Players: []string{"Steve"}}},
		"wake on nothing":     {State: presenceapi.StateParked, Reason: "r", WakeOn: &presenceapi.WakeOn{}},
		"wake on blank names": {State: presenceapi.StateParked, Reason: "r", WakeOn: &presenceapi.WakeOn{Players: []string{" "}}},
	}
	for name, req := range cases {
		if _, err := svc.Set(t.Context(), "afk-bot-1", req, APISource("ops")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := svc.Set(t.Context(), "afk-bot-9", parkReq(0), APISource("ops")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown actor: err = %v, want ErrNotFound", err)
	}
}

func TestStoreFailuresAreUnavailable(t *testing.T) {
	clock := t0
	store := newFakeStore()
	store.fail(errors.New("connection refused"))
	svc, _ := newTestService(t, store, &clock)
	if _, err := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Set: %v, want ErrUnavailable", err)
	}
	if _, err := svc.List(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("List: %v, want ErrUnavailable", err)
	}
	if _, err := svc.Presence(t.Context(), "agent"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Presence: %v, want ErrUnavailable", err)
	}
}

func TestSetGroupWritesEveryMember(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	got, err := svc.SetGroup(t.Context(), "bots", Request{State: presenceapi.StateParked, Reason: "r", Version: 99}, APISource("ops"))
	if err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if len(got) != 2 || got[0].ActorID != "afk-bot-1" || got[1].ActorID != "afk-bot-2" {
		t.Errorf("SetGroup = %+v, want both bots in configuration order", got)
	}
	if len(aud.all()) != 2 {
		t.Errorf("audited %d, want one per member", len(aud.all()))
	}
	if _, err := svc.SetGroup(t.Context(), "miners", parkReq(0), APISource("ops")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown group: %v, want ErrNotFound", err)
	}
}

func TestClearFallsBackToTheDefault(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	if _, err := svc.Set(t.Context(), "afk-bot-2", Request{State: presenceapi.StatePresent, Reason: "r"}, APISource("ops")); err != nil {
		t.Fatal(err)
	}
	bots, _ := svc.Registry().Group("bots")
	got, err := svc.Clear(t.Context(), bots, APISource("ops"))
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got[1].Effective != presenceapi.StateParked || got[1].Override != nil {
		t.Errorf("afk-bot-2 after clear = %+v, want its parked default", got[1])
	}
	recs := aud.all()
	if len(recs) != 2 || !strings.Contains(recs[1].Args, "cause=cleared") || !strings.Contains(recs[1].Args, "to=parked") {
		t.Errorf("audit = %+v, want one set and one clear, and no record for the bot that had nothing to clear", recs)
	}
}

func TestExpireRemovesOnlyTheVersionItRead(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, aud := newTestService(t, store, &clock)
	p, _ := svc.Set(t.Context(), "afk-bot-1", parkReq(0), APISource("ops"))

	stale := Removal{ActorID: "afk-bot-1", Cause: CauseExpired, Override: *p.Override}
	stale.Override.Version = 7
	if ok, err := svc.Expire(t.Context(), stale); ok || err != nil {
		t.Errorf("Expire at a stale version = %v, %v; want nothing removed", ok, err)
	}
	ok, err := svc.Expire(t.Context(), Removal{ActorID: "afk-bot-1", Cause: CauseWoken, Override: *p.Override})
	if !ok || err != nil {
		t.Fatalf("Expire = %v, %v; want removed", ok, err)
	}
	last := aud.all()[len(aud.all())-1]
	if last.XUID != "presence-loop" || !strings.Contains(last.Args, "cause=woken") || !strings.Contains(last.Args, "from=parked to=present") {
		t.Errorf("removal audit = %+v", last)
	}
}

// Staleness is judged against the agent's clock, so a bot's own clock never
// decides whether it looks connected.
func TestReportStatusStampsTheAgentsClock(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, _ := newTestService(t, store, &clock)
	err := svc.ReportStatus(t.Context(), "afk-bot-1", presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0.Add(-time.Hour), ProcessVersion: "1.4.0"})
	if err != nil {
		t.Fatalf("ReportStatus: %v", err)
	}
	if st := store.status["afk-bot-1"]; !st.LastSeen.Equal(t0) {
		t.Errorf("last_seen = %v, want the agent's %v", st.LastSeen, t0)
	}
	if err := svc.ReportStatus(t.Context(), "afk-bot-9", presenceapi.Status{ObservedState: presenceapi.StatePresent}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown actor: %v", err)
	}
	if err := svc.ReportStatus(t.Context(), "afk-bot-1", presenceapi.Status{ObservedState: "gone"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad state: %v", err)
	}
}

func TestListIncludesStatusAndNonNilGroups(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, _ := newTestService(t, store, &clock)
	store.status["afk-bot-1"] = presenceapi.Status{Connected: true, ObservedState: presenceapi.StatePresent, LastSeen: t0}
	views, err := svc.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(views) != 3 || views[0].Groups == nil || views[1].Status == nil || views[2].Status != nil {
		t.Errorf("views = %+v", views)
	}
}

// The store binds set_at verbatim, so a write the service forgot to stamp
// would land as year 1 and make every wake look like it came after the park.
func TestEveryWriteStampsSetAtFromTheClock(t *testing.T) {
	clock := t0
	store := newFakeStore()
	svc, _ := newTestService(t, store, &clock)
	if _, err := svc.Set(t.Context(), "agent", parkReq(0), APISource("ops")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	clock = t0.Add(time.Minute)
	if _, err := svc.SetGroup(t.Context(), "bots", parkReq(0), APISource("ops")); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	want := map[string]time.Time{"agent": t0, "afk-bot-1": clock, "afk-bot-2": clock}
	for id, at := range want {
		ov, ok := store.row(id)
		if !ok || ov.SetAt.IsZero() || !ov.SetAt.Equal(at) {
			t.Errorf("%s set_at = %v, want %v", id, ov.SetAt, at)
		}
	}
	clock = t0.Add(2 * time.Minute)
	bots, _ := svc.Registry().Group("bots")
	if _, err := svc.SetEach(t.Context(), bots, func(Actor) Request { return parkReq(0) }, APISource("ops")); err != nil {
		t.Fatalf("SetEach: %v", err)
	}
	for _, id := range []string{"afk-bot-1", "afk-bot-2"} {
		if ov, _ := store.row(id); !ov.SetAt.Equal(clock) {
			t.Errorf("%s set_at after SetEach = %v, want %v", id, ov.SetAt, clock)
		}
	}
}
