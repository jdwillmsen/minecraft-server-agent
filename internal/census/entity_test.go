package census

import "testing"

func TestEntityFromNBTReadsTheFieldTypesBedrockActuallyWrites(t *testing.T) {
	// These concrete types were read off the live FWB world. A decode into
	// map[string]any yields float32 inside an []interface{} for Pos, and
	// uint8 for the boolean-ish flags — not float64 and not bool.
	m := map[string]any{
		"identifier":        "minecraft:zombie",
		"Pos":               []any{float32(1.5), float32(64), float32(-2.5)},
		"UniqueID":          int64(42),
		"CustomName":        "Bob",
		"CustomNameVisible": uint8(1),
		"Persistent":        uint8(1),
		"Health":            int16(20),
	}
	e, ok := EntityFromNBT(m)
	if !ok {
		t.Fatal("EntityFromNBT returned ok=false for a record with a valid Pos")
	}
	if e.Identifier != "zombie" {
		t.Errorf("Identifier = %q, want %q — the minecraft: namespace must be stripped", e.Identifier, "zombie")
	}
	if e.X != 1.5 || e.Y != 64 || e.Z != -2.5 {
		t.Errorf("Pos = (%v,%v,%v), want (1.5,64,-2.5)", e.X, e.Y, e.Z)
	}
	if e.UniqueID != 42 {
		t.Errorf("UniqueID = %d, want 42", e.UniqueID)
	}
	if e.CustomName != "Bob" || !e.NameVisible {
		t.Errorf("CustomName = %q NameVisible = %v, want %q true", e.CustomName, e.NameVisible, "Bob")
	}
	if !e.Persistent {
		t.Error("Persistent = false, want true")
	}
	if e.Health != 20 {
		t.Errorf("Health = %d, want 20", e.Health)
	}
}

func TestEntityFromNBTRejectsARecordWithNoPosition(t *testing.T) {
	if _, ok := EntityFromNBT(map[string]any{"identifier": "minecraft:zombie"}); ok {
		t.Error("ok = true for a record with no Pos, want false")
	}
	if _, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:zombie",
		"Pos":        []any{float32(1)},
	}); ok {
		t.Error("ok = true for a Pos with one element, want false")
	}
}

func TestEntityFromNBTDefaultsAbsentOptionalFields(t *testing.T) {
	e, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:item",
		"Pos":        []any{float32(0), float32(0), float32(0)},
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if e.CustomName != "" || e.NameVisible || e.Persistent || e.Health != 0 {
		t.Errorf("absent optional fields did not default: %+v", e)
	}
}

func TestEntityFromNBTPreservesNonASCIINameTags(t *testing.T) {
	// Name tags are arbitrary player text. NBT strings are length-prefixed
	// UTF-8, so a multi-byte tag must survive byte-for-byte rather than
	// being truncated at a byte boundary inside a rune.
	const name = "Señor Oink 🐖"
	e, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:pig",
		"Pos":        []any{float32(0), float32(0), float32(0)},
		"CustomName": name,
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if e.CustomName != name {
		t.Errorf("CustomName = %q, want %q", e.CustomName, name)
	}
}

func TestDimensionString(t *testing.T) {
	for d, want := range map[Dimension]string{
		Overworld: "overworld", Nether: "nether", End: "end", UnknownDimension: "unknown",
	} {
		if got := d.String(); got != want {
			t.Errorf("Dimension(%d).String() = %q, want %q", int(d), got, want)
		}
	}
}

// The three record shapes below are the ones a scan of the live world turned
// up: an animal saved since variants existed, one saved before and never
// reloaded, and a mooshroom, whose colour is the integer Variant tag rather
// than a property.
func TestEntityFromNBTReadsAVariantBearingAnimal(t *testing.T) {
	e, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:cow",
		"Pos":        pos(170, 70, 250),
		"properties": map[string]any{
			"minecraft:climate_variant": "temperate",
			"minecraft:sound_variant":   "default",
		},
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if e.Climate != "temperate" || e.Sound != "default" {
		t.Errorf("Climate = %q Sound = %q, want temperate default", e.Climate, e.Sound)
	}
}

func TestEntityFromNBTReportsARecordWithNoPropertiesAsLegacy(t *testing.T) {
	e, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:chicken",
		"Pos":        pos(170, 70, 250),
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if e.Climate != ClimateLegacy {
		t.Errorf("Climate = %q, want %q: an animal saved before variants is not a temperate one", e.Climate, ClimateLegacy)
	}
	if e.Sound != "" {
		t.Errorf("Sound = %q, want empty", e.Sound)
	}
}

func TestEntityFromNBTReadsAMooshroomsColour(t *testing.T) {
	for variant, want := range map[int32]int{0: MooshroomRed, 1: MooshroomBrown} {
		e, ok := EntityFromNBT(map[string]any{
			"identifier": "minecraft:mooshroom",
			"Pos":        pos(0, 64, 0),
			"Variant":    variant,
		})
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if e.Variant != want {
			t.Errorf("Variant tag %d read as %d, want %d", variant, e.Variant, want)
		}
	}
}
