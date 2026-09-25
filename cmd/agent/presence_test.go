package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/presence"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

func unusedBridge() *adapters.BridgeClient {
	return adapters.NewBridgeClient("http://127.0.0.1:1", "test-token", 50*time.Millisecond)
}

func TestNewPresenceWithoutActorsChangesNothing(t *testing.T) {
	rt, err := newPresence(config.Config{PresenceSelfID: "agent"}, store.Nop{}, audit.Nop{}, unusedBridge(), roster.New(), quiet())
	if err != nil {
		t.Fatalf("newPresence: %v", err)
	}
	if _, ok := rt.sessionGate().(alwaysPresent); !ok {
		t.Errorf("gate = %T, want alwaysPresent when no actors are configured", rt.sessionGate())
	}
	if rt.api.Enabled() {
		t.Error("the presence API reports enabled with nothing configured")
	}
	rt.lead(t.Context())
}

func TestNewPresenceGatesTheSessionOnItsOwnActor(t *testing.T) {
	cfg := config.Config{
		PresenceSelfID: "agent",
		PresenceActors: []config.PresenceActor{{ID: "agent", Gamertag: "JdwAgent", Kind: "agent", DefaultState: "parked"}},
		PresenceTokens: []config.PresenceToken{{Name: "ops", Token: "0123456789abcdef", Scopes: []string{"presence:read"}}},
	}
	rt, err := newPresence(cfg, store.Nop{}, audit.Nop{}, unusedBridge(), roster.New(), quiet())
	if err != nil {
		t.Fatalf("newPresence: %v", err)
	}
	gate := rt.sessionGate()
	if _, ok := gate.(*presence.Gate); !ok {
		t.Fatalf("gate = %T, want the presence gate", gate)
	}
	if present, _ := gate.Wanted(); present {
		t.Error("an agent parked by default starts with the gate present; it would join before the first tick")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt.lead(ctx)
	if present, _ := gate.Wanted(); present {
		t.Error("with no database the gate must stay at the configured default")
	}
	if !rt.api.Enabled() {
		t.Error("tokens and actors are configured, but the API reports disabled")
	}
}

// Actors alone are not enough to open /v1: every route needs a token, and a
// mount with none would answer 401 to everyone instead of the plan-1 404.
func TestNewPresenceWithoutTokensMountsNoAPI(t *testing.T) {
	cfg := config.Config{
		PresenceSelfID: "agent",
		PresenceActors: []config.PresenceActor{{ID: "agent", Gamertag: "JdwAgent", Kind: "agent", DefaultState: "present"}},
	}
	rt, err := newPresence(cfg, store.Nop{}, audit.Nop{}, unusedBridge(), roster.New(), quiet())
	if err != nil {
		t.Fatalf("newPresence: %v", err)
	}
	if rt.api.Enabled() {
		t.Error("the presence API reports enabled with no PRESENCE_TOKENS")
	}
}

func TestNewPresenceRefusesASelfIDThatIsNotAnActor(t *testing.T) {
	cfg := config.Config{
		PresenceSelfID: "someone-else",
		PresenceActors: []config.PresenceActor{{ID: "agent", Gamertag: "JdwAgent", Kind: "agent", DefaultState: "present"}},
	}
	if _, err := newPresence(cfg, store.Nop{}, audit.Nop{}, unusedBridge(), roster.New(), quiet()); err == nil {
		t.Error("newPresence accepted a self id no actor carries")
	}
}

// memPresence is a presence database held in memory: enough for the loop to
// read overrides back and for a write to land.
type memPresence struct {
	presence.Nop
	mu        sync.Mutex
	overrides map[string]presenceapi.Override
	version   int64
	// stall, when set, holds the next read until it is closed, after
	// closing stalled.
	stall, stalled chan struct{}
}

func (m *memPresence) Enabled() bool { return true }

// stallNextRead makes the next Overrides call wait, as a slow database
// would. entered closes once a read is waiting; closing release frees it.
func (m *memPresence) stallNextRead() (entered, release chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stall, m.stalled = make(chan struct{}), make(chan struct{})
	return m.stalled, m.stall
}

func (m *memPresence) Overrides(context.Context) (map[string]presenceapi.Override, error) {
	m.mu.Lock()
	if stall, stalled := m.stall, m.stalled; stall != nil {
		m.stall, m.stalled = nil, nil
		m.mu.Unlock()
		close(stalled)
		<-stall
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	out := make(map[string]presenceapi.Override, len(m.overrides))
	for id, ov := range m.overrides {
		out[id] = ov
	}
	return out, nil
}

func (m *memPresence) Set(_ context.Context, id string, ov presenceapi.Override, _ int64) (presence.Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.version++
	ov.Version = m.version
	m.overrides[id] = ov
	return presence.Change{ActorID: id, Now: &ov}, nil
}

func (m *memPresence) Statuses(context.Context) (map[string]presenceapi.Status, error) {
	return map[string]presenceapi.Status{}, nil
}

func (m *memPresence) PutStatus(context.Context, string, presenceapi.Status) error { return nil }

// recordingConsole is the server console as the presence loop drives it.
type recordingConsole struct {
	mu     sync.Mutex
	online []string
	kicked []string
}

func (c *recordingConsole) OnlinePlayers(context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.online), nil
}

func (c *recordingConsole) Kick(_ context.Context, gamertag string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kicked = append(c.kicked, gamertag)
	return nil
}

func (c *recordingConsole) kicks() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.kicked)
}

