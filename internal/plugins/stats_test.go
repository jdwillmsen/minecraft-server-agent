package plugins

import (
	"context"
	"errors"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

type fakeFacts struct {
	output string
	err    error
}

func (f fakeFacts) PlayersOnline(ctx context.Context) (string, error) {
	return f.output, f.err
}

func TestStats_Players_RelaysRawFactsOutput(t *testing.T) {
	cmd := findCommand(t, NewStats().Commands(), "players")
	pctx := &plugin.Context{Facts: fakeFacts{output: "There are 2/20 players online: Steve, Alex"}}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "There are 2/20 players online: Steve, Alex" {
		t.Errorf("reply = %q, want the facts output relayed unchanged", reply)
	}
}

func TestStats_Players_PropagatesFactsError(t *testing.T) {
	cmd := findCommand(t, NewStats().Commands(), "players")
	pctx := &plugin.Context{Facts: fakeFacts{err: errors.New("bridge unreachable")}}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{}); err == nil {
		t.Fatal("expected the facts error to propagate")
	}
}

func TestStats_Players_NilFactsErrorsRatherThanPanicking(t *testing.T) {
	cmd := findCommand(t, NewStats().Commands(), "players")
	pctx := &plugin.Context{}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{}); err == nil {
		t.Fatal("expected an error when no Facts source is configured")
	}
}

func TestStats_Players_VisitorPermission(t *testing.T) {
	cmd := findCommand(t, NewStats().Commands(), "players")
	if cmd.Permission != plugin.PermissionVisitor {
		t.Errorf("players permission = %v, want visitor (anyone can ask who's online)", cmd.Permission)
	}
}
