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

// fallbackStopwords holds common short English function words that clear
// minFallbackTokenLen despite carrying no topic-identifying meaning. "the"
// is a substring of "nether", "weather", "feather" and "gather" -- all
// plausible Minecraft topics -- so without this list a question like
// "where is the nether portal" can substring-match a completely unrelated
// topic, and !kb states the top result as fact with no hedge for anything
// but a fallback-only match.
//
// This is deliberately a curated word list, not a lower length floor: "tnt"
// and "end" are exactly three characters and real topic words on this kind
// of server, so raising minFallbackTokenLen to exclude "the" would throw
// them out too. The two mechanisms guard against different things -- the
// floor against short fragments in general, this list against specific
// words that are long enough to pass the floor but still meaningless -- and
// collapsing them into one loses the "tnt"/"end" case. Do not "simplify"
// this list away in favor of a taller floor.
var fallbackStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true,
	"were": true, "how": true, "any": true, "but": true, "not": true,
	"all": true, "can": true, "has": true, "had": true, "that": true,
	"this": true, "with": true, "from": true, "will": true, "what": true,
	"when": true, "where": true, "who": true, "why": true, "does": true,
	"did": true, "been": true, "being": true, "into": true, "than": true,
	"then": true, "they": true, "them": true, "their": true, "there": true,
	"here": true, "its": true, "our": true, "out": true, "you": true,
	"your": true, "have": true, "about": true,
}

// SignificantWord reports whether a word says anything about which fact a
// player meant: long enough to clear minFallbackTokenLen and not a
// stopword.
//
// Exported because the eval fixtures search the same way offline. They kept
// a second copy of both the floor and the word list, the two drifted --
// "here" was a stopword in production and a search term in the fixtures --
// and a suite that scores different words from the ones production scores
// measures the fixtures rather than the agent.
func SignificantWord(w string) bool {
	return len(w) >= minFallbackTokenLen && !fallbackStopwords[w]
}

// queryTokens breaks a normalized lookup query into the words used to build
// both the full-text search and the substring fallback in Lookup.
//
// Split on every run of non-alphanumeric characters, not just trimmed at
// each field's edges: the query reaching Lookup can be a whole question
// forwarded verbatim by the model ("wheres the goldfarm?", "gold farm
// x=232"), and an edge-only trim leaves an interior "=" or "'" glued into
// one token. That matters for more than cosmetics -- Postgres's own parser
// treats "=" as a word boundary too, so a query built by joining tokens
// with " or " and one whole "x=232" survives will hand websearch_to_tsquery
// a chunk it reads as 'x' followed immediately by '232' (a phrase, not an
// OR), silently reintroducing the ANDing this whole fix exists to remove.
// Splitting here keeps every emitted token a single run of letters/digits,
// so every one of them joins the OR on equal footing.
func queryTokens(normalized string) []string {
	var out []string
	isSeparator := func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}
	for _, field := range strings.Fields(normalized) {
		out = append(out, strings.FieldsFunc(field, isSeparator)...)
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
// substring against a topic -- see minFallbackTokenLen and
// fallbackStopwords.
func fallbackTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if SignificantWord(t) {
			out = append(out, t)
		}
	}
	return out
}
