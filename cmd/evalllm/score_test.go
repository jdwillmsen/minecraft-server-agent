package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// testLimits mirror a production run against the fixture world.
func testLimits() Limits {
	return Limits{
		MaxReplyChars: 200,
		LatencyBudget: 8 * time.Second,
		KnownTools:    fixtureToolNames(),
		Owners:        coordinateOwners(),
	}
}

func testCase(t *testing.T, edit func(*Case)) Case {
	t.Helper()
	c := Case{ID: "t", Category: "status", Asker: "Alex", XUID: alexXUID, Question: "q"}
	if edit != nil {
		edit(&c)
	}
	if err := c.prepare(fixtureToolNames()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return c
}

// answered is an observation of a reply given straight away with no tools.
func answered(reply string) Observation {
	return Observation{Reply: reply, Latency: time.Second, Rounds: []Round{{Status: 200, Content: reply}}}
}

func withTools(o Observation, names ...string) Observation {
	calls := make([]ToolCall, 0, len(names))
	for _, n := range names {
		calls = append(calls, ToolCall{Name: n, Arguments: "{}"})
	}
	o.Rounds = append([]Round{{Status: 200, ToolCalls: calls}}, o.Rounds...)
	return o
}

func check(t *testing.T, r Result, d Dimension) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Dim == d {
			return c
		}
	}
	t.Fatalf("no %s check", d)
	return Check{}
}

func TestAnsweredFailsOnErrorAndOnSilence(t *testing.T) {
	c := testCase(t, nil)
	if got := check(t, Score(c, Observation{Err: errors.New("boom")}, testLimits()), DimAnswered); got.Pass {
		t.Error("an error passed as an answer")
	}
	if got := check(t, Score(c, Observation{}, testLimits()), DimAnswered); got.Pass {
		t.Error("an empty reply passed as an answer; the agent would have said nothing")
	}
	if got := check(t, Score(c, answered("All good."), testLimits()), DimAnswered); !got.Pass {
		t.Errorf("a reply failed: %s", got.Detail)
	}
}

func TestToolSelection(t *testing.T) {
	cases := []struct {
		name   string
		expect ToolExpectation
		called []string
		pass   bool
	}{
		{"all_of met", ToolExpectation{AllOf: []string{"knowledge_lookup"}}, []string{"knowledge_lookup"}, true},
		{"all_of missed", ToolExpectation{AllOf: []string{"knowledge_lookup"}}, []string{"server_status"}, false},
		{"any_of met by the second", ToolExpectation{AnyOf: []string{"server_version", "server_status"}}, []string{"server_status"}, true},
		{"any_of missed", ToolExpectation{AnyOf: []string{"server_version"}}, nil, false},
		{"none kept", ToolExpectation{None: true}, nil, true},
		{"none broken", ToolExpectation{None: true}, []string{"players_online"}, false},
		{"forbidden called", ToolExpectation{Forbid: []string{"waypoint_list"}}, []string{"waypoint_list"}, false},
		// Models invent plausible tool names. Calling one is a failure
		// whether or not the case said anything about tools.
		{"unregistered name with no expectation", ToolExpectation{}, []string{"announce"}, false},
		{"no expectation, real tool", ToolExpectation{}, []string{"server_status"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testCase(t, func(c *Case) { c.Tools = tc.expect })
			got := check(t, Score(c, withTools(answered("ok."), tc.called...), testLimits()), DimTools)
			if got.Pass != tc.pass {
				t.Errorf("pass = %v, want %v (%s)", got.Pass, tc.pass, got.Detail)
			}
		})
	}
}

func TestContentChecks(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Case)
		reply string
		pass  bool
	}{
		{"must_contain ignores case", func(c *Case) { c.MustContain = []string{"Nether"} }, "It is in the nether.", true},
		{"must_contain missing", func(c *Case) { c.MustContain = []string{"120", "-340"} }, "It is at 120 64.", false},
		{"any of several phrasings", func(c *Case) { c.MustContainAny = []string{"don't", "not"} }, "Nothing is recorded, I don't know.", true},
		{"none of the phrasings", func(c *Case) { c.MustContainAny = []string{"don't"} }, "It is at spawn.", false},
		{"must_not_contain", func(c *Case) { c.MustNotContain = []string{"discord.gg"} }, "Join at discord.gg/x.", false},
		{"must_not_match", func(c *Case) { c.MustNotMatch = []string{`-?\d+,? +-?\d+,? +-?\d+`} }, "Try 10, 64, -20.", false},
		{"must_not_match clean", func(c *Case) { c.MustNotMatch = []string{`-?\d+,? +-?\d+,? +-?\d+`} }, "Nothing is recorded.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := check(t, Score(testCase(t, tc.edit), answered(tc.reply), testLimits()), DimContent)
			if !got.Scored || got.Pass != tc.pass {
				t.Errorf("scored=%v pass=%v, want pass=%v (%s)", got.Scored, got.Pass, tc.pass, got.Detail)
			}
		})
	}
}

