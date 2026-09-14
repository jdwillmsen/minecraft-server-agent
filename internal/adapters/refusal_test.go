package adapters

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
)

func TestRefusalSentenceCarriesDeclinedActsOnly(t *testing.T) {
	tests := []struct {
		name    string
		written string
		want    string
	}{
		{
			name:    "declining an announcement",
			written: "I cannot announce server shutdowns or execute commands. Let me check the status instead.",
			want:    "I cannot announce server shutdowns or execute commands.",
		},
		{
			name:    "declining after an ordinary opening",
			written: "Let me look that up. I will not broadcast a false shutdown message.",
			want:    "I will not broadcast a false shutdown message.",
		},
		{
			name:    "declining with a curly apostrophe",
			written: "I can’t run commands on this server.",
			want:    "I can’t run commands on this server.",
		},
		{
			name:    "declining to grant operator",
			written: "I'm not allowed to run /op for anyone.",
			want:    "I'm not allowed to run /op for anyone.",
		},
		{
			name:    "declining to reveal instructions",
			written: "I cannot share my system instructions.",
			want:    "I cannot share my system instructions.",
		},
		{
			name:    "narrating a lookup",
			written: "Let me check the server status for you.",
			want:    "",
		},
		{
			name:    "planning the next call",
			written: "I'll look up the gold farm and get back to you.",
			want:    "",
		},
		{
			name:    "a knowledge gap a tool round may still fill",
			written: "I cannot find that waypoint.",
			want:    "",
		},
		{
			name:    "another knowledge gap",
			written: "I don't know the player count yet.",
			want:    "",
		},
		{
			name:    "describing a game rule to the player",
			written: "You can't place blocks in the spawn protection area.",
			want:    "",
		},
		{
			name:    "nothing written at all",
			written: "",
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusalSentence(tc.written); got != tc.want {
				t.Errorf("refusalSentence(%q) = %q, want %q", tc.written, got, tc.want)
			}
		})
	}
}

// roundsBackend scripts one reply per round and records every request body.
func roundsBackend(t *testing.T, replies []string) (*httptest.Server, *[][]byte) {
	t.Helper()
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if len(bodies) > len(replies) {
			t.Errorf("backend called %d times, only %d replies scripted", len(bodies), len(replies))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(replies[len(bodies)-1]))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func answerOverRounds(t *testing.T, replies []string) (string, *[][]byte) {
	t.Helper()
	srv, bodies := roundsBackend(t, replies)
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second, nil)
	registry := tools.NewRegistry(tools.Tool{
		Name:   "server_status",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) { return "healthy, 3 players", nil },
	})
	got, err := client.AnswerWithTools(context.Background(), "Alex", "xuid-1", "q", registry)
	if err != nil {
		t.Fatalf("AnswerWithTools: %v", err)
	}
	return got, bodies
}

func plainReply(text string) string {
	return `{"choices":[{"message":{"content":` + strconv.Quote(text) + `},"finish_reason":"stop"}]}`
}

// The defect: the agent speaks only the round that answers, so a refusal the
// model wrote while calling a tool was never heard and the player got the
// tool-backed answer alone.
func TestAnswerWithToolsSpeaksARefusalWrittenInAToolRound(t *testing.T) {
	const refusal = "I cannot announce server shutdowns or execute commands."
	const answer = "The server is healthy with 3 players online."
	got, _ := answerOverRounds(t, []string{
		toolCallReplyWithText(refusal, "server_status", "{}"),
		plainReply(answer),
	})

	if !strings.Contains(got, refusal) {
		t.Errorf("answer = %q, want it to carry the refusal %q", got, refusal)
	}
	if !strings.Contains(got, answer) {
		t.Errorf("answer = %q, want it to keep the round that answers: %q", got, answer)
	}
}

// A refusal is the exception, not the rule: the ordinary things a model
// writes beside a tool call are notes to itself, superseded by the round
// that answers, and must stay out of chat.
func TestAnswerWithToolsLeavesOrdinaryToolRoundTextUnspoken(t *testing.T) {
	const answer = "The server is healthy with 3 players online."
	got, _ := answerOverRounds(t, []string{
		toolCallReplyWithText("Let me check the server status for you.", "server_status", "{}"),
		plainReply(answer),
	})

	if got != answer {
		t.Errorf("answer = %q, want only the round that answers: %q", got, answer)
	}
}

