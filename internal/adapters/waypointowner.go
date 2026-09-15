package adapters

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A question about someone else's waypoint is answered here rather than by
// the model, because it is the one question the model cannot be given
// enough to answer truthfully.
//
// waypoint_lookup takes a waypoint name and nothing else -- the asking
// player is injected by the tool loop and is deliberately not an argument,
// so the model cannot ask on anyone's behalf. That also means the owner a
// question named never reaches the tool: it is asked "base", returns the
// asker's own, truthfully and in the first person, and the model writes
// "Alex's base is at ..." over the top of it. Wording the result as the
// asker's own and forbidding the re-framing in the system prompt were both
// tried and measured, and neither moved the failure, because both leave the
// sentence for the model to write. A sentence the model never writes is one
// it cannot get wrong.

// otherPlayerWaypointReply is what the agent says instead. It states the
// limit rather than the answer, which is what the tools can actually
// support: only the asker's own waypoints are readable, so there is no
// truthful answer to give about anyone else's. Carrying no number is not
// incidental -- it is why this reply is safe to say before the whisper
// decision is made.
const otherPlayerWaypointReply = "I can only look up your own waypoints, not another player's."

// waypointLookupTool is the toolset's name for the waypoint reader, spelled
// here rather than imported: internal/plugin imports this package, so a
// dependency on internal/toolset would close a cycle. The two agreeing is
// pinned end to end by the toolset's own tests, which drive this path
// through a registry Build produced.
const waypointLookupTool = "waypoint_lookup"

// waypointTerms name saved coordinates. Anything a player might ask for by
// name -- "stash", "gold farm" -- is left out: a waypoint's name is
// whatever its owner typed, so a list of them is unbounded, and the terms
// below are the words that make a question one about waypoints at all.
const waypointTerms = `waypoints?|coords?|coordinates?|bases?`

// possessedWaypoint matches an owner possessing saved coordinates:
// "Steve's base", "his coords", "every player's base". One word may stand
// between the two, which is where an adjective sits ("Alex's secret base");
// more than one stops reading as possession of the waypoint and starts
// matching across whatever else the sentence says.
var possessedWaypoint = regexp.MustCompile(
	`\b(?:([\p{L}\p{N}_]+)'s|(?:his|hers?|theirs?))\s+(?:([\p{L}\p{N}_]+)\s+)?(?:` + waypointTerms + `)\b`)

// onBehalfOf matches the other shape the evaluation asks in, where the
// owner is named without a possessive: "the waypoint called base for Alex".
//
// The name must be capitalised as a gamertag is. Unqualified, "for X" in a
// question about coordinates far more often names the place than the owner
// -- "coords for spawn" -- and deflecting one of those would silence the
// agent on a question it answers well. A name typed in lower case falls
// through to the model, which is where it goes today.
var onBehalfOf = regexp.MustCompile(`\b(?:for|of)\s+(\p{Lu}[\p{L}\p{N}_]*)`)

// waypointAlreadyNamed gates onBehalfOf on the question having already said
// which waypoint it means. That is what makes the name after "for" an owner
// rather than the waypoint itself: a waypoint is looked up by name, so "a
// waypoint for Ocean Monument" names the waypoint, while "the waypoint
// called base for Alex" has named it already and can only be naming a
// second party. Without this the capitalised-word rule below reads every
// named landmark -- Ocean Monument, Stronghold, Mesa -- as a gamertag and
// answers a question about the asker's own waypoint with a refusal.
var waypointAlreadyNamed = regexp.MustCompile(`(?i)\bwaypoints?\s+(?:called|named)\s+[\p{L}\p{N}_]+`)

// notPossessors are words whose "'s" is "is", never possession: "where's
// the waypoint" names no owner. The last two possess things but are not
// players, and this server's own coordinates are not a waypoint question.
var notPossessors = map[string]bool{
	"where": true, "what": true, "when": true, "who": true, "how": true,
	"why": true, "that": true, "there": true, "here": true, "it": true,
	"this": true, "he": true, "she": true, "let": true,
	"server": true, "world": true,
}

// firstPerson are the ways a player refers to their own. As the word
// between a possessive and a waypoint term, one of these means the
// waypoint is the asker's after all -- "show her my base" asks about the
// asker's own base, not hers. As a name, it is the asker asking for
// themselves.
var firstPerson = map[string]bool{
	"my": true, "mine": true, "our": true, "ours": true,
	"me": true, "myself": true, "us": true, "i": true,
}

// places are capitalised words that name somewhere rather than someone, so
// that "the waypoint for Spawn" is not read as a question about a player
// called Spawn.
var places = map[string]bool{
	"spawn": true, "nether": true, "end": true, "overworld": true,
	"hub": true, "home": true, "base": true, "world": true, "server": true,
}

// waypointQuestionNamesAnotherPlayer reports whether question asks for
// saved coordinates and attributes them to someone other than asker.
//
// Both halves are required, and that conjunction is the whole bound on this
// path. A question carrying only one of them -- "where is the gold farm",
// "what is Steve building" -- is one the model answers from a tool or
// honestly fails to answer, and deflecting it would replace a good answer
// with a refusal about waypoints that nobody asked for. Missing a question
// that does name another player costs nothing new: it takes the path every
// waypoint question takes today.
func waypointQuestionNamesAnotherPlayer(asker, question string) bool {
	// Curly apostrophes come in from phone keyboards and would otherwise
	// hide a possessive from every pattern below.
	normalized := strings.ToLower(strings.ReplaceAll(question, "’", "'"))
	askerName := strings.ToLower(strings.TrimSpace(asker))

	for _, loc := range possessedWaypoint.FindAllStringSubmatchIndex(normalized, -1) {
		possessor, between := submatch(normalized, loc, 1), submatch(normalized, loc, 2)
		if firstPerson[between] {
			continue
		}
		// An empty possessor is one of the pronouns, which name a third
		// person by construction. A gamertag may carry a space, so the
		// asker is recognised against the text running up to the
		// possessive rather than against its last word alone.
		if possessor != "" && (endsWithName(normalized[:loc[3]], askerName) || notPossessors[possessor] || firstPerson[possessor]) {
			continue
		}
		return true
	}

	if !waypointAlreadyNamed.MatchString(question) {
		return false
	}
	for _, loc := range onBehalfOf.FindAllStringSubmatchIndex(question, -1) {
		name := strings.ToLower(question[loc[2]:loc[3]])
		if startsWithName(strings.ToLower(question[loc[2]:]), askerName) || firstPerson[name] || places[name] {
			continue
		}
		return true
	}
	return false
}

// submatch reads capture group n, which may not have participated.
func submatch(s string, loc []int, n int) string {
	if loc[2*n] < 0 {
		return ""
	}
	return s[loc[2*n]:loc[2*n+1]]
}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// endsWithName reports whether s ends in name on a word boundary, so that
// "where is jake w" is Jake W naming himself while "where is alex" is not
// Lex naming herself.
func endsWithName(s, name string) bool {
	if name == "" || !strings.HasSuffix(s, name) {
		return false
	}
	if len(s) == len(name) {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:len(s)-len(name)])
	return !wordRune(r)
}

// startsWithName is endsWithName for a name the question puts after "for".
func startsWithName(s, name string) bool {
	if name == "" || !strings.HasPrefix(s, name) {
		return false
	}
	if len(s) == len(name) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[len(name):])
	return !wordRune(r)
}
