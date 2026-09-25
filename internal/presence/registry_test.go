package presence

import (
	"slices"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

func threeActors() []Actor {
	return []Actor{
		{ID: "agent", Gamertag: "JdwAgent", Kind: KindAgent, Default: presenceapi.StatePresent},
		{ID: "afk-bot-1", Gamertag: "JdwAfk1", Kind: KindAFKBot, Groups: []string{"bots"}, Default: presenceapi.StatePresent},
		{ID: "afk-bot-2", Gamertag: "JdwAfk2", Kind: KindAFKBot, Groups: []string{"bots"}, Default: presenceapi.StateParked},
	}
}

func ids(actors []Actor) []string {
	out := make([]string, 0, len(actors))
	for _, a := range actors {
		out = append(out, a.ID)
	}
	return out
}

func TestRegistryResolvesActorsGroupsAndAll(t *testing.T) {
	r, err := NewRegistry(threeActors(), "agent")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	cases := map[string][]string{
		"afk-bot-1":   {"afk-bot-1"},
		" AFK-Bot-1 ": {"afk-bot-1"},
		"bots":        {"afk-bot-1", "afk-bot-2"},
		"all":         {"agent", "afk-bot-1", "afk-bot-2"},
		"ALL":         {"agent", "afk-bot-1", "afk-bot-2"},
	}
	for target, want := range cases {
		got, ok := r.Resolve(target)
		if !ok || !slices.Equal(ids(got), want) {
			t.Errorf("Resolve(%q) = %v, %v; want %v", target, ids(got), ok, want)
		}
	}
	if got, ok := r.Resolve("nobody"); ok {
		t.Errorf("Resolve(nobody) = %v, want not found", ids(got))
	}
	// The Kelvin sign folds to "k" under Unicode rules. Ids are ASCII, so a
	// target spelled with it names nothing.
	if got, ok := r.Resolve("af\u212a-bot-1"); ok {
		t.Errorf("Resolve with a Kelvin sign = %v, want not found", ids(got))
	}
}

// The API's group route takes the path segment as written, so it is
// case-sensitive where chat is not.
func TestRegistryGroupIsExact(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	if _, ok := r.Group("BOTS"); ok {
		t.Error("Group(BOTS) resolved; the route is case-sensitive")
	}
	if got, ok := r.Group("all"); !ok || len(got) != 3 {
		t.Errorf("Group(all) = %v, %v", ids(got), ok)
	}
}

func TestRegistryRecognisesActorsByGamertagInAnyCase(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	if !r.IsActor("jdwafk1") || r.IsActor("Steve") {
		t.Error("IsActor must match actors case-insensitively and nobody else")
	}
}

func TestRegistryGroupsAreNeverNil(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	a, _ := r.Actor("agent")
	if a.Groups == nil {
		t.Error("an actor with no groups has nil Groups; the API would encode null instead of []")
	}
}

func TestRegistryTargetsListsWhatChatAccepts(t *testing.T) {
	r, _ := NewRegistry(threeActors(), "agent")
	want := []string{"agent", "afk-bot-1", "afk-bot-2", "bots", "all"}
	if got := r.Targets(); !slices.Equal(got, want) {
		t.Errorf("Targets() = %v, want %v", got, want)
	}
}

func TestRegistryRefusesASelfThatIsNotTheAgent(t *testing.T) {
	if _, err := NewRegistry(threeActors(), "afk-bot-1"); err == nil {
		t.Error("a bot accepted as self")
	}
	if _, err := NewRegistry(threeActors(), "server"); err == nil {
		t.Error("an unknown self accepted")
	}
}

func TestEmptyRegistryIsDisabled(t *testing.T) {
	r, err := NewRegistry(nil, "agent")
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	if r.Enabled() {
		t.Error("an empty registry reports enabled")
	}
	if _, ok := r.Group(GroupAll); ok {
		t.Error("all resolved on an empty registry")
	}
}
