package presence

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

type names map[string]string

func (n names) NameFor(xuid string) (string, bool) { v, ok := n[xuid]; return v, ok }

type chatRig struct {
	clock  time.Time
	store  *fakeStore
	audit  *fakeAudit
	joins  *JoinLog
	plugin *ChatPlugin
}

func newChatRig(t *testing.T) *chatRig {
	t.Helper()
	r := &chatRig{clock: t0, store: newFakeStore(), joins: NewJoinLog()}
	var svc *Service
	svc, r.audit = newTestService(t, r.store, &r.clock)
	r.plugin = NewChatPlugin(svc, names{"2535400000000001": "Jdwillmsen"}, r.joins)
	r.plugin.now = func() time.Time { return r.clock }
	return r
}

func (r *chatRig) run(t *testing.T, name string, args ...string) string {
	t.Helper()
	for _, c := range r.plugin.Commands() {
		if c.Name == name {
			reply, err := c.Run(t.Context(), &plugin.Context{}, plugin.Invocation{ActorXUID: "2535400000000001", ActorPermission: plugin.PermissionOperator, Args: args})
			if err != nil {
				t.Fatalf("!%s %v: %v", name, args, err)
			}
			return reply
		}
	}
	t.Fatalf("no command %q", name)
	return ""
}

func TestChatCommandPermissions(t *testing.T) {
	r := newChatRig(t)
	want := map[string]plugin.Permission{"presence": plugin.PermissionMember, "park": plugin.PermissionOperator, "unpark": plugin.PermissionOperator, "leave": plugin.PermissionOperator}
	reg := plugin.NewRegistry()
	if err := reg.Register(r.plugin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for name, perm := range want {
		cmd, ok := reg.Lookup(name)
		if !ok || cmd.Permission != perm {
			t.Errorf("!%s registered = %v at %v, want %v", name, ok, cmd.Permission, perm)
		}
	}
	_, err := reg.Dispatch(t.Context(), &plugin.Context{}, "park", plugin.Invocation{ActorXUID: "x", ActorPermission: plugin.PermissionMember, Args: []string{"bots"}})
	if !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Errorf("a member's !park = %v, want ErrPermissionDenied", err)
	}
}

func TestParkBotsForADuration(t *testing.T) {
	r := newChatRig(t)
	reply := r.run(t, "park", "bots", "2h")
	for _, id := range []string{"afk-bot-1", "afk-bot-2"} {
		ov, ok := r.store.row(id)
		if !ok || ov.State != presenceapi.StateParked || ov.Until == nil || !ov.Until.Equal(t0.Add(2*time.Hour)) || ov.WakeOn != nil {
			t.Errorf("%s = %+v, %v; want parked for two hours with no wake", id, ov, ok)
		}
		if ov.SetBy != "chat:Jdwillmsen" {
			t.Errorf("%s set_by = %q", id, ov.SetBy)
		}
		if ov.Reason != "parked from chat by Jdwillmsen" {
			t.Errorf("%s reason = %q", id, ov.Reason)
		}
	}
	if !strings.Contains(reply, "afk-bot-1 parked") || !strings.Contains(reply, "20:00 UTC") {
		t.Errorf("reply = %q", reply)
	}
}

func TestParkAllGivesTheAgentAWayBack(t *testing.T) {
	r := newChatRig(t)
	r.run(t, "park", "all")
	agent, _ := r.store.row("agent")
	if agent.Until == nil || !agent.Until.Equal(t0.Add(AgentParkWindow)) || agent.WakeOn == nil || !agent.WakeOn.AnyPlayerJoin {
		t.Errorf("agent = %+v, want an hour and a wake on any join", agent)
	}
	bot, _ := r.store.row("afk-bot-1")
	if bot.Until != nil || bot.WakeOn != nil {
		t.Errorf("bot = %+v, want parked until unparked", bot)
	}
	if agent.Reason != "parked from chat by Jdwillmsen" || agent.SetBy != "chat:Jdwillmsen" {
		t.Errorf("agent reason, set_by = %q, %q", agent.Reason, agent.SetBy)
	}
}

func TestLeaveParksTheAgent(t *testing.T) {
	r := newChatRig(t)
	reply := r.run(t, "leave", "30m")
	agent, ok := r.store.row("agent")
	if !ok || !agent.Until.Equal(t0.Add(30*time.Minute)) || !agent.WakeOn.AnyPlayerJoin {
		t.Errorf("agent = %+v, want thirty minutes and a wake", agent)
	}
	if !strings.Contains(reply, "18:30 UTC") || !strings.Contains(reply, "player joins") {
		t.Errorf("reply = %q, want when it comes back and that a join brings it back", reply)
	}
}

func TestUnparkReturnsToTheDefault(t *testing.T) {
	r := newChatRig(t)
	r.run(t, "park", "afk-bot-1")
	reply := r.run(t, "unpark", "afk-bot-1")
	if _, ok := r.store.row("afk-bot-1"); ok {
		t.Error("override still stored after !unpark")
	}
	if !strings.Contains(reply, "afk-bot-1 present (default)") {
		t.Errorf("reply = %q", reply)
	}
}

