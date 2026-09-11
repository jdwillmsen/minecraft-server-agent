package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
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

// recordingAudit is a test double for audit.Store: it collects every record
// written to it and, when err is set, fails every Write -- proving a broken
// audit store cannot take a command down with it.
type recordingAudit struct {
	mu      sync.Mutex
	records []audit.Record
	err     error
}

var _ audit.Store = (*recordingAudit)(nil)

func (r *recordingAudit) Write(_ context.Context, rec audit.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return r.err
}

func (r *recordingAudit) Enabled() bool { return true }

func (r *recordingAudit) all() []audit.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Record(nil), r.records...)
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

// flakyPlugin exists so the error and timeout audit outcomes can be pinned
// directly, rather than trusted by inspection: "boom" always fails, and
// "slow" never returns before the dispatch timeout does.
//
// Kept out of newHarness's registry rather than added there: newHarness
// backs TestChatCommandFlow's exact "Available: !help, !ping" assertion, and
// a third visitor-permission command would silently change what it lists.
type flakyPlugin struct{}

func (flakyPlugin) Name() string { return "flaky" }

func (flakyPlugin) Commands() []plugin.Command {
	return []plugin.Command{
		{
			Name:        "boom",
			Description: "Test command that always errors.",
			Permission:  plugin.PermissionVisitor,
			Run: func(context.Context, *plugin.Context, plugin.Invocation) (string, error) {
				return "", errors.New("boom")
			},
		},
		{
			Name:        "slow",
			Description: "Test command that outlives the dispatch timeout.",
			Permission:  plugin.PermissionVisitor,
			Run: func(ctx context.Context, _ *plugin.Context, _ plugin.Invocation) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		},
	}
}

const (
	playerXUID = "2535412345678901"
	selfXUID   = "2535499999999999"
	siblingBot = "2535488888888888"
	// bystanderXUID joins and leaves in the background of the concurrency
	// tests, so the roster is being written while an answer reads it.
	bystanderXUID   = "2535477777777777"
	otherPlayerXUID = "2535466666666666"
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
			// Refused, and told so. A denial can only happen for a command
			// that actually exists, so unlike the unknown case below there
			// is no stray-"!" traffic to stay quiet for.
			name: "operator-only command is refused for a visitor, and says so",
			pk:   chatPacket(playerXUID, "Steve", "!shutdown"),
			want: []string{"tell " + playerXUID + ": !shutdown isn't available to you - !help lists what is."},
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
			// Still silent in open chat, and deliberately: ParseTrigger
			// treats any message opening with "!" as a command, so "!!!"
			// and "!nice" arrive here too. Answering each one would have
			// the agent talking over ordinary conversation.
			name: "unknown command is silently ignored in open chat",
			pk:   chatPacket(playerXUID, "Steve", "!nope"),
			want: nil,
		},
		{
			// Whispered is the opposite case: the player addressed the
			// agent directly and nobody else can see it, so silence is
			// indistinguishable from the agent being down.
			name: "unknown command whispered is answered",
			pk:   whisperPacket(playerXUID, "Steve", "!nope"),
			want: []string{"tell " + playerXUID + ": I don't know !nope. Try !help to see what I can do."},
		},
		{
			name: "stray punctuation whispered is still answered, since it was aimed at the agent",
			pk:   whisperPacket(playerXUID, "Steve", "!!!"),
			want: []string{"tell " + playerXUID + ": I don't know !!!. Try !help to see what I can do."},
		},
		{
			name: "a known command still works when whispered",
			pk:   whisperPacket(playerXUID, "Steve", "!ping"),
			want: []string{"tell " + playerXUID + ": pong"},
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
			handlePacket(context.Background(), tc.pk, selfXUID, siblings, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})

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
		handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})
	}
	handlePacket(context.Background(), chatPacket(otherPlayer, "Alex", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})

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

	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "!ping now"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})
	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server hello"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})
	handlePacket(context.Background(), chatPacket(selfXUID, "Agent", "!ping"), selfXUID, nil, log, registry, pctx, eventBus, limiter, playerRoster, permResolver, testAnswering(), store.Nop{}, audit.Nop{})

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

