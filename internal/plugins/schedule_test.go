package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

type fakeScheduleStore struct {
	enabled     bool
	added       []announce.Schedule
	list        []announce.Schedule
	deactivated []int64
	active      map[int64]bool
	err         error
}

var _ plugin.ScheduleStore = (*fakeScheduleStore)(nil)

func (f *fakeScheduleStore) AddSchedule(_ context.Context, s announce.Schedule) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.added = append(f.added, s)
	return int64(len(f.added)), nil
}
func (f *fakeScheduleStore) ListSchedules(context.Context) ([]announce.Schedule, error) {
	return f.list, f.err
}
func (f *fakeScheduleStore) DeactivateSchedule(_ context.Context, id int64) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.deactivated = append(f.deactivated, id)
	return f.active[id], nil
}
func (f *fakeScheduleStore) Enabled() bool { return f.enabled }

func runScheduleAs(t *testing.T, store *fakeScheduleStore, perm plugin.Permission, args ...string) string {
	t.Helper()
	var cmd plugin.Command
	for _, c := range NewSchedule().Commands() {
		if c.Name == "schedule" {
			cmd = c
		}
	}
	reply, err := cmd.Run(context.Background(), &plugin.Context{Schedules: store}, plugin.Invocation{
		ActorXUID: "op", ActorPermission: perm, Args: args,
	})
	if err != nil {
		t.Fatalf("Run(%v): %v", args, err)
	}
	return reply
}

func TestScheduleAddParsesCadences(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		want        announce.Cadence
		wantRefusal string // substring; "" means stored
	}{
		{"daily", []string{"daily", "18:30", "vote"}, announce.Cadence{Daily: true, At: 18*time.Hour + 30*time.Minute}, ""},
		{"daily single-digit hour", []string{"daily", "9:05", "vote"}, announce.Cadence{Daily: true, At: 9*time.Hour + 5*time.Minute}, ""},
		{"daily midnight", []string{"DAILY", "00:00", "vote"}, announce.Cadence{Daily: true}, ""},
		{"every minutes at the floor", []string{"every", "15m", "vote"}, announce.Cadence{Every: 15 * time.Minute}, ""},
		{"every hours", []string{"every", "2H", "vote"}, announce.Cadence{Every: 2 * time.Hour}, ""},
		{"every under the floor", []string{"every", "14m", "vote"}, announce.Cadence{}, "at most every 15m"},
		{"every zero", []string{"every", "0h", "vote"}, announce.Cadence{}, "at most every 15m"},
		{"every past the ceiling", []string{"every", "169h", "vote"}, announce.Cadence{}, "at least every 168h"},
		{"every too large to multiply", []string{"every", "99999999999999999999h", "vote"}, announce.Cadence{}, "at least every"},
		{"every in seconds", []string{"every", "900s", "vote"}, announce.Cadence{}, "minutes or hours"},
		{"every without a unit", []string{"every", "30", "vote"}, announce.Cadence{}, "minutes or hours"},
		{"daily out of range", []string{"daily", "24:00", "vote"}, announce.Cadence{}, "HH:MM"},
		{"daily with seconds", []string{"daily", "18:00:00", "vote"}, announce.Cadence{}, "HH:MM"},
		{"unknown cadence", []string{"weekly", "mon", "vote"}, announce.Cadence{}, "Usage"},
		{"no message", []string{"every", "1h"}, announce.Cadence{}, "Usage"},
		{"flags but no message", []string{"every", "1h", "!urgent"}, announce.Cadence{}, "Usage"},
		{"a player target is refused", []string{"every", "1h", "@Steve", "hi"}, announce.Cadence{}, "one player"},
		{"over-long body", []string{"every", "1h", strings.Repeat("a", announce.MaxBodyChars+1)}, announce.Cadence{}, "at most"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeScheduleStore{enabled: true}
			reply := runScheduleAs(t, store, plugin.PermissionOperator, append([]string{"add"}, tc.args...)...)
			if tc.wantRefusal != "" {
				if len(store.added) != 0 {
					t.Fatalf("stored %+v, want it refused", store.added)
				}
				if !strings.Contains(reply, tc.wantRefusal) {
					t.Errorf("reply %q should contain %q", reply, tc.wantRefusal)
				}
				return
			}
			if len(store.added) != 1 {
				t.Fatalf("stored %d schedules (reply %q), want 1", len(store.added), reply)
			}
			if got := store.added[0].Cadence; got != tc.want {
				t.Errorf("cadence = %+v, want %+v", got, tc.want)
			}
			if !strings.Contains(reply, "UTC") {
				t.Errorf("reply %q should say the time is UTC", reply)
			}
		})
	}
}

// The flags are !announce's, parsed by the same helper: before the body
// only, and the same words inside it are text.
func TestScheduleAddReusesTheAnnounceFlags(t *testing.T) {
	store := &fakeScheduleStore{enabled: true}
	runScheduleAs(t, store, plugin.PermissionOperator, "add", "every", "1h", "!now", "!urgent", "restart", "!now", "soon")
	s := store.added[0]
	if s.TargetKind != announce.TargetOnlineOnly || s.Priority != announce.PriorityExpedited {
		t.Errorf("schedule = %s/%s, want online_only/expedited", s.TargetKind, s.Priority)
	}
	if s.Body != "restart !now soon" {
		t.Errorf("body = %q, want the in-body flag kept as text", s.Body)
	}

	runScheduleAs(t, store, plugin.PermissionOperator, "add", "daily", "08:00", "good", "morning")
	if s := store.added[1]; s.TargetKind != announce.TargetEveryone || s.Priority != announce.PriorityNormal {
		t.Errorf("schedule = %s/%s, want everyone/normal", s.TargetKind, s.Priority)
	}
}

