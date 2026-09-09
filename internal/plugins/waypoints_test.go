package plugins

import (
	"context"
	"fmt"
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

// !wp del names a waypoint the same way every other path does. Taking one
// token instead deleted whichever waypoint happened to share that first
// word, and then reported that name back as the one the player asked for.
func TestWPDeleteMultiWordNameLeavesTheShorterOneAlone(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	for _, name := range [][]string{{"gold"}, {"gold", "farm"}} {
		args := append(append([]string{"set"}, name...), "1", "2", "3")
		if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
			ActorXUID: "a", ActorPermission: plugin.PermissionMember, Args: args,
		}); err != nil {
			t.Fatalf("set %v: %v", name, err)
		}
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"del", "gold", "farm"},
	})
	if err != nil {
		t.Fatalf("del: %v", err)
	}
	if _, ok := fake.byOwner["a"]["gold"]; !ok {
		t.Error(`"!wp del gold farm" deleted "gold"`)
	}
	if _, ok := fake.byOwner["a"]["gold farm"]; ok {
		t.Error(`"gold farm" survived its own deletion`)
	}
	if !strings.Contains(reply, "gold farm") {
		t.Errorf("reply = %q, want it to name the waypoint the player asked to delete", reply)
	}
}

// !wp used to reply with names only, which made every lookup a two-step:
// !wp to see what exists, then !wp <name> to see where it actually is.
func TestWPListShowsCoordinatesInline(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "100", "64", "-200"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember, Args: nil,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(reply, "100") || !strings.Contains(reply, "-200") {
		t.Errorf("reply = %q, want the coordinates inline", reply)
	}
}

// A player is free to save as many waypoints as they like, and the reply
// still has to fit on one Bedrock chat line. This checks the summary form
// kicks in rather than the line silently growing past the cap.
func TestWPListManyWaypointsStaysWithinReplyCapAndSummarizes(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("waypoint-number-%d", i)
		if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
			ActorXUID: "a", ActorPermission: plugin.PermissionMember,
			Args: []string{"set", name, "100", "64", "-200"},
		}); err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember, Args: nil,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(reply) > 200 {
		t.Fatalf("reply is %d chars, want at most 200: %q", len(reply), reply)
	}
	if !strings.Contains(reply, "more") {
		t.Errorf("reply = %q, want a summary of the waypoints it could not fit", reply)
	}
}

// A player typing "!wp set base x=180 y=68 z=268" is pasting exactly what
// Minecraft's own coordinate display shows them -- that form must be
// accepted, not just the bare numbers.
//
// The labels here are already in x, y, z order, so this alone does not
// prove labels are read as labels rather than just stripped and read
// positionally -- it would pass identically under either implementation.
// TestWPSetLabelledPermutationOutOfOrderRoundTrips below is the test that
// actually guards the label-order regression.
func TestWPSetAcceptsMinecraftCoordinateDisplayFormAlreadyInOrder(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "x=180", "y=68", "z=268"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	wp, ok := fake.byOwner["a"]["base"]
	if !ok {
		t.Fatal("base was not saved")
	}
	if wp.X != 180 || wp.Y != 68 || wp.Z != 268 {
		t.Errorf("wp = %+v, want X=180 Y=68 Z=268", wp)
	}
}

// A player reading coordinates off Minecraft's own F3 display can copy them
// down in a different order than x y z. When every token is labelled and
// the labels are exactly x, y and z, the labels decide which number is
// which, not position -- that is unambiguous, so guessing anything else
// here would be pedantry, not safety.
func TestWPSetLabelledPermutationOutOfOrderRoundTrips(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "y=68", "x=180", "z=260"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	wp, ok := fake.byOwner["a"]["base"]
	if !ok {
		t.Fatal("base was not saved")
	}
	if wp.X != 180 || wp.Y != 68 || wp.Z != 260 {
		t.Errorf("wp = %+v, want X=180 Y=68 Z=260", wp)
	}
}

// A duplicated label (two "x="s, no "y=" anywhere) is not a permutation of
// x, y, z even though all three tokens are labelled, and there is no
// reading of it that is obviously what the player meant.
func TestWPSetDuplicatedCoordinateLabelIsRefused(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "x=180", "x=68", "z=260"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := fake.byOwner["a"]["base"]; ok {
		t.Fatal("a duplicated-label set was stored anyway")
	}
	if strings.Contains(strings.ToLower(reply), "saved") {
		t.Errorf("reply %q claims a save that must not have happened", reply)
	}
}

// Some coordinates labelled and some not -- even when the labelled ones sit
// in their own slot -- is not the same as every token being labelled, and
// guessing which axis the bare token belongs to is exactly the silent
// wrong-place failure this parser already has scars from. Refuse rather
// than guess.
func TestWPSetPartiallyLabelledCoordinatesAreRefused(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "X=180", "68", "Z=268"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := fake.byOwner["a"]["base"]; ok {
		t.Fatal("a partially-labelled set was stored anyway")
	}
	if strings.Contains(strings.ToLower(reply), "saved") {
		t.Errorf("reply %q claims a save that must not have happened", reply)
	}
}

// A single label sitting on the wrong slot ("z=260" first) with the other
// two tokens bare is the same partial-labelling case as above, just with
// the labelled token out of position rather than in it -- neither reading
// is safe to guess.
func TestWPSetPartiallyLabelledOutOfPositionIsRefused(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "z=260", "68", "180"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := fake.byOwner["a"]["base"]; ok {
		t.Fatal("a partially-labelled set was stored anyway")
	}
	if strings.Contains(strings.ToLower(reply), "saved") {
		t.Errorf("reply %q claims a save that must not have happened", reply)
	}
}

// A bare number must keep working exactly as it always has -- the display
// form is an addition, not a replacement.
func TestWPSetStillAcceptsBareCoordinates(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	if _, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "180", "68", "268"},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, ok := fake.byOwner["a"]["base"]; !ok {
		t.Fatal("base was not saved")
	}
}

// Nonsense wearing the display's clothing must still be refused the same
// way a bare nonsense token always was.
func TestWPSetRejectsNonNumericCoordinateDisplayForm(t *testing.T) {
	cmd := wpCommand(t)
	pctx := &plugin.Context{Waypoints: newFakeWaypoints()}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "x=abc", "y=68", "z=268"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToLower(reply), "not a number") {
		t.Errorf("reply = %q, want it to name the problem token", reply)
	}
	if !strings.Contains(reply, "x=abc") {
		t.Errorf("reply = %q, want it to quote the bad token as typed", reply)
	}
}

// The ambiguity guard tests whether the token before the coordinates is
// itself numeric-shaped. A previous version silently saved a waypoint under
// the wrong name with shifted coordinates because it missed exactly this
// case; the display-form prefix must not be a way around the guard.
func TestWPSetAmbiguityGuardCatchesCoordinateDisplayPrefixedToken(t *testing.T) {
	cmd := wpCommand(t)
	fake := newFakeWaypoints()
	pctx := &plugin.Context{Waypoints: fake}

	reply, err := cmd.Run(context.Background(), pctx, plugin.Invocation{
		ActorXUID: "a", ActorPermission: plugin.PermissionMember,
		Args: []string{"set", "base", "x=1", "2", "3", "4"},
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