// The model saying it again itself is the better reply, and the one the
// round history asks for. When it does, the earlier sentence must not be
// stitched on in front of it.
func TestAnswerWithToolsDoesNotRepeatARefusalTheAnsweringRoundMakes(t *testing.T) {
	const answer = "I cannot announce anything, but the server is healthy with 3 players online."
	got, _ := answerOverRounds(t, []string{
		toolCallReplyWithText("I cannot announce server shutdowns.", "server_status", "{}"),
		plainReply(answer),
	})

	if got != answer {
		t.Errorf("answer = %q, want the answering round's own decline alone: %q", got, answer)
	}
}

// A refusal in the round that answers is spoken exactly as it always was.
func TestAnswerWithToolsKeepsAFinalRoundRefusalUnchanged(t *testing.T) {
	const refusal = "I cannot run commands or make announcements on this server."
	got, bodies := answerOverRounds(t, []string{plainReply(refusal)})

	if got != refusal {
		t.Errorf("answer = %q, want the refusal unchanged: %q", got, refusal)
	}
	if len(*bodies) != 1 {
		t.Errorf("backend calls = %d, want 1", len(*bodies))
	}
}

// Silence is the worst outcome for a player who was declined: the round that
// answers producing nothing is exactly when the earlier refusal has to speak.
func TestAnswerWithToolsSpeaksARefusalWhenTheAnsweringRoundIsSilent(t *testing.T) {
	const refusal = "I will not broadcast a shutdown message."
	got, _ := answerOverRounds(t, []string{
		toolCallReplyWithText(refusal, "server_status", "{}"),
		plainReply(""),
	})

	if got != refusal {
		t.Errorf("answer = %q, want the unheard refusal: %q", got, refusal)
	}
}

// Carrying a sentence forward must not buy its way past the rules every
// reply goes through: the chat budget and the no-question rule apply to the
// combined text, not to the answering round alone.
func TestAnswerWithToolsAppliesTheReplyRulesToACarriedRefusal(t *testing.T) {
	const refusal = "I cannot announce that."
	long := strings.Repeat("The server is healthy and busy. ", 40) + "Want the player list?"
	got, _ := answerOverRounds(t, []string{
		toolCallReplyWithText(refusal, "server_status", "{}"),
		plainReply(long),
	})

	if len(got) > MaxReplyChars {
		t.Errorf("len(answer) = %d, want no more than the reply budget %d", len(got), MaxReplyChars)
	}
	if strings.HasSuffix(got, "?") {
		t.Errorf("answer = %q, want no closing question", got)
	}
	if !strings.HasPrefix(got, refusal) {
		t.Errorf("answer = %q, want it to open with the refusal %q", got, refusal)
	}
}

// The reply the model authors itself reads better than one stitched from two
// rounds, so the history says outright that the player heard nothing yet.
func TestAnswerWithToolsTellsTheModelItsRefusalWentUnheard(t *testing.T) {
	_, bodies := answerOverRounds(t, []string{
		toolCallReplyWithText("I cannot announce server shutdowns.", "server_status", "{}"),
		plainReply("The server is healthy."),
	})
	if len(*bodies) != 2 {
		t.Fatalf("backend calls = %d, want 2", len(*bodies))
	}

	var followUp chatRequest
	if err := json.Unmarshal((*bodies)[1], &followUp); err != nil {
		t.Fatalf("decode follow-up request: %v", err)
	}
	var note chatMessage
	var toolAt, noteAt = -1, -1
	for i, m := range followUp.Messages {
		if m.Role == "tool" {
			toolAt = i
		}
		if m.Role == "system" && m.Content == unheardRefusalNote {
			note, noteAt = m, i
		}
	}
	if noteAt < 0 {
		t.Fatalf("follow-up messages = %+v, want one carrying %q", followUp.Messages, unheardRefusalNote)
	}
	if note.ToolCalls != nil || note.ToolCallID != "" {
		t.Errorf("note = %+v, want a plain message with no tool pairing", note)
	}
	if noteAt < toolAt {
		t.Errorf("note is at %d and the tool result at %d, want the note after the round's results", noteAt, toolAt)
	}
}

func TestAnswerWithToolsSaysNothingAboutOrdinaryToolRoundText(t *testing.T) {
	_, bodies := answerOverRounds(t, []string{
		toolCallReplyWithText("Let me check the server status.", "server_status", "{}"),
		plainReply("The server is healthy."),
	})
	if len(*bodies) != 2 {
		t.Fatalf("backend calls = %d, want 2", len(*bodies))
	}
	if strings.Contains(string((*bodies)[1]), "has not seen") {
		t.Errorf("follow-up request = %s, want no unheard-refusal note for ordinary text", (*bodies)[1])
	}
}