func TestScheduleAddFirstFiresAtTheNextOccurrence(t *testing.T) {
	store := &fakeScheduleStore{enabled: true}
	before := time.Now()
	runScheduleAs(t, store, plugin.PermissionOperator, "add", "every", "2h", "vote")
	next := store.added[0].NextFireAt
	if d := next.Sub(before); d < 2*time.Hour-time.Second || d > 2*time.Hour+time.Minute {
		t.Errorf("first fire %v out, want one interval", d)
	}
}

func TestScheduleFromTheConsoleHasNoAuthor(t *testing.T) {
	store := &fakeScheduleStore{enabled: true}
	cmd := NewSchedule().Commands()[0]
	if _, err := cmd.Run(context.Background(), &plugin.Context{Schedules: store}, plugin.Invocation{
		ActorXUID: chat.ServerOrigin, ActorPermission: plugin.PermissionOperator,
		Args: []string{"add", "every", "1h", "vote"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := store.added[0].AuthorXUID; got != "" {
		t.Errorf("author = %q, want empty so the foreign key column is NULL", got)
	}
}

func TestScheduleIsOperatorOnly(t *testing.T) {
	store := &fakeScheduleStore{enabled: true}
	reply := runScheduleAs(t, store, plugin.PermissionMember, "add", "every", "1h", "vote")
	if len(store.added) != 0 || !strings.Contains(reply, "operator") {
		t.Errorf("a member got (%q, %d stored), want a refusal", reply, len(store.added))
	}
	if cmd := NewSchedule().Commands()[0]; cmd.Permission != plugin.PermissionOperator {
		t.Errorf("registry floor = %v, want operator so !help hides it from players", cmd.Permission)
	}
}

func TestScheduleListShowsUTCAndCaps(t *testing.T) {
	next := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	var list []announce.Schedule
	for i := 1; i <= maxListed+2; i++ {
		list = append(list, announce.Schedule{
			ID: int64(i), Body: strings.Repeat("long body ", 10), TargetKind: announce.TargetEveryone,
			Cadence: announce.Cadence{Daily: true, At: 18 * time.Hour}, NextFireAt: next,
		})
	}
	reply := runScheduleAs(t, &fakeScheduleStore{enabled: true, list: list}, plugin.PermissionOperator, "list")
	for _, want := range []string{"#1 daily 18:00 UTC", "next Sep 10 18:00 UTC", "and 2 more", "..."} {
		if !strings.Contains(reply, want) {
			t.Errorf("list %q should contain %q", reply, want)
		}
	}
	if strings.Contains(reply, fmt.Sprintf("#%d ", maxListed+1)) {
		t.Errorf("list %q shows more than %d schedules", reply, maxListed)
	}

	if reply := runScheduleAs(t, &fakeScheduleStore{enabled: true}, plugin.PermissionOperator, "list"); reply != "No schedules are set." {
		t.Errorf("empty list = %q", reply)
	}
}

func TestScheduleDel(t *testing.T) {
	store := &fakeScheduleStore{enabled: true, active: map[int64]bool{3: true}}
	if reply := runScheduleAs(t, store, plugin.PermissionOperator, "del", "3"); !strings.Contains(reply, "#3 stopped") {
		t.Errorf("reply = %q, want it stopped", reply)
	}
	if reply := runScheduleAs(t, store, plugin.PermissionOperator, "del", "#4"); !strings.Contains(reply, "no active schedule #4") {
		t.Errorf("reply = %q, want a plain not-found", reply)
	}
	for _, bad := range [][]string{{"del"}, {"del", "x"}, {"del", "0"}, {"del", "1", "2"}} {
		if reply := runScheduleAs(t, store, plugin.PermissionOperator, bad...); !strings.Contains(reply, "by its number") {
			t.Errorf("%v: reply = %q, want the syntax", bad, reply)
		}
	}
	if len(store.deactivated) != 2 {
		t.Errorf("deactivated %v, want only the two well-formed ids", store.deactivated)
	}
}

func TestScheduleDegradesWithoutAUsableStore(t *testing.T) {
	cases := []struct {
		name  string
		store *fakeScheduleStore
		want  string
	}{
		{"nil store", nil, noScheduleStore},
		{"disabled", &fakeScheduleStore{}, noScheduleStore},
		{"not migrated", &fakeScheduleStore{enabled: true, err: fmt.Errorf("x: %w", &pgconn.PgError{Code: "42P01"})}, noScheduleStore},
		{"not granted", &fakeScheduleStore{enabled: true, err: fmt.Errorf("x: %w", &pgconn.PgError{Code: "42501"})}, noScheduleAccess},
	}
	for _, tc := range cases {
		for _, args := range [][]string{{"add", "every", "1h", "vote"}, {"list"}, {"del", "1"}} {
			var pctx plugin.Context
			if tc.store != nil {
				pctx.Schedules = tc.store
			}
			reply, err := NewSchedule().Commands()[0].Run(context.Background(), &pctx, plugin.Invocation{
				ActorXUID: "op", ActorPermission: plugin.PermissionOperator, Args: args,
			})
			if err != nil {
				t.Fatalf("%s %v: %v", tc.name, args, err)
			}
			if reply != tc.want {
				t.Errorf("%s %v: reply = %q, want %q", tc.name, args, reply, tc.want)
			}
		}
	}

	// Any other failure is a failure, not a deploy state.
	store := &fakeScheduleStore{enabled: true, err: errors.New("connection refused")}
	if _, err := NewSchedule().Commands()[0].Run(context.Background(), &plugin.Context{Schedules: store}, plugin.Invocation{
		ActorXUID: "op", ActorPermission: plugin.PermissionOperator, Args: []string{"list"},
	}); err == nil {
		t.Error("a broken store was reported as unconfigured")
	}
}