func TestParkRefusesBadArgumentsWithUsage(t *testing.T) {
	r := newChatRig(t)
	for _, args := range [][]string{{}, {"miners"}, {"bots", "soon"}, {"bots", "-1h"}, {"bots", "1h", "extra"}} {
		reply := r.run(t, "park", args...)
		if !strings.HasPrefix(reply, "Usage: !park") || !strings.Contains(reply, "afk-bot-1, afk-bot-2, bots, all") {
			t.Errorf("!park %v = %q, want the usage line", args, reply)
		}
	}
	if len(r.audit.all()) != 0 {
		t.Error("a refused !park wrote something")
	}
}

func TestPresenceListsEveryActor(t *testing.T) {
	r := newChatRig(t)
	r.run(t, "park", "afk-bot-1", "2h")
	got := r.run(t, "presence")
	want := "agent present (default); afk-bot-1 parked by chat:Jdwillmsen until 20:00 UTC; afk-bot-2 parked (default)"
	if got != want {
		t.Errorf("!presence = %q, want %q", got, want)
	}
}

func TestChatSaysSoWhenTheStoreIsDown(t *testing.T) {
	r := newChatRig(t)
	r.store.fail(errors.New("connection refused"))
	if got := r.run(t, "park", "bots"); !strings.Contains(got, "database") {
		t.Errorf("!park = %q, want it to say the database is not answering", got)
	}
	if got := r.run(t, "presence"); !strings.Contains(got, "database") {
		t.Errorf("!presence = %q", got)
	}
}

func TestChatWithoutActorsSaysItIsNotSetUp(t *testing.T) {
	reg, _ := NewRegistry(nil, "agent")
	p := NewChatPlugin(NewService(reg, Nop{}, &fakeAudit{}, logging.New("error")), names{}, NewJoinLog())
	for _, c := range p.Commands() {
		reply, err := c.Run(t.Context(), &plugin.Context{}, plugin.Invocation{ActorPermission: plugin.PermissionOperator})
		if err != nil || reply != notConfigured {
			t.Errorf("!%s = %q, %v; want %q", c.Name, reply, err, notConfigured)
		}
	}
}

