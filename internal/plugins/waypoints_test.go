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
func (f *fakeWaypoints) Delete(_ context.Context, xuid, name string) (bool, error) {
	key := waypoints.NormalizeName(name)
	if _, ok := f.byOwner[xuid][key]; !ok {
		return false, nil
	}
	delete(f.byOwner[xuid], key)
	return true, nil
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

func TestWPSetRefusesReservedName(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "del", "100", "64", "-200"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.byOwner["a"]) != 0 {
		t.Fatal("a reserved waypoint name was stored")
	}
	if !strings.Contains(strings.ToLower(reply), "reserved") {
		t.Errorf("reply %q should say the name is reserved", reply)
	}
}

func TestWPDeleteReportsWhenNothingExisted(t *testing.T) {
	cmd := wpCommand(t)
	pctx := &plugin.Context{Waypoints: newFakeWaypoints()}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"del", "nope"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.ToLower(reply), "deleted") {
		t.Errorf("reply %q claims a delete that never happened", reply)
	}
}

func TestWPSetMultiWordNameThenGet(t *testing.T) {
	cmd := wpCommand(t)
	pctx := &plugin.Context{Waypoints: newFakeWaypoints()}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "Home", "Base", "100", "64", "-200"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"home", "base"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(reply, "100") || !strings.Contains(reply, "-200") {
		t.Errorf("reply = %q, want the coordinates", reply)
	}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "Far", "Base", "1", "2", "3", "nether"},
	}); err != nil {
		t.Fatalf("set with dimension: %v", err)
	}
	reply, err = cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"far", "base"},
	})
	if err != nil {
		t.Fatalf("get with dimension: %v", err)
	}
	if !strings.Contains(reply, "nether") {
		t.Errorf("reply = %q, want the dimension", reply)
	}
}

func TestWPSetAmbiguousAllNumericTailIsRefused(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	// "base 1" could be the name with coordinates 2 3 4, or "base" with a
	// stray extra number before three coordinates -- both readings are
	// grammatically valid, so guessing either one risks silently saving the
	// wrong name at the wrong place. Refusing is the only safe reply.
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "1", "2", "3", "4"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.byOwner["a"]) != 0 {
		t.Fatalf("an ambiguous set was stored anyway: %v", fake.byOwner["a"])
	}
	if strings.Contains(strings.ToLower(reply), "saved") {
		t.Errorf("reply %q claims a save that must not have happened", reply)
	}
}

func TestWPSetMultiWordNameBoundaryStillRoundTrips(t *testing.T) {
	cmd := wpCommand(t)
	pctx := &plugin.Context{Waypoints: newFakeWaypoints()}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "Home", "Base", "100", "64", "-200"},
	}); err != nil {
		t.Fatalf("set without dimension: %v", err)
	}
	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"home", "base"},
	})
	if err != nil {
		t.Fatalf("get without dimension: %v", err)
	}
	if !strings.Contains(reply, "100") || !strings.Contains(reply, "-200") {
		t.Errorf("reply = %q, want the coordinates", reply)
	}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "Home", "Base", "1", "2", "3", "nether"},
	}); err != nil {
		t.Fatalf("set with dimension: %v", err)
	}
	reply, err = cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"home", "base"},
	})
	if err != nil {
		t.Fatalf("get with dimension: %v", err)
	}
	if !strings.Contains(reply, "nether") {
		t.Errorf("reply = %q, want the dimension", reply)
	}
}
