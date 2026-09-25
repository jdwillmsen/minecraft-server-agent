//go:build livedb

package presence

import (
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// One leader parks the agent for a while and a bot indefinitely, then goes
// away. A second process, sharing nothing with the first but the database,
// takes over after the expiry: it must remove what expired, keep what did
// not, and decide its own gate from the stored rows alone.
func TestOverridesAndExpiriesSurviveALeaderHandover(t *testing.T) {
	ctx := t.Context()
	actors := []Actor{
		{ID: "zz-test-agent", Gamertag: "ZzAgent", Kind: KindAgent, Default: presenceapi.StatePresent},
		{ID: "zz-test-a", Gamertag: "ZzBot", Kind: KindAFKBot, Default: presenceapi.StatePresent},
	}
	reg, err := NewRegistry(actors, "zz-test-agent")
	if err != nil {
		t.Fatal(err)
	}
	quiet := logging.New("error")
	// Both pools are opened before anything is written: livePool clears the
	// test rows as it opens, and would otherwise erase the first leader's.
	firstStore, secondStore := NewPostgres(livePool(t)), NewPostgres(livePool(t))

	first := NewService(reg, firstStore, &fakeAudit{}, quiet)
	until := time.Now().Add(time.Minute)
	if _, err := first.Set(ctx, "zz-test-agent", Request{State: presenceapi.StateParked, Until: &until, Reason: "handover"}, APISource("test")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Set(ctx, "zz-test-a", Request{State: presenceapi.StateParked, Reason: "handover"}, APISource("test")); err != nil {
		t.Fatal(err)
	}

	second := NewService(reg, secondStore, &fakeAudit{}, quiet)
	second.now = func() time.Time { return until.Add(time.Second) }
	gate := NewGate(false)
	NewLoop(LoopConfig{Service: second, Console: &fakeConsole{}, Joins: NewJoinLog(), Gate: gate, SessionUp: func() bool { return false }, Log: quiet}).Prime(ctx)

	if p, _ := gate.Wanted(); !p {
		t.Error("the new leader kept the agent parked past its stored expiry")
	}
	rows, err := secondStore.Overrides(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows["zz-test-agent"]; ok {
		t.Error("the expired agent override survived the handover")
	}
	if rows["zz-test-a"].State != presenceapi.StateParked {
		t.Error("the open-ended bot override was lost in the handover")
	}
}
