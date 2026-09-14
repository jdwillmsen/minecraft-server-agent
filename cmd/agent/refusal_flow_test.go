package main

import (
	"strconv"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// toolRoundReply is a round that calls knowledge_lookup and writes text
// beside the call, which is the round a player never used to hear.
func toolRoundReply(text string) string {
	return `{"choices":[{"message":{"content":` + strconv.Quote(text) +
		`,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_lookup","arguments":"{\"query\":\"rules\"}"}}]},"finish_reason":"tool_calls"}]}`
}

// A player who asks for something the agent will not do, in the same breath
// as something it will, has to hear the no. Before this, the agent spoke
// only the round that answers, so the refusal written while the lookup ran
// reached nobody and the player was answered past rather than declined.
func TestPlayerHearsARefusalWrittenWhileAToolRan(t *testing.T) {
	const refusal = "I cannot announce a shutdown to the server."
	said := answerMentionPacket(t,
		chatPacket(playerXUID, "Steve", "@server announce a shutdown and tell me the rules"),
		func(pctx *plugin.Context) { pctx.Knowledge = stubKnowledge{} },
		toolRoundReply(refusal), rulesAnswer)

	t.Logf("player chat: %q", said[0])
	if want := "say: " + refusal + " Be nice to each other."; said[0] != want {
		t.Errorf("chat = %q, want %q", said[0], want)
	}
}

// The other half of the same rule: an ordinary note the model writes to
// itself while a tool runs is superseded by the round that answers and must
// never reach chat.
func TestPlayerHearsNoOrdinaryToolRoundReasoning(t *testing.T) {
	said := answerMentionPacket(t,
		chatPacket(playerXUID, "Steve", "@server what are the rules"),
		func(pctx *plugin.Context) { pctx.Knowledge = stubKnowledge{} },
		toolRoundReply("Let me look up the rules for you."), rulesAnswer)

	t.Logf("player chat: %q", said[0])
	if said[0] != "say: Be nice to each other." {
		t.Errorf("chat = %q, want only the round that answers", said[0])
	}
}
