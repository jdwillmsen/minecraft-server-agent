package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
)

// scriptedBackend answers each chat-completions call with the next canned
// payload and keeps the request bodies it received.
type scriptedBackend struct {
	mu       sync.Mutex
	replies  []string
	requests []string
	auth     []string
}

func (b *scriptedBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, string(body))
	b.auth = append(b.auth, r.Header.Get("Authorization"))
	if len(b.replies) == 0 {
		http.Error(w, "script exhausted", http.StatusInternalServerError)
		return
	}
	reply := b.replies[0]
	b.replies = b.replies[1:]
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, reply)
}

func toolCallPayload(name, args string) string {
	return fmt.Sprintf(`{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, name, args)
}

// toolCallWithTextPayload is the round a model writes prose in while asking
// for a tool, which is where a refusal the player never hears comes from.
func toolCallWithTextPayload(content, name, args string) string {
	encoded, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"finish_reason":"tool_calls","message":{"content":%s,"tool_calls":[{"id":"c1","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`, encoded, name, args)
}

func textPayload(content string) string {
	encoded, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"content":%s}}],"usage":{"completion_tokens":12}}`, encoded)
}

func newTestRunner(t *testing.T, backend http.Handler) runner {
	t.Helper()
	upstream := httptest.NewServer(backend)
	t.Cleanup(upstream.Close)
	recorder := NewRecorder(upstream.URL + "/v1")
	base, stop, err := recorder.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return runner{
		client:   adapters.NewLLMClient(base, "test-model", "k", 192, 2*time.Second, nil),
		recorder: recorder,
		total:    5 * time.Second,
		world:    fixtureContext,
	}
}

// End to end through the production client, offline: the model reads the
// asker's base, answers, and the harness sees the tool call, the whisper
// and the reply exactly as production would have acted on them.
func TestRunnerObservesToolsWhisperAndReply(t *testing.T) {
	backend := &scriptedBackend{replies: []string{
		toolCallPayload("waypoint_lookup", `{"name":"base"}`),
		textPayload("Your base is at 1843 64 -2291."),
	}}
	r := newTestRunner(t, backend)
	yes := true
	c := testCase(t, func(c *Case) {
		c.Tools = ToolExpectation{AllOf: []string{"waypoint_lookup"}}
		c.MustContain = []string{"1843"}
		c.Private = &yes
	})

	obs := r.run(t.Context(), c)
	if obs.Err != nil {
		t.Fatal(obs.Err)
	}
	if !obs.Private {
		t.Error("an answer built from the asker's waypoints was not marked for a whisper")
	}
	if got := obs.ToolsCalled(); len(got) != 1 || got[0] != "waypoint_lookup" {
		t.Errorf("tools called = %v", got)
	}
	if len(obs.Rounds) != 2 || obs.Rounds[0].ToolsOffered != 8 || obs.Rounds[1].CompletionTokens != 12 {
		t.Errorf("rounds = %+v", obs.Rounds)
	}
	// The waypoint tool reads the case's XUID, not whatever the model said.
	if !strings.Contains(backend.requests[1], "1843 64 -2291") {
		t.Errorf("second request did not carry the asker's own waypoint: %s", backend.requests[1])
	}
	if backend.auth[0] != "Bearer k" {
		t.Errorf("recorder forwarded auth %q", backend.auth[0])
	}
	if res := Score(c, obs, testLimits()); !res.Passed() {
		t.Errorf("failed: %+v", res.Failed())
	}
}

// The empirical claim the scorer's inputs rest on, driven through the
// production client rather than assumed: the agent carries a refusal out of
// a tool round, so the line the player hears holds a sentence the round
// that answered never wrote, and a fact invented in that sentence is
// invisible to any check reading the answering round alone.
func TestRunnerDeliversARefusalTheAnsweringRoundNeverWrote(t *testing.T) {
	backend := &scriptedBackend{replies: []string{
		toolCallWithTextPayload("I can't announce that to the 47 players online.", "server_status", `{}`),
		textPayload("The server is up with 3 players online."),
	}}
	r := newTestRunner(t, backend)
	c := testCase(t, nil)

	obs := r.run(t.Context(), c)
	if obs.Err != nil {
		t.Fatal(obs.Err)
	}
	if !strings.Contains(obs.Reply, "47 players online") {
		t.Fatalf("no refusal was carried into the delivered line: %q", obs.Reply)
	}
	if written, _ := modelText(obs); strings.Contains(written, "47") {
		t.Fatalf("the round that answered wrote the refusal itself: %q", written)
	}
	if got := check(t, Score(c, obs, testLimits()), DimGrounded); got.Pass {
		t.Error("a count invented in a carried refusal passed")
	}
}

func TestRunnerReportsAnUnreachableBackendAsUnanswered(t *testing.T) {
	backend := &scriptedBackend{}
	r := newTestRunner(t, backend)
	c := testCase(t, nil)

	obs := r.run(t.Context(), c)
	if obs.Err == nil {
		t.Fatal("no error from a backend that answered 500")
	}
	if check(t, Score(c, obs, testLimits()), DimAnswered).Pass {
		t.Error("a failed call scored as answered")
	}
	if len(obs.Rounds) != 1 || obs.Rounds[0].Status != http.StatusInternalServerError {
		t.Errorf("rounds = %+v", obs.Rounds)
	}
}

// A call the client abandoned can finish upstream after the next case has
// started. It must not be counted as part of that case.
func TestRecorderDropsExchangesFromAnEarlierCase(t *testing.T) {
	rec := NewRecorder("http://unused.invalid")
	rec.Reset()
	stale := rec.gen
	rec.Reset()
	rec.record(stale, Round{Status: 200, Content: "late"})
	if got := rec.Rounds(); len(got) != 0 {
		t.Errorf("stale exchange recorded: %+v", got)
	}
}

func TestParseRoundToleratesJunk(t *testing.T) {
	round := parseRound(200, []byte(`{"tools":[{},{}]}`), []byte("not json"))
	if round.ToolsOffered != 2 || round.Content != "" || len(round.ToolCalls) != 0 {
		t.Errorf("round = %+v", round)
	}
}

func TestRunWithoutAnEndpointIsAnError(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"-model", "m"}, &out, &errOut); code != 1 || !strings.Contains(out.String(), "no endpoint") {
		t.Errorf("code %d, stdout %q", code, out.String())
	}
}
