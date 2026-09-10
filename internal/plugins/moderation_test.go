package plugins

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

const (
	modPlayer = "2535400000000001"
	modOther  = "2535400000000002"
	shouting  = "EVERYONE COME TO MY BASE RIGHT NOW"
)

var modEpoch = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// modStore is a concurrency-safe moderation store: the plugin writes from its
// worker while the test reads.
type modStore struct {
	mu          sync.Mutex
	disabled    bool
	events      []moderation.Event
	recordErr   error
	recent      []moderation.Event
	recentErr   error
	recentCalls int
	recentXUID  string
	recentLimit int
	// block, when set, holds every Record until it is closed or the
	// caller's context ends -- a database that has stopped answering.
	block chan struct{}
}

var _ plugin.ModerationStore = (*modStore)(nil)

func (s *modStore) Record(ctx context.Context, e moderation.Event) error {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	s.events = append(s.events, e)
	return nil
}

func (s *modStore) Recent(_ context.Context, xuid string, limit int) ([]moderation.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recentCalls++
	s.recentXUID, s.recentLimit = xuid, limit
	if len(s.recent) > limit {
		return s.recent[:limit], s.recentErr
	}
	return s.recent, s.recentErr
}

func (s *modStore) Enabled() bool { return !s.disabled }

func (s *modStore) recorded() []moderation.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]moderation.Event(nil), s.events...)
}

type modVoice struct {
	mu    sync.Mutex
	tells []string
	err   error
}

func (v *modVoice) Tell(_ context.Context, xuid, message string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tells = append(v.tells, xuid+": "+message)
	return v.err
}

func (v *modVoice) Say(context.Context, string) error { return nil }

func (v *modVoice) told() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.tells...)
}

type modAnnouncements struct {
	mu       sync.Mutex
	inserted []announce.Announcement
}

func (a *modAnnouncements) Insert(_ context.Context, ann announce.Announcement) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inserted = append(a.inserted, ann)
	return int64(len(a.inserted)), nil
}

func (a *modAnnouncements) Enabled() bool { return true }

func (a *modAnnouncements) all() []announce.Announcement {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]announce.Announcement(nil), a.inserted...)
}

type modDeliverer struct {
	mu   sync.Mutex
	sent []int64
}

func (d *modDeliverer) SendNow(_ context.Context, _ announce.Announcement, id int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, id)
	return 1, nil
}

func (d *modDeliverer) DrainAll(context.Context, string, time.Time) (int, int, error) {
	return 0, 0, nil
}

func (d *modDeliverer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

// modClock is the plugin's time, moved by the test.
type modClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *modClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *modClock) set(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = modEpoch.Add(d)
}

type modRig struct {
	m             *Moderation
	pctx          *plugin.Context
	store         *modStore
	voice         *modVoice
	announcements *modAnnouncements
	deliverer     *modDeliverer
	clock         *modClock
}

func newModRig(t *testing.T, terms ...string) *modRig {
	t.Helper()
	m, err := NewModeration(t.Context(), terms, logging.New("error"))
	if err != nil {
		t.Fatalf("NewModeration: %v", err)
	}
	r := &modRig{
		m: m, store: &modStore{}, voice: &modVoice{},
		announcements: &modAnnouncements{}, deliverer: &modDeliverer{},
		clock: &modClock{at: modEpoch},
	}
	m.now = r.clock.now
	r.pctx = &plugin.Context{Voice: r.voice, Moderation: r.store, Announcements: r.announcements, Deliverer: r.deliverer}
	return r
}

func (r *modRig) say(t *testing.T, xuid, message string) {
	t.Helper()
	r.sayOn(t, r.pctx, chat.MessageEvent{ActorXUID: xuid, Gamertag: "Steve", Message: message, Public: true})
}

