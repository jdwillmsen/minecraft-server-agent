// Package presence decides which actors -- the accounts that put a player
// into the world -- should be in it, and keeps them there or out.
//
// Git is the baseline: every actor has a default from its Helm values. What
// this package stores is only ever a deviation from that default, with an
// owner, a reason and usually an expiry, and clearing one always falls back
// to what git says.
package presence

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

const (
	KindAgent  = "agent"
	KindAFKBot = "afk-bot"
	// GroupAll names every actor. Implicit: it is never listed in an actor's
	// groups, and configuration refuses it there.
	GroupAll = "all"
)

// Actor is an account that puts a player into the world.
type Actor struct {
	ID       string
	Gamertag string
	Kind     string
	Groups   []string
	Default  presenceapi.State
}

// Registry is the configured actor list. Fixed for the life of the process.
type Registry struct {
	actors []Actor
	byID   map[string]int
	groups map[string][]int
	selfID string
}

// NewRegistry builds the registry. Configuration has already validated the
// list; the checks here are the ones this package relies on, not a second
// copy of every rule.
func NewRegistry(actors []Actor, selfID string) (*Registry, error) {
	r := &Registry{byID: make(map[string]int), groups: make(map[string][]int), selfID: selfID}
	for i, a := range actors {
		if _, dup := r.byID[a.ID]; dup {
			return nil, fmt.Errorf("presence: actor %q is registered twice", a.ID)
		}
		if a.Groups == nil {
			a.Groups = []string{}
		}
		r.actors = append(r.actors, a)
		r.byID[a.ID] = i
		for _, g := range a.Groups {
			r.groups[g] = append(r.groups[g], i)
		}
	}
	if len(actors) > 0 {
		if self, ok := r.Actor(selfID); !ok || self.Kind != KindAgent {
			return nil, fmt.Errorf("presence: self %q is not a registered agent", selfID)
		}
	}
	return r, nil
}

// Enabled reports whether any actor is configured. With none, presence
// control is off and the agent is always in the world.
func (r *Registry) Enabled() bool { return len(r.actors) > 0 }

// Actors returns every actor in configuration order.
func (r *Registry) Actors() []Actor { return slices.Clone(r.actors) }

func (r *Registry) Actor(id string) (Actor, bool) {
	i, ok := r.byID[id]
	if !ok {
		return Actor{}, false
	}
	return r.actors[i], true
}

// SelfID is the actor this process is.
func (r *Registry) SelfID() string { return r.selfID }

// Group returns the members of a configured group, or every actor for all.
// Exact: it serves a URL path segment, not something typed in chat.
func (r *Registry) Group(name string) ([]Actor, bool) {
	if name == GroupAll {
		return r.Actors(), r.Enabled()
	}
	idx, ok := r.groups[name]
	if !ok {
		return nil, false
	}
	out := make([]Actor, 0, len(idx))
	for _, i := range idx {
		out = append(out, r.actors[i])
	}
	return out, true
}

// Resolve turns a chat target -- an actor id, a group or all -- into actors.
// Case-insensitive because it is typed in chat; configuration keeps ids and
// groups lower-case ASCII, so folding cannot make two targets collide, and
// an ASCII-only fold keeps a look-alike such as the Kelvin sign from naming
// one.
func (r *Registry) Resolve(target string) ([]Actor, bool) {
	t := text.FoldASCII(strings.TrimSpace(target))
	if a, ok := r.Actor(t); ok {
		return []Actor{a}, true
	}
	return r.Group(t)
}

// IsActor reports whether gamertag belongs to an actor. An actor arriving is
// never a player arriving, whatever case the server reports the name in.
func (r *Registry) IsActor(gamertag string) bool { return isActorName(r.actors, gamertag) }

// isActorName folds through text.FoldASCII, the one gamertag case-fold this
// repo uses, so it agrees with the bridge's own kick-name folding on which
// gamertag is which.
func isActorName(actors []Actor, gamertag string) bool {
	folded := text.FoldASCII(gamertag)
	for _, a := range actors {
		if text.FoldASCII(a.Gamertag) == folded {
			return true
		}
	}
	return false
}

// Targets lists what Resolve accepts, for a usage reply: actors in
// configuration order, then groups by name, then all.
func (r *Registry) Targets() []string {
	out := make([]string, 0, len(r.actors)+len(r.groups)+1)
	for _, a := range r.actors {
		out = append(out, a.ID)
	}
	groups := make([]string, 0, len(r.groups))
	for g := range r.groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return append(append(out, groups...), GroupAll)
}
