package census

import (
	"fmt"
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

func TestAggregateIsDeterministicOverTiedRows(t *testing.T) {
	// Build input that ties on the old comparator keys:
	// - Same identifier and same count in different dimensions (ties Totals and Regions)
	// - Concentrations of equal size in well-separated locations in different dimensions
	var entities []Entity

	// 5 zombies in Overworld, all in region (0, 0)
	for i := 0; i < 5; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: float64(i), Z: 0})
	}

	// 5 zombies in Nether, all in region (0, 0) [also ties Regions]
	for i := 0; i < 5; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Nether, X: float64(i), Z: 0})
	}

	// 25 items in Overworld at one location (concentration cluster 1)
	for i := 0; i < 25; i++ {
		entities = append(entities, Entity{Identifier: "item", Dimension: Overworld, X: 100 + float64(i%3), Z: 100 + float64(i%3)})
	}

	// 25 items in Nether at a different location (concentration cluster 2, same count as cluster 1)
	for i := 0; i < 25; i++ {
		entities = append(entities, Entity{Identifier: "item", Dimension: Nether, X: 5000 + float64(i%3), Z: 5000 + float64(i%3)})
	}

	// Fingerprint a census by converting its structured output to a string
	fingerprint := func(c Census) string {
		var fp string
		for _, tot := range c.Totals {
			fp += fmt.Sprintf("T:%d:%s:%d:%d,", tot.Dimension, tot.Identifier, tot.Category, tot.Count)
		}
		for _, reg := range c.Regions {
			fp += fmt.Sprintf("R:%d:%d:%d:%d:%d,", reg.Key.Dimension, reg.Key.X, reg.Key.Z, reg.Category, reg.Count)
		}
		for _, nam := range c.Named {
			fp += fmt.Sprintf("N:%s:%s:%d,", nam.Name, nam.Identifier, nam.Dimension)
		}
		for _, con := range c.Concentrations {
			fp += fmt.Sprintf("C:%d:%s:%d,", con.Dimension, con.Identifier, con.Cluster.Count)
		}
		return fp
	}

	// Run Aggregate 50 times and collect fingerprints
	var firstFP string
	var firstTotals, firstRegions, firstConcentrations int
	var diffSection string
	for run := 0; run < 50; run++ {
		c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
		fp := fingerprint(c)
		if run == 0 {
			firstFP = fp
			firstTotals, firstRegions, firstConcentrations = len(c.Totals), len(c.Regions), len(c.Concentrations)
		} else if fp != firstFP {
			// Find which section differed
			if len(c.Totals) != firstTotals {
				diffSection = "Totals"
			} else if len(c.Regions) != firstRegions {
				diffSection = "Regions"
			} else if len(c.Concentrations) != firstConcentrations {
				diffSection = "Concentrations"
			}
			if diffSection == "" {
				diffSection = "ordering within a section"
			}
			t.Fatalf("run %d produced different output than run 0 (differed in %s): %q vs %q", run, diffSection, fp, firstFP)
		}
	}
}

func TestAggregateOrdersConcentrationsTiedOnEverythingButY(t *testing.T) {
	// Two clusters of the same identifier, dimension, size and X/Z, but at
	// clearly different Y - a two-storey mob farm, or stacked item piles.
	// The old comparator stopped at CentreZ, so nothing distinguished these
	// two clusters. sort.Slice's instability on a fully-tied pair only
	// surfaces once the slice is long enough to leave the small-n insertion
	// path, so a spread of unrelated concentration types rides along
	// purely to grow c.Concentrations past that threshold.
	var entities []Entity
	for n := 0; n < 12; n++ {
		id := fmt.Sprintf("noise%02d", n)
		x := float64(n) * 5000
		for i := 0; i < ConcentrationThreshold; i++ {
			entities = append(entities, Entity{Identifier: id, Dimension: Overworld, X: x, Y: 0, Z: 0})
		}
	}
	for _, y := range []float64{0, 1000} {
		for i := 0; i < ConcentrationThreshold; i++ {
			entities = append(entities, Entity{Identifier: "creaking", Dimension: Overworld, X: 99999, Y: y, Z: 0})
		}
	}

	var firstOrder [2]float64
	for run := 0; run < 100; run++ {
		c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
		var order [2]float64
		found := 0
		for _, con := range c.Concentrations {
			if con.Identifier != "creaking" {
				continue
			}
			if found >= 2 {
				t.Fatalf("run %d: more than 2 creaking concentrations", run)
			}
			order[found] = con.Cluster.CentreY
			found++
		}
		if found != 2 {
			t.Fatalf("run %d: found %d creaking concentrations, want 2", run, found)
		}
		if run == 0 {
			firstOrder = order
			continue
		}
		if order != firstOrder {
			t.Fatalf("run %d produced a different concentration order than run 0: %v vs %v", run, order, firstOrder)
		}
	}
}
