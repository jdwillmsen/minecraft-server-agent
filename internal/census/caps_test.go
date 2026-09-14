package census

import "testing"

func TestCapsForMatchesTheBedrockTable(t *testing.T) {
	for _, tc := range []struct {
		d    Dimension
		c    Category
		want Caps
	}{
		{Overworld, Monster, Caps{Surface: 8, Cave: 16}},
		{Overworld, Animal, Caps{Surface: 4, Cave: NoSpawn}},
		{Overworld, WaterAnimal, Caps{Surface: 36, Cave: NoSpawn}},
		{Overworld, Ambient, Caps{Surface: NoSpawn, Cave: 2}},
		{Overworld, Pillager, Caps{Surface: 8, Cave: 8}},
		{Nether, Monster, Caps{Surface: NoSpawn, Cave: 16}},
		{Nether, Animal, Caps{Surface: NoSpawn, Cave: 4}},
		{End, Monster, Caps{Surface: 10, Cave: 8}},
	} {
		got, ok := CapsFor(tc.d, tc.c)
		if !ok {
			t.Errorf("CapsFor(%v,%v) ok = false, want true", tc.d, tc.c)
			continue
		}
		if got != tc.want {
			t.Errorf("CapsFor(%v,%v) = %+v, want %+v", tc.d, tc.c, got, tc.want)
		}
	}
}

func TestCapsForHasNoEntryForUncountedCategories(t *testing.T) {
	for _, c := range []Category{Ignored, Uncapped} {
		if _, ok := CapsFor(Overworld, c); ok {
			t.Errorf("CapsFor(overworld,%v) ok = true, want false", c)
		}
	}
	if _, ok := CapsFor(UnknownDimension, Monster); ok {
		t.Error("CapsFor(unknown dimension) ok = true, want false")
	}
}