func TestConsoleIsNamedConsole(t *testing.T) {
	r := newChatRig(t)
	for _, c := range r.plugin.Commands() {
		if c.Name == "park" {
			if _, err := c.Run(t.Context(), &plugin.Context{}, plugin.Invocation{ActorXUID: chat.ServerOrigin, ActorPermission: plugin.PermissionOperator, Args: []string{"afk-bot-1"}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ov, _ := r.store.row("afk-bot-1"); ov.SetBy != "chat:console" {
		t.Errorf("set_by = %q, want chat:console", ov.SetBy)
	}
}

func TestJoinEventsFeedTheJoinLog(t *testing.T) {
	r := newChatRig(t)
	if !slices.Contains(r.plugin.Kinds(), roster.JoinKind) {
		t.Fatalf("Kinds() = %v, want the roster join", r.plugin.Kinds())
	}
	r.joins.now = func() time.Time { return t0 }
	if err := r.plugin.HandleEvent(t.Context(), &plugin.Context{}, roster.JoinEvent{Entry: roster.Entry{XUID: "1", Username: "Steve"}}); err != nil {
		t.Fatal(err)
	}
	if got := r.joins.Recent(); len(got) != 1 || got[0].Gamertag != "Steve" {
		t.Errorf("join log = %+v", got)
	}
}

func TestLeaveArgs(t *testing.T) {
	cases := []struct {
		message string
		args    []string
		ok      bool
	}{
		{"@server leave", []string{}, true},
		{"@Server LEAVE 30m", []string{"30m"}, true},
		{"  @server   leave  2h ", []string{"2h"}, true},
		{"@server please leave", nil, false},
		{"@server leave now please", nil, false},
		{"can @server leave?", nil, false},
		{"@server where is the leaves farm", nil, false},
		// U+017F, the long s, folds to "s" under Unicode rules only.
		{"@\u017fERVER leave", nil, false},
	}
	for _, tc := range cases {
		args, ok := LeaveArgs(tc.message)
		if ok != tc.ok || (ok && !slices.Equal(args, tc.args)) {
			t.Errorf("LeaveArgs(%q) = %v, %v; want %v, %v", tc.message, args, ok, tc.args, tc.ok)
		}
	}
}

func TestServerLeaveParksTheAgentWithTheGivenTimer(t *testing.T) {
	r := newChatRig(t)
	args, ok := LeaveArgs("@server leave 30m")
	if !ok {
		t.Fatal("LeaveArgs refused @server leave 30m")
	}
	r.run(t, "leave", args...)
	agent, ok := r.store.row("agent")
	if !ok || agent.State != presenceapi.StateParked || !agent.Until.Equal(t0.Add(30*time.Minute)) || agent.WakeOn == nil || !agent.WakeOn.AnyPlayerJoin {
		t.Fatalf("agent = %+v, %v; want parked for thirty minutes with a wake", agent, ok)
	}
	if agent.Reason != "asked to leave from chat by Jdwillmsen" || agent.SetBy != "chat:Jdwillmsen" {
		t.Errorf("agent reason, set_by = %q, %q", agent.Reason, agent.SetBy)
	}
}

// sayRecorder notes, for each broadcast, whether the agent's park had
// already been written when it was said, and whether its context was live.
type sayRecorder struct {
	store     *fakeStore
	said      []string
	parkedYet []bool
	ctxErr    []error
	err       error
}

func (v *sayRecorder) Tell(context.Context, string, string) error { return nil }
func (v *sayRecorder) Say(ctx context.Context, msg string) error {
	_, parked := v.store.row("agent")
	v.said = append(v.said, msg)
	v.parkedYet = append(v.parkedYet, parked)
	v.ctxErr = append(v.ctxErr, ctx.Err())
	return v.err
}

func (r *chatRig) runWith(t *testing.T, pctx *plugin.Context, name string, args ...string) string {
	t.Helper()
	for _, c := range r.plugin.Commands() {
		if c.Name == name {
			reply, err := c.Run(t.Context(), pctx, plugin.Invocation{ActorXUID: "2535400000000001", ActorPermission: plugin.PermissionOperator, Args: args})
			if err != nil {
				t.Fatalf("!%s %v: %v", name, args, err)
			}
			return reply
		}
	}
	t.Fatalf("no command %q", name)
	return ""
}

func TestTheAgentConfirmsOnceTheLeaveIsWritten(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{{"leave", nil}, {"leave", []string{"30m"}}, {"park", []string{"all"}}, {"park", []string{"agent", "2h"}}} {
		r := newChatRig(t)
		voice := &sayRecorder{store: r.store}
		r.runWith(t, &plugin.Context{Voice: voice}, tc.command, tc.args...)
		if len(voice.said) != 1 || !voice.parkedYet[0] {
			t.Errorf("!%s %v: said %q (park already written: %v), want one confirmation after the write", tc.command, tc.args, voice.said, voice.parkedYet)
			continue
		}
		if !strings.Contains(voice.said[0], "Leaving the world") || !strings.Contains(voice.said[0], "player joins") {
			t.Errorf("!%s %v: said %q, want when and how it comes back", tc.command, tc.args, voice.said[0])
		}
	}
}

// The write wakes the loop, which ends the session whose context the
// handler runs on; the broadcast must survive that.
func TestTheConfirmationOutlivesTheSessionItEnds(t *testing.T) {
	r := newChatRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.plugin.svc.OnChange(cancel)
	voice := &sayRecorder{store: r.store}
	for _, c := range r.plugin.Commands() {
		if c.Name == "leave" {
			if _, err := c.Run(ctx, &plugin.Context{Voice: voice}, plugin.Invocation{ActorXUID: "2535400000000001", ActorPermission: plugin.PermissionOperator}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ctx.Err() == nil {
		t.Fatal("the write did not cancel the handler's context; the test proves nothing")
	}
	if len(voice.ctxErr) != 1 || voice.ctxErr[0] != nil {
		t.Errorf("broadcast context errors = %v, want one live context", voice.ctxErr)
	}
}

func TestAFailedLeaveIsNeverAnnounced(t *testing.T) {
	for _, tc := range []struct {
		command string
		args    []string
	}{{"leave", nil}, {"park", []string{"all"}}} {
		r := newChatRig(t)
		r.store.fail(errors.New("connection refused"))
		voice := &sayRecorder{store: r.store}
		reply := r.runWith(t, &plugin.Context{Voice: voice}, tc.command, tc.args...)
		if len(voice.said) != 0 {
			t.Errorf("!%s %v: said %q after a failed write", tc.command, tc.args, voice.said)
		}
		if !strings.Contains(reply, "database") || !strings.Contains(reply, "nothing was parked") || !strings.Contains(reply, "staying") {
			t.Errorf("!%s %v: reply = %q, want that nothing was parked and it is staying", tc.command, tc.args, reply)
		}
	}
}

func TestAFailedBotParkSaysNothingWasParked(t *testing.T) {
	r := newChatRig(t)
	r.store.fail(errors.New("connection refused"))
	if got := r.run(t, "park", "bots"); !strings.Contains(got, "nothing was parked") {
		t.Errorf("!park bots = %q, want that nothing was parked", got)
	}
}

func TestParkingOnlyBotsSaysNothingInAdvance(t *testing.T) {
	r := newChatRig(t)
	voice := &sayRecorder{store: r.store}
	r.runWith(t, &plugin.Context{Voice: voice}, "park", "bots")
	if len(voice.said) != 0 {
		t.Errorf("said %q, want nothing: the agent is not leaving", voice.said)
	}
}

func TestLeaveRepliesWithTheConfirmationWhenItCouldNotBeSaid(t *testing.T) {
	r := newChatRig(t)
	voice := &sayRecorder{store: r.store, err: errors.New("bridge down")}
	reply := r.runWith(t, &plugin.Context{Voice: voice}, "leave")
	if !strings.Contains(reply, "Leaving the world") {
		t.Errorf("reply = %q, want the confirmation that could not be broadcast", reply)
	}
}
