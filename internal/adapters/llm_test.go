package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
)

func newTestClient(url string) *LLMClient {
	return NewLLMClient(url, "test-model", "", 96, 2*time.Second)
}

func okResponse(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnswerReturnsTheReply(t *testing.T) {
	got, err := newTestClient(okResponse(t, "Diamonds spawn below Y level 16.").URL).
		Answer(context.Background(), "Steve", "where is diamond")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if got != "Diamonds spawn below Y level 16." {
		t.Errorf("Answer = %q", got)
	}
}

// Bedrock chat is one line. A model that wraps its answer must not produce a
// multi-line chat message.
func TestAnswerCollapsesWhitespace(t *testing.T) {
	got, err := newTestClient(okResponse(t, "line one\n\n  line two\ttabbed").URL).
		Answer(context.Background(), "Steve", "q")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if strings.ContainsAny(got, "\n\t") || strings.Contains(got, "  ") {
		t.Errorf("Answer = %q, want a single collapsed line", got)
	}
}

func TestAnswerTruncatesLongReplies(t *testing.T) {
	got, err := newTestClient(okResponse(t, strings.Repeat("x", 500)).URL).
		Answer(context.Background(), "Steve", "q")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(got) != MaxReplyChars {
		t.Errorf("len(Answer) = %d, want %d", len(got), MaxReplyChars)
	}
}

// The question is player-typed and goes to a model; it is bounded before it
// leaves the process.
func TestBuildRequestTruncatesTheQuestion(t *testing.T) {
	_, _, body := newTestClient("http://x").BuildRequest("Steve", strings.Repeat("q", 999))
	user := body.Messages[len(body.Messages)-1].Content
	if len(user) > MaxQuestionChars+len("Steve asked: ") {
		t.Errorf("user message length %d, want the question truncated to %d", len(user), MaxQuestionChars)
	}
}

// An empty auth header is not the same as no auth header: some
// OpenAI-compatible backends reject the former, and the cluster's vLLM needs
// neither.
func TestBuildRequestOmitsEmptyAuthHeader(t *testing.T) {
	_, headers, _ := NewLLMClient("http://x", "m", "", 96, time.Second).BuildRequest("a", "b")
	if _, present := headers["authorization"]; present {
		t.Error("authorization header present despite an empty API key")
	}
	_, headers, _ = NewLLMClient("http://x", "m", "secret", 96, time.Second).BuildRequest("a", "b")
	if headers["authorization"] != "Bearer secret" {
		t.Errorf("authorization = %q", headers["authorization"])
	}
}

// The prompt is where loop safety lives, so it is asserted rather than assumed.
func TestSystemPromptForbidsEndingOnAQuestion(t *testing.T) {
	_, _, body := newTestClient("http://x").BuildRequest("Steve", "q")
	sys := body.Messages[0].Content
	if !strings.Contains(sys, "never end your reply with a question mark") {
		t.Errorf("system prompt lost its loop-safety clause: %q", sys)
	}
}

// A misbehaving backend must make the agent fall silent, not error into chat.
func TestExtractTextFallsSilentOnJunk(t *testing.T) {
	for name, payload := range map[string]string{
		"not json":        "<html>502</html>",
		"no choices":      `{"choices":[]}`,
		"null content":    `{"choices":[{"message":{"content":null}}]}`,
		"missing message": `{"choices":[{}]}`,
	} {
		if got := ExtractText([]byte(payload)); got != "" {
			t.Errorf("%s: ExtractText = %q, want empty", name, got)
		}
	}
}

func TestAnswerReportsBackendErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	if _, err := newTestClient(srv.URL).Answer(context.Background(), "a", "b"); err == nil {
		t.Fatal("want an error for a 500 backend, got none")
	}
}

// No backend configured is a supported state, not a failure: it is how the
// agent runs before this feature is switched on.
func TestDisabledClientAnswersNothingWithoutError(t *testing.T) {
	c := NewLLMClient("", "m", "", 96, time.Second)
	if c.Enabled() {
		t.Error("Enabled() true for an empty base URL")
	}
	got, err := c.Answer(context.Background(), "a", "b")
	if err != nil || got != "" {
		t.Errorf("Answer = (%q, %v), want empty and no error", got, err)
	}
}

