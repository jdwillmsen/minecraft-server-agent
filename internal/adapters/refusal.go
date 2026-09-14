package adapters

import "strings"

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
// person and names something the system prompt withholds: running commands,
// changing rules, or making announcements. A decline about knowledge --
// "I cannot find that waypoint" -- is left behind deliberately, because the
// tool round running underneath it exists to overturn exactly that, and
// speaking it in front of the answer would contradict the answer.

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
// large, running a command, or rewriting how the agent behaves. A leading
// space where a word is a common suffix keeps "stop" from reading as "op".
var withheldActs = []string{
	"announce", "broadcast", "command", "execute", "run ",
	" op", "/op", "operator", "admin", "permission", "whitelist",
	"kick", "ban", "mute", "shutdown", "shut down", "restart",
	"teleport", "grant", "rule", "change", "modify",
	"instruction", "prompt",
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
		if containsAny(normalized, declinePhrases) && containsAny(normalized, withheldActs) {
			return strings.TrimSpace(sentence)
		}
	}
	return ""
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
	return refusal + " " + reply
}

// splitSentences cuts s at each sentence terminator, by the rule
// sentenceBoundaries applies. Text with no terminator at all is one
// sentence, since the model regularly writes a refusal without a full stop.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for _, end := range sentenceBoundaries(s) {
		out = append(out, s[start:end])
		start = end
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, s[start:])
	}
	return out
}

func containsAny(s string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
