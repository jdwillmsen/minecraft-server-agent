package toolset

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

const (
	steveXUID = "2535400000000002"
	alexXUID  = "2535400000000001"
)

// scriptedModel answers each request with the next canned completion. It
// fails the test if it is asked more often than the script allows, which is
// how a test that expects no model call at all says so.
func scriptedModel(t *testing.T, replies ...string) *httptest.Server {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls >= len(replies) {
			t.Errorf("the model was asked %d times, only %d replies scripted", calls+1, len(replies))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply := replies[calls]
		calls++
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A question naming another player is answered by the agent itself, so the
// store is never read: nothing personal reaches the answer, and with
// nothing personal in it there is no coordinate a broadcast could publish.
// That is what makes answering in code safe to do before the whisper
// decision rather than after it.
func TestAnotherPlayersWaypointReadsNoCoordinatesAtAll(t *testing.T) {
	store := &recordingWaypoints{}
	registry, scoped := Build(&plugin.Context{Waypoints: store})
	client := adapters.NewLLMClient(scriptedModel(t).URL, "m", "", 192, 2*time.Second, nil)

	reply, err := client.AnswerWithTools(t.Context(), "Steve", steveXUID, "where is Alex's base", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if reply == "" {
		t.Fatal("answer was empty, which the agent says nothing for")
	}
	if strings.ContainsAny(reply, "0123456789") {
		t.Errorf("answer = %q, want no number a player could read as a coordinate", reply)
	}
	if store.askedFor != "" {
		t.Errorf("the waypoint store was read for %q, want it untouched", store.askedFor)
	}
	if scoped.Happened() {
		t.Error("the answer was marked personal, though no waypoint was read for it")
	}
}

// The ordinary case, end to end and unchanged: the model calls the tool,
// the tool reads the asker's own waypoint, and the answer carrying it is
// marked personal so handleMention whispers it.
func TestTheAskersOwnWaypointQuestionStillWhispers(t *testing.T) {
	store := &recordingWaypoints{}
	registry, scoped := Build(&plugin.Context{Waypoints: store})

	lookup := `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function",` +
		`"function":{"name":"waypoint_lookup","arguments":"{\"name\":\"base\"}"}}]},"finish_reason":"tool_calls"}]}`
	answer := `{"choices":[{"message":{"content":"Your base is at 1 2 3 in the overworld."},"finish_reason":"stop"}]}`
	client := adapters.NewLLMClient(scriptedModel(t, lookup, answer).URL, "m", "", 192, 2*time.Second, nil)

	reply, err := client.AnswerWithTools(t.Context(), "Alex", alexXUID, "where is my base", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if reply != "Your base is at 1 2 3 in the overworld." {
		t.Errorf("answer = %q, want the model's own reply", reply)
	}
	if store.askedFor != alexXUID {
		t.Errorf("the waypoint store was read for %q, want the asking player", store.askedFor)
	}
	if !scoped.Happened() {
		t.Error("an answer built from the asker's own waypoint was not marked personal")
	}
}

// The tool argument shape the deflection exists because of: the model names
// a waypoint and nothing else, so the owner a question named can never
// reach the tool.
func TestWaypointLookupTakesNoOwnerArgument(t *testing.T) {
	registry, _ := Build(&plugin.Context{Waypoints: &recordingWaypoints{}})
	for _, d := range registry.Definitions() {
		if d.Function.Name != "waypoint_lookup" {
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(d.Function.Parameters, &schema); err != nil {
			t.Fatalf("waypoint_lookup schema: %v", err)
		}
		if len(schema.Properties) != 1 || schema.Properties["name"] == nil {
			t.Errorf("waypoint_lookup arguments = %v, want only the waypoint name", schema.Properties)
		}
		return
	}
	t.Fatal("waypoint_lookup was not registered")
}
