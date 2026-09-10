package announce

import (
	"strings"
	"testing"
	"time"
)

func utc(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCadenceNext(t *testing.T) {
	daily1830 := Cadence{Daily: true, At: 18*time.Hour + 30*time.Minute}
	hourly := Cadence{Every: time.Hour}
	cases := []struct {
		name string
		c    Cadence
		due  string // "" for a new schedule
		now  string
		want string
	}{
		{"daily, new, before today's time", daily1830, "", "2026-09-10 12:00", "2026-09-10 18:30"},
		{"daily, new, after today's time", daily1830, "", "2026-09-10 19:00", "2026-09-11 18:30"},
		{"daily, exactly at the time moves on a day", daily1830, "2026-09-10 18:30", "2026-09-10 18:30", "2026-09-11 18:30"},
		// Three days down: one fire for the slot that was due, then straight
		// to the next one still ahead -- never three replays.
		{"daily, three days behind", daily1830, "2026-09-07 18:30", "2026-09-10 20:00", "2026-09-11 18:30"},
		{"every, new, starts one interval out", hourly, "", "2026-09-10 12:07", "2026-09-10 13:07"},
		{"every, on time", hourly, "2026-09-10 12:00", "2026-09-10 12:00", "2026-09-10 13:00"},
		{"every, a minute late keeps its phase", hourly, "2026-09-10 12:00", "2026-09-10 12:01", "2026-09-10 13:00"},
		{"every, five hours behind keeps its phase", hourly, "2026-09-10 07:00", "2026-09-10 12:30", "2026-09-10 13:00"},
		{"every, behind by exactly whole intervals", hourly, "2026-09-10 07:00", "2026-09-10 12:00", "2026-09-10 13:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var due time.Time
			if tc.due != "" {
				due = utc(tc.due)
			}
			now := utc(tc.now)
			got := tc.c.Next(due, now)
			if !got.Equal(utc(tc.want)) {
				t.Errorf("Next = %s, want %s", got.Format("2006-01-02 15:04"), tc.want)
			}
			if !got.After(now) {
				t.Errorf("Next = %s is not strictly after now %s", got, now)
			}
		})
	}
}

func TestCadenceValidate(t *testing.T) {
	cases := []struct {
		name    string
		c       Cadence
		wantErr string
	}{
		{"the floor itself", Cadence{Every: MinEvery}, ""},
		{"a minute under the floor", Cadence{Every: MinEvery - time.Minute}, "at most every 15m"},
		{"zero", Cadence{}, "at most every 15m"},
		{"the ceiling itself", Cadence{Every: MaxEvery}, ""},
		{"past the ceiling", Cadence{Every: MaxEvery + time.Hour}, "at least every 168h"},
		{"daily midnight", Cadence{Daily: true}, ""},
		{"daily out of range", Cadence{Daily: true, At: 24 * time.Hour}, "between 00:00 and 23:59"},
		{"both at once", Cadence{Daily: true, Every: time.Hour}, "not both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestCadenceStringSaysUTC(t *testing.T) {
	cases := map[string]Cadence{
		"daily 09:05 UTC": {Daily: true, At: 9*time.Hour + 5*time.Minute},
		"every 2h":        {Every: 2 * time.Hour},
		"every 90m":       {Every: 90 * time.Minute},
	}
	for want, c := range cases {
		if got := c.String(); got != want {
			t.Errorf("String = %q, want %q", got, want)
		}
	}
}

// A fired reminder expires at the next occurrence, so a missed one is
// superseded rather than stacked -- and online_only never queues at all.
func TestScheduleAnnouncementExpiresAtTheNextOccurrence(t *testing.T) {
	now := utc("2026-09-10 12:00")
	next := utc("2026-09-10 13:00")
	s := Schedule{ID: 7, Body: "vote", AuthorXUID: "op", TargetKind: TargetEveryone, Priority: PriorityExpedited}

	a := s.Announcement(now, next)
	if a.Source != SourceSchedule || a.ScheduleID != 7 || a.Priority != PriorityExpedited {
		t.Errorf("announcement = %+v, want schedule-sourced, id 7, expedited", a)
	}
	if a.ExpiresAt == nil || !a.ExpiresAt.Equal(next) {
		t.Errorf("expiry = %v, want the next occurrence %v", a.ExpiresAt, next)
	}
	if a.AuthorXUID != "" {
		t.Errorf("author = %q, want none: the schedule row carries the person", a.AuthorXUID)
	}

	s.TargetKind = TargetOnlineOnly
	if a := s.Announcement(now, next); a.ExpiresAt != nil || a.Delivery != DeliveryBroadcast {
		t.Errorf("online_only announcement = %+v, want a broadcast with no expiry", a)
	}
}
