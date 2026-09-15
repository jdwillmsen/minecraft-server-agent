package knowledge

import "strings"

// MarkPartialMatches downgrades to MatchPartial any entry that answers a
// different compound from the one the query asked about, so a caller can
// offer it as the nearest thing on file instead of stating it as the
// answer. Only MatchExact rows are considered: a fallback-only row is
// already the weaker claim.
//
// The rule is deliberately narrow. It fires only where the entry itself
// contradicts the query: the entry is about a compound ("gold farm"), the
// query uses that compound's head word ("... slime farm"), and none of the
// modifiers the query put in front of it is one the entry attaches to that
// head. Then the entry and the query name two different things of the same
// kind, and the entry is evidence for the wrong one.
//
// The obvious alternative -- hedge whenever the query has words the entry
// does not cover -- is wrong here, and wrong in the direction that costs
// most. Nearly every real question carries words its answer does not
// contain: "where can i get mending books" is answered correctly by a
// trading hall entry that mentions neither getting nor books, and counting
// coverage would hedge it into uselessness. The discriminator is therefore
// not how much of the query is left over but whether what is left over
// contradicts the entry: a modifier sitting in front of the entry's own
// head word is a claim about which farm, and filler anywhere else is not.
//
// What this deliberately does not catch: an entry that never names a
// compound at all ("restarts", bodied "Restarts happen every day at 09:00
// UTC") has no modifier of its own for a query to contradict, so "when does
// the server restart" stays exact rather than reading "server" as a
// competing kind of restart. That keeps the hedge off questions the
// knowledge base really does answer, at the price of missing a query that
// modifies such a topic into something else ("mob spawner" against a
// "spawn" entry). A wrongly hedged hit costs an answer a player needed; a
// missed hedge costs the same wrong answer we have today.
func MarkPartialMatches(query string, entries []Entry) {
	tokens := queryTokens(NormalizeTopic(query))
	for i := range entries {
		if entries[i].Matched == MatchExact && answersAnotherCompound(tokens, entries[i]) {
			entries[i].Matched = MatchPartial
		}
	}
}

// answersAnotherCompound reports whether every use the query makes of the
// entry's head word puts a modifier in front of it that the entry does not
// attach to that head.
//
// tokens keep their stopwords, unlike everywhere else in this package: the
// modifier is identified by position, and dropping the filler first would
// close the gap in "wheres the gold farm" and read "wheres" as a competing
// kind of farm.
func answersAnotherCompound(tokens []string, e Entry) bool {
	heads := entryHeads(e)

	used := false
	for i, tok := range tokens {
		head, ok := headOf(heads, tok)
		if !ok {
			continue
		}
		used = true
		mods := modifiers(tokens, i)
		// The head asked for bare ("where is the farm") asks for whichever
		// one there is, and there is one -- nothing to contradict.
		if len(mods) == 0 {
			return false
		}
		for _, m := range mods {
			if identifies(e, head, m) {
				return false
			}
		}
	}
	return used
}

// entryHeads returns the head words of the compounds this entry is about --
// the kinds of thing it names, whichever way the fact happens to be filed.
//
// A multi-word topic ("gold farm") names its own compound and the head is
// its last word. A one-word topic names the compound in its body instead,
// and has to: !kb set takes a single whitespace-delimited argument as the
// topic (see runKB), so every fact an operator can store is filed under one
// word, and a rule that read compounds out of the topic alone could not
// fire on anything production can store. "gold" bodied "The gold farm is in
// the nether" is the same fact about the same farm.
//
// The body pair is anchored on the topic word from either side, because
// which half of the compound an operator filed the fact under is their
// choice: "gold" is its modifier and "hall" (bodied "the villager trading
// hall is ...") is its head. Anchoring is also what keeps the scan honest
// -- every adjacent pair of words would make a head out of "64" in "120 64
// -340".
func entryHeads(e Entry) []string {
	topicWords := strings.Fields(NormalizeTopic(e.Topic))
	if len(topicWords) == 0 {
		return nil
	}
	if len(topicWords) > 1 {
		return []string{topicWords[len(topicWords)-1]}
	}
	var heads []string
	body := queryTokens(NormalizeTopic(e.Body))
	for i := 1; i < len(body); i++ {
		if !SignificantWord(body[i-1]) || !SignificantWord(body[i]) {
			continue
		}
		if SameStem(body[i-1], topicWords[0]) || SameStem(body[i], topicWords[0]) {
			heads = append(heads, body[i])
		}
	}
	return heads
}

// headOf returns the head word tok is a use of, so the entry's spelling of
// it ("farm") is what the body is later searched for rather than the
// query's ("farms").
func headOf(heads []string, tok string) (string, bool) {
	for _, h := range heads {
		if SameStem(tok, h) {
			return h, true
		}
	}
	return "", false
}

// modifiers returns the run of significant words immediately in front of
// the word at i -- the words saying which one of its kind is meant.
//
// The whole run rather than the nearest word alone: "where is the iron
// golem farm" does name the iron farm's own modifier, and reading only
// "golem" would hedge a question the entry answers exactly. The run stops
// at the first filler word, which is what keeps "wheres the gold farm" from
// reading "wheres" as a kind of farm.
func modifiers(tokens []string, i int) []string {
	var out []string
	for j := i - 1; j >= 0 && SignificantWord(tokens[j]); j-- {
		out = append(out, tokens[j])
	}
	return out
}

// identifies reports whether the entry says word is which head it is about.
// The topic counts whole, because the topic is what the fact is filed as;
// the body counts only where it puts word in front of the head itself.
//
// Any word of the body would be broader than the question asked. The gold
// farm's body ends "reached through the portal at spawn", so a body-wide
// test reads "where is the spawn farm" as a confirmed hit and hands back
// the gold farm's coordinates as the answer -- the confident wrong answer
// this grading exists to stop. A word in front of the head is a claim about
// which farm; a word elsewhere in the prose is about something else in the
// same sentence.
func identifies(e Entry, head, word string) bool {
	for _, t := range strings.Fields(NormalizeTopic(e.Topic)) {
		if SameStem(word, t) {
			return true
		}
	}
	body := queryTokens(NormalizeTopic(e.Body))
	for i, tok := range body {
		if !SameStem(tok, head) {
			continue
		}
		for _, m := range modifiers(body, i) {
			if SameStem(word, m) {
				return true
			}
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
