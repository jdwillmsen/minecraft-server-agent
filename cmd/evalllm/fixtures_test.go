package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/toolset"
	"github.com/jdwillmsen/minecraft-server-agent/internal/wiki"
)

// partialMarker is the wording knowledge_lookup puts in front of an entry
// that answers a different topic from the one asked about. Asserted here,
// in the eval fixtures, because a hedge the fixtures cannot produce is a
// hedge the suite cannot measure: the eval harness reads this store, not
// Postgres, so a production-only fix leaves kb_miss_slime_farm failing and
// looks like the fix did not work.
const partialMarker = "no entry for what was asked"

// fixtureLookup is what the model is shown for a question, built through
// the registry the eval run itself builds.
func fixtureLookup(t *testing.T, query string) string {
	t.Helper()
	registry, _ := toolset.Build(fixtureContext(true))
	args, err := json.Marshal(struct {
		Query string `json:"query"`
	}{query})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	out, err := registry.Invoke(t.Context(), "knowledge_lookup", args, steveXUID)
	if err != nil {
		t.Fatalf("knowledge_lookup(%q): %v", query, err)
	}
	return out
}

// The fixture world records a gold farm and no slime farm, so the only
// thing the two share is the word they are both farms by. Handing that back
// unflagged is what made the model state the gold farm's coordinates as the
// slime farm's.
func TestFixtureLookupFlagsAFactAboutADifferentFarm(t *testing.T) {
	out := fixtureLookup(t, "where is the slime farm")
	if !strings.Contains(out, "gold farm") {
		t.Fatalf("output = %q, want the gold farm as the nearest topic", out)
	}
	if !strings.Contains(out, partialMarker) {
		t.Errorf("output = %q, want it flagged with %q", out, partialMarker)
	}
}

// The hedge has to stay off the questions the knowledge base does answer:
// hedging a real hit trades a confidently wrong answer for a uselessly
// vague one. "where can i get mending books" and "what are the rules here"
// are the cases a coverage-counting rule would wrongly hedge -- most of
// each question's words are absent from the entry that answers it.
func TestFixtureLookupDoesNotFlagAConfirmedHit(t *testing.T) {
	cases := map[string]string{
		"what are the rules here":         "griefing",
		"wheres the gold farm":            "120 64 -340",
		"where can i get mending books":   "stall 4",
		"when does the server restart":    "09:00",
		"where is the end portal":         "-1200 30 800",
		"where is the gold farm entrance": "120 64 -340",
	}
	for query, want := range cases {
		out := fixtureLookup(t, query)
		if !strings.Contains(out, want) {
			t.Errorf("lookup(%q) = %q, want it to contain %q", query, out, want)
		}
		if strings.Contains(out, partialMarker) {
			t.Errorf("lookup(%q) = %q, a confirmed hit must not be hedged", query, out)
		}
	}
}

// A topic nothing in the fixture world touches must still come back empty
// rather than hedged: "nothing recorded about that" is a stronger answer
// than a nearest match, and these two cases already pass on it.
func TestFixtureLookupFindsNothingForAnAbsentTopic(t *testing.T) {
	for _, query := range []string{"whats the discord link", "where is the player shop district"} {
		if out := fixtureLookup(t, query); out != "nothing recorded about that" {
			t.Errorf("lookup(%q) = %q, want nothing recorded", query, out)
		}
	}
}

func TestFixtureWikiAnswersInProductionShape(t *testing.T) {
	out, err := fixtureWiki{}.Lookup(t.Context(), "torch", "crafting")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `Reference text from minecraft.wiki page "Torch"`) {
		t.Errorf("fixture result = %q, want wiki.Format's shape", out)
	}
	if _, err := (fixtureWiki{}).Lookup(t.Context(), "herobrine", ""); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("unknown topic err = %v, want wiki.ErrNotFound", err)
	}
	if !fixtureToolNames()["wiki_lookup"] {
		t.Error("the fixture world offers no wiki_lookup, so no wiki case can pass")
	}
}

func TestFixtureContextWithWikiOffOffersNoWikiTool(t *testing.T) {
	registry, _ := toolset.Build(fixtureContext(false))
	if registry.Has("wiki_lookup") {
		t.Error("wiki_lookup registered with fixtureContext(false), want the WIKI_ENABLED=off toolset")
	}
	// The rest of the production toolset is untouched by the wiki setting.
	if !registry.Has("knowledge_lookup") {
		t.Error("fixtureContext(false) dropped an unrelated tool")
	}
}
