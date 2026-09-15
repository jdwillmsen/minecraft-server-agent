package plugins

import (
	"context"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// reservedTopics collides with the subcommand names runKB switches on. A
// fact stored under one would be unreadable through !kb <topic> (the switch
// would treat it as list/set/del) while the LLM's lookup path reads the
// store directly and would still find it -- refusing the write keeps both
// surfaces agreeing on what the agent knows.
var reservedTopics = map[string]bool{"list": true, "set": true, "del": true, "help": true}

// kbHelpReply is what !kb help answers. Reads are open to everyone, so it
// names the write subcommands as operator-only rather than hiding them: a
// member who tries !kb set should learn why it refused, not that it exists.
const kbHelpReply = "!kb <topic> = look up. !kb list = topics I know. " +
	"!kb set <topic> <text> = teach me (operator). !kb del <topic> = make me forget (operator)."

// Knowledge exposes the curated fact store in chat.
//
// One command with a subcommand rather than !kb, !kbset and !kbdel: !help
// lists every command to every player, and three entries for one feature
// crowds out the rest on a Bedrock chat line.
type Knowledge struct{}

// NewKnowledge builds the knowledge plugin.
func NewKnowledge() *Knowledge { return &Knowledge{} }

func (*Knowledge) Name() string { return "knowledge" }

func (*Knowledge) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "kb",
			Description: "Look up what the server knows: !kb <topic>, !kb list, !kb help.",
			// Visitor, because reads are open. The write subcommands check
			// the actor's level themselves -- see runKB.
			Permission: plugin.PermissionVisitor,
			Run:        runKB,
		},
	}
}

func runKB(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if pctx.Knowledge == nil || !pctx.Knowledge.Enabled() {
		return "I have no knowledge store configured.", nil
	}
	if len(inv.Args) == 0 {
		return "Usage: !kb <topic>, !kb list, !kb set <topic> <text>, !kb del <topic>.", nil
	}

	switch strings.ToLower(inv.Args[0]) {
	case "help":
		// Without this, "!kb help" is a lookup for a topic called help and
		// answers "I don't know anything about help", which a player cannot
		// tell apart from the command being broken.
		return kbHelpReply, nil

	case "list":
		entries, err := pctx.Knowledge.List(ctx)
		if err != nil {
			return "", fmt.Errorf("knowledge: !kb list: %w", err)
		}
		if len(entries) == 0 {
			return "I know nothing yet. An operator can teach me with !kb set.", nil
		}
		topics := make([]string, 0, len(entries))
		for _, e := range entries {
			topics = append(topics, e.Topic)
		}
		return "I know about: " + strings.Join(topics, ", "), nil

	case "set":
		if inv.ActorPermission < plugin.PermissionOperator {
			return "Only an operator can teach me new facts.", nil
		}
		if len(inv.Args) < 3 {
			return "Usage: !kb set <topic> <text>.", nil
		}
		topic, body := inv.Args[1], strings.Join(inv.Args[2:], " ")
		if reservedTopics[knowledge.NormalizeTopic(topic)] {
			return "That name is reserved: pick another topic.", nil
		}
		if err := pctx.Knowledge.Upsert(ctx, topic, body, inv.ActorXUID); err != nil {
			return "", fmt.Errorf("knowledge: !kb set: %w", err)
		}
		return "Learned: " + topic + ".", nil

	case "del":
		if inv.ActorPermission < plugin.PermissionOperator {
			return "Only an operator can make me forget.", nil
		}
		if len(inv.Args) < 2 {
			return "Usage: !kb del <topic>.", nil
		}
		topic := knowledge.NormalizeTopic(inv.Args[1])
		removed, err := pctx.Knowledge.Delete(ctx, topic)
		if err != nil {
			return "", fmt.Errorf("knowledge: !kb del: %w", err)
		}
		if !removed {
			return "I know nothing about " + topic + ", so there is nothing to forget.", nil
		}
		return "Forgotten: " + topic + ".", nil

	default:
		query := strings.Join(inv.Args, " ")
		entries, err := pctx.Knowledge.Lookup(ctx, query, 1)
		if err != nil {
			return "", fmt.Errorf("knowledge: !kb: %w", err)
		}
		if len(entries) == 0 {
			return "I don't know anything about " + query + ".", nil
		}
		// Neither weak match is a lookup of the topic the player actually
		// meant -- a fallback-only row (ts_rank 0, found by substring
		// alone) is a guess, and a partial row answers a different compound
		// that happens to share its head word. Stating either as fact the
		// way a full-text hit deserves hands a player an unrelated fact
		// with the same confidence as a real answer.
		e := entries[0]
		switch e.Matched {
		case knowledge.MatchFallback:
			return "Closest I have is " + e.Topic + ": " + e.Body, nil
		case knowledge.MatchPartial:
			return "I have nothing on that. Closest I have is " + e.Topic + ": " + e.Body, nil
		}
		return e.Topic + ": " + e.Body, nil
	}
}
