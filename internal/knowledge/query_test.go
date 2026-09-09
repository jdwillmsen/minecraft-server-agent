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
		"don't go there":       {"don't", "go", "there"},
		"???":                  {},
		"":                     {},
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
