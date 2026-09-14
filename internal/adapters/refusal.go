package adapters

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A refusal is the one thing the model writes in a tool round that a player
// must still hear. The agent speaks the round that answers and nothing else,
// which is right for almost everything written earlier: "let me look that
// up" is a note to itself, and the round that answers supersedes it. A
// refusal is superseded by nothing -- no tool result makes the agent willing
// to run the command -- so a player who asked for it and hears only the
// tool-backed answer has been answered past rather than declined.
//
// The line between the two is drawn on what a sentence declines, not on how
// certain it sounds. It carries forward only when it says no in the first
// person and what follows that no is something the system prompt withholds:
// running commands, changing rules, or making announcements. A decline about
// knowledge -- "I cannot find that waypoint", "I cannot find the server
// rules" -- is left behind deliberately, because the tool round running
// underneath it exists to overturn exactly that, and speaking it in front of
// the answer would contradict the answer.

// declinePhrases are first-person ways of saying no. Second-person and
// impersonal wordings are excluded on purpose: "you can't build there" is
// the model explaining a game rule, not declining a request.
var declinePhrases = []string{
	"i cannot", "i can not", "i can't", "i won't", "i will not",
	"i am unable", "i'm unable", "i am not able", "i'm not able",
	"i am not allowed", "i'm not allowed", "i am not permitted",
	"i'm not permitted", "i do not have permission", "i don't have permission",
	"i refuse", "i'm not going to", "i am not going to",
}

// withheldActs name what the agent declines to do, as opposed to what it
// happens not to know. Each is something a player has asked for and the
// prompt or the console-bridge allowlist refuses: speaking to the server at
// large, running a command, or rewriting how the agent behaves.
var withheldActs = []string{
	"announce", "broadcast", "command", "execute", "run",
	"op", "operator", "admin", "permission", "whitelist",
	"kick", "ban", "mute", "shutdown", "shut down", "restart",
	"teleport", "grant", "rule", "change", "modify",
	"instruction", "prompt",
}

// knowledgeVerbs are what the agent says no to when the obstacle is what it
// knows rather than what it is allowed to do. One of these standing between
// the decline and the act means the act is named in passing -- "I cannot
// find the server rules" is a gap the tool round underneath may still fill,
// not a refusal to change the rules.
var knowledgeVerbs = []string{
	"find", "locate", "see", "know", "recall",
	"remember", "look up", "determine", "confirm", "verify",
}

var (
	withheldActPattern   = wordPattern(withheldActs)
	knowledgeVerbPattern = wordPattern(knowledgeVerbs)
)

// wordPattern matches any of words as a whole word, allowing the endings
// English inflects them with. Plain substring matching reads "open" as "op"
// and "urban" as "ban", each of which turns an ordinary sentence into a
// refusal the player then hears in front of their answer.
func wordPattern(words []string) *regexp.Regexp {
	return regexp.MustCompile(`\b(?:` + strings.Join(words, "|") + `)(?:s|es|ed|ing)?\b`)
}

// unheardRefusalNote closes the gap between what the model has written and
// what the player has heard. Told that its refusal went nowhere, the model
// can decline again in the round that answers, and one authored sentence
// reads better than two rounds stitched together.
const unheardRefusalNote = "The player has not seen anything you have written so far; only your next reply reaches them. " +
	"If you are declining any part of what they asked, say so in that reply."

// refusalSentence is the first sentence of s that declines a withheld act,
// or "" when none does.
func refusalSentence(s string) string {
	for _, sentence := range splitSentences(s) {
		normalized := strings.ToLower(strings.ReplaceAll(sentence, "’", "'"))
		declined, ok := afterDecline(normalized)
		if !ok {
			continue
		}
		act := withheldActPattern.FindStringIndex(declined)
		if act == nil {
			continue
		}
		if gap := knowledgeVerbPattern.FindStringIndex(declined); gap != nil && gap[0] < act[0] {
			continue
		}
		return strings.TrimSpace(sentence)
	}
	return ""
}

// afterDecline is the part of s that its first first-person decline governs,
// and whether s declines at all. What a sentence refuses is what follows the
// no, so an act named ahead of it -- "the rules say no griefing, but I can't
// help with that" -- is not what is being refused.
func afterDecline(s string) (string, bool) {
	end := -1
	for _, phrase := range declinePhrases {
		if i := strings.Index(s, phrase); i >= 0 && (end < 0 || i+len(phrase) < end) {
			end = i + len(phrase)
		}
	}
	if end < 0 {
		return "", false
	}
	return s[end:], true
}

// withUnheardRefusal puts a refusal the player never heard in front of the
// round that answers. The answering round declining on its own is the better
// outcome and the common one once the model has been told, so nothing is
// stitched on in that case. The caller runs the result through cleanReply,
// which is what keeps a carried sentence inside the same chat budget and
// no-question rule as any other reply.
func withUnheardRefusal(refusal, reply string) string {
	if refusal == "" || refusalSentence(reply) != "" {
		return reply
	}
	if reply == "" {
		return refusal
	}
	return terminated(refusal) + " " + reply
}

// terminated ends s the way a sentence ends. A refusal is carried whether or
// not the model punctuated it, and an unpunctuated one joined to the answer
// behind it reads as a single run-on line.
func terminated(s string) string {
	body := strings.TrimRight(s, sentenceClosers)
	if r, _ := utf8.DecodeLastRuneInString(body); strings.ContainsRune(".!?。！？", r) {
		return s
	}
	return s + "."
}

// splitSentences cuts s at each sentence terminator, by the rule
// sentenceBoundaries applies, and at each line break. A line with no
// terminator at all is one sentence, since the model regularly writes a
// refusal without a full stop; the break itself still ends it, so a refusal
// jotted above a note to itself does not carry the note into chat with it.
func splitSentences(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		start := 0
		for _, end := range sentenceBoundaries(line) {
			out = append(out, line[start:end])
			start = end
		}
		if strings.TrimSpace(line[start:]) != "" {
			out = append(out, line[start:])
		}
	}
	return out
}