func TestStatusOfReportsARangeNotAFalsePrecision(t *testing.T) {
	// Surface-versus-cave is fixed when a mob spawns and is not written to
	// the save, so the exact cap is unknowable. Overworld monsters are
	// capped somewhere in 8..16: below 8 there is headroom for certain, at
	// 16 the region is saturated for certain, between the two it depends on
	// how those mobs spawned.
	for _, tc := range []struct {
		count int
		want  Status
	}{
		{0, Headroom},
		{7, Headroom},
		{8, AtRisk},
		{9, AtRisk},
		{15, AtRisk},
		{16, Capped},
		{17, Capped},
	} {
		if got := StatusOf(Overworld, Monster, tc.count); got != tc.want {
			t.Errorf("StatusOf(overworld,monster,%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestStatusOfGradesOnlyTheEnvironmentsACategorySpawnsIn(t *testing.T) {
	// An environment a category cannot spawn in carries no ceiling, and an
	// absent ceiling is not a ceiling of zero. Grading against one made a
	// single cow, bat or zombified piglin enough to call a region at risk,
	// which left the status column with nothing to say.
	for _, tc := range []struct {
		d     Dimension
		c     Category
		count int
		want  Status
	}{
		{Overworld, Animal, 1, Headroom},
		{Overworld, Animal, 3, Headroom},
		{Overworld, Animal, 4, Capped},
		{Overworld, Ambient, 1, Headroom},
		{Overworld, Ambient, 2, Capped},
		{Nether, Monster, 1, Headroom},
		{Nether, Monster, 15, Headroom},
		{Nether, Monster, 16, Capped},
	} {
		if got := StatusOf(tc.d, tc.c, tc.count); got != tc.want {
			t.Errorf("StatusOf(%v,%v,%d) = %v, want %v", tc.d, tc.c, tc.count, got, tc.want)
		}
	}
}

func TestCapTableHasNoCellWithoutAnApplicableCap(t *testing.T) {
	// A cell where neither environment spawns the category is not a cap of
	// nothing - it means the category does not spawn in that dimension at
	// all, and the row must be absent so CapsFor reports false.
	for d, byCategory := range capTable {
		for c, caps := range byCategory {
			if caps.Surface == NoSpawn && caps.Cave == NoSpawn {
				t.Errorf("capTable[%v][%v] has no applicable cap; drop the row instead", d, c)
			}
		}
	}
}

func TestCapsForHasNoEntryWhereACategoryCannotSpawn(t *testing.T) {
	// The nether has no water, no bats and no pillager patrols, so these
	// categories have no environmental spawning there to grade against.
	for _, c := range []Category{WaterAnimal, Ambient, Pillager} {
		if caps, ok := CapsFor(Nether, c); ok {
			t.Errorf("CapsFor(nether,%v) = %+v, want no entry", c, caps)
		}
	}
}

func TestStatusOfIsUnknownForUncountedCategories(t *testing.T) {
	if got := StatusOf(Overworld, Ignored, 5000); got != StatusUnknown {
		t.Errorf("StatusOf(ignored) = %v, want StatusUnknown", got)
	}
}

func TestStatusOfGradesTheEndsInvertedCaps(t *testing.T) {
	// The End's monster caps are 10 surface / 8 cave - inverted relative to
	// every other dimension in the table - so this is the one boundary
	// where getting Range() backwards would actually change the answer.
	for _, tc := range []struct {
		count int
		want  Status
	}{
		{7, Headroom},
		{8, AtRisk},
		{9, AtRisk},
		{10, Capped},
	} {
		if got := StatusOf(End, Monster, tc.count); got != tc.want {
			t.Errorf("StatusOf(end,monster,%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestCapsForHasNoEntryForCategoriesThatCannotSpawnInTheEnd(t *testing.T) {
	// The enderman is the End's only environmental spawn. Shulkers and the
	// dragon come from world generation and endermites from ender pearls,
	// and no animal, fish, bat or pillager will ever spawn there, so there
	// is no cap for them to press against.
	for _, c := range []Category{Animal, WaterAnimal, Ambient, Pillager} {
		if caps, ok := CapsFor(End, c); ok {
			t.Errorf("CapsFor(end,%v) = %+v, want no entry", c, caps)
		}
	}
}

func TestStatusOfTreatsACountOnItsCapAsCapped(t *testing.T) {
	// Bedrock stops spawning once the count reaches the ceiling, not once
	// it passes it, so a region sitting exactly on its cap has no room
	// left. "16 / 16 headroom" is precisely the line an operator would read
	// as room to spare.
	for _, tc := range []struct {
		d     Dimension
		c     Category
		count int
		want  Status
	}{
		{Nether, Monster, 16, Capped},
		{Overworld, Animal, 4, Capped},
		{Overworld, Ambient, 2, Capped},
		{Overworld, WaterAnimal, 36, Capped},
		{Overworld, Pillager, 8, Capped},
	} {
		if got := StatusOf(tc.d, tc.c, tc.count); got != tc.want {
			t.Errorf("StatusOf(%v,%v,%d) = %v, want %v", tc.d, tc.c, tc.count, got, tc.want)
		}
	}
}

func TestStatusOfTreatsACountOnTheLowerBoundAsAtRisk(t *testing.T) {
	// Where the two environments carry different ceilings, reaching the
	// lower one is already enough to stop spawning if that is the ceiling
	// that applies - which the save cannot say.
	for _, tc := range []struct {
		d     Dimension
		c     Category
		count int
		want  Status
	}{
		{Overworld, Monster, 8, AtRisk},
		{End, Monster, 8, AtRisk},
	} {
		if got := StatusOf(tc.d, tc.c, tc.count); got != tc.want {
			t.Errorf("StatusOf(%v,%v,%d) = %v, want %v", tc.d, tc.c, tc.count, got, tc.want)
		}
	}
}

func TestCapsWithNoSpawnableEnvironmentHaveNoRangeToGradeAgainst(t *testing.T) {
	// Caps and its methods are exported, so a caller can hand in a cell the
	// table would never hold. Falling through to the first arm returned a
	// range of -1..-1, which graded a count of zero as capped and printed a
	// ceiling of -1.
	none := Caps{Surface: NoSpawn, Cave: NoSpawn}
	lower, upper := none.Range()
	if lower != NoSpawn || upper != NoSpawn {
		t.Errorf("Range() = (%d,%d), want (%d,%d)", lower, upper, NoSpawn, NoSpawn)
	}
	for _, count := range []int{0, 1, 100} {
		if got := none.Status(count); got != StatusUnknown {
			t.Errorf("Status(%d) = %v, want StatusUnknown", count, got)
		}
	}
}
