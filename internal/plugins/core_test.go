package plugins

import (
	"context"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

func TestCorePing(t *testing.T) {
	cmds := NewCore().Commands()
	ping := findCommand(t, cmds, "ping")

	reply, err := ping.Run(context.Background(), &plugin.Context{}, plugin.Invocation{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "pong" {
		t.Errorf("reply = %q, want pong", reply)
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
