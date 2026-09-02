package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
)

// recordingVoice stands in for the Stage 2 console bridge so a test can
// assert what a player would actually have seen.
type recordingVoice struct {
	mu   sync.Mutex
	said []string
}

func (v *recordingVoice) Tell(_ context.Context, xuid, message string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.said = append(v.said, "tell "+xuid+": "+message)
	return nil
}

func (v *recordingVoice) Say(_ context.Context, message string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.said = append(v.said, "say: "+message)
	return nil
}

func (v *recordingVoice) output() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.said...)
}

// opOnlyPlugin exists so the permission gate has something above the
// visitor level that main's dispatch path must refuse.
type opOnlyPlugin struct{}

func (opOnlyPlugin) Name() string { return "opsonly" }

func (opOnlyPlugin) Commands() []plugin.Command {
	return []plugin.Command{{
		Name:        "shutdown",
		Description: "Operator-only test command.",
		Permission:  plugin.PermissionOperator,
		Run: func(context.Context, *plugin.Context, plugin.Invocation) (string, error) {
			return "shutting down", nil
		},
	}}
}

const (
	playerXUID = "2535412345678901"
	selfXUID   = "2535499999999999"
	siblingBot = "2535488888888888"
)

func newHarness(t *testing.T) (*plugin.Registry, *plugin.Context, *recordingVoice, *bus.Bus, <-chan bus.Event) {
	t.Helper()

	registry := plugin.NewRegistry()
	if err := registry.Register(plugins.NewCore()); err != nil {
		t.Fatalf("register core: %v", err)
	}
	if err := registry.Register(opOnlyPlugin{}); err != nil {
		t.Fatalf("register opsonly: %v", err)
	}

	voice := &recordingVoice{}
	pctx := &plugin.Context{Voice: voice, Directory: registry}
	eventBus := bus.New()
	events := eventBus.Subscribe(chat.MessageKind, 8)
	return registry, pctx, voice, eventBus, events
}

func chatPacket(xuid, sourceName, message string) *packet.Text {
	return &packet.Text{
		TextType:   packet.TextTypeChat,
		XUID:       xuid,
		SourceName: sourceName,
		Message:    message,
	}
}

// TestChatCommandFlow drives the same function the live packet loop calls,
// so each case is the end-user behaviour: a chat line in, what the player
// hears out.
func TestChatCommandFlow(t *testing.T) {
	cases := []struct {
		name string
		pk   *packet.Text
		want []string
	}{
		{
			name: "player ping is answered privately",
			pk:   chatPacket(playerXUID, "Steve", "!ping"),
			want: []string{"tell " + playerXUID + ": pong"},
		},
		{
			name: "help lists only commands the actor may run",
			pk:   chatPacket(playerXUID, "Steve", "!help"),
			want: []string{"tell " + playerXUID + ": Available: !help, !ping"},
		},
		{
			name: "command matching is case and whitespace insensitive",
			pk:   chatPacket(playerXUID, "Steve", "   !PiNg   "),
			want: []string{"tell " + playerXUID + ": pong"},
		},
		{
			name: "console-originated command is answered to everyone",
			pk:   chatPacket("", "", "!ping"),
			want: []string{"say: pong"},
		},
		{
			name: "operator-only command is refused for a visitor",
			pk:   chatPacket(playerXUID, "Steve", "!shutdown"),
			want: nil,
		},
		{
			name: "empty xuid with a name is not trusted as the console",
			pk:   chatPacket("", "Steve", "!ping"),
			want: nil,
		},
		{
			name: "the agent ignores its own echo",
			pk:   chatPacket(selfXUID, "Agent", "!ping"),
			want: nil,
		},
		{
			name: "a sibling bot cannot trigger a reply loop",
			pk:   chatPacket(siblingBot, "AfkBot", "!ping"),
			want: nil,
		},
		{
			name: "unknown command is silently ignored",
			pk:   chatPacket(playerXUID, "Steve", "!nope"),
			want: nil,
		},
		{
			name: "ordinary conversation produces no reply",
			pk:   chatPacket(playerXUID, "Steve", "anyone got spare iron"),
			want: nil,
		},
		{
			name: "a mention is detected but not answered in stage 1",
			pk:   chatPacket(playerXUID, "Steve", "@server how do I craft a beacon"),
			want: nil,
		},
		{
			name: "non-chat text types are ignored",
			pk: &packet.Text{
				TextType: packet.TextTypePopup,
				XUID:     playerXUID,
				Message:  "!ping",
			},
			want: nil,
		},
	}

	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry, pctx, voice, eventBus, _ := newHarness(t)
			handlePacket(context.Background(), tc.pk, selfXUID, siblings, log, registry, pctx, eventBus)

			got := voice.output()
			if len(got) != len(tc.want) {
				t.Fatalf("voice output = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("voice output[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestChatMessagePublishedOnBus proves the "ear" half of the split: every
// answerable message reaches subscribers with the resolved XUID identity
// and its parsed trigger, whether or not a command ran.
func TestChatMessagePublishedOnBus(t *testing.T) {
	registry, pctx, _, eventBus, events := newHarness(t)
	log := logging.New("info")

	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "!ping now"), selfXUID, nil, log, registry, pctx, eventBus)
	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server hello"), selfXUID, nil, log, registry, pctx, eventBus)
	handlePacket(context.Background(), chatPacket(selfXUID, "Agent", "!ping"), selfXUID, nil, log, registry, pctx, eventBus)

	first, ok := (<-events).(chat.MessageEvent)
	if !ok {
		t.Fatalf("first event is not a chat.MessageEvent")
	}
	if first.ActorXUID != playerXUID {
		t.Errorf("actor = %q, want the XUID %q", first.ActorXUID, playerXUID)
	}
	if first.Trigger.Kind != chat.TriggerCommand || first.Trigger.Command != "ping" {
		t.Errorf("trigger = %+v, want a ping command", first.Trigger)
	}
	if len(first.Trigger.Args) != 1 || first.Trigger.Args[0] != "now" {
		t.Errorf("args = %v, want [now]", first.Trigger.Args)
	}

	second := (<-events).(chat.MessageEvent)
	if second.Trigger.Kind != chat.TriggerMention || !strings.Contains(second.Trigger.Message, "@server") {
		t.Errorf("second trigger = %+v, want a mention", second.Trigger)
	}

	select {
	case ev := <-events:
		t.Fatalf("self message was published to the bus: %+v", ev)
	default:
	}
}
