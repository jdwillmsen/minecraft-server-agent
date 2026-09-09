package knowledge

import (
	"strings"
	"unicode"
)

// minFallbackTokenLen is the shortest word Lookup will test as a plain
// substring against a topic. Full-text search discards stopwords on its
// side of the OR below; without a matching floor here, a token like "is" or
// "at" would substring-match nearly every topic in the table and turn the
// fallback into "return everything", which is the failure mode it exists to
// avoid rather than cause.
const minFallbackTokenLen = 3

// queryTokens breaks a normalized lookup query into the words used to build
// both the full-text search and the substring fallback in Lookup.
//
// Each token is trimmed of leading/trailing punctuation rather than split
// on it: the query reaching Lookup can be a whole question forwarded
// verbatim by the model ("wheres the goldfarm?"), and a stray "?" stuck to
// "farm?" would quietly defeat both the OR'd tsquery and the substring
// fallback below, while an internal apostrophe ("don't") still belongs to
// one word.
func queryTokens(normalized string) []string {
	fields := strings.Fields(normalized)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// searchQuery turns query tokens into the input for websearch_to_tsquery,
// matching an entry containing ANY of them rather than ALL of them.
//
// plainto_tsquery (the previous behaviour) ANDs every term, so one word the
// stored fact doesn't happen to share was enough to return nothing -- and a
// player's question ("where is the gold farm") rarely matches a curated
// one-line answer word for word. websearch_to_tsquery's "or" keyword is the
// documented way to ask for OR semantics instead, and unlike to_tsquery it
// never raises a syntax error on stray player-typed punctuation, so joining
// the tokens with it gets both properties without hand-building a tsquery
// string ourselves.
func searchQuery(tokens []string) string {
	return strings.Join(tokens, " or ")
}

// fallbackTokens is the subset of query tokens worth testing as a plain
// substring against a topic -- see minFallbackTokenLen.
func fallbackTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if len(t) >= minFallbackTokenLen {
			out = append(out, t)
		}
	}
	return out
}
