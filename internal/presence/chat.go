package presence

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// NameResolver names the player behind an XUID. Satisfied by *roster.Roster.
type NameResolver interface {
	NameFor(xuid string) (string, bool)
}

const (
	notConfigured     = "Presence control isn't set up on this server."
	storeDownRead     = "I can't read presence right now - the database isn't answering."
	storeDownWrite    = "I couldn't save that - the database isn't answering."
	storeDownParked   = "I couldn't save that - the database isn't answering, so nothing was parked."
	storeDownStaying  = "I couldn't save that - the database isn't answering, so nothing was parked and I'm staying."
	chatParkReason    = "parked from chat by "
	chatLeaveReason   = "asked to leave from chat by "
	leaveUsage        = "Usage: !leave [duration], or @server leave [duration]. Durations look like 30m or 2h."
	leaveConfirmation = "Leaving the world. I'll be back at %s, or as soon as a player joins."
)

// ChatPlugin is presence as players and operators reach it in chat. It also
// feeds the join log from the session's roster events, which is how a
// player arriving wakes a parked actor while the agent is in the world.
type ChatPlugin struct {
	svc   *Service
	names NameResolver
	joins *JoinLog
	now   func() time.Time
}

var (
	_ plugin.Plugin       = (*ChatPlugin)(nil)
	_ plugin.EventHandler = (*ChatPlugin)(nil)
)

func NewChatPlugin(svc *Service, names NameResolver, joins *JoinLog) *ChatPlugin {
	return &ChatPlugin{svc: svc, names: names, joins: joins, now: time.Now}
}

func (*ChatPlugin) Name() string { return "presence" }

func (p *ChatPlugin) Kinds() []string { return []string{roster.JoinKind} }

func (p *ChatPlugin) HandleEvent(_ context.Context, _ *plugin.Context, ev bus.Event) error {
	if j, ok := ev.(roster.JoinEvent); ok {
		p.joins.Record(j.Username)
	}
	return nil
}

func (p *ChatPlugin) Commands() []plugin.Command {
	return []plugin.Command{
		{Name: "presence", Description: "Show which actors are in the world, and why.", Permission: plugin.PermissionMember, Run: p.status},
		{Name: "park", Description: "Take an actor, group or all out of the world: !park <target> [duration].", Permission: plugin.PermissionOperator, Run: p.park},
		{Name: "unpark", Description: "Return an actor, group or all to its default: !unpark <target>.", Permission: plugin.PermissionOperator, Run: p.unpark},
		{Name: "leave", Description: "Send me out of the world: !leave [duration], or @server leave.", Permission: plugin.PermissionOperator, Run: p.leave},
	}
}

func (p *ChatPlugin) status(ctx context.Context, _ *plugin.Context, _ plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	views, err := p.svc.List(ctx)
	if errors.Is(err, ErrUnavailable) {
		return storeDownRead, nil
	}
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, describe(v.Presence))
	}
	return strings.Join(parts, "; "), nil
}

func (p *ChatPlugin) park(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	if len(inv.Args) < 1 || len(inv.Args) > 2 {
		return p.usage("park <target> [duration]"), nil
	}
	actors, ok := p.svc.Registry().Resolve(inv.Args[0])
	if !ok {
		return p.usage("park <target> [duration]"), nil
	}
	var d time.Duration
	if len(inv.Args) == 2 {
		var err error
		if d, err = time.ParseDuration(inv.Args[1]); err != nil || d <= 0 {
			return p.usage("park <target> [duration]"), nil
		}
	}
	return p.parkActors(ctx, pctx, actors, d, chatParkReason, inv)
}

func (p *ChatPlugin) leave(ctx context.Context, pctx *plugin.Context, inv plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	var d time.Duration
	switch len(inv.Args) {
	case 0:
	case 1:
		var err error
		if d, err = time.ParseDuration(inv.Args[0]); err != nil || d <= 0 {
			return leaveUsage, nil
		}
	default:
		return leaveUsage, nil
	}
	self, _ := p.svc.Registry().Actor(p.svc.Registry().SelfID())
	return p.parkActors(ctx, pctx, []Actor{self}, d, chatLeaveReason, inv)
}

func (p *ChatPlugin) unpark(ctx context.Context, _ *plugin.Context, inv plugin.Invocation) (string, error) {
	if !p.svc.Enabled() {
		return notConfigured, nil
	}
	if len(inv.Args) != 1 {
		return p.usage("unpark <target>"), nil
	}
	actors, ok := p.svc.Registry().Resolve(inv.Args[0])
	if !ok {
		return p.usage("unpark <target>"), nil
	}
	views, err := p.svc.Clear(ctx, actors, p.source(inv))
	if errors.Is(err, ErrUnavailable) {
		return storeDownWrite, nil
	}
	if err != nil {
		return "", err
	}
	return "Back to default: " + describeAll(views), nil
}