// stubWaypoints is an enabled waypoint store, so the answer path under test
// actually offers and runs a tool rather than a bare completion.
type stubWaypoints struct {
	waypoints.Nop
	mu      sync.Mutex
	callers []string
}

func (w *stubWaypoints) Enabled() bool { return true }

func (w *stubWaypoints) List(_ context.Context, xuid string) ([]waypoints.Waypoint, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.callers = append(w.callers, xuid)
	return []waypoints.Waypoint{{Name: "base"}}, nil
}

func (w *stubWaypoints) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.callers...)
}

// heldBackend is an LLM backend that reports when a request arrives and
// serves it only once the test releases it, so a test can assert what is
// true while an answer is in flight.
type heldBackend struct {
	srv      *httptest.Server
	arrived  chan struct{}
	release  chan struct{}
	served   sync.Once
	mu       sync.Mutex
	requests int
}

// serve releases every held request. Idempotent, because it is both what a
// test calls to let an answer through and what cleanup calls to make sure a
// failed assertion does not leave a handler parked -- httptest.Server.Close
// waits for outstanding requests, so a test that gave up while holding one
// would hang instead of failing.
func (b *heldBackend) serve() { b.served.Do(func() { close(b.release) }) }

// newHeldBackend replies with replies[n] to the n-th request, repeating the
// last one thereafter.
func newHeldBackend(t *testing.T, replies ...string) *heldBackend {
	t.Helper()
	b := &heldBackend{arrived: make(chan struct{}, 8), release: make(chan struct{})}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		n := b.requests
		b.requests++
		b.mu.Unlock()

		b.arrived <- struct{}{}
		<-b.release
		if n >= len(replies) {
			n = len(replies) - 1
		}
		w.Write([]byte(replies[n]))
	}))
	// Registered after the server's own cleanup so it runs before it: LIFO.
	t.Cleanup(b.srv.Close)
	t.Cleanup(b.serve)
	return b
}

func (b *heldBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests
}

const (
	waypointListCall = `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"waypoint_list","arguments":"{}"}}]}}]}`
	beaconAnswer     = `{"choices":[{"message":{"content":"Your base waypoint is saved."}}]}`
)

// captureStdout returns everything fn writes to stdout, which is where the
// logger puts info-level events. A logger captures its writers at
// construction, so any logger whose output matters must be built inside fn.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// waitForOutput polls the recorded voice until something was said.
func waitForOutput(t *testing.T, voice *recordingVoice) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := voice.output(); len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing was ever said")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A mention is answered on its own goroutine. Held at the backend so the
// assertion is about ordering rather than speed: while the answer is still in
// flight the read loop has already returned and is handling further packets,
// which is what the inline version could not do.
//
// The whole exchange runs concurrently with a stream of PlayerList packets:
// the answer resolves the asker's gamertag from the same roster those packets
// rewrite, and the tool round reads a store the read loop can also reach, so
// this is the shared state the goroutine actually exposes.
func TestMentionIsAnsweredWithoutBlockingTheReadLoop(t *testing.T) {
	backend := newHeldBackend(t, waypointListCall, beaconAnswer)
	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
	waypointStore := &stubWaypoints{}
	pctx.Waypoints = waypointStore
	log := logging.New("info")
	ans := testAnswering()
	ans.llm = adapters.NewLLMClient(backend.srv.URL, "test-model", "", 192, 5*time.Second, log)

	// Paced rather than spun: the roster write is the point, one every few
	// milliseconds proves it, and an unthrottled loop only buys a core's
	// worth of CPU and tens of thousands of join/leave log lines.
	var churns atomic.Int64
	churnDone := make(chan struct{})
	churnStop := make(chan struct{})
	go func() {
		defer close(churnDone)
		for i := 0; ; i++ {
			select {
			case <-churnStop:
				return
			case <-time.After(time.Millisecond):
			}
			pk := &packet.PlayerList{Entries: []protocol.PlayerListEntry{addEntry(playerXUID, "Steve"), addEntry(bystanderXUID, "Alex")}}
			if i%2 == 1 {
				pk = &packet.PlayerList{Entries: []protocol.PlayerListEntry{removeEntry(bystanderXUID)}}
			}
			handlePacket(context.Background(), pk, selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})
			churns.Add(1)
		}
	}()

	handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server where is my base"),
		selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})

	<-backend.arrived
	if got := voice.output(); len(got) != 0 {
		t.Fatalf("the read loop waited for the backend: %v", got)
	}

	// The answer is parked at the backend, so anything the churn does now is
	// a roster write concurrent with an answer in flight. Waiting for two of
	// them is what makes that a fact of this run rather than a hope.
	held := churns.Load()
	deadline := time.Now().Add(5 * time.Second)
	for churns.Load() < held+2 {
		if time.Now().After(deadline) {
			t.Fatal("no roster write happened while the answer was in flight")
		}
		time.Sleep(time.Millisecond)
	}
	backend.serve()

	said := waitForOutput(t, voice)
	close(churnStop)
	<-churnDone

	if want := "tell " + playerXUID + ": Your base waypoint is saved."; said[0] != want {
		t.Errorf("answer = %q, want %q -- it was built from the asker's own waypoints", said[0], want)
	}
	if seen := waypointStore.seen(); len(seen) != 1 || seen[0] != playerXUID {
		t.Errorf("waypoint_list callers = %v, want exactly the asker", seen)
	}
	if n := backend.count(); n != 2 {
		t.Errorf("backend calls = %d, want 2 (the tool round and the answer)", n)
	}
}