func twoActors() config.Config {
	return config.Config{
		PresenceSelfID: "agent",
		PresenceActors: []config.PresenceActor{
			{ID: "agent", Gamertag: "JdwAgent", Kind: "agent", DefaultState: "present"},
			{ID: "afk-bot-1", Gamertag: "JdwBot", Kind: "bot", DefaultState: "parked"},
		},
	}
}

// The loop asks the console only about actors the roster shows online, so a
// loop built without the live roster would never kick anyone.
func TestLeadKicksAParkedActorThePlayerRosterShows(t *testing.T) {
	playerRoster := roster.New()
	playerRoster.Seed([]roster.Entry{{XUID: "111", Username: "JdwBot"}})
	console := &recordingConsole{online: []string{"JdwBot"}}
	rt, err := buildPresence(twoActors(), &memPresence{overrides: map[string]presenceapi.Override{}}, audit.Nop{}, console, playerRoster, quiet())
	if err != nil {
		t.Fatalf("buildPresence: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt.lead(ctx)
	if got := console.kicks(); !slices.Equal(got, []string{"JdwBot"}) {
		t.Errorf("kicked %v, want the parked bot the roster shows", got)
	}
}

// A turn ends by cancelling its context, but the loop it started may still
// be inside a tick. The next turn must wait for that loop to return, or its
// deferred reset lands after the new turn exported its gauges and wipes them.
func TestLeadWaitsForThePreviousTurnsLoopToReturn(t *testing.T) {
	mem := &memPresence{overrides: map[string]presenceapi.Override{}}
	rt, err := buildPresence(twoActors(), mem, audit.Nop{}, &recordingConsole{}, roster.New(), quiet())
	if err != nil {
		t.Fatalf("buildPresence: %v", err)
	}
	first, endFirst := context.WithCancel(t.Context())
	rt.lead(first)
	entered, release := mem.stallNextRead()
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free)
	if _, err := rt.svc.Set(first, "afk-bot-1", presence.Request{State: presenceapi.StatePresent, Reason: "test"}, presence.APISource("ops")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first turn's loop never ticked on the write")
	}
	endFirst()

	second, endSecond := context.WithCancel(t.Context())
	defer endSecond()
	led := make(chan struct{})
	go func() {
		defer close(led)
		rt.lead(second)
	}()
	select {
	case <-led:
		t.Fatal("the next turn started while the previous turn's loop was still running")
	case <-time.After(100 * time.Millisecond):
	}
	free()
	select {
	case <-led:
	case <-time.After(5 * time.Second):
		t.Fatal("the next turn never started once the previous loop returned")
	}
	if !metricstest.Exists(t, "mc_presence_desired", "actor", "agent") {
		t.Error("the new turn's gauges are gone")
	}
}

// A write on the leader moves its own gate at once: the loop's nudge is the
// service's one change callback, and nothing may take that slot from it.
func TestAWriteOnTheLeaderMovesItsGateWithoutWaitingForATick(t *testing.T) {
	rt, err := buildPresence(twoActors(), &memPresence{overrides: map[string]presenceapi.Override{}}, audit.Nop{}, &recordingConsole{}, roster.New(), quiet())
	if err != nil {
		t.Fatalf("buildPresence: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rt.lead(ctx)
	present, changed := rt.sessionGate().Wanted()
	if !present {
		t.Fatal("an agent present by default starts parked")
	}
	if _, err := rt.svc.Set(ctx, "agent", presence.Request{State: presenceapi.StateParked, Reason: "test"}, presence.APISource("ops")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate did not move until the next tick")
	}
	if present, _ := rt.sessionGate().Wanted(); present {
		t.Error("the gate moved, but not to parked")
	}
}

func TestPresenceModesAnnounceAReturnAndReportAParkedLeaderReady(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
		ready atomic.Bool
	)
	record := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }
	modes := presenceModes(sessionModes{
		present: func(context.Context) { record("present") },
		left:    func() { record("left") },
		absent:  func(context.Context) { record(fmt.Sprintf("absent ready=%v", ready.Load())) },
	}, ready.Store, func(context.Context) { record("rejoined") })

	ctx := t.Context()
	modes.present(ctx)
	modes.left()
	modes.absent(ctx)
	modes.present(ctx)

	want := []string{"present", "left", "absent ready=true", "rejoined", "present"}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
	if ready.Load() {
		t.Error("still ready after the absence ended; the session sets it again on spawn")
	}
}

// The wrapped modes under the real lifecycle: the gate flips the agent out
// and back, the follower and the session never overlap, and the return is
// announced once.
func TestPresenceModesUnderRunSessionsNeverOverlap(t *testing.T) {
	gate := presence.NewGate(true)
	var (
		running atomic.Int32
		overlap atomic.Bool
		mu      sync.Mutex
		calls   []string
	)
	record := func(s string) { mu.Lock(); calls = append(calls, s); mu.Unlock() }
	entered := make(chan string, 8)
	mode := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			if running.Add(1) > 1 {
				overlap.Store(true)
			}
			record(name)
			entered <- name
			<-ctx.Done()
			running.Add(-1)
		}
	}
	modes := presenceModes(sessionModes{present: mode("present"), left: func() { record("left") }, absent: mode("absent")},
		func(bool) {}, func(context.Context) { record("rejoined") })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); runSessions(ctx, gate, modes, quiet()) }()
	for _, step := range []struct {
		want string
		then func()
	}{
		{"present", func() { gate.Set(false) }},
		{"absent", func() { gate.Set(true) }},
		{"present", cancel},
	} {
		select {
		case got := <-entered:
			if got != step.want {
				t.Fatalf("entered %s, want %s", got, step.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("never entered %s", step.want)
		}
		step.then()
	}
	<-done

	if overlap.Load() {
		t.Error("two modes ran at once")
	}
	want := []string{"present", "left", "absent", "rejoined", "present"}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %v, want %v", calls, want)
	}
}

