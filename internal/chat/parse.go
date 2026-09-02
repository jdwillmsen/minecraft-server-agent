// Package chat turns raw Bedrock text packets into agent-relevant facts:
// who sent it (by XUID, never by the spoofable gamertag), whether it's
// something the agent should react to (an @server mention or a ! command),
// and whether it should be ignored as our own echo or a sibling bot's.
package chat

import (
	"strings"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// ServerOrigin is the sentinel identity used for console-originated messages
// (e.g. `send-command say ...`), which arrive with both SourceName and XUID
// empty.
const ServerOrigin = "<server>"

// CommandPrefix marks a chat line as a bot command, e.g. "!help".
const CommandPrefix = "!"

// MentionToken is the case-insensitive substring that triggers an @server
// answer, e.g. "@server how do I craft a beacon".
const MentionToken = "@server"

// answerableTypes are the Text packet types that represent a message a real
// person could have typed, as opposed to popups/tips/system text.
var answerableTypes = map[byte]struct{}{
	packet.TextTypeChat:         {},
	packet.TextTypeWhisper:      {},
	packet.TextTypeAnnouncement: {},
}

// IsAnswerableType reports whether textType is one the agent should ever
// look at for commands or mentions.
func IsAnswerableType(textType byte) bool {
	_, ok := answerableTypes[textType]
	return ok
}

// Identity resolves the XUID-based identity of a Text packet's sender.
//
// A real player always carries a non-empty XUID on chat/whisper/announcement
// packets, so that XUID is the identity — SourceName is never trusted for
// identity because it is attacker-controlled (a player can set an arbitrary
// display name).
//
// A console-originated message (send-command say/tellraw) carries both an
// empty XUID and an empty SourceName; that combination resolves to
// ServerOrigin. A message with an empty XUID but a non-empty SourceName is
// unidentifiable and is rejected — trusting the name alone would let a
// player impersonate the server sentinel.
func Identity(pk *packet.Text) (id string, ok bool) {
	if pk.XUID != "" {
		return pk.XUID, true
	}
	if pk.SourceName == "" {
		return ServerOrigin, true
	}
	return "", false
}

// IsSelfOrSibling reports whether id belongs to this bot or a sibling bot,
// so the agent never reacts to its own messages or another bot's — the only
// defence against an infinite reply loop when multiple bots occupy the
// server.
func IsSelfOrSibling(id, selfXUID string, siblingXUIDs map[string]struct{}) bool {
	if id == selfXUID {
		return true
	}
	_, sibling := siblingXUIDs[id]
	return sibling
}

// TriggerKind classifies what, if anything, a chat message asks the agent
// to do.
type TriggerKind int

const (
	// TriggerNone means the message is ordinary player conversation.
	TriggerNone TriggerKind = iota
	// TriggerCommand means the message starts with CommandPrefix.
	TriggerCommand
	// TriggerMention means the message contains MentionToken.
	TriggerMention
)

// Trigger is the result of inspecting a chat message for something the
// agent should react to.
type Trigger struct {
	Kind TriggerKind
	// Command and Args are set only when Kind == TriggerCommand. Command is
	// lowercased and has CommandPrefix stripped; Args is the remaining
	// whitespace-split tokens, unmodified.
	Command string
	Args    []string
	// Message is set when Kind == TriggerMention: the original message,
	// unmodified, for the LLM to read as the question.
	Message string
}

// ParseTrigger inspects a chat message and classifies it. Command detection
// takes priority over mention detection: "!ask @server ..." is a command,
// not a mention.
func ParseTrigger(message string) Trigger {
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return Trigger{Kind: TriggerNone}
	}

	if strings.HasPrefix(trimmed, CommandPrefix) {
		body := strings.TrimPrefix(trimmed, CommandPrefix)
		fields := strings.Fields(body)
		if len(fields) == 0 {
			return Trigger{Kind: TriggerNone}
		}
		return Trigger{
			Kind:    TriggerCommand,
			Command: strings.ToLower(fields[0]),
			Args:    fields[1:],
		}
	}

	if strings.Contains(strings.ToLower(trimmed), MentionToken) {
		return Trigger{Kind: TriggerMention, Message: trimmed}
	}

	return Trigger{Kind: TriggerNone}
}
