package census

import "testing"

func entityAt(x, y, z float64) Entity {
	return Entity{Identifier: "item", Dimension: Overworld, X: x, Y: y, Z: z}
}

func TestClusterEntitiesGroupsNeighboursAndSeparatesDistantOnes(t *testing.T) {
	got := ClusterEntities([]Entity{
		entityAt(0, 0, 0),
		entityAt(1, 0, 0),
		entityAt(2, 0, 0),
		entityAt(500, 0, 0),
	}, 8)
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got))
	}
	if got[0].Count != 3 {
		t.Errorf("largest cluster has %d, want 3", got[0].Count)
	}
	if got[1].Count != 1 {
		t.Errorf("second cluster has %d, want 1", got[1].Count)
	}
}

func TestClusterEntitiesChainsThroughIntermediateMembers(t *testing.T) {
	// Transitive grouping: the ends are 20 apart, further than the radius,
	// but each hop is within it, so all three are one cluster.
	got := ClusterEntities([]Entity{
		entityAt(0, 0, 0),
		entityAt(10, 0, 0),
		entityAt(20, 0, 0),
	}, 12)
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("got %d clusters (first count %d), want one cluster of 3", len(got), got[0].Count)
	}
}

func TestClusterEntitiesReportsCentreAndBounds(t *testing.T) {
	got := ClusterEntities([]Entity{
		entityAt(0, 10, 0),
		entityAt(4, 14, 8),
	}, 32)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	c := got[0]
	if c.CentreX != 2 || c.CentreY != 12 || c.CentreZ != 4 {
		t.Errorf("centre = (%v,%v,%v), want (2,12,4)", c.CentreX, c.CentreY, c.CentreZ)
	}
	if c.MinX != 0 || c.MaxX != 4 || c.MinZ != 0 || c.MaxZ != 8 {
		t.Errorf("bounds = x %v..%v z %v..%v, want x 0..4 z 0..8", c.MinX, c.MaxX, c.MinZ, c.MaxZ)
	}
}

func TestClusterEntitiesGroupsAcrossTheOriginOnNegativeCoordinates(t *testing.T) {
	// Points either side of an axis must still group. The nether item pile
	// that motivated this sits at negative x, so getting this wrong splits
	// the exact finding the tool exists to make.
	got := ClusterEntities([]Entity{
		entityAt(-2, 0, -2),
		entityAt(-1, 0, -1),
		entityAt(1, 0, 1),
	}, 16)
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("got %d clusters (first count %d), want one cluster of 3", len(got), got[0].Count)
	}
	if got[0].MinX != -2 || got[0].MaxX != 1 {
		t.Errorf("bounds x = %v..%v, want -2..1", got[0].MinX, got[0].MaxX)
	}
}

func TestClusterEntitiesOnAnEmptyInput(t *testing.T) {
	if got := ClusterEntities(nil, 16); len(got) != 0 {
		t.Errorf("got %d clusters for no entities, want 0", len(got))
	}
}
