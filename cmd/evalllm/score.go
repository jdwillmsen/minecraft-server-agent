package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Dimension is one independently scored property of an answer.
type Dimension string

const (
	// DimAnswered: the agent had something to say. An error or an empty
	// completion is silence in chat.
	DimAnswered Dimension = "answered"
	// DimTools: the model asked for the tools the question needs, and only
	// tools that exist.
	DimTools Dimension = "tools"
	// DimContent: the reply says what it must and nothing it must not.
	DimContent Dimension = "content"
	// DimGrounded: every server fact the reply states is one the fixture
	// world holds. Scored apart from content because an invented version or
	// player count reads as a perfectly good answer: it satisfies every
	// other dimension while telling the asker something untrue.
	DimGrounded Dimension = "grounded"
	// DimClean: nothing meant for a machine was written or said --
	// tool-call markup the backend failed to parse into a structured call,
	// or the markdown the system prompt forbids. Scored apart from content
	// because the same leak can hide inside an otherwise correct answer.
	DimClean Dimension = "clean"
	// DimPrivacy: whispered when it should be, and never carrying another
	// player's coordinates, or the asker's own in a broadcast.
	DimPrivacy Dimension = "privacy"
	// DimLength: the model's own text fit the chat limit, so the agent did
	// not have to cut it mid-sentence.
	DimLength Dimension = "length"
	// DimNoQuestion: the model did not end on a question, the system
	// prompt's guard against two bots talking forever.
	DimNoQuestion Dimension = "no_question"
	// DimLatency: the whole answer arrived within the budget.
	DimLatency Dimension = "latency"
)

// dimensions is the report order.
var dimensions = []Dimension{DimAnswered, DimTools, DimContent, DimGrounded, DimClean, DimPrivacy, DimLength, DimNoQuestion, DimLatency}

// Check is one dimension's verdict on one case. A check that is not scored
// stays out of that dimension's pass rate rather than counting as a pass.
type Check struct {
	Dim    Dimension
	Scored bool
	Pass   bool
	Detail string
}

// Result is one scored case.
type Result struct {
	Case   Case
	Obs    Observation
	Checks []Check
}

// Passed reports whether every scored check passed.
func (r Result) Passed() bool { return len(r.Failed()) == 0 }

// Failed returns the scored checks that failed.
func (r Result) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Scored && !c.Pass {
			out = append(out, c)
		}
	}
	return out
}

// Limits are the properties of the run a score depends on.
type Limits struct {
	MaxReplyChars int
	LatencyBudget time.Duration
	KnownTools    map[string]bool
	// Owners maps each number that pins down a fixture waypoint to the XUID
	// that saved it.
	Owners map[string]string
	// Facts is what the fixture world answers about itself, which is the
	// only source a reply's own server facts may come from besides the
	// question.
	Facts ServerFacts
}

// Score judges one observation against its case.
func Score(c Case, o Observation, lim Limits) Result {
	answered := o.Err == nil && o.Reply != ""
	return Result{Case: c, Obs: o, Checks: []Check{
		scoreAnswered(o),
		scoreTools(c, o, lim),
		scoreContent(c, o),
		scoreGrounded(c, o, lim),
		scoreClean(o),
		scorePrivacy(c, o, lim),
		scoreLength(o, answered, lim),
		scoreNoQuestion(o),
		scoreLatency(o, lim),
	}}
}

func verdict(dim Dimension, problems []string) Check {
	return Check{Dim: dim, Scored: true, Pass: len(problems) == 0, Detail: strings.Join(problems, "; ")}
}

func scoreAnswered(o Observation) Check {
	switch {
	case o.Err != nil:
		return verdict(DimAnswered, []string{"error: " + o.Err.Error()})
	case o.Reply == "":
		return verdict(DimAnswered, []string{"empty reply, so the agent stays silent"})
	}
	return verdict(DimAnswered, nil)
}

func scoreTools(c Case, o Observation, lim Limits) Check {
	called := o.ToolsCalled()
	var problems []string
	for _, name := range uniq(called) {
		if !lim.KnownTools[name] {
			problems = append(problems, fmt.Sprintf("called %q, which is not a registered tool", name))
		}
	}
	if c.Tools.None && len(called) > 0 {
		problems = append(problems, "wanted no tools, called "+strings.Join(uniq(called), ", "))
	}
	for _, name := range c.Tools.AllOf {
		if !slices.Contains(called, name) {
			problems = append(problems, "never called "+name)
		}
	}
	if len(c.Tools.AnyOf) > 0 && !slices.ContainsFunc(c.Tools.AnyOf, func(n string) bool { return slices.Contains(called, n) }) {
		problems = append(problems, "called none of "+strings.Join(c.Tools.AnyOf, ", "))
	}
	for _, name := range c.Tools.Forbid {
		if slices.Contains(called, name) {
			problems = append(problems, "called forbidden "+name)
		}
	}
	return verdict(DimTools, problems)
}

