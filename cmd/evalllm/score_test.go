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
		Facts:         fixtureFacts(),
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

// Production cleanup strips markup and closing questions from the reply, so
// clean and no_question must judge what the model wrote or they would pass
// by construction.
func TestCleanAndNoQuestionJudgeTheModelsOwnText(t *testing.T) {
	o := answered("Your stash is saved.")
	o.Rounds[0].Content = "Your stash is saved. <tool_call>"
	if check(t, Score(testCase(t, nil), o, testLimits()), DimClean).Pass {
		t.Error("markup the model wrote passed because the reply had it cut")
	}

	o = answered("Your stash is saved.")
	o.Rounds[0].Content = "Your stash is saved. Anything else?"
	if check(t, Score(testCase(t, nil), o, testLimits()), DimNoQuestion).Pass {
		t.Error("a closing question the model wrote passed because the reply had it trimmed")
	}

	o = Observation{Latency: time.Second, Rounds: []Round{{Status: 200, Content: "How can I help?"}}}
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimNoQuestion); !got.Scored || got.Pass {
		t.Errorf("a question the cleanup emptied: scored = %v, pass = %v, want a scored failure", got.Scored, got.Pass)
	}
}

// A failed answer reached nobody, so the text of an earlier round must not
// be scored as if it had.
func TestCleanAndNoQuestionSkipAnAnswerThatFailed(t *testing.T) {
	o := Observation{
		Err:     errors.New("llm: backend returned HTTP 500"),
		Latency: time.Second,
		Rounds: []Round{
			{Status: 200, Content: "Let me look that up.", ToolCalls: []ToolCall{{Name: "server_status", Arguments: "{}"}}},
			{Status: 500},
		},
	}
	r := Score(testCase(t, nil), o, testLimits())
	for _, d := range []Dimension{DimClean, DimNoQuestion} {
		if check(t, r, d).Scored {
			t.Errorf("%s was scored on an answer that failed", d)
		}
	}
}

// A refusal carried out of a tool round is a sentence the player hears that
// no round's content holds, so a check reading the model's own text alone
// never sees it. The cleanup it passes through cuts tool markup and trims a
// closing question; markdown and a stated fact it leaves, so those are what
// arrive in chat unexamined.
func TestCleanAndGroundedJudgeWhatTheAgentStitchedOn(t *testing.T) {
	o := answered("The server is up.")
	o.Reply = "**I can't run that command.** The server is up."
	if check(t, Score(testCase(t, nil), o, testLimits()), DimClean).Pass {
		t.Error("markdown in a carried refusal passed because no round wrote it")
	}

	o = answered("The server is up.")
	o.Reply = "I can't announce that to the 47 players online. The server is up."
	if check(t, Score(testCase(t, nil), o, testLimits()), DimGrounded).Pass {
		t.Error("a count invented in a carried refusal passed because no round wrote it")
	}

	// no_question stays on the model's own text: a refusal goes in front of
	// the reply and never behind it, so nothing stitched on can change what
	// the reply ends on.
	o = answered("The server is up.")
	o.Reply = "Anything else?"
	if !check(t, Score(testCase(t, nil), o, testLimits()), DimNoQuestion).Pass {
		t.Error("no_question judged the delivered line instead of the model's own text")
	}
}

// A deflection the agent writes in code reaches the player with no model
// round behind it at all. Judging it is the point: a line nobody scored is
// not a line that passed, and the next mechanism to answer without the
// model inherits the same treatment.
func TestCleanAndGroundedJudgeAReplyNoRoundWrote(t *testing.T) {
	o := Observation{Reply: "I can only look up your own waypoints, not another player's.", Latency: time.Second}
	r := Score(testCase(t, nil), o, testLimits())
	for _, d := range []Dimension{DimClean, DimGrounded} {
		if got := check(t, r, d); !got.Scored || !got.Pass {
			t.Errorf("%s: scored = %v, pass = %v, want a scored pass", d, got.Scored, got.Pass)
		}
	}
	if check(t, r, DimNoQuestion).Scored {
		t.Error("no_question was scored on a line the model did not write")
	}
}

