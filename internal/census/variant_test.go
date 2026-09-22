package census

import (
	"context"
	"testing"
	"time"
)

func variantCensus(entities []Entity) Census {
	return Aggregate(entities, ScanStats{Records: len(entities), Decoded: len(entities)}, time.Time{}, "archive")
}

func groupOf(t *testing.T, c Census, identifier, variant string) VariantGroup {
	t.Helper()
	for _, g := range c.Variants {
		if g.Identifier == identifier && g.Variant == variant {
			return g
		}
	}
	t.Fatalf("no %s %s group in %+v", identifier, variant, c.Variants)
	return VariantGroup{}
}

func TestAggregateCountsVariantsWithLegacyKeptApart(t *testing.T) {
	c := variantCensus([]Entity{
		{Identifier: "cow", Dimension: Overworld, Climate: "temperate", X: 0, Z: 0},
		{Identifier: "cow", Dimension: Overworld, Climate: "temperate", X: 1, Z: 0},
		{Identifier: "cow", Dimension: Overworld, Climate: ClimateLegacy, X: 2, Z: 0},
		{Identifier: "cow", Dimension: Overworld, Climate: "cold", X: 3, Z: 0},
		{Identifier: "mooshroom", Dimension: Overworld, Variant: MooshroomRed},
		{Identifier: "mooshroom", Dimension: Overworld, Variant: MooshroomBrown},
		{Identifier: "mooshroom", Dimension: Overworld, Variant: MooshroomBrown},
		{Identifier: "zombie", Dimension: Overworld, Climate: ClimateLegacy},
	})
	for _, want := range []struct {
		identifier, variant string
		count               int
	}{
		{"cow", "temperate", 2},
		{"cow", ClimateLegacy, 1},
		{"cow", "cold", 1},
		{"mooshroom", "red", 1},
		{"mooshroom", "brown", 2},
	} {
		if got := groupOf(t, c, want.identifier, want.variant).Count; got != want.count {
			t.Errorf("%s %s = %d, want %d", want.identifier, want.variant, got, want.count)
		}
	}
	for _, g := range c.Variants {
		if g.Identifier == "zombie" {
			t.Errorf("zombie has a variant group %+v; only farm animals and mooshrooms are broken down", g)
		}
	}
}

func TestAggregateGroupsAVariantIntoHerdsRatherThanIndividuals(t *testing.T) {
	c := variantCensus([]Entity{
		{Identifier: "cow", Dimension: Overworld, Climate: "warm", X: 500, Y: 70, Z: 500},
		{Identifier: "cow", Dimension: Overworld, Climate: "warm", X: 502, Y: 70, Z: 501},
		{Identifier: "cow", Dimension: Overworld, Climate: "warm", X: 504, Y: 70, Z: 499},
		{Identifier: "cow", Dimension: Overworld, Climate: "warm", X: -900, Y: 70, Z: 40},
	})
	g := groupOf(t, c, "cow", "warm")
	if len(g.Herds) != 2 {
		t.Fatalf("herds = %+v, want the pen of three and the stray as two herds", g.Herds)
	}
	if g.Herds[0].Count != 3 {
		t.Errorf("largest herd holds %d, want 3", g.Herds[0].Count)
	}
}

func TestAggregateLeavesAnimalsOutsideTheOverworldOutOfItsHerds(t *testing.T) {
	c := variantCensus([]Entity{
		{Identifier: "pig", Dimension: Overworld, Climate: "temperate", X: 10, Z: 10},
		{Identifier: "pig", Dimension: Nether, Climate: "temperate", X: 10, Z: 10},
	})
	g := groupOf(t, c, "pig", "temperate")
	if g.Count != 2 || g.Elsewhere != 1 {
		t.Errorf("Count = %d Elsewhere = %d, want 2 and 1", g.Count, g.Elsewhere)
	}
	if len(g.Herds) != 1 || g.Herds[0].Count != 1 {
		t.Errorf("herds = %+v, want only the overworld pig", g.Herds)
	}
}

func TestAggregateSeparatesAnUnrecordedClimateFromLegacy(t *testing.T) {
	c := variantCensus([]Entity{{Identifier: "chicken", Dimension: Overworld}})
	groupOf(t, c, "chicken", ClimateUnrecorded)
}

func TestNearestHerdsOrdersByFlatDistanceFromTheReference(t *testing.T) {
	g := VariantGroup{Herds: []Cluster{
		{Count: 9, CentreX: 1168, CentreY: 70, CentreZ: 248},
		{Count: 2, CentreX: 168, CentreY: -40, CentreZ: 278},
		{Count: 5, CentreX: 468, CentreY: 70, CentreZ: 248},
	}}
	got := g.NearestHerds(FWBBase, 2)
	if len(got) != 2 {
		t.Fatalf("got %d herds, want 2", len(got))
	}
	// The height gap on the nearest herd is ignored: flat distance is 30.
	if d := FWBBase.Distance(got[0]); got[0].Count != 2 || d != 30 {
		t.Errorf("nearest = %+v at %.1f, want the pair at 30 blocks", got[0], d)
	}
	if got[1].Count != 5 {
		t.Errorf("second = %+v, want the herd of five", got[1])
	}
}

// Built through the same NBT encoding the game writes, so the properties
// compound arrives as whatever the decoder really makes of it rather than as
// the map a test author assumed.
func TestScanReadsVariantsOutOfRealRecordShapes(t *testing.T) {
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 1, NBT: map[string]any{
			"identifier": "minecraft:cow",
			"Pos":        pos(170, 70, 250),
			"UniqueID":   int64(1),
			"properties": map[string]any{
				"minecraft:climate_variant": "temperate",
				"minecraft:sound_variant":   "default",
			},
		}},
		{ID: 2, NBT: map[string]any{
			"identifier": "minecraft:cow",
			"Pos":        pos(172, 70, 251),
			"UniqueID":   int64(2),
		}},
		{ID: 3, NBT: map[string]any{
			"identifier": "minecraft:mooshroom",
			"Pos":        pos(-300, 70, 900),
			"UniqueID":   int64(3),
			"Variant":    int32(1),
		}},
	})
	entities, _, err := Scan(context.Background(), path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	c := variantCensus(entities)
	if got := groupOf(t, c, "cow", "temperate"); got.Count != 1 {
		t.Errorf("temperate cows = %d, want 1", got.Count)
	}
	if got := groupOf(t, c, "cow", ClimateLegacy); got.Count != 1 {
		t.Errorf("legacy cows = %d, want 1", got.Count)
	}
	if got := groupOf(t, c, "mooshroom", "brown"); got.Count != 1 {
		t.Errorf("brown mooshrooms = %d, want 1", got.Count)
	}
}
