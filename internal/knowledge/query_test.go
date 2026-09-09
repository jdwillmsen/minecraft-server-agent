package knowledge

import (
	"reflect"
	"testing"
)

func TestQueryTokens(t *testing.T) {
	cases := map[string][]string{
		"goldfarm":             {"goldfarm"},
		"gold farm":            {"gold", "farm"},
		"gold farm location":   {"gold", "farm", "location"},
		"wheres the goldfarm?": {"wheres", "the", "goldfarm"},
		// An interior "=" must split, not survive whole: Postgres's own
		// parser treats it as a word boundary too, and a joined "x=232"
		// reaching websearch_to_tsquery reads as a phrase ('x' followed by
		// '232'), not an OR of the two.
		"gold farm x=232": {"gold", "farm", "x", "232"},
		// Same reasoning for any interior punctuation, not just "=" -- an
		// apostrophe splits too, even though the two halves it produces are
		// each searched independently rather than as the original word.
		"farm's spawn.":  {"farm", "s", "spawn"},
		"don't go there": {"don", "t", "go", "there"},
		"???":            {},
		"":               {},
	}
	for in, want := range cases {
		got := queryTokens(in)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("queryTokens(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestSearchQueryJoinsWithOr(t *testing.T) {
	got := searchQuery([]string{"gold", "farm", "location"})
	want := "gold or farm or location"
	if got != want {
		t.Errorf("searchQuery = %q, want %q", got, want)
	}
}

func TestFallbackTokensDropsShortWords(t *testing.T) {
	got := fallbackTokens([]string{"is", "at", "gold", "farm", "x"})
	want := []string{"gold", "farm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallbackTokens = %#v, want %#v", got, want)
	}
}

func TestFallbackTokensCanBeEmpty(t *testing.T) {
	got := fallbackTokens([]string{"is", "at", "to"})
	if len(got) != 0 {
		t.Errorf("fallbackTokens = %#v, want empty", got)
	}
}

// "the" clears minFallbackTokenLen (three characters) but is a substring of
// "nether", "weather", "feather" and "gather" -- all plausible topics -- so
// it must be dropped by the stopword list even though the length floor
// alone would let it through.
func TestFallbackTokensDropsStopwordsThatClearTheLengthFloor(t *testing.T) {
	got := fallbackTokens([]string{"where", "is", "the", "nether"})
	want := []string{"nether"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallbackTokens = %#v, want %#v", got, want)
	}
}

// The length floor and the stopword list guard against different things:
// "tnt" and "end" are exactly three characters and real topic words, so
// they must still qualify even though they're as short as "the" or "any".
func TestFallbackTokensKeepsShortRealWords(t *testing.T) {
	got := fallbackTokens([]string{"tnt", "end", "and", "any"})
	want := []string{"tnt", "end"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fallbackTokens = %#v, want %#v", got, want)
	}
}