func (r *modRig) sayOn(t *testing.T, pctx *plugin.Context, ev chat.MessageEvent) {
	t.Helper()
	if err := r.m.HandleEvent(context.Background(), pctx, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
}

// waitRecorded waits for the worker to have written n events. The worker
// takes jobs in order, so once it has, everything queued before is done.
func (r *modRig) waitRecorded(t *testing.T, n int) []moderation.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := r.store.recorded()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("recorded %d events, want %d: %+v", len(got), n, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestModerationWarnsAndRecordsATerm(t *testing.T) {
	r := newModRig(t, "griefer")
	r.say(t, modPlayer, "you absolute GRIEFER")

	got := r.waitRecorded(t, 1)
	want := moderation.Event{XUID: modPlayer, Gamertag: "Steve", Message: "you absolute GRIEFER",
		Rule: moderation.RuleTerm, Detail: "griefer", Action: moderation.ActionWarned, OccurredAt: modEpoch}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
	if told := r.voice.told(); len(told) != 1 || told[0] != modPlayer+": "+moderationWarning {
		t.Errorf("whispers = %v, want one warning to the player", told)
	}
}

func TestModerationWarnsAtMostOncePerMinute(t *testing.T) {
	r := newModRig(t, "griefer")
	r.say(t, modPlayer, "griefer")
	r.clock.set(30 * time.Second)
	r.say(t, modPlayer, "griefer again")
	r.clock.set(time.Minute)
	r.say(t, modPlayer, "griefer thrice")

	got := r.waitRecorded(t, 3)
	actions := []moderation.Action{got[0].Action, got[1].Action, got[2].Action}
	want := []moderation.Action{moderation.ActionWarned, moderation.ActionLogged, moderation.ActionWarned}
	for i := range want {
		if actions[i] != want[i] {
			t.Errorf("actions = %v, want %v", actions, want)
			break
		}
	}
	if n := len(r.voice.told()); n != 2 {
		t.Errorf("%d warnings whispered, want 2", n)
	}
}

// The record says what the player was told, not what was attempted.
func TestModerationRecordsAFailedWarningAsLogged(t *testing.T) {
	r := newModRig(t, "griefer")
	r.voice.err = errors.New("bridge down")
	r.say(t, modPlayer, "griefer")

	if got := r.waitRecorded(t, 1); got[0].Action != moderation.ActionLogged {
		t.Errorf("action = %q, want logged: the warning never arrived", got[0].Action)
	}
}

func TestModerationLogsCapsWithoutWarning(t *testing.T) {
	r := newModRig(t)
	r.say(t, modPlayer, shouting)

	got := r.waitRecorded(t, 1)
	if got[0].Rule != moderation.RuleCaps || got[0].Action != moderation.ActionLogged || got[0].Detail != "100% of 28 letters" {
		t.Errorf("recorded %+v, want a logged caps flag", got[0])
	}
	if told := r.voice.told(); len(told) != 0 {
		t.Errorf("caps whispered %v; only a term warns", told)
	}
}

// Commands and @server questions were typed into public chat like anything
// else, so they are moderated like anything else.
func TestModerationChecksCommandsAndMentions(t *testing.T) {
	r := newModRig(t, "griefer")
	for i := 0; i < 5; i++ {
		r.say(t, modPlayer, "!ping")
	}
	r.say(t, modPlayer, "!ping")
	r.say(t, modOther, "!announce griefer lives here")
	r.say(t, modOther, "@server WHY IS EVERYONE SO ANNOYING TODAY")

	got := r.waitRecorded(t, 3)
	if len(got) != 3 {
		t.Fatalf("recorded %+v, want three flags", got)
	}
	if got[0].Rule != moderation.RuleFlood || got[0].Detail != "6 messages in 10s" {
		t.Errorf("first flag = %+v, want the sixth !ping as a flood", got[0])
	}
	if got[1].Rule != moderation.RuleTerm || got[2].Rule != moderation.RuleCaps {
		t.Errorf("flags = %+v, want the command's term and the mention's caps", got)
	}
}

// One message can fail several rules, and each is its own row.
func TestModerationRecordsEveryRuleAMessageFails(t *testing.T) {
	r := newModRig(t, "griefer")
	r.say(t, modPlayer, "YOU ARE A GRIEFER AND EVERYONE KNOWS IT")

	got := r.waitRecorded(t, 2)
	if len(got) != 2 || got[0].Rule != moderation.RuleTerm || got[1].Rule != moderation.RuleCaps {
		t.Errorf("recorded %+v, want a term and a caps flag", got)
	}
}

func TestModerationIgnoresWhispersAndTheConsole(t *testing.T) {
	r := newModRig(t, "griefer")
	r.sayOn(t, r.pctx, chat.MessageEvent{ActorXUID: modPlayer, Message: "griefer " + shouting, Public: false})
	r.sayOn(t, r.pctx, chat.MessageEvent{ActorXUID: chat.ServerOrigin, Message: "griefer " + shouting, Public: true})
	r.say(t, modOther, "griefer")

	// Ignored events are never queued, so once the later one is recorded
	// nothing from the earlier two can still arrive.
	if got := r.waitRecorded(t, 1); len(got) != 1 || got[0].XUID != modOther {
		t.Errorf("recorded %+v, want only the public player message", got)
	}
}

// With no database there is nothing to audit into, and a warning with no
// record behind it is enforcement nobody can review.
func TestModerationDoesNothingWithoutAStore(t *testing.T) {
	r := newModRig(t, "griefer")
	for _, disabled := range []*plugin.Context{
		{Voice: r.voice, Moderation: moderation.Nop{}},
		{Voice: r.voice},
		nil,
	} {
		r.sayOn(t, disabled, chat.MessageEvent{ActorXUID: modPlayer, Message: "griefer", Public: true})
	}
	r.say(t, modOther, "griefer")

	r.waitRecorded(t, 1)
	if told := r.voice.told(); len(told) != 1 || !strings.HasPrefix(told[0], modOther) {
		t.Errorf("whispers = %v, want only the enabled context's warning", told)
	}
	if r.m.tracker.Len() != 1 {
		t.Errorf("tracker holds %d players; a disabled context must not grow it", r.m.tracker.Len())
	}
}

func TestModerationNotifiesOperatorsAtMostOncePerTenMinutes(t *testing.T) {
	r := newModRig(t, "griefer")
	r.say(t, modPlayer, "griefer")
	r.waitRecorded(t, 1)
	r.clock.set(9 * time.Minute)
	r.say(t, modPlayer, shouting)
	r.say(t, modOther, "griefer")
	r.waitRecorded(t, 3)
	r.clock.set(10 * time.Minute)
	r.say(t, modPlayer, shouting)
	r.waitRecorded(t, 4)
	// A notice follows its record, so the fourth record landing does not
	// mean the third notice has.
	deadline := time.Now().Add(5 * time.Second)
	for r.deliverer.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	notices := r.announcements.all()
	if len(notices) != 3 {
		t.Fatalf("%d notices, want 3 (player at 0, other at 9m, player again at 10m): %+v", len(notices), notices)
	}
	n := notices[0]
	if n.Source != announce.SourceEvent || n.TargetKind != announce.TargetPermission ||
		n.TargetValue != "operator" || n.Delivery != announce.DeliveryWhisper || n.AuthorXUID != "" {
		t.Errorf("notice = %+v, want an event whispered to the operator permission", n)
	}
	if n.ExpiresAt == nil || !n.ExpiresAt.Equal(modEpoch.Add(24*time.Hour)) {
		t.Errorf("notice expires %v, want a day after the flag", n.ExpiresAt)
	}
	if want := "Moderation: Steve was flagged (term). Say !modlog Steve for details."; n.Body != want {
		t.Errorf("body = %q, want %q", n.Body, want)
	}
	if strings.Contains(strings.ToLower(n.Body), "griefer") {
		t.Error("the notice repeats what was said; announcements outlive the 90-day limit")
	}
	if r.deliverer.count() != 3 {
		t.Errorf("SendNow called %d times, want once per notice", r.deliverer.count())
	}
}

// The notice points at !modlog, so a flag that was never written must not
// send an operator looking for it -- and must not spend the throttle.
func TestModerationDoesNotNotifyAboutAFlagItCouldNotRecord(t *testing.T) {
	r := newModRig(t, "griefer")
	r.store.recordErr = &pgconn.PgError{Code: "42P01"}
	r.say(t, modPlayer, "griefer")
	deadline := time.Now().Add(5 * time.Second)
	for len(r.voice.told()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the flag was never processed")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(r.announcements.all()); n != 0 {
		t.Fatalf("%d notices about an unrecorded flag", n)
	}

	r.store.mu.Lock()
	r.store.recordErr = nil
	r.store.mu.Unlock()
	r.clock.set(time.Minute)
	r.say(t, modPlayer, "griefer")
	r.waitRecorded(t, 1)
	deadline = time.Now().Add(5 * time.Second)
	for len(r.announcements.all()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first recorded flag was not reported; the failed one spent the throttle")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestModerationNeverBlocksTheDispatcher(t *testing.T) {
	r := newModRig(t)
	r.store.block = make(chan struct{})
	defer close(r.store.block)

	start := time.Now()
	for i := 0; i < 5*moderationQueue; i++ {
		r.say(t, modPlayer, shouting)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("%d flagged messages took %s to hand off against a stalled store", 5*moderationQueue, elapsed)
	}
}

func modlog(t *testing.T, pctx *plugin.Context, perm plugin.Permission, args ...string) string {
	t.Helper()
	m, err := NewModeration(t.Context(), nil, logging.New("error"))
	if err != nil {
		t.Fatalf("NewModeration: %v", err)
	}
	c := m.Commands()[0]
	reply, err := c.Run(context.Background(), pctx, plugin.Invocation{ActorXUID: modPlayer, ActorPermission: perm, Args: args})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return reply
}

func TestModlogIsOperatorOnly(t *testing.T) {
	m, _ := NewModeration(t.Context(), nil, logging.New("error"))
	if c := m.Commands()[0]; c.Name != "modlog" || c.Permission != plugin.PermissionOperator {
		t.Fatalf("command = %s at %s, want modlog at operator", c.Name, c.Permission)
	}
	store := &modStore{}
	reply := modlog(t, &plugin.Context{Moderation: store}, plugin.PermissionMember)
	if !strings.Contains(reply, "Only an operator") || store.recentCalls != 0 {
		t.Errorf("member got %q after %d reads, want a refusal before any read", reply, store.recentCalls)
	}
}

// A console reply is broadcast, which would read every flag out to the server.
func TestModlogRefusesTheConsole(t *testing.T) {
	m, _ := NewModeration(t.Context(), nil, logging.New("error"))
	store := &modStore{}
	reply, err := m.Commands()[0].Run(context.Background(), &plugin.Context{Moderation: store},
		plugin.Invocation{ActorXUID: chat.ServerOrigin, ActorPermission: plugin.PermissionOperator})
	if err != nil || !strings.Contains(reply, "console") || store.recentCalls != 0 {
		t.Errorf("console got (%q, %v) after %d reads, want a refusal", reply, err, store.recentCalls)
	}
}

func TestModlogLimits(t *testing.T) {
	cases := []struct {
		args      []string
		wantLimit int
	}{
		{nil, 5},
		{[]string{"3"}, 3},
		{[]string{"10"}, 10},
		{[]string{"50"}, 10},
	}
	for _, tc := range cases {
		store := &modStore{}
		modlog(t, &plugin.Context{Moderation: store}, plugin.PermissionOperator, tc.args...)
		if store.recentLimit != tc.wantLimit || store.recentXUID != "" {
			t.Errorf("!modlog %v read (%q, %d), want everyone's newest %d", tc.args, store.recentXUID, store.recentLimit, tc.wantLimit)
		}
	}
	for _, bad := range []string{"0", "-2"} {
		store := &modStore{}
		if reply := modlog(t, &plugin.Context{Moderation: store}, plugin.PermissionOperator, bad); reply != modlogUsage || store.recentCalls != 0 {
			t.Errorf("!modlog %s = %q, want usage", bad, reply)
		}
	}
}

func TestModlogFiltersByPlayerThroughTheLookup(t *testing.T) {
	store := &modStore{}
	pctx := &plugin.Context{Moderation: store, Roster: fakeAnnounceRoster{
		online:  map[string]string{"Steve": modPlayer},
		offline: map[string]string{"Big Alex": modOther},
	}}

	modlog(t, pctx, plugin.PermissionOperator, "Steve", "3")
	if store.recentXUID != modPlayer || store.recentLimit != 3 {
		t.Errorf("read (%q, %d), want Steve's newest 3", store.recentXUID, store.recentLimit)
	}
	// Offline and with a space in the name: the second tier of the lookup,
	// and the reason a trailing count is what splits the arguments.
	modlog(t, pctx, plugin.PermissionOperator, "@Big", "Alex")
	if store.recentXUID != modOther || store.recentLimit != 5 {
		t.Errorf("read (%q, %d), want Big Alex's newest 5", store.recentXUID, store.recentLimit)
	}

	calls := store.recentCalls
	if reply := modlog(t, pctx, plugin.PermissionOperator, "Nobody"); reply != "I don't know a player named Nobody." || store.recentCalls != calls {
		t.Errorf("unknown player = %q, want a refusal without a read", reply)
	}
	if reply := modlog(t, &plugin.Context{Moderation: store}, plugin.PermissionOperator, "Steve"); !strings.Contains(reply, "don't know") {
		t.Errorf("no roster = %q, want a refusal", reply)
	}
}

func TestModlogSurfacesAFailedLookup(t *testing.T) {
	m, _ := NewModeration(t.Context(), nil, logging.New("error"))
	pctx := &plugin.Context{Moderation: &modStore{}, Roster: fakeAnnounceRoster{err: errors.New("connection refused")}}
	if _, err := m.Commands()[0].Run(context.Background(), pctx,
		plugin.Invocation{ActorXUID: modPlayer, ActorPermission: plugin.PermissionOperator, Args: []string{"Steve"}}); err == nil {
		t.Error("a lookup that could not be made was answered as if it had")
	}
}

func TestModlogDegrades(t *testing.T) {
	cases := []struct {
		name string
		pctx *plugin.Context
		want string
	}{
		{"no store", &plugin.Context{}, noModlog},
		{"nop store", &plugin.Context{Moderation: moderation.Nop{}}, noModlog},
		{"table missing", &plugin.Context{Moderation: &modStore{recentErr: &pgconn.PgError{Code: "42P01"}}}, noModlog},
		{"grant missing", &plugin.Context{Moderation: &modStore{recentErr: &pgconn.PgError{Code: "42501"}}}, noModlogAccess},
		{"empty", &plugin.Context{Moderation: &modStore{}}, "Nothing recorded."},
	}
	for _, tc := range cases {
		if got := modlog(t, tc.pctx, plugin.PermissionOperator); got != tc.want {
			t.Errorf("%s: reply = %q, want %q", tc.name, got, tc.want)
		}
	}

	m, _ := NewModeration(t.Context(), nil, logging.New("error"))
	if _, err := m.Commands()[0].Run(context.Background(), &plugin.Context{Moderation: &modStore{recentErr: errors.New("boom")}},
		plugin.Invocation{ActorXUID: modPlayer, ActorPermission: plugin.PermissionOperator}); err == nil {
		t.Error("an unexpected database failure was answered as if nothing were wrong")
	}
}

func TestModlogFormatsNewestFirstWithATruncatedMessage(t *testing.T) {
	long := strings.Repeat("spam ", 30) + "\nsecond line"
	store := &modStore{recent: []moderation.Event{
		{Gamertag: "Steve", Rule: moderation.RuleCaps, Detail: "100% of 29 letters", Message: shouting,
			OccurredAt: time.Date(2026, 9, 10, 14, 3, 0, 0, time.FixedZone("x", 2*3600))},
		{Gamertag: "Alex", Rule: moderation.RuleFlood, Detail: "6 messages in 10s", Message: long,
			OccurredAt: time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC)},
	}}
	reply := modlog(t, &plugin.Context{Moderation: store}, plugin.PermissionOperator)

	entries := strings.Split(reply, " | ")
	if len(entries) != 2 {
		t.Fatalf("reply = %q, want two entries", reply)
	}
	if want := `09-10 12:03 UTC Steve caps (100% of 29 letters): "` + shouting + `"`; entries[0] != want {
		t.Errorf("entry 0 = %q, want %q", entries[0], want)
	}
	if !strings.HasPrefix(entries[1], `09-09 08:00 UTC Alex flood (6 messages in 10s): "spam spam`) ||
		!strings.HasSuffix(entries[1], `..."`) || strings.Contains(entries[1], "second line") || strings.Contains(entries[1], "\n") {
		t.Errorf("entry 1 = %q, want a one-line message cut short", entries[1])
	}
}

// The counter must track what was written, so deleting the recording call
// beside the write is a failure here, not a quietly flat dashboard.
func TestModerationCountsEveryFlagItRecords(t *testing.T) {
	r := newModRig(t, "griefer")
	got := metricstest.Delta(t, func() {
		r.say(t, modPlayer, "griefer")
		r.waitRecorded(t, 1)
	}, "mc_agent_moderation_flags_total", "rule", "term", "action", "warned")
	// At least rather than exactly: the series is process-global, and another
	// test's worker may still be finishing. Nothing else can move it if the
	// recording call is gone.
	if got < 1 {
		t.Errorf("term/warned moved by %v after a recorded warning, want at least 1", got)
	}
}
