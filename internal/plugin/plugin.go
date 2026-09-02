// Package plugin defines the extension surface every feature of the agent
// is built on: a Plugin registers ! commands (and, via optional interfaces,
// reacts to server events or runs on a schedule); a Registry dispatches
// incoming commands to the right plugin after a permission check.
//
// Plugins never touch the Bedrock connection or the console bridge
// directly - they only ever see the adapters handed to them through
// Context, so a plugin bug can say the wrong thing but cannot reach past
// its sanctioned capabilities.
package plugin

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Permission is the minimum privilege level a command requires, resolved
// from the server's permissions.json (read by mc-console-bridge, wired in
// a later stage).
type Permission int

const (
	PermissionVisitor Permission = iota
	PermissionMember
	PermissionOperator
)

func (p Permission) String() string {
	switch p {
	case PermissionVisitor:
		return "visitor"
	case PermissionMember:
		return "member"
	case PermissionOperator:
		return "operator"
	default:
		return "unknown"
	}
}

// Voice is how a plugin speaks - always through the console bridge, never
// as a player. Implemented against mc-console-bridge starting Stage 2; a
// no-op implementation is used until then.
type Voice interface {
	// Tell whispers message to the player identified by xuid.
	Tell(ctx context.Context, xuid, message string) error
	// Say broadcasts message to everyone.
	Say(ctx context.Context, message string) error
}

// Facts is how a plugin reads live server state. Its method set grows in
// later stages as real capabilities (online players, server status, ...)
// are wired to the bridge and mc-monitor; it is intentionally empty for
// now so Stage 1 doesn't commit to a shape those stages haven't earned yet.
type Facts interface{}

// Invocation is one player's attempt to run a command.
type Invocation struct {
	// ActorXUID identifies who issued the command (never a gamertag - see
	// internal/chat for why).
	ActorXUID string
	// ActorPermission is the actor's resolved permission level.
	ActorPermission Permission
	// Args is the whitespace-split argument list, command name excluded.
	Args []string
}

// Command is one ! command a plugin exposes.
type Command struct {
	// Name is matched case-insensitively against the command a player
	// typed, without the "!" prefix.
	Name string
	// Description is shown by the core plugin's !help.
	Description string
	// Permission is the minimum level required to run this command.
	Permission Permission
	// Run executes the command. ctx carries the process lifetime; pctx
	// carries the adapters this command is allowed to use.
	Run func(ctx context.Context, pctx *Context, inv Invocation) (reply string, err error)
}

// Plugin is one self-contained unit of agent behaviour.
type Plugin interface {
	// Name identifies the plugin in logs and diagnostics.
	Name() string
	// Commands returns the ! commands this plugin contributes. May be
	// empty for a plugin that only reacts to events.
	Commands() []Command
}

// EventHandler is an optional Plugin capability: react to bus events
// (join/leave/death/server-log events, ...). A Plugin that doesn't need
// this simply doesn't implement it.
type EventHandler interface {
	// HandleEvent is called for every event the plugin's Kinds() selects.
	// It must not block for long - slow work should be started in a
	// goroutine.
	HandleEvent(ctx context.Context, pctx *Context, kind string) error
	// Kinds lists the event kinds this plugin wants delivered.
	Kinds() []string
}

// Directory lets a plugin (typically "core", for !help) list every
// registered command. Registry implements this.
type Directory interface {
	Commands() []Command
}

// Context is everything a running command or event handler is allowed to
// touch.
type Context struct {
	Voice     Voice
	Facts     Facts
	Directory Directory
}

// Registry holds every registered plugin and routes commands to them.
type Registry struct {
	mu       sync.RWMutex
	plugins  []Plugin
	commands map[string]Command
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry {
	return &Registry{commands: make(map[string]Command)}
}

// Register adds a plugin's commands to the registry. It fails if any
// command name collides with one already registered, so two plugins can
// never silently shadow each other.
func (r *Registry) Register(p Plugin) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, cmd := range p.Commands() {
		if _, exists := r.commands[cmd.Name]; exists {
			return fmt.Errorf("plugin %s: command %q is already registered", p.Name(), cmd.Name)
		}
	}
	for _, cmd := range p.Commands() {
		r.commands[cmd.Name] = cmd
	}
	r.plugins = append(r.plugins, p)
	return nil
}

// ErrUnknownCommand is returned by Dispatch when no plugin registered the
// requested command.
var ErrUnknownCommand = fmt.Errorf("unknown command")

// ErrPermissionDenied is returned by Dispatch when the actor's permission
// is below the command's required level.
var ErrPermissionDenied = fmt.Errorf("permission denied")

// Dispatch finds the command named name and runs it, after checking that
// inv.ActorPermission meets the command's requirement. name is matched
// case-insensitively by the caller (see internal/chat.ParseTrigger, which
// already lowercases it).
func (r *Registry) Dispatch(ctx context.Context, pctx *Context, name string, inv Invocation) (reply string, err error) {
	r.mu.RLock()
	cmd, ok := r.commands[name]
	r.mu.RUnlock()

	if !ok {
		return "", ErrUnknownCommand
	}
	if inv.ActorPermission < cmd.Permission {
		return "", ErrPermissionDenied
	}
	return cmd.Run(ctx, pctx, inv)
}

// Commands returns every registered command, sorted by name. Satisfies
// Directory.
func (r *Registry) Commands() []Command {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Command, 0, len(r.commands))
	for _, cmd := range r.commands {
		out = append(out, cmd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Plugins returns every registered plugin, in registration order.
// Primarily useful for wiring event subscriptions in main.
func (r *Registry) Plugins() []Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Plugin, len(r.plugins))
	copy(out, r.plugins)
	return out
}
