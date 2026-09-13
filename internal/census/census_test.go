package census

import (
	"testing"
	"time"
)

func TestAggregateCountsTotalsPerDimensionAndIdentifier(t *testing.T) {
	c := Aggregate([]Entity{
		{Identifier: "zombie", Dimension: Overworld, X: 0, Z: 0},
		{Identifier: "zombie", Dimension: Overworld, X: 1, Z: 1},
		{Identifier: "zombie", Dimension: Nether, X: 0, Z: 0},
	}, ScanStats{Decoded: 3}, time.Unix(0, 0), "archive")

	counts := map[Dimension]int{}
	for _, tot := range c.Totals {
		if tot.Identifier != "zombie" {
			t.Fatalf("unexpected identifier %q", tot.Identifier)
		}
		if tot.Category != Monster {
			t.Errorf("category = %v, want monster", tot.Category)
		}
		counts[tot.Dimension] = tot.Count
	}
	if counts[Overworld] != 2 || counts[Nether] != 1 {
		t.Errorf("counts = %v, want overworld 2 and nether 1", counts)
	}
}

func TestAggregateGradesRegionsAndExcludesUncountedCategories(t *testing.T) {
	var entities []Entity
	for i := 0; i < 17; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: float64(i), Z: 0})
	}
	// Items share the region but must not appear as a graded row.
	entities = append(entities, Entity{Identifier: "item", Dimension: Overworld, X: 0, Z: 0})

	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
	if len(c.Regions) != 1 {
		t.Fatalf("got %d graded regions, want 1", len(c.Regions))
	}
	r := c.Regions[0]
	if r.Category != Monster || r.Count != 17 {
		t.Fatalf("region = %+v, want 17 monsters", r)
	}
	if r.Status != Capped {
		t.Errorf("status = %v, want capped (17 is above the overworld monster upper bound of 16)", r.Status)
	}
}

func TestAggregateRanksRegionsWorstFirst(t *testing.T) {
	var entities []Entity
	for i := 0; i < 3; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: float64(i), Z: 0})
	}
	for i := 0; i < 20; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: 200 + float64(i), Z: 0})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
	if len(c.Regions) != 2 {
		t.Fatalf("got %d regions, want 2", len(c.Regions))
	}
	if c.Regions[0].Count != 20 {
		t.Errorf("first region count = %d, want the worst region (20) first", c.Regions[0].Count)
	}
}

func TestAggregateCollectsNamedAndPersistentEntities(t *testing.T) {
	c := Aggregate([]Entity{
		{Identifier: "cat", Dimension: Overworld, X: 1, Y: 2, Z: 3, CustomName: "OJ", Persistent: true},
		{Identifier: "piglin_brute", Dimension: Nether, Persistent: true},
		{Identifier: "zombie", Dimension: Overworld},
	}, ScanStats{}, time.Unix(0, 0), "archive")

	if len(c.Named) != 1 {
		t.Fatalf("got %d named entities, want 1", len(c.Named))
	}
	if c.Named[0].Name != "OJ" || c.Named[0].Identifier != "cat" || c.Named[0].X != 1 {
		t.Errorf("named = %+v, want OJ the cat at x=1", c.Named[0])
	}
	if c.PersistentByIdentifier["piglin_brute"] != 1 || c.PersistentByIdentifier["cat"] != 1 {
		t.Errorf("persistent counts = %v, want one brute and one cat", c.PersistentByIdentifier)
	}
	if _, ok := c.PersistentByIdentifier["zombie"]; ok {
		t.Error("a non-persistent zombie appears in the persistent counts")
	}
}

func TestAggregateFindsConcentrationsAndLocatesThem(t *testing.T) {
	// A pile of one entity type in one spot is the shape of every entity
	// leak found so far. A count alone says there is a problem; the cluster
	// centre says where to stand to fix it.
	var entities []Entity
	for i := 0; i < 40; i++ {
		entities = append(entities, Entity{
			Identifier: "item", Dimension: Nether,
			X: -24 + float64(i%4), Y: 14, Z: 1320 + float64(i%4),
		})
	}
	// Scattered singletons of the same type must not become a concentration.
	for i := 0; i < 5; i++ {
		entities = append(entities, Entity{
			Identifier: "item", Dimension: Nether,
			X: float64(i) * 5000, Y: 60, Z: 0,
		})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")

	if len(c.Concentrations) != 1 {
		t.Fatalf("got %d concentrations, want 1", len(c.Concentrations))
	}
	got := c.Concentrations[0]
	if got.Identifier != "item" || got.Dimension != Nether {
		t.Errorf("concentration = %+v, want nether items", got)
	}
	if got.Cluster.Count != 40 {
		t.Errorf("cluster count = %d, want 40", got.Cluster.Count)
	}
	if got.Cluster.CentreX > -20 || got.Cluster.CentreX < -28 {
		t.Errorf("centre x = %v, want it near -24", got.Cluster.CentreX)
	}
}

func TestAggregateIgnoresTypesBelowTheConcentrationThreshold(t *testing.T) {
	var entities []Entity
	for i := 0; i < ConcentrationThreshold-1; i++ {
		entities = append(entities, Entity{Identifier: "cow", Dimension: Overworld, X: float64(i), Z: 0})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
	if len(c.Concentrations) != 0 {
		t.Errorf("got %d concentrations below the threshold, want 0", len(c.Concentrations))
	}
}

func TestAggregateCarriesProvenance(t *testing.T) {
	when := time.Unix(1757800000, 0).UTC()
	c := Aggregate(nil, ScanStats{Records: 7}, when, "archive")
	if !c.TakenAt.Equal(when) || c.SourceKind != "archive" || c.Stats.Records != 7 {
		t.Errorf("provenance = %v/%q/%+v, want it carried through unchanged", c.TakenAt, c.SourceKind, c.Stats)
	}
}