// parkActors parks actors, and announces it once the write has landed if
// the agent is among them, so a leave that did not happen is never
// announced.
//
// The announcement goes through the bridge, which outlives the session the
// write is about to end, but the handler's ctx does not: it is the session's,
// and the write wakes the loop that cancels it. So the broadcast runs
// detached from that cancellation, under its own bound.
func (p *ChatPlugin) parkActors(ctx context.Context, pctx *plugin.Context, actors []Actor, d time.Duration, reason string, inv plugin.Invocation) (string, error) {
	now := p.now()
	by := p.source(inv)
	views, err := p.svc.SetEach(ctx, actors, func(a Actor) Request {
		until, wake := ChatPark(a, d, now)
		return Request{State: presenceapi.StateParked, Until: until, WakeOn: wake, Reason: reason + by.Gamertag}
	}, by)
	leaving := slices.ContainsFunc(actors, func(a Actor) bool { return a.ID == p.svc.Registry().SelfID() })
	switch {
	case errors.Is(err, ErrUnavailable) && leaving:
		return storeDownStaying, nil
	case errors.Is(err, ErrUnavailable):
		return storeDownParked, nil
	case err != nil:
		return "", err
	}
	parked := "Parked: " + describeAll(views)
	if !leaving {
		return parked, nil
	}
	self, _ := p.svc.Registry().Actor(p.svc.Registry().SelfID())
	until, _ := ChatPark(self, d, now)
	confirmation := fmt.Sprintf(leaveConfirmation, utcClock(*until))
	if pctx == nil || pctx.Voice == nil {
		return confirmation + " " + parked, nil
	}
	sayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plugin.DefaultDispatchTimeout)
	defer cancel()
	if pctx.Voice.Say(sayCtx, confirmation) != nil {
		return confirmation + " " + parked, nil
	}
	return parked, nil
}

// source names who typed the command. The console has no gamertag, and the
// roster may not know a name the moment after a reconnect, when the XUID is
// the only honest record.
func (p *ChatPlugin) source(inv plugin.Invocation) Source {
	name := inv.ActorXUID
	switch {
	case inv.ActorXUID == chat.ServerOrigin:
		name = "console"
	case p.names != nil:
		if n, ok := p.names.NameFor(inv.ActorXUID); ok && n != "" {
			name = n
		}
	}
	return ChatSource(inv.ActorXUID, name, inv.ActorPermission.String())
}

func (p *ChatPlugin) usage(form string) string {
	return fmt.Sprintf("Usage: !%s, where target is one of: %s. Durations look like 30m or 2h.", form, strings.Join(p.svc.Registry().Targets(), ", "))
}

func describeAll(views []presenceapi.Presence) string {
	parts := make([]string, 0, len(views))
	for _, v := range views {
		parts = append(parts, describe(v))
	}
	return strings.Join(parts, "; ")
}

// describe is one actor in one short clause: chat lines wrap early, and a
// reply about three actors has to stay readable.
func describe(v presenceapi.Presence) string {
	if v.Override == nil {
		return fmt.Sprintf("%s %s (default)", v.ActorID, v.Effective)
	}
	s := fmt.Sprintf("%s %s by %s", v.ActorID, v.Effective, v.Override.SetBy)
	if v.Override.Until != nil {
		s += " until " + utcClock(*v.Override.Until)
	}
	if w := v.Override.WakeOn; w != nil {
		if w.AnyPlayerJoin {
			s += " or a player joins"
		} else {
			s += " or " + strings.Join(w.Players, "/") + " joins"
		}
	}
	return s
}

func utcClock(t time.Time) string { return t.UTC().Format("15:04 UTC") }

// LeaveArgs recognises "@server leave [duration]" and returns its arguments.
// Only the exact form, at the start of the message: "@server where are the
// leaves" is a question for the model, not a command to walk out. Both
// keywords are ASCII, so the fold is too, and a look-alike letter such as
// the long s does not spell one.
func LeaveArgs(message string) ([]string, bool) {
	fields := strings.Fields(message)
	if len(fields) < 2 || len(fields) > 3 || text.FoldASCII(fields[0]) != chat.MentionToken || text.FoldASCII(fields[1]) != "leave" {
		return nil, false
	}
	return fields[2:], true
}