// The per-player limiter is a rolling-minute budget, so it cannot bound how
// many answers run at once -- four questions in one second are four allowed
// answers. Without a global cap those became four concurrent exchanges
// against one small backend and four interleaved broadcasts.
func TestMentionIsDroppedWhenTheAgentIsAlreadyBusy(t *testing.T) {
	backend := newHeldBackend(t, beaconAnswer)
	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)

	out := captureStdout(t, func() {
		log := logging.New("info")
		ans := testAnswering()
		ans.llm = adapters.NewLLMClient(backend.srv.URL, "test-model", "", 192, 5*time.Second, log)
		// One slot, so the second question meets a full agent rather than
		// waiting on a real backend to be slow.
		ans.inFlight = make(chan struct{}, 1)

		handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server first"),
			selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})
		<-backend.arrived

		// A different player, so the per-player limiter has nothing to say
		// about this one: only the global cap can refuse it.
		handlePacket(context.Background(), chatPacket(otherPlayerXUID, "Alex", "@server second"),
			selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})

		backend.serve()
		waitForOutput(t, voice)
	})

	if !strings.Contains(out, `"event":"mention_answer_dropped_busy"`) {
		t.Errorf("stdout = %q, want a mention_answer_dropped_busy event", out)
	}
	if n := backend.count(); n != 1 {
		t.Errorf("backend calls = %d, want 1 -- the dropped answer reached the model anyway", n)
	}
	if got := voice.output(); len(got) != 1 {
		t.Errorf("broadcasts = %v, want only the answer that held the slot", got)
	}
}

// The limiter has to run before the spawn, not inside it. Proved by which
// refusal is logged: the slot is already taken by the held answer, so a
// startAnswer that acquired the semaphore first would report this as a busy
// agent instead of a rate-limited player.
func TestRateLimitedMentionIsRefusedBeforeAGoroutineExists(t *testing.T) {
	backend := newHeldBackend(t, beaconAnswer)
	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)

	out := captureStdout(t, func() {
		log := logging.New("info")
		ans := testAnswering()
		ans.llm = adapters.NewLLMClient(backend.srv.URL, "test-model", "", 192, 5*time.Second, log)
		ans.limiter = ratelimit.NewPerActor(1, time.Minute)
		ans.inFlight = make(chan struct{}, 1)

		handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server first"),
			selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})
		<-backend.arrived

		handlePacket(context.Background(), chatPacket(playerXUID, "Steve", "@server second"),
			selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})

		backend.serve()
		waitForOutput(t, voice)
	})

	if !strings.Contains(out, `"event":"mention_rate_limited"`) {
		t.Errorf("stdout = %q, want a mention_rate_limited event", out)
	}
	if strings.Contains(out, `"event":"mention_answer_dropped_busy"`) {
		t.Errorf("stdout = %q: the concurrency cap answered before the rate limiter did", out)
	}
	if n := backend.count(); n != 1 {
		t.Errorf("backend calls = %d, want 1 -- the rate-limited question reached the model anyway", n)
	}
}

