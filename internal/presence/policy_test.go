package presence

import (
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

var t0 = time.Date(2026, 9, 23, 18, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { t := t0.Add(d); return &t }

func parkedAt(set time.Time, until *time.Time, wake *presenceapi.WakeOn) presenceapi.Override {
	return presenceapi.Override{State: presenceapi.StateParked, Until: until, WakeOn: wake, Reason: "r", SetBy: "chat:Op", SetAt: set, Version: 4}
}

func TestEvaluate(t *testing.T) {
	anyJoin := &presenceapi.WakeOn{AnyPlayerJoin: true}
	named := &presenceapi.WakeOn{Players: []string{"Steve"}}
	now := t0.Add(10 * time.Minute)
	cases := []struct {
		name          string
		actor         string
		override      *presenceapi.Override
		joins         []Join
		wantEffective presenceapi.State
		wantCause     Cause // "" means nothing removed
	}{
		{name: "no override, present default", actor: "afk-bot-1", wantEffective: presenceapi.StatePresent},
		{name: "no override, parked default", actor: "afk-bot-2", wantEffective: presenceapi.StateParked},
		{name: "override without expiry holds", actor: "afk-bot-1", override: ptr(parkedAt(t0, nil, nil)), wantEffective: presenceapi.StateParked},
		{name: "override before its expiry holds", actor: "afk-bot-1", override: ptr(parkedAt(t0, at(time.Hour), nil)), wantEffective: presenceapi.StateParked},
		{name: "expiry reached exactly", actor: "afk-bot-1", override: ptr(parkedAt(t0, at(10*time.Minute), nil)), wantEffective: presenceapi.StatePresent, wantCause: CauseExpired},
		{name: "expired falls back to a parked default", actor: "afk-bot-2", override: &presenceapi.Override{State: presenceapi.StatePresent, Until: at(time.Minute), SetAt: t0, Version: 1}, wantEffective: presenceapi.StateParked, wantCause: CauseExpired},
		{name: "present override over parked default", actor: "afk-bot-2", override: &presenceapi.Override{State: presenceapi.StatePresent, SetAt: t0, Version: 1}, wantEffective: presenceapi.StatePresent},
		{name: "any join wakes", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"Steve", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent, wantCause: CauseWoken},
		{name: "an actor joining is not a player joining", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"jdwafk1", t0.Add(time.Minute)}}, wantEffective: presenceapi.StateParked},
		{name: "join before the park is ignored", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"Steve", t0.Add(-time.Second)}}, wantEffective: presenceapi.StateParked},
		{name: "join at the park instant counts", actor: "agent", override: ptr(parkedAt(t0, at(time.Hour), anyJoin)), joins: []Join{{"Steve", t0}}, wantEffective: presenceapi.StatePresent, wantCause: CauseWoken},
		{name: "named player wakes in any case", actor: "afk-bot-1", override: ptr(parkedAt(t0, nil, named)), joins: []Join{{"STEVE", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent, wantCause: CauseWoken},
		{name: "another player does not wake a named list", actor: "afk-bot-1", override: ptr(parkedAt(t0, nil, named)), joins: []Join{{"Alex", t0.Add(time.Minute)}}, wantEffective: presenceapi.StateParked},
		{name: "wake_on never removes a present override", actor: "afk-bot-2", override: &presenceapi.Override{State: presenceapi.StatePresent, WakeOn: anyJoin, SetAt: t0, Version: 1}, joins: []Join{{"Steve", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent},
		{name: "expiry wins over a wake in the same tick", actor: "agent", override: ptr(parkedAt(t0, at(5*time.Minute), anyJoin)), joins: []Join{{"Steve", t0.Add(time.Minute)}}, wantEffective: presenceapi.StatePresent, wantCause: CauseExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrides := map[string]presenceapi.Override{}
			if tc.override != nil {
				overrides[tc.actor] = *tc.override
			}
			d := Evaluate(threeActors(), overrides, tc.joins, now)
			if got := d.Effective[tc.actor]; got != tc.wantEffective {
				t.Errorf("effective = %q, want %q", got, tc.wantEffective)
			}
			switch {
			case tc.wantCause == "" && len(d.Remove) != 0:
				t.Errorf("removed %+v, want nothing removed", d.Remove)
			case tc.wantCause != "" && (len(d.Remove) != 1 || d.Remove[0].Cause != tc.wantCause || d.Remove[0].ActorID != tc.actor || d.Remove[0].Override.Version != tc.override.Version):
				t.Errorf("removed %+v, want %s of %s at version %d", d.Remove, tc.wantCause, tc.actor, tc.override.Version)
			}
			if len(d.Effective) != 3 {
				t.Errorf("effective covers %d actors, want every actor", len(d.Effective))
			}
		})
	}
}

// Removing an actor from configuration leaves its row behind; the policy
// has nothing to say about an actor it does not know.
func TestEvaluateIgnoresAnOverrideForAnUnknownActor(t *testing.T) {
	d := Evaluate(threeActors(), map[string]presenceapi.Override{"afk-bot-9": parkedAt(t0, at(-time.Hour), nil)}, nil, t0)
	if len(d.Remove) != 0 || len(d.Effective) != 3 {
		t.Errorf("decision = %+v, want the unknown row left alone", d)
	}
}

func TestChatPark(t *testing.T) {
	agent, bot := threeActors()[0], threeActors()[1]
	cases := []struct {
		name      string
		actor     Actor
		d         time.Duration
		wantUntil *time.Time
		wantWake  bool
	}{
		{"agent without a duration gets an hour and a wake", agent, 0, at(time.Hour), true},
		{"agent with a duration keeps the wake", agent, 30 * time.Minute, at(30 * time.Minute), true},
		{"bot without a duration lasts until unparked", bot, 0, nil, false},
		{"bot with a duration expires and nothing else", bot, 2 * time.Hour, at(2 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			until, wake := ChatPark(tc.actor, tc.d, t0)
			switch {
			case (until == nil) != (tc.wantUntil == nil):
				t.Errorf("until = %v, want %v", until, tc.wantUntil)
			case until != nil && !until.Equal(*tc.wantUntil):
				t.Errorf("until = %v, want %v", *until, *tc.wantUntil)
			}
			if got := wake != nil && wake.AnyPlayerJoin; got != tc.wantWake {
				t.Errorf("wake = %+v, want any_player_join %v", wake, tc.wantWake)
			}
		})
	}
}

func TestViewTakesTheOverrideStateWhenOneExists(t *testing.T) {
	bot := threeActors()[1]
	if v := View(bot, nil); v.Effective != presenceapi.StatePresent || v.Override != nil || v.ActorID != "afk-bot-1" {
		t.Errorf("View without override = %+v", v)
	}
	ov := parkedAt(t0, nil, nil)
	v := View(bot, &ov)
	if v.Effective != presenceapi.StateParked || v.Default != presenceapi.StatePresent || v.Override == nil {
		t.Errorf("View with override = %+v", v)
	}
	ov.Reason = "changed after"
	if v.Override.Reason == "changed after" {
		t.Error("View shares the caller's override; later edits would leak into a reply")
	}
}

func ptr[T any](v T) *T { return &v }
