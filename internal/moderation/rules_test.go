package moderation

import (
	"strings"
	"testing"
)

func mustTerms(t *testing.T, terms ...string) *Terms {
	t.Helper()
	m, err := NewTerms(terms)
	if err != nil {
		t.Fatalf("NewTerms: %v", err)
	}
	return m
}

func TestTermsMatchOnlyWholeWords(t *testing.T) {
	terms := mustTerms(t, "ass")
	cases := []struct {
		message string
		want    bool
	}{
		{"you ass", true},
		{"ass", true},
		{"ASS!", true},
		{"what an Ass.", true},
		{"bad-ass move", true},
		// The reason for word boundaries: a short term inside an innocent
		// word must never fire.
		{"class act", false},
		{"assassin", false},
		{"passes", false},
		{"ass_hat", false},
		{"ass9", false},
	}
	for _, tc := range cases {
		if _, got := terms.Match(tc.message); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}

// RE2's \b is ASCII-only and would see a boundary between "x" and "ü".
func TestTermsBoundariesAreUnicodeAware(t *testing.T) {
	terms := mustTerms(t, "über")
	if _, ok := terms.Match("xüber alles"); ok {
		t.Error(`"über" matched inside "xüber"`)
	}
	if _, ok := terms.Match("ÜBER alles"); !ok {
		t.Error(`"über" did not match "ÜBER" case-insensitively`)
	}
}

// A configured term is text to find, never a pattern: unquoted, "a.b" would
// match "axb" and "(" would fail to compile and take startup with it.
func TestTermsAreQuotedNotInterpreted(t *testing.T) {
	terms := mustTerms(t, "a.b", "(", "c++", ".*")
	if _, ok := terms.Match("axb"); ok {
		t.Error(`"a.b" matched "axb": the term was used as a pattern`)
	}
	if _, ok := terms.Match("hello world"); ok {
		t.Error(`".*" matched an ordinary line: the term was used as a pattern`)
	}
	for _, msg := range []string{"it is a.b here", "i love c++ ok", "look ( there"} {
		if _, ok := terms.Match(msg); !ok {
			t.Errorf("Match(%q) = false, want the literal term found", msg)
		}
	}
	if _, ok := terms.Match("c++x"); ok {
		t.Error(`"c++" matched with a letter straight after it`)
	}
}

func TestTermsReportTheConfiguredSpellingOfTheLongestMatch(t *testing.T) {
	terms := mustTerms(t, "free", "Free Diamonds", "BadWord")
	if got, ok := terms.Match("FREE DIAMONDS at spawn"); !ok || got != "Free Diamonds" {
		t.Errorf("Match = (%q, %v), want the longer configured term", got, ok)
	}
	if got, ok := terms.Match("such a badword"); !ok || got != "BadWord" {
		t.Errorf("Match = (%q, %v), want the configured spelling", got, ok)
	}
}

func TestNoTermsMeansTheRuleIsOff(t *testing.T) {
	for _, list := range [][]string{nil, {}, {"", "  ", "\t"}} {
		terms := mustTerms(t, list...)
		if terms.Enabled() {
			t.Errorf("NewTerms(%q) is enabled", list)
		}
		if _, ok := terms.Match("anything at all"); ok {
			t.Errorf("NewTerms(%q) matched a message", list)
		}
	}
	var nilTerms *Terms
	if _, ok := nilTerms.Match("x"); ok || nilTerms.Enabled() || nilTerms.Len() != 0 {
		t.Error("a nil *Terms is not a disabled rule")
	}
}

func TestTermsDropBlanksAndDuplicates(t *testing.T) {
	if n := mustTerms(t, " grief ", "GRIEF", "", "raid").Len(); n != 2 {
		t.Errorf("Len = %d, want 2", n)
	}
}

func TestCapsNeedsTwentyLetters(t *testing.T) {
	nineteen := strings.Repeat("A", 19)
	if _, ok := Caps(nineteen + "!!! 123"); ok {
		t.Error("19 capitals were flagged; the floor is 20 letters")
	}
	if detail, ok := Caps(nineteen + "A!!!"); !ok {
		t.Error("20 capitals were not flagged")
	} else if detail != "100% of 20 letters" {
		t.Errorf("detail = %q", detail)
	}
}

func TestCapsNeedsEightyPercent(t *testing.T) {
	seventyNine := strings.Repeat("A", 79) + strings.Repeat("a", 21)
	if _, ok := Caps(seventyNine); ok {
		t.Error("79% capitals were flagged")
	}
	eighty := strings.Repeat("A", 80) + strings.Repeat("a", 20)
	if detail, ok := Caps(eighty); !ok {
		t.Error("80% capitals were not flagged")
	} else if detail != "80% of 100 letters" {
		t.Errorf("detail = %q", detail)
	}
}

// A caseless script cannot shout, so its letters count toward neither side.
func TestCapsIgnoresLettersWithoutCase(t *testing.T) {
	if _, ok := Caps("HELLO " + strings.Repeat("你好", 20)); ok {
		t.Error("five capitals were flagged because caseless letters padded the count")
	}
	if _, ok := Caps(strings.Repeat("A", 20) + strings.Repeat("你", 40)); !ok {
		t.Error("twenty capitals were diluted below the threshold by caseless letters")
	}
}