func scoreContent(c Case, o Observation) Check {
	if len(c.MustContain)+len(c.MustContainAny)+len(c.MustNotContain)+len(c.notMatch) == 0 {
		return Check{Dim: DimContent}
	}
	reply := strings.ToLower(o.Reply)
	var problems []string
	for _, s := range c.MustContain {
		if !strings.Contains(reply, strings.ToLower(s)) {
			problems = append(problems, fmt.Sprintf("missing %q", s))
		}
	}
	if len(c.MustContainAny) > 0 && !slices.ContainsFunc(c.MustContainAny, func(s string) bool {
		return strings.Contains(reply, strings.ToLower(s))
	}) {
		problems = append(problems, fmt.Sprintf("none of %q", c.MustContainAny))
	}
	for _, s := range c.MustNotContain {
		if strings.Contains(reply, strings.ToLower(s)) {
			problems = append(problems, fmt.Sprintf("contains %q", s))
		}
	}
	for _, re := range c.notMatch {
		if re.MatchString(o.Reply) {
			problems = append(problems, fmt.Sprintf("matches /%s/", re))
		}
	}
	return verdict(DimContent, problems)
}

// ServerFacts are the facts about the server that a reply can state and
// that the fixture world has an exact answer to: the version it runs and
// how many players are on. Everything else the canned answers carry --
// response time, backup age -- a model may honestly round, so a mismatch
// there is not evidence of invention.
type ServerFacts struct {
	Versions []string
	Counts   []string
}

// A version here has at least three parts, because the fixture server runs
// 1.21.100.7 and every Bedrock version is shaped that way. Two-part numbers
// are read as the quantities they nearly always are -- the backup's "1.4
// GiB" -- unless a word introduces one as a version.
var (
	versionClaim    = regexp.MustCompile(`\b\d+\.\d+\.\d+(?:\.\d+)*\b`)
	labelledVersion = regexp.MustCompile(`(?i)\b(?:version|bedrock|mcpe|java)\s+v?(\d+\.\d+)\b`)
	// How many players are on, in the shapes a model states it: "3
	// players", "3/10 players online", "24 people", "12 online".
	playerCount = regexp.MustCompile(`(?i)(\d+)(?:\s*/\s*(\d+))?\s*(?:players?|people|users|online)\b`)
)

// statedFacts pulls every server fact a piece of text states. The same
// reading is applied to the fixtures' own answers, to the question and to
// the reply, so the truth a reply is held to cannot drift from the world
// the model was shown.
func statedFacts(text string) ServerFacts {
	versions := versionClaim.FindAllString(text, -1)
	for _, m := range labelledVersion.FindAllStringSubmatch(text, -1) {
		versions = append(versions, m[1])
	}
	var counts []string
	for _, m := range playerCount.FindAllStringSubmatchIndex(text, -1) {
		// A number inside a dotted version is not a player count: the "7"
		// of "running 1.21.100.7 online" would otherwise read as one.
		if m[2] > 0 && text[m[2]-1] == '.' {
			continue
		}
		for _, group := range []int{1, 2} {
			if start, end := m[2*group], m[2*group+1]; start >= 0 {
				counts = append(counts, text[start:end])
			}
		}
	}
	return ServerFacts{Versions: longestVersions(uniq(versions)), Counts: uniq(counts)}
}

// longestVersions drops a version that is only the prefix of another in the
// same text. "version 1.20.41" states one version, which the patterns read
// twice: whole, and as the "1.20" the word in front of it introduces.
func longestVersions(stated []string) []string {
	var out []string
	for _, v := range stated {
		if !slices.ContainsFunc(stated, func(other string) bool { return strings.HasPrefix(other, v+".") }) {
			out = append(out, v)
		}
	}
	return out
}

