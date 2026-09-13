// Package adapters holds the implementations of the plugin package's
// capability interfaces (Voice, Facts, ...) that reach mc-console-bridge —
// the only thing with write access to the server console.
package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// NoopVoice logs what it would have said instead of actually reaching the
// console bridge. Kept around as a lightweight stand-in for tests that
// exercise plugin dispatch without needing a real (or fake) bridge.
type NoopVoice struct {
	log *logging.Logger
}

// NewNoopVoice builds a NoopVoice that logs through log.
func NewNoopVoice(log *logging.Logger) NoopVoice {
	return NoopVoice{log: log}
}

var _ plugin.Voice = NoopVoice{}

func (v NoopVoice) Tell(ctx context.Context, xuid, message string) error {
	v.log.Info("voice_tell_noop", logging.Fields{"xuid": xuid, "message": message})
	return nil
}

func (v NoopVoice) Say(ctx context.Context, message string) error {
	v.log.Info("voice_say_noop", logging.Fields{"message": message})
	return nil
}

// NameResolver looks up a player's current gamertag from their XUID.
// Implemented by internal/roster.Roster: Voice.Tell only carries an XUID,
// but Bedrock's tellraw needs a selector or a name to target, so something
// has to bridge that gap. Voice never trusts a caller-supplied name for
// this — the whole point of internal/chat resolving identity by XUID only
// is defeated if a display name it never checked can steer where a reply
// goes.
type NameResolver interface {
	// NameFor returns the last gamertag recorded for xuid. ok is false only
	// when nothing has ever named this xuid — Tell must not guess or fall
	// back to xuid itself, since that is never a valid tellraw target. A
	// player who has since left, or whose session ended with the agent's
	// connection, still resolves: the tellraw then reaches nobody, which
	// costs less than a reply this process can no longer address at all.
	NameFor(xuid string) (name string, ok bool)
}

// BridgeVoice is the real console-bridge-backed Voice: every Tell/Say goes
// out as a tellraw/say console command through mc-console-bridge's
// POST /command, the only way anything in this system can speak.
type BridgeVoice struct {
	client *BridgeClient
	names  NameResolver
}

// NewBridgeVoice builds a BridgeVoice. names resolves the gamertag a Tell
// call's xuid should be targeted at.
func NewBridgeVoice(client *BridgeClient, names NameResolver) *BridgeVoice {
	return &BridgeVoice{client: client, names: names}
}

var _ plugin.Voice = (*BridgeVoice)(nil)

// tellrawPayload is the JSON body mc-console-bridge's allowlist requires
// for a tellraw command: an object carrying a "rawtext" array. Built with
// encoding/json rather than string concatenation so a message containing
// quotes, backslashes, or other JSON-significant characters can never
// produce malformed or (worse) unintended JSON.
type tellrawPayload struct {
	RawText []tellrawRun `json:"rawtext"`
}

type tellrawRun struct {
	Text string `json:"text"`
}

// Tell whispers message to the player identified by xuid, by resolving
// their last recorded gamertag from the roster and targeting them with a
// Bedrock name-selector (`@a[name="..."]`), not a bare name token. Bedrock
// gamertags may contain spaces, which a bare name token cannot represent in
// mc-console-bridge's allowlist grammar (a bare token is deliberately
// whitespace-free there, so a player's chat text can never smuggle a second
// console command via an embedded space) — the quoted selector form is
// real Bedrock target-selector syntax and sidesteps that limit entirely.
func (v *BridgeVoice) Tell(ctx context.Context, xuid, message string) error {
	name, ok := v.names.NameFor(xuid)
	if !ok {
		return fmt.Errorf("bridge voice: no known gamertag for xuid %q (never named on any roster), cannot target a tellraw reply", xuid)
	}
	if strings.Contains(name, `"`) {
		// A real Xbox gamertag cannot contain a double quote, but this
		// guards against building a broken (or, worse, differently-scoped)
		// selector out of a name this process didn't validate at the
		// source.
		return fmt.Errorf("bridge voice: gamertag %q for xuid %q contains a double quote, refusing to build a selector from it", name, xuid)
	}

	payload, err := json.Marshal(tellrawPayload{RawText: []tellrawRun{{Text: message}}})
	if err != nil {
		return fmt.Errorf("bridge voice: encode tellraw payload: %w", err)
	}

	target := fmt.Sprintf(`@a[name="%s"]`, name)
	cmd := fmt.Sprintf("tellraw %s %s", target, payload)
	if _, err := v.client.runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("bridge voice: tell %s: %w", xuid, err)
	}
	return nil
}

// sayLineBreaks flattens every line-break form into a single space.
// Bedrock's `say` consumes the rest of the console line, so a message
// carrying a newline would either smuggle a second console line or (as
// mc-console-bridge's allowlist does) be refused outright. Console output
// relayed through Facts is routinely multi-line, so this is the normal
// case, not an edge one.
var sayLineBreaks = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// Say broadcasts message to everyone via the console's own `say`, which is
// the entire reason replies go through the bridge rather than a connected
// player's own chat: the message carries no gamertag prefix at all. The
// message is flattened to one line first; `say` with nothing left to
// broadcast is an error rather than a silently discarded reply.
func (v *BridgeVoice) Say(ctx context.Context, message string) error {
	line := strings.TrimSpace(sayLineBreaks.Replace(message))
	if line == "" {
		return fmt.Errorf("bridge voice: say: message is empty after flattening line breaks, nothing to broadcast")
	}
	if _, err := v.client.runCommand(ctx, "say "+line); err != nil {
		return fmt.Errorf("bridge voice: say: %w", err)
	}
	return nil
}