// The chat limit cuts on a byte, so a reply at the cap can end mid-version.
// The whole of it was judged on the model's own text; the fragment left in
// the line is the same claim, not a second one.
func TestGroundedIgnoresAVersionTheChatCutHalved(t *testing.T) {
	o := answered("The server is running 1.21.100.7.")
	o.Reply = "The server is running 1.21.10…"
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimGrounded); !got.Pass {
		t.Errorf("a version the cut halved was reported as invented: %s", got.Detail)
	}
}

// The three replies that motivated this check were real: each passed the
// whole suite by calling no tool, while telling the asker a version and a
// player count the fixture world contradicts.
func TestGroundedSeparatesInventedServerFactsFromHonestOnes(t *testing.T) {
	tests := []struct {
		name     string
		question string
		reply    string
		pass     bool
	}{
		{"a joke and nothing else", "tell me a minecraft joke", "Why did the creeper go to therapy? It had too much pent-up energy.", true},
		{"invented version and count", "tell me a minecraft joke", "I don't know any Minecraft jokes, but I can tell you that the server is running on version 1.20.40 and has 24 players online.", false},
		{"invented version alone", "tell me a minecraft joke", "I don't know any Minecraft jokes, but I can tell you that the server is running on Bedrock version 1.20.40.", false},
		{"invented count alone", "hi server!", "Hey there, 12 people are on right now.", false},
		{"a two-part version a word introduces", "tell me a minecraft joke", "No jokes here, but the server is on Bedrock 1.20.", false},
		{"the fixture version is not an invention", "tell me a minecraft joke", "No jokes, but the server is running 1.21.100.7.", true},
		{"a truthful shortening of it", "what version is the server on", "The server runs Bedrock 1.21.", true},
		{"a version the asker named", "can i join from bedrock 1.20.80 or do i need to update", "1.20.80 is older than the server, so you need to update.", true},
		{"a count the asker named", "are there 24 players on right now", "No, not 24 players.", true},
		{"the fixture counts", "how many people are on", "There are 3/10 players online.", true},
		{"a backup size is a quantity, not a version", "when was the world last backed up", "The world was backed up 3 hours ago, 1.4 GiB.", true},
		{"a version is not a player count", "what version is the server on", "It is running 1.21.100.7 online.", true},
		{"coordinates are not versions or counts", "wheres the gold farm", "The gold farm is in the nether at 120 64 -340.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testCase(t, func(c *Case) { c.Question = tt.question })
			got := check(t, Score(c, answered(tt.reply), testLimits()), DimGrounded)
			if got.Pass != tt.pass {
				t.Errorf("pass = %v, want %v (%s)", got.Pass, tt.pass, got.Detail)
			}
		})
	}
}

// A fabrication in a reply too long for chat is still a fabrication: the
// production cut would remove it from the reply and from nothing else.
func TestGroundedJudgesWhatTheModelWroteNotTheCutReply(t *testing.T) {
	o := answered("The server is running version 1.20.40.")
	o.Reply = "The server is running"
	if got := check(t, Score(testCase(t, nil), o, testLimits()), DimGrounded); got.Pass {
		t.Error("a claim the chat limit cut passed as grounded")
	}
	if got := check(t, Score(testCase(t, nil), Observation{Err: errors.New("boom")}, testLimits()), DimGrounded); got.Scored {
		t.Error("an answer that never arrived was scored for its facts")
	}
}

// One invented version is one failure. The patterns read "version 1.20.41"
// twice -- whole, and as the "1.20" the word in front of it introduces --
// and a report that listed both would read as two separate inventions.
func TestGroundedReportsOneProblemPerInventedVersion(t *testing.T) {
	got := check(t, Score(testCase(t, nil), answered("The server is running version 1.20.41."), testLimits()), DimGrounded)
	if got.Pass || strings.Count(got.Detail, "stated version") != 1 {
		t.Errorf("detail = %q, want exactly one invented version", got.Detail)
	}
}

// A version the asker named is a source for a shortening of it too: the
// reply is repeating the question, not contradicting the fixtures.
func TestGroundedAcceptsAShorteningOfTheVersionTheAskerNamed(t *testing.T) {
	c := testCase(t, func(c *Case) { c.Question = "can i join from bedrock 1.20.80 or do i need to update" })
	if got := check(t, Score(c, answered("Bedrock 1.20 is too old, update."), testLimits()), DimGrounded); !got.Pass {
		t.Errorf("a version the question named failed: %s", got.Detail)
	}
}