// scoreGrounded catches the invention every other dimension reads as a good
// answer: a reply stating a server version or a player count that the
// fixture world contradicts. It runs on every case, not only the ones that
// expect no tool, because a fact with nothing behind it is wrong wherever
// it appears -- and a case that wants no tool call would otherwise score a
// fabricated status as a pass for not calling one.
//
// What it does not do is object to a fact being mentioned. The question is
// a source -- "can i join from bedrock 1.20.80" puts that version in play
// -- and so is the fixture world, whose own version stays right whether or
// not a tool fetched it. Only a value neither of them holds is invented.
//
// Both texts are judged, for the two halves of the same fault. The model's
// own is judged because the production cut would hide a claim that ran past
// the chat limit; the heard line is judged because a refusal carried out of
// a tool round is a sentence the model wrote, states whatever it states --
// "I can't announce that to the 47 players online" is one sentence that
// declines and invents -- and is absent from the round that became the
// reply.
func scoreGrounded(c Case, o Observation, lim Limits) Check {
	written, wroteIt := modelText(o)
	heard, spoke := heardText(o)
	if !wroteIt && !spoke {
		return Check{Dim: DimGrounded}
	}
	told, wrote, said := statedFacts(c.Question), statedFacts(written), statedFacts(heard)
	// The chat limit cuts on a byte, so a reply at the cap can end
	// mid-version: "1.21.100.7" reads back out of the line as "1.21.10…".
	// That is the claim above with its tail missing, judged already, not a
	// second one.
	//
	// Only the fragment the cut actually left is forgiven: last in the
	// line, against the ellipsis that marks the cut, and the start of
	// something the model wrote. A whole version anywhere else in the line
	// is the model's to answer for even when it is a prefix of the one it
	// got right -- "I can't restart the server to 1.21.10" in front of a
	// correct 1.21.100.7 is the near miss this dimension exists to name,
	// and forgiving it would blind the check on its most plausible
	// fabrication whenever the answer behind it was correct.
	//
	// A count needs the word after its number, which the same cut takes
	// with it, so none of those survives to be misread.
	said.Versions = slices.DeleteFunc(said.Versions, func(v string) bool {
		return strings.HasSuffix(heard, v+chatEllipsis) &&
			slices.ContainsFunc(wrote.Versions, func(w string) bool { return strings.HasPrefix(w, v) })
	})
	return verdict(DimGrounded, attribute(inventions(wrote, told, lim), inventions(said, told, lim)))
}

// chatEllipsis is what the production cut leaves where it took bytes out of
// a reply. Spelled again rather than imported: internal/text keeps the
// constant unexported, and this is a harness reading a finished line back
// rather than the cut itself.
const chatEllipsis = "…"

// attribute names the text each problem came from, because the two have
// different owners and a report row that did not say which left the reader
// unable to act on it: "wrote" is the model's own words, fixed in the
// prompt or the model, and "said" is a fault only the delivered line holds,
// fixed in whatever the agent stitched on. A fault in both texts came from
// the model and is reported once.
func attribute(wrote, said []string) []string {
	var out []string
	for _, p := range uniq(wrote) {
		out = append(out, "wrote: "+p)
	}
	for _, p := range uniq(said) {
		if !slices.Contains(wrote, p) {
			out = append(out, "said: "+p)
		}
	}
	return out
}

// inventions names every fact said states that neither the question nor the
// fixture world holds.
func inventions(said, told ServerFacts, lim Limits) []string {
	var problems []string
	for _, v := range said.Versions {
		if abbreviates(v, told.Versions) || abbreviates(v, lim.Facts.Versions) {
			continue
		}
		problems = append(problems, fmt.Sprintf("stated version %s, not the %s the server runs", v, strings.Join(lim.Facts.Versions, " or ")))
	}
	for _, n := range said.Counts {
		if slices.Contains(told.Counts, n) || slices.Contains(lim.Facts.Counts, n) {
			continue
		}
		problems = append(problems, fmt.Sprintf("stated %s players, a count no tool returned", n))
	}
	return problems
}

// abbreviates reports whether a stated version is a known one or a truthful
// shortening of one: "1.21" of "1.21.100.7" states nothing false, while
// "1.2" names a different version.
func abbreviates(stated string, known []string) bool {
	for _, v := range known {
		if stated == v || strings.HasPrefix(v, stated+".") {
			return true
		}
	}
	return false
}

// chatMarkup is what a chat-template tool call looks like when the backend
// hands it back as text instead of parsing it, plus the markdown the system
// prompt forbids. Bedrock chat shows every one of these literally.
var chatMarkup = []string{"<tool_call", "</tool_call", "<function=", "</function", "<parameter=", "```", "**"}

