package plugins

import (
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
)

var now = time.Date(2026, 9, 8, 3, 0, 0, 0, time.UTC)

// The greeting the agent gave through Stages 1-4. Losing Postgres must cost
// personalisation, never the greeting itself.
func TestGreetingFallsBackWhenTheStoreIsDisabled(t *testing.T) {
	got := greeting("Steve", store.Profile{}, false, now)
	if got != "Welcome, Steve!" {
		t.Errorf("greeting = %q, want the plain Stage 1-4 wording", got)
	}
}

// The bug this design exists to prevent: RecordJoin returns the profile as it
// stood *before* the arrival, so a first-timer is never told "welcome back".
func TestGreetingTreatsAFirstArrivalAsNew(t *testing.T) {
	got := greeting("Steve", store.Profile{JoinCount: 1}, true, now)
	if !strings.Contains(got, "First time here") {
		t.Errorf("greeting = %q, want a first-visit greeting", got)
	}
	if strings.Contains(strings.ToLower(got), "back") {
		t.Errorf("greeting = %q, greeted a first-time player as returning", got)
	}
}

// A player with a join count but no recorded first_seen is still new: the
// zero time means nothing was ever stored about them.
func TestGreetingTreatsAnUnknownProfileAsNew(t *testing.T) {
	got := greeting("Steve", store.Profile{JoinCount: 5}, true, now)
	if !strings.Contains(got, "First time here") {
		t.Errorf("greeting = %q, want new-player wording when first_seen is zero", got)
	}
}

func TestGreetingVariesByAbsence(t *testing.T) {
	base := store.Profile{JoinCount: 12, FirstSeen: now.Add(-200 * 24 * time.Hour)}
	cases := []struct {
		name     string
		lastSeen time.Time
		want     string
	}{
		{"same day", now.Add(-2 * time.Hour), "Welcome back, Steve!"},
		{"a week", now.Add(-8 * 24 * time.Hour), "Long time no see - 8 days"},
		{"a month", now.Add(-45 * 24 * time.Hour), "It has been 45 days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.LastSeen = tc.lastSeen
			got := greeting("Steve", p, true, now)
			if !strings.Contains(got, tc.want) {
				t.Errorf("greeting = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestGreetingCallsOutARegular(t *testing.T) {
	p := store.Profile{JoinCount: 100, FirstSeen: now.Add(-365 * 24 * time.Hour), LastSeen: now.Add(-time.Hour)}
	got := greeting("Steve", p, true, now)
	if !strings.Contains(got, "visit number 100") {
		t.Errorf("greeting = %q, want the milestone called out", got)
	}
}

// Absence wins over the milestone: someone back after a month cares more about
// that than about their visit count.
func TestAbsenceOutranksTheMilestone(t *testing.T) {
	p := store.Profile{JoinCount: 150, FirstSeen: now.Add(-365 * 24 * time.Hour), LastSeen: now.Add(-60 * 24 * time.Hour)}
	got := greeting("Steve", p, true, now)
	if !strings.Contains(got, "It has been 60 days") {
		t.Errorf("greeting = %q, want the absence greeting to win", got)
	}
}

func TestGreetingHandlesAMissingName(t *testing.T) {
	if got := greeting("", store.Profile{}, false, now); !strings.Contains(got, "a new player") {
		t.Errorf("greeting = %q, want a placeholder for an unknown name", got)
	}
}