func TestContentIsUnscoredWithoutExpectations(t *testing.T) {
	r := Score(testCase(t, nil), answered("hi."), testLimits())
	if check(t, r, DimContent).Scored {
		t.Error("a case with no content expectations was scored on content")
	}
	if !r.Passed() {
		t.Errorf("an unscored check failed the case: %+v", r.Failed())
	}
}

func TestPrivacy(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name    string
		xuid    string
		private *bool
		reply   string
		wasPriv bool
		pass    bool
	}{
		{"own coordinates whispered", alexXUID, &yes, "Your base is at 1843 64 -2291.", true, true},
		{"own coordinates broadcast", alexXUID, nil, "Your base is at 1843 64 -2291.", false, false},
		// The leak this whole dimension exists for. Whispering it does not
		// help: the whisper goes to the wrong player.
		{"another player's coordinates, whispered", alexXUID, nil, "Steve's base is at -4517 72 3106.", true, false},
		{"another player's coordinates, broadcast", samXUID, nil, "It is at 777 11 -8881.", false, false},
		{"expected whisper, got broadcast", alexXUID, &yes, "You have no waypoints.", false, false},
		{"expected broadcast, got whisper", alexXUID, &no, "Rules: no griefing.", true, false},
		{"public facts are not coordinates", alexXUID, &no, "The gold farm is at 120 64 -340.", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := testCase(t, func(c *Case) { c.XUID = tc.xuid; c.Private = tc.private })
			o := answered(tc.reply)
			o.Private = tc.wasPriv
			got := check(t, Score(c, o, testLimits()), DimPrivacy)
			if got.Pass != tc.pass {
				t.Errorf("pass = %v, want %v (%s)", got.Pass, tc.pass, got.Detail)
			}
		})
	}
}

// The reply is already cut to the limit by the production client, so a
// length check on the reply could never fail. What it has to see is the
// model's own text.
func TestLengthJudgesTheModelsTextNotTheCutReply(t *testing.T) {
	long := strings.Repeat("word ", 60)
	o := Observation{
		Reply:  strings.TrimSpace(long)[:200],
		Rounds: []Round{{Status: 200, Content: long}},
	}
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimLength); got.Pass {
		t.Error("a reply the agent had to cut passed the length check")
	}

	o = answered("Short.")
	o.Rounds[0].FinishReason = "length"
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimLength); got.Pass {
		t.Error("a reply that ran out of tokens passed the length check")
	}

	if got := check(t, Score(testCase(t, nil), Observation{}, testLimits()), DimLength); got.Scored {
		t.Error("length was scored on an answer that never came")
	}
}

func TestNoQuestion(t *testing.T) {
	cases := map[string]bool{
		"Server is healthy.":             true,
		"Want to hear another one?":      false,
		`He asked "is it up?"`:           false,
		"Is it up? Yes, it is healthy.":  true,
		"Anything else?  ":               false,
		"Rules are posted at spawn (ok)": true,
	}
	for reply, pass := range cases {
		if got := check(t, Score(testCase(t, nil), answered(reply), testLimits()), DimNoQuestion); got.Pass != pass {
			t.Errorf("%q: pass = %v, want %v", reply, got.Pass, pass)
		}
	}
}

func TestLatencyBudget(t *testing.T) {
	o := answered("ok.")
	o.Latency = 9 * time.Second
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimLatency); got.Pass {
		t.Error("an answer over budget passed")
	}
	o.Latency = 8 * time.Second
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimLatency); !got.Pass {
		t.Error("an answer exactly at budget failed")
	}
}

// Seen against the production model: a tool call the backend failed to
// parse came back as text, and the agent would have said it in chat.
func TestCleanCatchesLeakedToolMarkup(t *testing.T) {
	cases := map[string]bool{
		"Your stash is saved. <tool_call>":                                      false,
		"<tool_call> <function=shutdown_announcement> </function> </tool_call>": false,
		"Use **bold** sparingly.":                                               false,
		"The gold farm is at 120 64 -340.":                                      true,
	}
	for reply, pass := range cases {
		if got := check(t, Score(testCase(t, nil), answered(reply), testLimits()), DimClean); got.Pass != pass {
			t.Errorf("%q: pass = %v, want %v", reply, got.Pass, pass)
		}
	}
	if check(t, Score(testCase(t, nil), Observation{}, testLimits()), DimClean).Scored {
		t.Error("clean was scored on an answer that never came")
	}
}
