package audit

import (
	"context"
	"testing"
	"time"
)

func TestNopAcceptsEverythingAndPersistsNothing(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Fatal("Nop reports itself enabled")
	}
	err := s.Write(context.Background(), Record{
		XUID: "x", Gamertag: "g", Permission: "visitor",
		Command: "ping", Outcome: OutcomeOK, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Write on Nop must not error: %v", err)
	}
}

func TestOutcomesMatchTheSchemaCheckConstraint(t *testing.T) {
	// The migration constrains this column. A constant that drifts from it
	// fails at the first write of that kind, in production, on the one code
	// path nobody exercises by hand.
	want := map[Outcome]bool{
		"ok": true, "denied": true, "unknown": true,
		"error": true, "rate_limited": true, "timeout": true,
	}
	for _, got := range []Outcome{
		OutcomeOK, OutcomeDenied, OutcomeUnknown,
		OutcomeError, OutcomeRateLimited, OutcomeTimeout,
	} {
		if !want[got] {
			t.Errorf("outcome %q is not one the schema allows", got)
		}
		delete(want, got)
	}
	if len(want) != 0 {
		t.Errorf("schema allows outcomes with no constant: %v", want)
	}
}