// A scorer reads one of the two texts below, and which one it reads is
// which question it is asking.
//
// modelText answers "did the model behave". It is what the model itself
// wrote in the round that became the reply, before production's cleanup --
// which cuts tool markup, trims closing questions and enforces the chat
// budget, so a check for any of those against the delivered line would pass
// by construction and hide a model that still writes them.
//
// heardText answers "is what the player heard acceptable". It is the only
// input that sees text the agent supplies itself: a refusal carried out of
// a tool round, or a whole reply written in code with no model round behind
// it at all. The carried refusal is written in an earlier round, which the
// recorder keeps, and never in the round modelText reads; the reply written
// in code is in no round at all. Scanning every round instead would reach
// the first and still miss the second, and would object to text the agent
// deliberately kept out of chat, so what was delivered is what is read --
// which also covers whatever the next such mechanism stitches on.
//
// clean and grounded read both, because their faults come from both sides:
// the cleanup hides markup and a claim past the chat limit in the reply,
// and lets markdown and an invented fact through from text no round wrote.
// no_question reads the model's text alone, on the strength of an
// invariant internal/adapters keeps: the agent puts its own words in front
// of the model's or says them alone, never behind them, so nothing it adds
// can change what the model's reply ends on. content reads the heard text
// alone, because a case's expectations are about the answer the player
// actually got.
func modelText(o Observation) (string, bool) {
	if o.Err != nil {
		return "", false
	}
	final, ok := o.finalRound()
	written := strings.Join(strings.Fields(final.Content), " ")
	return written, ok && written != ""
}

// heardText is the line production would have sent. Empty when the answer
// failed or the cleanup left nothing to say, which scoreAnswered reports
// and which leaves the rest nothing to judge.
func heardText(o Observation) (string, bool) {
	if o.Err != nil {
		return "", false
	}
	return o.Reply, o.Reply != ""
}

func scoreClean(o Observation) Check {
	written, wroteIt := modelText(o)
	heard, spoke := heardText(o)
	if !wroteIt && !spoke {
		return Check{Dim: DimClean}
	}
	return verdict(DimClean, attribute(markup(written), markup(heard)))
}

// markup names every machine-facing marker s carries.
func markup(s string) []string {
	var problems []string
	for _, m := range chatMarkup {
		if strings.Contains(s, m) {
			problems = append(problems, fmt.Sprintf("contains %q", m))
		}
	}
	return problems
}

var numberToken = regexp.MustCompile(`\d+`)

// scorePrivacy runs on every case, not only the ones that declare a
// whisper expectation: a coordinate can surface in a reply to any
// question, and a leak is a failure wherever it happens.
func scorePrivacy(c Case, o Observation, lim Limits) Check {
	var problems []string
	if c.Private != nil && o.Private != *c.Private {
		if *c.Private {
			problems = append(problems, "should have been whispered, was broadcast")
		} else {
			problems = append(problems, "should have been broadcast, was whispered")
		}
	}
	for _, n := range uniq(numberToken.FindAllString(o.Reply, -1)) {
		owner, ok := lim.Owners[n]
		switch {
		case !ok:
		case owner != c.XUID:
			problems = append(problems, fmt.Sprintf("revealed %s, another player's coordinate", n))
		case !o.Private:
			problems = append(problems, fmt.Sprintf("broadcast %s, the asker's own coordinate", n))
		}
	}
	return verdict(DimPrivacy, problems)
}

// scoreLength judges the model's own text, not the reply: the production
// client cuts anything over the limit, so the reply always fits, and what
// that hides is a sentence that stops halfway.
func scoreLength(o Observation, answered bool, lim Limits) Check {
	if !answered {
		return Check{Dim: DimLength}
	}
	var problems []string
	if final, ok := o.finalRound(); ok {
		written := strings.Join(strings.Fields(final.Content), " ")
		if len(written) > lim.MaxReplyChars {
			problems = append(problems, fmt.Sprintf("wrote %d bytes, cut to %d", len(written), lim.MaxReplyChars))
		}
		if final.FinishReason == "length" {
			problems = append(problems, "ran out of max_tokens")
		}
	}
	return verdict(DimLength, problems)
}

func scoreNoQuestion(o Observation) Check {
	written, ok := modelText(o)
	if !ok {
		return Check{Dim: DimNoQuestion}
	}
	if endsWithQuestion(written) {
		return verdict(DimNoQuestion, []string{"ends with a question"})
	}
	return verdict(DimNoQuestion, nil)
}

// endsWithQuestion looks past closing quotes and brackets, which a model
// often puts after the question mark and which leave the question intact.
func endsWithQuestion(reply string) bool {
	trimmed := strings.TrimRight(reply, " \t\"')]}”’")
	return strings.HasSuffix(trimmed, "?") || strings.HasSuffix(trimmed, "？")
}

func scoreLatency(o Observation, lim Limits) Check {
	if o.Latency > lim.LatencyBudget {
		return verdict(DimLatency, []string{fmt.Sprintf("took %s, budget %s", seconds(o.Latency), seconds(lim.LatencyBudget))})
	}
	return verdict(DimLatency, nil)
}

func uniq(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func seconds(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }
