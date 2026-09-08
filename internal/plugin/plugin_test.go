package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
)

type stubPlugin struct {
	name string
	cmds []Command
}

func (p stubPlugin) Name() string        { return p.name }
func (p stubPlugin) Commands() []Command { return p.cmds }

func okCommand(name string, perm Permission) Command {
	return Command{
		Name:       name,
		Permission: perm,
		Run: func(ctx context.Context, pctx *Context, inv Invocation) (string, error) {
			return "ok:" + name, nil
		},
	}
}

func TestRegister_DuplicateCommandFails(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("ping", PermissionVisitor)}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(stubPlugin{name: "b", cmds: []Command{okCommand("ping", PermissionVisitor)}})
	if err == nil {
		t.Fatal("expected error registering a duplicate command name")
	}
}

func TestRegister_PartialFailureDoesNotRegisterAnyCommand(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("ping", PermissionVisitor)}}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// second plugin has one new command and one colliding command
	err := r.Register(stubPlugin{name: "b", cmds: []Command{
		okCommand("newcmd", PermissionVisitor),
		okCommand("ping", PermissionVisitor),
	}})
	if err == nil {
		t.Fatal("expected error for the colliding command")
	}
	if _, err := r.Dispatch(context.Background(), &Context{}, "newcmd", Invocation{ActorPermission: PermissionVisitor}); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("expected newcmd to remain unregistered after a partial-collision Register call, got err=%v", err)
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	r := NewRegistry()
	_, err := r.Dispatch(context.Background(), &Context{}, "nope", Invocation{})
	if !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("err = %v, want ErrUnknownCommand", err)
	}
}

func TestDispatch_PermissionDenied(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("op-only", PermissionOperator)}}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Dispatch(context.Background(), &Context{}, "op-only", Invocation{ActorPermission: PermissionVisitor})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestDispatch_PermissionGrantedAtExactLevel(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("member-only", PermissionMember)}}); err != nil {
		t.Fatal(err)
	}
	reply, err := r.Dispatch(context.Background(), &Context{}, "member-only", Invocation{ActorPermission: PermissionMember})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "ok:member-only" {
		t.Errorf("reply = %q, want ok:member-only", reply)
	}
}

func TestDispatch_HigherPermissionCanRunLowerCommand(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("visitor-cmd", PermissionVisitor)}}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Dispatch(context.Background(), &Context{}, "visitor-cmd", Invocation{ActorPermission: PermissionOperator})
	if err != nil {
		t.Errorf("operator should be able to run a visitor-level command, got err=%v", err)
	}
}

func TestCommands_SortedByName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{
		okCommand("zeta", PermissionVisitor),
		okCommand("alpha", PermissionVisitor),
	}}); err != nil {
		t.Fatal(err)
	}
	cmds := r.Commands()
	if len(cmds) != 2 {
		t.Fatalf("len(cmds) = %d, want 2", len(cmds))
	}
	if cmds[0].Name != "alpha" || cmds[1].Name != "zeta" {
		t.Errorf("cmds = %v, want [alpha zeta]", []string{cmds[0].Name, cmds[1].Name})
	}
}

func TestPlugins_ReturnsRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubPlugin{name: "second"}); err != nil {
		t.Fatal(err)
	}
	ps := r.Plugins()
	if len(ps) != 2 || ps[0].Name() != "first" || ps[1].Name() != "second" {
		t.Errorf("Plugins() = %v, want [first second]", ps)
	}
}

func TestPermissionString(t *testing.T) {
	cases := map[Permission]string{
		PermissionVisitor:  "visitor",
		PermissionMember:   "member",
		PermissionOperator: "operator",
		Permission(99):     "unknown",
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Errorf("Permission(%d).String() = %q, want %q", p, got, want)
		}
	}
}

func TestRegister_CommandNameIsNormalisedForDispatch(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("Help", PermissionVisitor)}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// chat.ParseTrigger lowercases what the player typed, so a command
	// registered with any casing must still be reachable.
	reply, err := r.Dispatch(context.Background(), &Context{}, "help", Invocation{ActorPermission: PermissionVisitor})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if reply != "ok:Help" {
		t.Errorf("reply = %q, want ok:Help", reply)
	}
}

func TestDispatch_MatchesCommandNameCaseInsensitively(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("ping", PermissionVisitor)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Dispatch(context.Background(), &Context{}, "PING", Invocation{ActorPermission: PermissionVisitor}); err != nil {
		t.Errorf("dispatch of PING: %v", err)
	}
}

func TestRegister_CaseDifferingNamesCollide(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("help", PermissionVisitor)}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubPlugin{name: "b", cmds: []Command{okCommand("HELP", PermissionVisitor)}}); err == nil {
		t.Fatal("expected HELP to collide with the already-registered help")
	}
}

func TestRegister_RejectsDuplicateNamesWithinOnePlugin(t *testing.T) {
	r := NewRegistry()
	err := r.Register(stubPlugin{name: "a", cmds: []Command{
		okCommand("ping", PermissionVisitor),
		okCommand("Ping", PermissionVisitor),
	}})
	if err == nil {
		t.Fatal("expected an error for a plugin declaring the same command twice")
	}
	if len(r.Commands()) != 0 {
		t.Errorf("Commands() = %v, want none registered after a failed Register", r.Commands())
	}
}

func TestRegister_RejectsEmptyCommandName(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("  ", PermissionVisitor)}}); err == nil {
		t.Fatal("expected an error for a command with an empty name")
	}
}

func TestRegister_RejectsNilRun(t *testing.T) {
	r := NewRegistry()
	err := r.Register(stubPlugin{name: "a", cmds: []Command{{Name: "broken", Permission: PermissionVisitor}}})
	if err == nil {
		t.Fatal("expected an error for a command with a nil Run")
	}
	if _, err := r.Dispatch(context.Background(), &Context{}, "broken", Invocation{}); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("err = %v, want ErrUnknownCommand for a rejected command", err)
	}
}

