package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

const privateReply = "base at 123 64 -456"

// replyPlugin has one command whose reply is private and one whose is not,
// so a test can tell redaction apart from a log that was never captured.
type replyPlugin struct{}

func (replyPlugin) Name() string { return "replies" }

func (replyPlugin) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name: "whereami", Description: "Private reply.", Permission: plugin.PermissionVisitor, RedactReply: true,
			Run: func(context.Context, *plugin.Context, plugin.Invocation) (string, error) { return privateReply, nil },
		},
		{
			Name: "echo", Description: "Public reply.", Permission: plugin.PermissionVisitor,
			Run: func(context.Context, *plugin.Context, plugin.Invocation) (string, error) { return "public words", nil },
		},
	}
}

// The log outlives every retention promise the agent makes, so a reply
// whispered to keep it private must never be copied into it.
func TestRedactedReplyNeverReachesTheLog(t *testing.T) {
	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
	if err := registry.Register(replyPlugin{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	out := captureAgentStdout(t, func() {
		log := logging.New("debug")
		for _, line := range []string{"!whereami", "!echo"} {
			handlePacket(context.Background(), chatPacket(playerXUID, "Steve", line), selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})
		}
	})

	if strings.Contains(out, "123 64 -456") {
		t.Errorf("a redacted reply reached the log:\n%s", out)
	}
	if !strings.Contains(out, `"reply_chars":19`) {
		t.Errorf("the redacted reply's length was not logged in its place:\n%s", out)
	}
	if !strings.Contains(out, "public words") {
		t.Errorf("an ordinary reply was not logged, so this capture proves nothing:\n%s", out)
	}
	if got := voice.output(); len(got) != 2 || got[0] != "tell "+playerXUID+": "+privateReply {
		t.Errorf("voice output = %v, want the private reply still whispered", got)
	}
}

// The flag protects nothing unless the commands whose replies are private
// actually carry it.
func TestPrivateRepliesAreRedacted(t *testing.T) {
	registry := plugin.NewRegistry()
	deliverer := announce.NewDeliverer(&recordingAnnounceStore{}, &recordingVoice{},
		newDeliveryAudience(roster.New(), siblingBotXUIDs()),
		announcePermissions{resolver: fakePermResolver(t, nil)}, logging.New("error"))
	if err := registerPlugins(t.Context(), registry, deliverer, nil, logging.New("error")); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range []string{"wp", "modlog"} {
		cmd, ok := registry.Lookup(name)
		if !ok {
			t.Fatalf("!%s is not registered", name)
		}
		if !cmd.RedactReply {
			t.Errorf("!%s replies are logged in full", name)
		}
	}
}
