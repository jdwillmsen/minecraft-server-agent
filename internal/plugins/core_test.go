package plugins

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

func TestCorePing_WithoutAPingerAnswersFromTheAgent(t *testing.T) {
	ping := findCommand(t, NewCore().Commands(), "ping")

	reply, err := ping.Run(context.Background(), &plugin.Context{}, plugin.Invocation{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "pong" {
		t.Errorf("reply = %q, want pong", reply)
	}
}

type fixedPinger plugin.ServerPing

func (f fixedPinger) Ping(context.Context) plugin.ServerPing { return plugin.ServerPing(f) }

func TestCorePing_ReportsTheServer(t *testing.T) {
	ping := findCommand(t, NewCore().Commands(), "ping")

	for _, tc := range []struct {
		name string
		got  plugin.ServerPing
		want string
	}{
		{
			name: "everything measured",
			got:  plugin.ServerPing{TPS: 19.96, TPSKnown: true, Link: 6 * time.Millisecond, LinkKnown: true},
			want: "pong - TPS 20.0, link 6ms",
		},
		{
			name: "link under a millisecond",
			got:  plugin.ServerPing{TPS: 20, TPSKnown: true, Link: 400 * time.Microsecond, LinkKnown: true},
			want: "pong - TPS 20.0, link under 1ms",
		},
		{
			name: "no baseline yet",
			got:  plugin.ServerPing{Link: 6 * time.Millisecond, LinkKnown: true},
			want: "pong - TPS still measuring, try again in a minute, link 6ms",
		},
		{
			name: "console down, between sessions",
			got:  plugin.ServerPing{TPSErr: errors.New("console not connected")},
			want: "pong - TPS unavailable (console didn't answer), link unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, err := ping.Run(context.Background(), &plugin.Context{Pinger: fixedPinger(tc.got)}, plugin.Invocation{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reply != tc.want {
				t.Errorf("reply = %q, want %q", reply, tc.want)
			}
		})
	}
}

func TestCoreHelp_NoDirectoryFails(t *testing.T) {
	cmds := NewCore().Commands()
	help := findCommand(t, cmds, "help")

	_, err := help.Run(context.Background(), &plugin.Context{}, plugin.Invocation{})
	if err == nil {
		t.Fatal("expected error when Directory is nil")
	}
}

type fakeDirectory struct {
	cmds []plugin.Command
}

func (d fakeDirectory) Commands() []plugin.Command { return d.cmds }

func TestCoreHelp_ListsCommandsWithinActorPermission(t *testing.T) {
	cmds := NewCore().Commands()
	help := findCommand(t, cmds, "help")

	dir := fakeDirectory{cmds: []plugin.Command{
		{Name: "ping", Permission: plugin.PermissionVisitor},
		{Name: "wipe", Permission: plugin.PermissionOperator},
	}}
	pctx := &plugin.Context{Directory: dir}

	reply, err := help.Run(context.Background(), pctx, plugin.Invocation{ActorPermission: plugin.PermissionVisitor})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "Available: !ping" {
		t.Errorf("reply = %q, want only the visitor-level command listed", reply)
	}
}

func TestCoreHelp_OperatorSeesEverything(t *testing.T) {
	cmds := NewCore().Commands()
	help := findCommand(t, cmds, "help")

	dir := fakeDirectory{cmds: []plugin.Command{
		{Name: "ping", Permission: plugin.PermissionVisitor},
		{Name: "wipe", Permission: plugin.PermissionOperator},
	}}
	pctx := &plugin.Context{Directory: dir}

	reply, err := help.Run(context.Background(), pctx, plugin.Invocation{ActorPermission: plugin.PermissionOperator})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "Available: !ping, !wipe" {
		t.Errorf("reply = %q, want both commands listed for an operator", reply)
	}
}

func TestCoreHelp_EmptyDirectory(t *testing.T) {
	cmds := NewCore().Commands()
	help := findCommand(t, cmds, "help")

	pctx := &plugin.Context{Directory: fakeDirectory{}}
	reply, err := help.Run(context.Background(), pctx, plugin.Invocation{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "No commands registered." {
		t.Errorf("reply = %q, want the empty-directory message", reply)
	}
}

func findCommand(t *testing.T, cmds []plugin.Command, name string) plugin.Command {
	t.Helper()
	for _, cmd := range cmds {
		if cmd.Name == name {
			return cmd
		}
	}
	t.Fatalf("command %q not found", name)
	return plugin.Command{}
}
