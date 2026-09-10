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
	// DimPrivacy: whispered when it should be, and never carrying another
	// player's coordinates, or the asker's own in a broadcast.
	DimPrivacy Dimension = "privacy"
	// DimLength: the model's own text fit the chat limit, so the agent did
	// not have to cut it mid-sentence.
	DimLength Dimension = "length"
	// DimNoQuestion: the reply does not end on a question, the system
	// prompt's guard against two bots talking forever.
	DimNoQuestion Dimension = "no_question"
	// DimLatency: the whole answer arrived within the budget.
	DimLatency Dimension = "latency"
)

// dimensions is the report order.
var dimensions = []Dimension{DimAnswered, DimTools, DimContent, DimPrivacy, DimLength, DimNoQuestion, DimLatency}

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
}

// Score judges one observation against its case.
func Score(c Case, o Observation, lim Limits) Result {
	answered := o.Err == nil && o.Reply != ""
	return Result{Case: c, Obs: o, Checks: []Check{
		scoreAnswered(o),
		scoreTools(c, o, lim),
		scoreContent(c, o),
		scorePrivacy(c, o, lim),
		scoreLength(o, answered, lim),
		scoreNoQuestion(o, answered),
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

func scoreNoQuestion(o Observation, answered bool) Check {
	if !answered {
		return Check{Dim: DimNoQuestion}
	}
	if endsWithQuestion(o.Reply) {
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