// broadcastVoice inspects the context an answer is broadcast on: the deadline
// it carries, and whether cancelling the process context reaches it.
type broadcastVoice struct {
	recordingVoice
	onSay  func()
	mu     sync.Mutex
	budget time.Duration
	err    error
}

func (v *broadcastVoice) Say(ctx context.Context, message string) error {
	if v.onSay != nil {
		v.onSay()
	}
	v.mu.Lock()
	if deadline, ok := ctx.Deadline(); ok {
		v.budget = time.Until(deadline)
	}
	v.err = ctx.Err()
	v.mu.Unlock()
	return v.recordingVoice.Say(ctx, message)
}

func (v *broadcastVoice) observed() (time.Duration, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.budget, v.err
}

// answerOnce drives one mention to completion against a backend that replies
// immediately, and returns what the voice saw.
func answerOnce(t *testing.T, ctx context.Context, voice *broadcastVoice, adjust func(*answering)) {
	t.Helper()
	backend := newHeldBackend(t, beaconAnswer)
	backend.serve()

	registry, pctx, _, eventBus, _, playerRoster, permResolver := newHarness(t)
	pctx.Voice = voice
	log := logging.New("info")
	ans := testAnswering()
	ans.llm = adapters.NewLLMClient(backend.srv.URL, "test-model", "", 192, 5*time.Second, log)
	adjust(&ans)

	handlePacket(ctx, chatPacket(playerXUID, "Steve", "@server hello"),
		selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})
	waitForOutput(t, &voice.recordingVoice)
}

// The bridge client bounds every call it makes by CONSOLE_BRIDGE_TIMEOUT_MS,
// and takes the tighter of that and its caller's deadline. A deadline chosen
// here instead would therefore be the one that applies, and raising the
// configured value to rescue a slow bridge would fix every path except the one
// carrying an answer that has already been paid for.
func TestAnswerBroadcastUsesTheConfiguredBridgeTimeout(t *testing.T) {
	const configured = 9 * time.Second
	voice := &broadcastVoice{}

	answerOnce(t, context.Background(), voice, func(ans *answering) { ans.broadcast = configured })

	budget, _ := voice.observed()
	if budget <= configured-time.Second || budget > configured {
		t.Errorf("broadcast deadline = %v, want the configured %v", budget, configured)
	}
}

// A SIGTERM landing between the model replying and the broadcast must not
// discard an answer the bridge could still deliver.
func TestAnswerBroadcastSurvivesACancelledProcessContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancelled from inside Say, which is the exact gap being defended:
	// cancel propagates to every derived context before it returns, so a
	// broadcast context that was not detached would already be done here.
	voice := &broadcastVoice{onSay: cancel}

	answerOnce(t, ctx, voice, func(ans *answering) { ans.broadcast = 5 * time.Second })

	if _, err := voice.observed(); err != nil {
		t.Errorf("broadcast context was already %v: shutdown discarded a computed answer", err)
	}
}

// testAnswering supplies the chat path's answering dependencies with no LLM
// backend configured, which is both what these command-path tests need and the
// production behaviour when LLM_BASE_URL is unset.
func testAnswering() answering {
	return newAnswering(adapters.NewLLMClient("", "", "", 192, time.Second, nil), 4, 20*time.Second, 5*time.Second)
}

// stubKnowledge is an enabled fact store, so an answer can be built from a
// tool that reads nothing belonging to the asker.
type stubKnowledge struct{ knowledge.Nop }

