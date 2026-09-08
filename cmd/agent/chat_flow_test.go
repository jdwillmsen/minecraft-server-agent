package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// unlimitedRateLimit is a generous limiter for tests that exercise chat flow
// but aren't themselves testing rate limiting - see TestHandleCommand_RateLimit
// for that behavior in isolation.
func unlimitedRateLimit() *ratelimit.PerActor {
	return ratelimit.NewPerActor(1000, time.Minute)
}

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

func newHarness(t *testing.T) (*plugin.Registry, *plugin.Context, *recordingVoice, *bus.Bus, <-chan bus.Event, *roster.Roster, *adapters.PermissionResolver) {
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
	events, _ := eventBus.Subscribe(chat.MessageKind, 8)
	return registry, pctx, voice, eventBus, events, roster.New(), fakePermResolver(t, nil)
}

// fakePermResolver returns a PermissionResolver backed by a throwaway HTTP
// server serving perms (nil behaves as an empty map — every real XUID
// resolves to visitor, matching this harness's default expectations; only
// chat.ServerOrigin resolves to operator, without ever reaching this
// server).
func fakePermResolver(t *testing.T, perms map[string]string) *adapters.PermissionResolver {
	t.Helper()
	if perms == nil {
		perms = map[string]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(perms)
	}))
	t.Cleanup(srv.Close)
	client := adapters.NewBridgeClient(srv.URL, "test-token", time.Second)
	return adapters.NewPermissionResolver(client, time.Minute, logging.New("info"))
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
			// The scenario the Stage 1 TODO explicitly called out: an XUID
			// absent from permissions.json (playerXUID above) must never
			// be granted operator trust, but the console sentinel must —
			// it is not a real XUID and will never appear in that map.
			name: "operator-only command is allowed for the console origin",
			pk:   chatPacket("", "", "!shutdown"),
			want: []string{"say: shutting down"},
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
			registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
			handlePacket(context.Background(), tc.pk, selfXUID, siblings, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, testAnswering(), store.Nop{})

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

// TestHandleCommand_RateLimitBlocksASpammingActorButNotOthers proves the
// limiter is actually wired into the dispatch path, not just unit-tested in
// isolation: a player who exceeds their budget stops getting replies, while
// an unrelated player is unaffected.
func TestHandleCommand_RateLimitBlocksASpammingActorButNotOthers(t *testing.T) {
	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
	log := logging.New("info")
	limiter := ratelimit.NewPerActor(2, time.Minute)

	const otherPlayer = "2535499999999998"
	for i := 0; i < 5; i++ {
		handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{})
	}
	handlePacket(context.Background(), chatPacket(otherPlayer, "Alex", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{})

	got := voice.output()
	want := []string{
		"tell " + playerXUID + ": pong",
		"tell " + playerXUID + ": pong",
		"tell " + otherPlayer + ": pong",
	}
	if len(got) != len(want) {
		t.Fatalf("voice output = %v (%d replies), want %d replies: 2 for the spammer (rate-limited after that), 1 for the other player", got, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("voice output[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChatMessagePublishedOnBus proves the "ear" half of the split: every
// answerable message reaches subscribers with the resolved XUID identity
// and its parsed trigger, whether or not a command ran.
func TestChatMessagePublishedOnBus(t *testing.T) {
	registry, pctx, _, eventBus, events, playerRoster, permResolver := newHarness(t)
	log := logging.New("info")
	limiter := unlimitedRateLimit()

	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "!ping now"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{})
	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server hello"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{})
	handlePacket(context.Background(), chatPacket(selfXUID, "Agent", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{})

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

// A mention is answered on its own goroutine. Held here at the backend so
// the assertion is about ordering rather than speed: while the answer is
// still in flight the read loop has already returned and would be handling
// the next packet, which is what the inline version could not do.
func TestMentionIsAnsweredWithoutBlockingTheReadLoop(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte(`{"choices":[{"message":{"content":"Beacons need a nether star."}}]}`))
	}))
	defer backend.Close()

	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
	log := logging.New("info")
	ans := testAnswering()
	ans.llm = adapters.NewLLMClient(backend.URL, "test-model", "", 192, 5*time.Second)

	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server how do I craft a beacon"),
		selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{})

	if got := voice.output(); len(got) != 0 {
		t.Fatalf("the read loop waited for the backend: %v", got)
	}
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for len(voice.output()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the mention was never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := voice.output()[0]; got != "say: Beacons need a nether star." {
		t.Errorf("answer = %q, want it broadcast to everyone who saw the question", got)
	}
}

// testAnswering supplies the chat path's answering dependencies with no LLM
// backend configured, which is both what these command-path tests need and the
// production behaviour when LLM_BASE_URL is unset.
func testAnswering() answering {
	return answering{
		limiter:  ratelimit.NewPerActor(4, time.Minute),
		llm:      adapters.NewLLMClient("", "", "", 192, time.Second),
		toolsFor: func(p *plugin.Context) *tools.Registry { return buildToolset(p, p.Profiles) },
		total:    20 * time.Second,
	}
}
