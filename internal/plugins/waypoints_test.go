package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

type fakeWaypoints struct {
	byOwner map[string]map[string]waypoints.Waypoint
}

func newFakeWaypoints() *fakeWaypoints {
	return &fakeWaypoints{byOwner: map[string]map[string]waypoints.Waypoint{}}
}

func (f *fakeWaypoints) Get(_ context.Context, xuid, name string) (waypoints.Waypoint, bool, error) {
	wp, ok := f.byOwner[xuid][waypoints.NormalizeName(name)]
	return wp, ok, nil
}
func (f *fakeWaypoints) Set(_ context.Context, xuid string, wp waypoints.Waypoint) error {
	dim, err := waypoints.NormalizeDimension(wp.Dimension)
	if err != nil {
		return err
	}
	wp.Dimension = dim
	wp.Name = waypoints.NormalizeName(wp.Name)
	if f.byOwner[xuid] == nil {
		f.byOwner[xuid] = map[string]waypoints.Waypoint{}
	}
	f.byOwner[xuid][wp.Name] = wp
	return nil
}
func (f *fakeWaypoints) Delete(_ context.Context, xuid, name string) error {
	delete(f.byOwner[xuid], waypoints.NormalizeName(name))
	return nil
}
func (f *fakeWaypoints) List(_ context.Context, xuid string) ([]waypoints.Waypoint, error) {
	out := make([]waypoints.Waypoint, 0, len(f.byOwner[xuid]))
	for _, wp := range f.byOwner[xuid] {
		out = append(out, wp)
	}
	return out, nil
}
func (f *fakeWaypoints) Enabled() bool { return true }

func wpCommand(t *testing.T) plugin.Command {
	t.Helper()
	for _, c := range NewWaypoints().Commands() {
		if c.Name == "wp" {
			return c
		}
	}
	t.Fatal("wp command not registered")
	return plugin.Command{}
}

func TestWPSetAndGetIsPerPlayer(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "Base", "100", "64", "-200"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-a", ActorPermission: plugin.PermissionMember, Args: []string{"base"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(reply, "100") || !strings.Contains(reply, "-200") {
		t.Errorf("reply = %q, want the coordinates", reply)
	}

	other, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "player-b", ActorPermission: plugin.PermissionMember, Args: []string{"base"},
	})
	if err != nil {
		t.Fatalf("other player get: %v", err)
	}
	if strings.Contains(other, "100") {
		t.Errorf("player-b read player-a's waypoint: %q", other)
	}
}

func TestWPRejectsBadCoordinatesAndDimension(t *testing.T) {
	cmd := wpCommand(t)
	pctx := &plugin.Context{Waypoints: newFakeWaypoints()}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "x", "64", "-200"},
	})
	if err != nil {
		t.Fatalf("a bad coordinate must be a reply, not an error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "number") {
		t.Errorf("reply = %q, want it to name the problem", reply)
	}

	reply, err = cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "1", "2", "3", "moon"},
	})
	if err != nil {
		t.Fatalf("a bad dimension must be a reply, not an error: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "overworld") {
		t.Errorf("reply = %q, want it to list the valid dimensions", reply)
	}
}

func TestWPWithoutStore(t *testing.T) {
	cmd := wpCommand(t)
	reply, err := cmd.Run(context.Background(), &plugin.Context{}, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember, Args: nil,
	})
	if err != nil {
		t.Fatalf("a nil Waypoints must not error: %v", err)
	}
	if reply == "" {
		t.Error("reply should explain the feature is unconfigured")
	}
}