func (stubKnowledge) Enabled() bool { return true }

func (stubKnowledge) Lookup(context.Context, string, int) ([]knowledge.Entry, error) {
	return []knowledge.Entry{{Topic: "rules", Body: "be nice"}}, nil
}

const (
	knowledgeLookupCall = `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_lookup","arguments":"{\"query\":\"rules\"}"}}]}}]}`
	rulesAnswer         = `{"choices":[{"message":{"content":"Be nice to each other."}}]}`
)

// answerMention drives one @server question to completion against a backend
// that answers the n-th call with replies[n], and returns what the voice
// recorded.
func answerMention(t *testing.T, configure func(*plugin.Context), replies ...string) []string {
	t.Helper()
	return answerMentionPacket(t, chatPacket(playerXUID, "Steve", "@server where is my base"), configure, replies...)
}

// answerMentionPacket is answerMention with the incoming packet chosen by the
// caller, so a test can vary how the question arrived rather than only what
// the backend answers.
func answerMentionPacket(t *testing.T, pk *packet.Text, configure func(*plugin.Context), replies ...string) []string {
	t.Helper()
	backend := newHeldBackend(t, replies...)
	backend.serve()

	registry, pctx, voice, eventBus, _, playerRoster, permResolver := newHarness(t)
	configure(pctx)
	log := logging.New("info")
	ans := testAnswering()
	ans.llm = adapters.NewLLMClient(backend.srv.URL, "test-model", "", 192, 5*time.Second, log)

	handlePacket(context.Background(), pk,
		selfXUID, nil, log, registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, permResolver, ans, store.Nop{}, audit.Nop{})
	return waitForOutput(t, voice)
}

// whisperPacket is the same question arriving through /tell rather than open
// chat. Bedrock delivers that as TextTypeWhisper, which only the sender and
// the recipient see.
func whisperPacket(xuid, sourceName, message string) *packet.Text {
	return &packet.Text{
		TextType:   packet.TextTypeWhisper,
		XUID:       xuid,
		SourceName: sourceName,
		Message:    message,
	}
}

// A question nobody else saw asked should not be answered in front of
// everybody. The broadcast default exists because an answer only the asker
// sees reads as no answer to the rest of the chat that watched them ask --
// which is not true of a whisper, where there is no such audience.
func TestWhisperedQuestionIsAnsweredPrivately(t *testing.T) {
	said := answerMentionPacket(t, whisperPacket(playerXUID, "Steve", "@server what are the rules"),
		func(pctx *plugin.Context) { pctx.Knowledge = stubKnowledge{} },
		knowledgeLookupCall, rulesAnswer)

	want := "tell " + playerXUID + ": Be nice to each other."
	if said[0] != want {
		t.Errorf("answer = %q, want %q -- a whispered question must not be answered in open chat", said[0], want)
	}
}

// Coordinates are personal. !wp whispers them because broadcasting where
// someone lives is a griefing vector, and the same coordinates read out of
// the same store by the answer path are no less personal for having been
// asked for in public.
func TestAnswerBuiltFromTheAskersOwnDataIsWhispered(t *testing.T) {
	said := answerMention(t, func(pctx *plugin.Context) { pctx.Waypoints = &stubWaypoints{} },
		waypointListCall, beaconAnswer)

	want := "tell " + playerXUID + ": Your base waypoint is saved."
	if said[0] != want {
		t.Errorf("answer = %q, want %q", said[0], want)
	}
}

// Every other answer stays public: an @server question is asked in front of
// everyone, and an answer only the asker sees reads to the rest of them as
// no answer at all.
func TestAnswerFromSharedKnowledgeIsStillBroadcast(t *testing.T) {
	said := answerMention(t, func(pctx *plugin.Context) { pctx.Knowledge = stubKnowledge{} },
		knowledgeLookupCall, rulesAnswer)

	if said[0] != "say: Be nice to each other." {
		t.Errorf("answer = %q, want it broadcast to everyone who saw the question", said[0])
	}
}