func TestAnnounceRejoinSaysSoThroughTheBridge(t *testing.T) {
	voice := &recordingVoice{}
	announceRejoin(voice, quiet())(t.Context())
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], rejoinLine) {
		t.Errorf("voice = %v, want the rejoin line said once", out)
	}
}

func TestRegisterPluginsIncludesExtraPlugins(t *testing.T) {
	registry := plugin.NewRegistry()
	reg, _ := presence.NewRegistry(nil, "agent")
	extra := presence.NewChatPlugin(presence.NewService(reg, presence.Nop{}, audit.Nop{}, quiet()), roster.New(), presence.NewJoinLog())
	if err := registerPlugins(t.Context(), registry, nil, newJoinTimes(), nil, quiet(), extra); err != nil {
		t.Fatalf("registerPlugins: %v", err)
	}
	for _, name := range []string{"presence", "park", "unpark", "leave"} {
		if _, ok := registry.Lookup(name); !ok {
			t.Errorf("!%s not registered", name)
		}
	}
}

type leaveStub struct {
	got chan []string
	// during runs inside the command, as the leave's write waking the loop
	// that cancels the session does.
	during func()
}

func (leaveStub) Name() string { return "leavestub" }

func (s leaveStub) Commands() []plugin.Command {
	return []plugin.Command{{
		Name: "leave", Description: "stub", Permission: plugin.PermissionOperator,
		Run: func(_ context.Context, _ *plugin.Context, inv plugin.Invocation) (string, error) {
			s.got <- inv.Args
			if s.during != nil {
				s.during()
			}
			return "leaving", nil
		},
	}}
}