func TestDispatch_RecoversPluginPanic(t *testing.T) {
	r := NewRegistry()
	err := r.Register(stubPlugin{name: "boom", cmds: []Command{{
		Name:       "explode",
		Permission: PermissionVisitor,
		Run: func(ctx context.Context, pctx *Context, inv Invocation) (string, error) {
			panic("plugin bug")
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	reply, err := r.Dispatch(context.Background(), &Context{}, "explode", Invocation{ActorPermission: PermissionVisitor})
	if !errors.Is(err, ErrCommandPanicked) {
		t.Fatalf("err = %v, want ErrCommandPanicked", err)
	}
	if reply != "" {
		t.Errorf("reply = %q, want empty after a panic", reply)
	}
}

func TestDispatch_SurvivesAPanicAndKeepsServingOtherCommands(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "boom", cmds: []Command{{
		Name:       "explode",
		Permission: PermissionVisitor,
		Run: func(ctx context.Context, pctx *Context, inv Invocation) (string, error) {
			panic("plugin bug")
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("ping", PermissionVisitor)}}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Dispatch(context.Background(), &Context{}, "explode", Invocation{ActorPermission: PermissionVisitor}); err == nil {
		t.Fatal("expected an error from the panicking command")
	}
	reply, err := r.Dispatch(context.Background(), &Context{}, "ping", Invocation{ActorPermission: PermissionVisitor})
	if err != nil || reply != "ok:ping" {
		t.Errorf("after a panic, ping returned (%q, %v), want (ok:ping, nil)", reply, err)
	}
}

func TestDispatch_TimesOutAHungCommandWithoutWaitingForIt(t *testing.T) {
	orig := DefaultDispatchTimeout
	DefaultDispatchTimeout = 30 * time.Millisecond
	defer func() { DefaultDispatchTimeout = orig }()

	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "hangs", cmds: []Command{{
		Name:       "hang",
		Permission: PermissionVisitor,
		Run: func(ctx context.Context, pctx *Context, inv Invocation) (string, error) {
			<-ctx.Done() // never returns on its own; only the timeout unblocks it
			return "", ctx.Err()
		},
	}}}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := r.Dispatch(context.Background(), &Context{}, "hang", Invocation{ActorPermission: PermissionVisitor})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCommandTimedOut) {
		t.Fatalf("err = %v, want ErrCommandTimedOut", err)
	}
	// Dispatch must return at (approximately) DefaultDispatchTimeout, not
	// hang forever waiting for a command that only unblocks because of that
	// same timeout - proving the caller (in production, the packet read
	// loop) is never stalled past this bound.
	if elapsed > DefaultDispatchTimeout+time.Second {
		t.Errorf("Dispatch took %v, want close to DefaultDispatchTimeout (%v)", elapsed, DefaultDispatchTimeout)
	}
}

func TestDispatch_StillReturnsPromptlyForAFastCommand(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "a", cmds: []Command{okCommand("ping", PermissionVisitor)}}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	reply, err := r.Dispatch(context.Background(), &Context{}, "ping", Invocation{ActorPermission: PermissionVisitor})
	if err != nil || reply != "ok:ping" {
		t.Fatalf("reply=%q err=%v, want ok:ping, nil", reply, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a fast command took %v to return", elapsed)
	}
}

type recordingEventHandler struct {
	kinds    []string
	received []bus.Event
}

func (h *recordingEventHandler) Name() string        { return "recorder" }
func (h *recordingEventHandler) Commands() []Command { return nil }
func (h *recordingEventHandler) Kinds() []string     { return h.kinds }
func (h *recordingEventHandler) HandleEvent(ctx context.Context, pctx *Context, ev bus.Event) error {
	h.received = append(h.received, ev)
	return nil
}

type joinEvent struct{ playerXUID string }

func (joinEvent) Kind() string { return "join" }

func TestEventHandler_ReceivesTheActualEventNotJustItsKind(t *testing.T) {
	h := &recordingEventHandler{kinds: []string{"join"}}
	var eh EventHandler = h

	ev := joinEvent{playerXUID: "2535412345678901"}
	if err := eh.HandleEvent(context.Background(), &Context{}, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	if len(h.received) != 1 {
		t.Fatalf("received %d events, want 1", len(h.received))
	}
	got, ok := h.received[0].(joinEvent)
	if !ok {
		t.Fatalf("received event has type %T, want joinEvent", h.received[0])
	}
	if got.playerXUID != ev.playerXUID {
		t.Errorf("playerXUID = %q, want %q - a welcome plugin needs this to know who joined", got.playerXUID, ev.playerXUID)
	}
}

func TestDispatch_PanicIsCheckedAfterPermission(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubPlugin{name: "boom", cmds: []Command{{
		Name:       "op-explode",
		Permission: PermissionOperator,
		Run: func(ctx context.Context, pctx *Context, inv Invocation) (string, error) {
			panic("must never run")
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Dispatch(context.Background(), &Context{}, "op-explode", Invocation{ActorPermission: PermissionVisitor}); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("err = %v, want ErrPermissionDenied", err)
	}
}

func TestContextKnowledgeAndWaypointsMayBeNil(t *testing.T) {
	// The zero Context is what a test or an unconfigured binary hands a
	// plugin. Reading these fields must not panic; plugins guard.
	var pctx Context
	if pctx.Knowledge != nil {
		t.Error("zero Context.Knowledge should be nil")
	}
	if pctx.Waypoints != nil {
		t.Error("zero Context.Waypoints should be nil")
	}
}