// TestEveryCommandOutcomeIsAudited proves handleCommand writes exactly one
// audit record for every dispatch outcome, and that the record never carries
// the reply text the command produced -- the one privacy property
// internal/audit's package doc promises the table.
func TestEveryCommandOutcomeIsAudited(t *testing.T) {
	cases := []struct {
		name    string
		command string
		perm    plugin.Permission
		want    audit.Outcome
	}{
		{"successful command", "ping", plugin.PermissionVisitor, audit.OutcomeOK},
		{"unknown command", "nosuchcommand", plugin.PermissionVisitor, audit.OutcomeUnknown},
		{"denied command", "shutdown", plugin.PermissionVisitor, audit.OutcomeDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry, pctx, voice, _, _, playerRoster, _ := newHarness(t)
			permResolver := fakePermResolver(t, map[string]string{playerXUID: tc.perm.String()})
			log := logging.New("info")
			rec := &recordingAudit{}

			handleCommand(context.Background(), playerXUID, chat.ParseTrigger("!"+tc.command), false, log,
				registry, pctx, unlimitedRateLimit(), permResolver, rec, playerRoster)

			records := rec.all()
			if len(records) != 1 {
				t.Fatalf("got %d audit records, want exactly 1", len(records))
			}
			got := records[0]
			if got.Outcome != tc.want {
				t.Errorf("outcome = %q, want %q", got.Outcome, tc.want)
			}

			// audit.Record has no field named Reply, and none should be added:
			// what matters is that no *existing* field ever holds reply text.
			// Only the successful case produced one; checking every field
			// against it either way is what makes this a guarantee rather
			// than a case-by-case hope.
			if said := voice.output(); len(said) == 1 {
				if idx := strings.LastIndex(said[0], ": "); idx != -1 {
					reply := said[0][idx+2:]
					for _, field := range []string{got.XUID, got.Gamertag, got.Permission, got.Command, got.Args, string(got.Outcome)} {
						if reply != "" && strings.Contains(field, reply) {
							t.Errorf("audit record %+v has a field containing reply text %q", got, reply)
						}
					}
				}
			}
		})
	}
}

// TestAFailingAuditWriteDoesNotFailTheCommand proves a broken audit store
// cannot take a command down with it: a compliance record that can fail a
// command is a worse liability than a gap in the record.
func TestAFailingAuditWriteDoesNotFailTheCommand(t *testing.T) {
	registry, pctx, voice, _, _, playerRoster, permResolver := newHarness(t)
	log := logging.New("info")
	rec := &recordingAudit{err: errors.New("database on fire")}

	handleCommand(context.Background(), playerXUID, chat.ParseTrigger("!ping"), false, log,
		registry, pctx, unlimitedRateLimit(), permResolver, rec, playerRoster)

	got := voice.output()
	want := "tell " + playerXUID + ": pong"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("voice output = %v, want [%q] -- a failing audit write must never cost the player their reply", got, want)
	}
	if n := len(rec.all()); n != 1 {
		t.Errorf("got %d audit records, want exactly 1 (the write is attempted and its failure logged, not skipped)", n)
	}
}

// TestRateLimitedCommandIsAudited proves the limiter's refusal is itself
// audited. This path produces no chat reply, so before Task 3 it was
// invisible outside stdout -- exactly the gap an audit trail exists to close.
func TestRateLimitedCommandIsAudited(t *testing.T) {
	registry, pctx, _, _, _, playerRoster, permResolver := newHarness(t)
	log := logging.New("info")
	rec := &recordingAudit{}
	// max=0: Allow refuses every actor on every call, so the command never
	// reaches Dispatch.
	deniedLimiter := ratelimit.NewPerActor(0, time.Minute)

	handleCommand(context.Background(), playerXUID, chat.ParseTrigger("!ping"), false, log,
		registry, pctx, deniedLimiter, permResolver, rec, playerRoster)

	records := rec.all()
	if len(records) != 1 {
		t.Fatalf("got %d audit records, want exactly 1", len(records))
	}
	if records[0].Outcome != audit.OutcomeRateLimited {
		t.Errorf("outcome = %q, want %q", records[0].Outcome, audit.OutcomeRateLimited)
	}
}