func newLeaveStub(t *testing.T, registry *plugin.Registry) leaveStub {
	t.Helper()
	stub := leaveStub{got: make(chan []string, 2)}
	if err := registry.Register(stub); err != nil {
		t.Fatal(err)
	}
	return stub
}

func TestServerLeaveMentionRunsTheLeaveCommand(t *testing.T) {
	registry, pctx, voice, eventBus, _, playerRoster, _ := newHarness(t)
	stub := newLeaveStub(t, registry)
	perms := fakePermResolver(t, map[string]string{playerXUID: "operator"})

	handlePacket(t.Context(), chatPacket(playerXUID, "Steve", "@server leave 30m"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	select {
	case args := <-stub.got:
		if !slices.Equal(args, []string{"30m"}) {
			t.Errorf("leave got args %v, want [30m]", args)
		}
	default:
		t.Fatal("@server leave did not reach the leave command")
	}
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], "leaving") {
		t.Errorf("voice = %v, want the command's reply", out)
	}

	handlePacket(t.Context(), chatPacket(playerXUID, "Steve", "@server where are the leaves"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, testAnswering(), store.Nop{}, audit.Nop{}, newJoinTimes())
	select {
	case args := <-stub.got:
		t.Errorf("a question about leaves ran !leave with %v", args)
	default:
	}
}

// The mention form is the command, so it is refused exactly as !leave is.
func TestServerLeaveMentionFromAMemberIsRefusedLikeTheCommand(t *testing.T) {
	registry, pctx, voice, eventBus, _, playerRoster, _ := newHarness(t)
	stub := newLeaveStub(t, registry)
	perms := fakePermResolver(t, map[string]string{playerXUID: "member"})
	auditor := &recordingAudit{}

	handlePacket(t.Context(), chatPacket(playerXUID, "Steve", "@server leave"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, testAnswering(), store.Nop{}, auditor, newJoinTimes())
	select {
	case args := <-stub.got:
		t.Fatalf("a member's @server leave ran the command with %v", args)
	default:
	}
	if out := voice.output(); len(out) != 1 || !strings.Contains(out[0], deniedCommandReply("leave")) {
		t.Errorf("voice = %v, want the same refusal !leave gets", out)
	}
	if recs := auditor.all(); len(recs) != 1 || recs[0].Command != "leave" || recs[0].Outcome != audit.OutcomeDenied {
		t.Errorf("audit = %+v, want one denied leave", recs)
	}
}

// Anything that is not exactly the leave form is a question for the model.
func TestServerMentionThatIsNotALeaveStillReachesTheModel(t *testing.T) {
	backend := newHeldBackend(t, rulesAnswer)
	backend.serve()
	registry, pctx, voice, eventBus, _, playerRoster, _ := newHarness(t)
	stub := newLeaveStub(t, registry)
	perms := fakePermResolver(t, map[string]string{playerXUID: "operator"})
	ans := testAnswering()
	ans.llm = adapters.NewLLMClient(backend.srv.URL, "test-model", "", 192, 5*time.Second, quiet())

	handlePacket(t.Context(), chatPacket(playerXUID, "Steve", "@server where are the leaves"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, ans, store.Nop{}, audit.Nop{}, newJoinTimes())
	if out := waitForOutput(t, voice); len(out) != 1 || !strings.Contains(out[0], "Be nice to each other.") {
		t.Errorf("voice = %v, want the model's answer", out)
	}
	select {
	case args := <-stub.got:
		t.Errorf("a question about leaves ran !leave with %v", args)
	default:
	}
}

// ctxVoice records whether the context each reply was sent on was already
// done.
type ctxVoice struct {
	mu   sync.Mutex
	errs []error
}

func (v *ctxVoice) Tell(ctx context.Context, _, _ string) error { return v.Say(ctx, "") }

func (v *ctxVoice) Say(ctx context.Context, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.errs = append(v.errs, ctx.Err())
	return ctx.Err()
}

// ctxAudit records whether the context each row was written on was already
// done.
type ctxAudit struct {
	mu   sync.Mutex
	errs []error
}

func (a *ctxAudit) Write(ctx context.Context, _ audit.Record) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errs = append(a.errs, ctx.Err())
	return ctx.Err()
}

func (*ctxAudit) Enabled() bool { return true }

// A leave's write wakes the loop, which cancels the session the command
// arrived on before the command has even returned. The operator must still
// hear the reply and the audit row must still land.
func TestACommandThatEndsTheSessionStillRepliesAndIsAudited(t *testing.T) {
	registry, pctx, _, eventBus, _, playerRoster, _ := newHarness(t)
	voice := &ctxVoice{}
	pctx.Voice = voice
	sessionCtx, endSession := context.WithCancel(t.Context())
	stub := leaveStub{got: make(chan []string, 1), during: endSession}
	if err := registry.Register(stub); err != nil {
		t.Fatal(err)
	}
	auditor := &ctxAudit{}
	perms := fakePermResolver(t, map[string]string{playerXUID: "operator"})

	handlePacket(sessionCtx, chatPacket(playerXUID, "Steve", "!leave"), selfXUID, nil, quiet(), registry, pctx, eventBus, unlimitedRateLimit(), playerRoster, perms, testAnswering(), store.Nop{}, auditor, newJoinTimes())
	if len(voice.errs) != 1 || voice.errs[0] != nil {
		t.Errorf("reply contexts = %v, want one reply on a live context", voice.errs)
	}
	if len(auditor.errs) != 1 || auditor.errs[0] != nil {
		t.Errorf("audit contexts = %v, want one row on a live context", auditor.errs)
	}
}

// A recycle already under way when the session ends must finish before the
// session reports that it has: the caller's handover would otherwise close
// the same playtime the recycle is closing.
func TestStoppingARecycleThatHasFiredWaitsForIt(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	stop := afterFuncWaited(time.Millisecond, func() {
		close(started)
		<-release
	})
	<-started
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while the recycle was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stop never returned after the recycle finished")
	}
}

func TestStoppingARecycleBeforeItFiresCancelsIt(t *testing.T) {
	var ran atomic.Bool
	stop := afterFuncWaited(time.Hour, func() { ran.Store(true) })
	stop()
	if ran.Load() {
		t.Error("the recycle ran after being stopped")
	}
}
