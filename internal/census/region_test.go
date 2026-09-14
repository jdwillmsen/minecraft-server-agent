package census

import "testing"

func TestRegionOfBucketsAt144Blocks(t *testing.T) {
	for _, tc := range []struct {
		x, z         float64
		wantX, wantZ int
	}{
		{0, 0, 0, 0},
		{143.9, 143.9, 0, 0},
		{144, 144, 1, 1},
		{468.5, 268.6, 3, 1}, // the FWB iron farm
	} {
		got := RegionOf(Overworld, tc.x, tc.z)
		if got.X != tc.wantX || got.Z != tc.wantZ {
			t.Errorf("RegionOf(%v,%v) = (%d,%d), want (%d,%d)", tc.x, tc.z, got.X, got.Z, tc.wantX, tc.wantZ)
		}
	}
}

func TestRegionOfFloorsNegativeCoordinates(t *testing.T) {
	// Integer truncation rounds toward zero, so -1 would land in region 0
	// alongside +1 and merge two regions into one. Only floor is correct.
	for _, tc := range []struct {
		x     float64
		wantX int
	}{
		{-0.5, -1},
		{-1, -1},
		{-144, -1},
		{-144.5, -2},
		{-145, -2},
	} {
		if got := RegionOf(Overworld, tc.x, 0); got.X != tc.wantX {
			t.Errorf("RegionOf(%v).X = %d, want %d", tc.x, got.X, tc.wantX)
		}
	}
}

func TestRegionKeysSeparateDimensions(t *testing.T) {
	if RegionOf(Overworld, 0, 0) == RegionOf(Nether, 0, 0) {
		t.Error("overworld and nether regions at the same coordinates compare equal")
	}
}

func TestRegionBounds(t *testing.T) {
	minX, maxX, minZ, maxZ := RegionOf(Overworld, 200, -10).Bounds()
	if minX != 144 || maxX != 287 || minZ != -144 || maxZ != -1 {
		t.Errorf("Bounds() = (%d,%d,%d,%d), want (144,287,-144,-1)", minX, maxX, minZ, maxZ)
	}
}