// TestErroredCommandIsAudited proves a plugin's own failure is recorded as
// OutcomeError, distinct from the unknown/denied/timeout cases the switch in
// handleCommand also handles.
func TestErroredCommandIsAudited(t *testing.T) {
	registry := plugin.NewRegistry()
	if err := registry.Register(flakyPlugin{}); err != nil {
		t.Fatalf("register flaky: %v", err)
	}
	voice := &recordingVoice{}
	pctx := &plugin.Context{Voice: voice, Directory: registry}
	log := logging.New("info")
	rec := &recordingAudit{}

	handleCommand(context.Background(), playerXUID, chat.ParseTrigger("!boom"), false, log,
		registry, pctx, unlimitedRateLimit(), fakePermResolver(t, nil), rec, roster.New())

	records := rec.all()
	if len(records) != 1 {
		t.Fatalf("got %d audit records, want exactly 1", len(records))
	}
	if records[0].Outcome != audit.OutcomeError {
		t.Errorf("outcome = %q, want %q", records[0].Outcome, audit.OutcomeError)
	}
	// A failure the player is never told about is indistinguishable from a
	// command that worked and had nothing to say.
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], commandFailedReply) {
		t.Errorf("output = %v, want one line carrying %q", out, commandFailedReply)
	}
}

// TestTimedOutCommandIsAudited pins the branch ordering handleCommand
// depends on: ErrCommandTimedOut must be checked ahead of the generic
// err != nil case, or a timeout silently records as OutcomeError instead.
// Driving a command that genuinely outlives DefaultDispatchTimeout is the
// honest way to reach that branch -- it is a var precisely so a test can
// shorten it rather than waiting out the real production value.
func TestTimedOutCommandIsAudited(t *testing.T) {
	registry := plugin.NewRegistry()
	if err := registry.Register(flakyPlugin{}); err != nil {
		t.Fatalf("register flaky: %v", err)
	}
	voice := &recordingVoice{}
	pctx := &plugin.Context{Voice: voice, Directory: registry}
	log := logging.New("info")
	rec := &recordingAudit{}

	orig := plugin.DefaultDispatchTimeout
	plugin.DefaultDispatchTimeout = 10 * time.Millisecond
	defer func() { plugin.DefaultDispatchTimeout = orig }()

	handleCommand(context.Background(), playerXUID, chat.ParseTrigger("!slow"), false, log,
		registry, pctx, unlimitedRateLimit(), fakePermResolver(t, nil), rec, roster.New())

	records := rec.all()
	if len(records) != 1 {
		t.Fatalf("got %d audit records, want exactly 1", len(records))
	}
	if records[0].Outcome != audit.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", records[0].Outcome, audit.OutcomeTimeout)
	}
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], commandTimedOutReply) {
		t.Errorf("output = %v, want one line carrying %q", out, commandTimedOutReply)
	}
}

// The console gets the same failure line, broadcast rather than whispered:
// it has no player identity to whisper to, and an operator typing into the
// console is the reader most likely to act on it.
func TestAFailedConsoleCommandIsAnsweredOnTheConsolePath(t *testing.T) {
	registry := plugin.NewRegistry()
	if err := registry.Register(flakyPlugin{}); err != nil {
		t.Fatalf("register flaky: %v", err)
	}
	voice := &recordingVoice{}
	pctx := &plugin.Context{Voice: voice, Directory: registry}

	handleCommand(context.Background(), chat.ServerOrigin, chat.ParseTrigger("!boom"), false, logging.New("info"),
		registry, pctx, unlimitedRateLimit(), fakePermResolver(t, nil), &recordingAudit{}, roster.New())

	out := voice.output()
	if len(out) != 1 || !strings.HasPrefix(out[0], "say: ") || !strings.Contains(out[0], commandFailedReply) {
		t.Errorf("output = %v, want one broadcast carrying %q", out, commandFailedReply)
	}
}
