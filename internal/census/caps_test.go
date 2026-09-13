package census

import "testing"

func TestCapsForMatchesTheBedrockTable(t *testing.T) {
	for _, tc := range []struct {
		d    Dimension
		c    Category
		want Caps
	}{
		{Overworld, Monster, Caps{Surface: 8, Cave: 16}},
		{Overworld, Animal, Caps{Surface: 4, Cave: 0}},
		{Overworld, WaterAnimal, Caps{Surface: 36, Cave: 0}},
		{Overworld, Ambient, Caps{Surface: 0, Cave: 2}},
		{Overworld, Pillager, Caps{Surface: 8, Cave: 8}},
		{Nether, Monster, Caps{Surface: 0, Cave: 16}},
		{Nether, Animal, Caps{Surface: 0, Cave: 4}},
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
	// capped somewhere in 8..16: at or below 8 there is headroom for
	// certain, above 16 the region is saturated for certain, between the
	// two it depends on how those mobs spawned.
	for _, tc := range []struct {
		count int
		want  Status
	}{
		{0, Headroom},
		{8, Headroom},
		{9, AtRisk},
		{16, AtRisk},
		{17, Capped},
	} {
		if got := StatusOf(Overworld, Monster, tc.count); got != tc.want {
			t.Errorf("StatusOf(overworld,monster,%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestStatusOfHandlesAZeroSurfaceCap(t *testing.T) {
	// Nether monsters are 0 surface / 16 cave. The lower bound is zero, so
	// any mob at all is already past it.
	if got := StatusOf(Nether, Monster, 1); got != AtRisk {
		t.Errorf("StatusOf(nether,monster,1) = %v, want AtRisk", got)
	}
	if got := StatusOf(Nether, Monster, 17); got != Capped {
		t.Errorf("StatusOf(nether,monster,17) = %v, want Capped", got)
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
		{8, Headroom},
		{9, AtRisk},
		{10, AtRisk},
		{11, Capped},
	} {
		if got := StatusOf(End, Monster, tc.count); got != tc.want {
			t.Errorf("StatusOf(end,monster,%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}
