package knowledge

import "strings"

// MarkPartialMatches downgrades to MatchPartial any entry that answers a
// different compound from the one the query asked about, so a caller can
// offer it as the nearest thing on file instead of stating it as the
// answer. Only MatchExact rows are considered: a fallback-only row is
// already the weaker claim.
//
// The rule is deliberately narrow. It fires only where the entry itself
// contradicts the query: the topic is a compound ("gold farm"), the query
// uses that compound's head word ("... slime farm"), and the modifier the
// query put in front of it appears nowhere in the entry. Then the topic and
// the query name two different things of the same kind, and the entry is
// evidence for the wrong one.
//
// The obvious alternative -- hedge whenever the query has words the entry
// does not cover -- is wrong here, and wrong in the direction that costs
// most. Nearly every real question carries words its answer does not
// contain: "where can i get mending books" is answered correctly by a
// trading hall entry that mentions neither getting nor books, and counting
// coverage would hedge it into uselessness. The discriminator is therefore
// not how much of the query is left over but whether what is left over
// contradicts the topic: a modifier sitting in front of the topic's own
// head word is a claim about which farm, and filler anywhere else is not.
//
// What this deliberately does not catch: a one-word topic ("restarts") has
// no modifier of its own for a query to contradict, so "when does the
// server restart" stays exact rather than reading "server" as a competing
// kind of restart. That keeps the hedge off questions the knowledge base
// really does answer, at the price of missing a query that modifies a
// one-word topic into something else ("mob spawner" against a "spawn"
// entry). A wrongly hedged hit costs an answer a player needed; a missed
// hedge costs the same wrong answer we have today.
func MarkPartialMatches(query string, entries []Entry) {
	tokens := queryTokens(NormalizeTopic(query))
	for i := range entries {
		if entries[i].Matched == MatchExact && answersAnotherCompound(tokens, entries[i]) {
			entries[i].Matched = MatchPartial
		}
	}
}

// answersAnotherCompound reports whether every use the query makes of the
// topic's head word puts a modifier in front of it that the entry knows
// nothing about.
//
// tokens keep their stopwords, unlike everywhere else in this package: the
// modifier is identified by position, and dropping the filler first would
// close the gap in "wheres the gold farm" and read "wheres" as a competing
// kind of farm.
func answersAnotherCompound(tokens []string, e Entry) bool {
	topicWords := strings.Fields(e.Topic)
	if len(topicWords) < 2 {
		return false
	}
	head := topicWords[len(topicWords)-1]

	used := false
	for i, tok := range tokens {
		if !SameStem(tok, head) {
			continue
		}
		used = true
		// The head asked for bare ("where is the farm") asks for whichever
		// one there is, and there is one -- nothing to contradict.
		if i == 0 || !SignificantWord(tokens[i-1]) {
			return false
		}
		if mentions(e, tokens[i-1]) {
			return false
		}
	}
	return used
}

// mentions reports whether the entry says anything about word, topic or
// body. The body counts because an entry that names the modifier is about
// it however the topic happens to be filed: the gold farm's body places it
// in the nether, so "where is the nether farm" is a hit, not a guess.
func mentions(e Entry, word string) bool {
	for _, have := range queryTokens(NormalizeTopic(e.Topic + " " + e.Body)) {
		if SameStem(word, have) {
			return true
		}
	}
	return false
}

// SameStem reports whether two words are plausibly the same word, so
// "farms" reads as "farm" and "restart" as "restarts".
//
// A prefix test, not a stemmer: Postgres does the real stemming inside the
// index, and this only has to decide whether to hedge a row that search
// already returned. The four-character floor keeps short topic words whole
// -- "end" must not read as "ender", and "tnt" as nothing but itself.
//
// Exported for the eval fixtures, which approximate this same search
// offline and must not drift into agreeing with production only by
// coincidence.
func SameStem(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