func toolBackend(t *testing.T, replies []string) (*httptest.Server, *int) {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if calls >= len(replies) {
			t.Errorf("backend called %d times, only %d replies scripted", calls+1, len(replies))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply := replies[calls]
		calls++
		// The final call must not offer tools -- that is what forces text.
		if calls == len(replies) && strings.Contains(string(body), `"tools"`) && len(replies) > MaxToolRounds {
			t.Errorf("final call still offered tools: %s", body)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const textReply = `{"choices":[{"message":{"content":"The gold farm is under spawn."},"finish_reason":"stop"}]}`

func toolCallReply(name, args string) string {
	return `{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"` +
		name + `","arguments":` + strconv.Quote(args) + `}}]},"finish_reason":"tool_calls"}]}`
}

func TestAnswerWithToolsOneRound(t *testing.T) {
	srv, calls := toolBackend(t, []string{toolCallReply("knowledge_lookup", `{"query":"gold farm"}`), textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)

	var gotCaller string
	registry := tools.NewRegistry(tools.Tool{
		Name:   "knowledge_lookup",
		Schema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
		Invoke: func(_ context.Context, args json.RawMessage, caller string) (string, error) {
			gotCaller = caller
			return "gold farm: under spawn at y 12", nil
		},
	})

	got, err := client.AnswerWithTools(context.Background(), "Dot", "xuid-1", "where is the gold farm", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if got != "The gold farm is under spawn." {
		t.Errorf("answer = %q", got)
	}
	if *calls != 2 {
		t.Errorf("backend calls = %d, want 2", *calls)
	}
	if gotCaller != "xuid-1" {
		t.Errorf("tool caller = %q, want the asking XUID", gotCaller)
	}
}

func TestAnswerWithToolsStopsAtRoundCap(t *testing.T) {
	// Three tool-call replies, but the cap is 2 rounds: the client must stop
	// asking for tools and take the third call's text.
	srv, calls := toolBackend(t, []string{
		toolCallReply("t", "{}"), toolCallReply("t", "{}"), textReply,
	})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)
	registry := tools.NewRegistry(tools.Tool{
		Name:   "t",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) { return "ok", nil },
	})

	got, err := client.AnswerWithTools(context.Background(), "Dot", "xuid-1", "q", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if got == "" {
		t.Error("expected the forced text answer")
	}
	if *calls != 3 {
		t.Errorf("backend calls = %d, want 3 (2 tool rounds + 1 forced)", *calls)
	}
}

func TestAnswerWithToolsHandlesToolErrorAndUnknownTool(t *testing.T) {
	srv, _ := toolBackend(t, []string{toolCallReply("broken", "{}"), textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)
	registry := tools.NewRegistry(tools.Tool{
		Name:   "broken",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) {
			return "", errors.New("bridge unreachable")
		},
	})
	got, err := client.AnswerWithTools(context.Background(), "Dot", "x", "q", registry)
	if err != nil {
		t.Fatalf("a failing tool must not fail the answer: %v", err)
	}
	if got == "" {
		t.Error("expected the model's answer despite the tool error")
	}

	srv2, _ := toolBackend(t, []string{toolCallReply("invented_tool", "{}"), textReply})
	client2 := NewLLMClient(srv2.URL, "m", "", 192, 5*time.Second)
	if _, err := client2.AnswerWithTools(context.Background(), "Dot", "x", "q", registry); err != nil {
		t.Fatalf("an invented tool name must not fail the answer: %v", err)
	}
}

func TestAnswerWithNilRegistryMakesOneCall(t *testing.T) {
	srv, calls := toolBackend(t, []string{textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second)
	if _, err := client.AnswerWithTools(context.Background(), "Dot", "x", "q", nil); err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	if *calls != 1 {
		t.Errorf("backend calls = %d, want 1 when no tools are offered", *calls)
	}
}
