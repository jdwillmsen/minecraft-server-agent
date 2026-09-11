package moderation

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// The thresholds are fixed rather than configured: a threshold an operator
// can tune per deploy is a record whose meaning changes between rows, and
// the whole value of the record is that a flag in March means what a flag
// in June does.
const (
	// FloodMessages is how many messages FloodWindow may hold before the
	// next one is a flood: the sixth inside ten seconds, not the fifth.
	FloodMessages = 5
	FloodWindow   = 10 * time.Second
	// CapsMinLetters keeps a short shout ("GG", "LOL NICE") from counting:
	// below it, a capitals ratio says nothing about tone.
	CapsMinLetters = 20
	CapsPercent    = 80
)

// Flag is one rule a message failed, before anything has been done about it.
type Flag struct {
	Rule   Rule
	Detail string
}

// notWord is what may sit either side of a term: anything but a letter, a
// combining mark, a digit or an underscore, in any script. Not \b, which in
// RE2 is ASCII-only -- it would find a boundary between "x" and "ü" and
// match "über" inside "xüber", and it can never anchor a term that itself
// begins or ends with punctuation. Marks count as part of the word because
// a decomposed "café" is "cafe" followed by a combining accent, and a
// boundary there would let "cafe" match the accented word.
const notWord = `[^\p{L}\p{M}\p{N}_]`

// Terms matches the configured terms against a message, case-insensitively
// and only as whole words, so a short term never fires inside an innocent
// longer word.
type Terms struct {
	re *regexp.Regexp
	// terms is the configured spelling, longest first, reported as a flag's
	// detail so the record shows the list entry rather than whatever casing
	// the player happened to type.
	terms []string
}

// NewTerms compiles terms into one matcher. Blank entries and
// case-insensitive duplicates are dropped; with nothing left the rule is off.
//
// Every term is quoted before it reaches the pattern. The list comes from
// deploy configuration, not from players, but a term like "c++" or "(" is
// an ordinary thing to want to match, and unquoted it would either fail to
// compile and take the agent down at startup, or quietly match something
// else entirely.
//
// The only error left is a list too large for RE2 to compile, which is a
// configuration mistake worth failing startup over rather than running with
// the rule silently off.
func NewTerms(terms []string) (*Terms, error) {
	seen := make(map[string]bool, len(terms))
	var kept []string
	for _, t := range terms {
		t = strings.TrimSpace(t)
		key := strings.ToLower(t)
		if t == "" || seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		return &Terms{}, nil
	}
	// Longest first: RE2's alternation is leftmost-first, so where two terms
	// match at the same place ("free" and "free diamonds") the detail names
	// the more specific one.
	sort.SliceStable(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })
	quoted := make([]string, len(kept))
	for i, t := range kept {
		quoted[i] = regexp.QuoteMeta(t)
	}
	re, err := regexp.Compile(`(?i)(?:^|` + notWord + `)(` + strings.Join(quoted, "|") + `)(?:` + notWord + `|$)`)
	if err != nil {
		return nil, fmt.Errorf("moderation: compile %d terms: %w", len(kept), err)
	}
	return &Terms{re: re, terms: kept}, nil
}

// Enabled reports whether any term is configured.
func (t *Terms) Enabled() bool { return t != nil && t.re != nil }

// Len reports how many distinct terms are configured.
func (t *Terms) Len() int {
	if t == nil {
		return 0
	}
	return len(t.terms)
}

// Match reports the configured term message contains, if any.
func (t *Terms) Match(message string) (string, bool) {
	if !t.Enabled() {
		return "", false
	}
	m := t.re.FindStringSubmatch(message)
	if m == nil {
		return "", false
	}
	for _, term := range t.terms {
		if strings.EqualFold(term, m[1]) {
			return term, true
		}
	}
	return m[1], true
}

// Caps reports whether message is shouted: at least CapsMinLetters letters,
// CapsPercent or more of them capitals.
//
// Only letters that have a case are counted. A script without one cannot
// shout, and counting its letters as "not capitals" would let a line of
// them dilute an all-caps English line below the threshold.
func Caps(message string) (string, bool) {
	var letters, upper int
	for _, r := range message {
		switch {
		case unicode.IsUpper(r):
			letters++
			upper++
		case unicode.IsLower(r):
			letters++
		}
	}
	// Integer arithmetic, so 80% is exactly 80% and not a float that rounds
	// either side of it.
	if letters < CapsMinLetters || upper*100 < letters*CapsPercent {
		return "", false
	}
	return fmt.Sprintf("%d%% of %d letters", upper*100/letters, letters), true
}
