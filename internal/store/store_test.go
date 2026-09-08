package store

import (
	"context"
	"testing"
	"time"
)

var at = time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)

// A player the server has never seen has no first_seen, whatever else the
// row says. Reading JoinCount alone would greet a stranger as a regular the
// first time a row is half-written.
func TestProfileNew(t *testing.T) {
	cases := map[string]struct {
		profile Profile
		want    bool
	}{
		"empty":                   {Profile{}, true},
		"first arrival":           {Profile{JoinCount: 1, FirstSeen: at}, true},
		"second arrival":          {Profile{JoinCount: 2, FirstSeen: at}, false},
		"count but no first seen": {Profile{JoinCount: 9}, true},
	}
	for name, tc := range cases {
		if got := tc.profile.New(); got != tc.want {
			t.Errorf("%s: New() = %v, want %v", name, got, tc.want)
		}
	}
}

// AwayFor must be zero rather than "56 years" for a player with no recorded
// previous visit -- the zero time is absence of data, not a date.
func TestAwayForIsZeroWithoutAPreviousVisit(t *testing.T) {
	if got := (Profile{}).AwayFor(at); got != 0 {
		t.Errorf("AwayFor = %v, want 0 for an unseen player", got)
	}
}

func TestAwayForMeasuresFromThepreviousVisit(t *testing.T) {
	p := Profile{LastSeen: at.Add(-48 * time.Hour)}
	if got := p.AwayFor(at); got != 48*time.Hour {
		t.Errorf("AwayFor = %v, want 48h", got)
	}
}

// The Nop store is what runs when no database is configured, which was the
// agent's entire existence through Stages 1-4. Every method must be safe.
func TestNopStoreIsSafeAndInert(t *testing.T) {
	var s Store = Nop{}
	if s.Enabled() {
		t.Error("Nop reports itself enabled")
	}
	profile, err := s.RecordJoin(context.Background(), "xuid", "Steve", at)
	if err != nil {
		t.Errorf("RecordJoin: %v", err)
	}
	if !profile.New() {
		t.Error("Nop returned a profile that is not new; greetings would claim knowledge it does not have")
	}
	if err := s.RecordLeave(context.Background(), "xuid", at); err != nil {
		t.Errorf("RecordLeave: %v", err)
	}
	if n, err := s.CloseOrphans(context.Background(), at); err != nil || n != 0 {
		t.Errorf("CloseOrphans = (%d, %v), want (0, nil)", n, err)
	}
	s.Close()
}
