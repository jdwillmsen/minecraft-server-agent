package plugins

import (
	"context"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

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
			Description: "Look up what the server knows: !kb <topic>, !kb list.",
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
		if err := pctx.Knowledge.Delete(ctx, inv.Args[1]); err != nil {
			return "", fmt.Errorf("knowledge: !kb del: %w", err)
		}
		return "Forgotten: " + inv.Args[1] + ".", nil

	default:
		query := strings.Join(inv.Args, " ")
		entries, err := pctx.Knowledge.Lookup(ctx, query, 1)
		if err != nil {
			return "", fmt.Errorf("knowledge: !kb: %w", err)
		}
		if len(entries) == 0 {
			return "I don't know anything about " + query + ".", nil
		}
		return entries[0].Topic + ": " + entries[0].Body, nil
	}
}
