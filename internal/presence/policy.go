package presence

import (
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Join is a player arriving on the server.
type Join struct {
	Gamertag string
	At       time.Time
}

// Cause is why an override was written or removed, as the audit trail and
// the log record it.
type Cause string

const (
	CauseSet     Cause = "set"
	CauseCleared Cause = "cleared"
	CauseExpired Cause = "expired"
	CauseWoken   Cause = "woken"
)

// Removal is an override the policy says has ended. Override is the row as
// it was read: its Version and SetAt together are what make the removal safe
// against a newer write, or a re-park, landing in between.
type Removal struct {
	ActorID  string
	Cause    Cause
	Override presenceapi.Override
}

// Decision is what the policy concludes for one moment.
type Decision struct {
	Effective map[string]presenceapi.State
	Remove    []Removal
}

// Evaluate decides every actor's effective state from the stored overrides,
// the players who recently arrived, and the time.
//
// An override that has ended is reported for removal and its actor is given
// its default at once, so a tick that removes an override also acts on the
// result instead of waiting for the next one. Expiry is checked before a
// wake, so one override ending both ways is recorded as the timer that was
// set, not the arrival that coincided with it.
func Evaluate(actors []Actor, overrides map[string]presenceapi.Override, joins []Join, now time.Time) Decision {
	d := Decision{Effective: make(map[string]presenceapi.State, len(actors))}
	for _, a := range actors {
		ov, ok := overrides[a.ID]
		switch {
		case !ok:
			d.Effective[a.ID] = a.Default
		case ov.Until != nil && !now.Before(*ov.Until):
			d.Remove = append(d.Remove, Removal{ActorID: a.ID, Cause: CauseExpired, Override: ov})
			d.Effective[a.ID] = a.Default
		case ov.State == presenceapi.StateParked && woken(ov, actors, joins):
			d.Remove = append(d.Remove, Removal{ActorID: a.ID, Cause: CauseWoken, Override: ov})
			d.Effective[a.ID] = a.Default
		default:
			d.Effective[a.ID] = ov.State
		}
	}
	return d
}

// woken reports whether a join the override waits for has happened since it
// was set. An arrival from before set_at is not one: it is the player the
// operator parked the actor around, and counting it would undo the park on
// the very next tick.
func woken(ov presenceapi.Override, actors []Actor, joins []Join) bool {
	if ov.WakeOn == nil {
		return false
	}
	for _, j := range joins {
		if j.At.Before(ov.SetAt) || isActorName(actors, j.Gamertag) {
			continue
		}
		if ov.WakeOn.AnyPlayerJoin {
			return true
		}
		folded := text.FoldASCII(j.Gamertag)
		for _, p := range ov.WakeOn.Players {
			if text.FoldASCII(p) == folded {
				return true
			}
		}
	}
	return false
}

// AgentParkWindow is how long a chat park of the agent lasts when no
// duration is given.
const AgentParkWindow = time.Hour

// ChatPark is the expiry and wake a park from chat gives actor a.
//
// A parked agent reads no chat, so a park typed in chat could never be typed
// away: the agent always gets a timer and a wake on the next player to
// arrive, and an explicit duration only replaces the timer. A bot keeps
// exactly what was asked for.
func ChatPark(a Actor, d time.Duration, now time.Time) (until *time.Time, wake *presenceapi.WakeOn) {
	if a.Kind == KindAgent {
		if d <= 0 {
			d = AgentParkWindow
		}
		t := now.Add(d)
		return &t, &presenceapi.WakeOn{AnyPlayerJoin: true}
	}
	if d <= 0 {
		return nil, nil
	}
	t := now.Add(d)
	return &t, nil
}

// View is the presence actor a has with ov as its override row, if it has
// one. The effective state is the row's state for as long as the row
// exists: an expiry that has passed waits for the leader's loop to remove
// it, and until then every reader sees the same answer.
func View(a Actor, ov *presenceapi.Override) presenceapi.Presence {
	p := presenceapi.Presence{ActorID: a.ID, Default: a.Default, Effective: a.Default}
	if ov != nil {
		c := *ov
		p.Override = &c
		p.Effective = ov.State
	}
	return p
}
