// Package census reads a Bedrock world save and reports what lives in it.
package census

import "strings"

// Dimension identifies which of the three worlds an entity is saved in.
type Dimension int

const (
	Overworld Dimension = 0
	Nether    Dimension = 1
	End       Dimension = 2

	// UnknownDimension covers actors whose owning chunk carried no digp
	// record. They are real entities that cannot be placed, so they are
	// counted separately rather than silently attributed to the overworld.
	UnknownDimension Dimension = -1
)

func (d Dimension) String() string {
	switch d {
	case Overworld:
		return "overworld"
	case Nether:
		return "nether"
	case End:
		return "end"
	default:
		return "unknown"
	}
}

// Entity is one actor record, reduced to the fields the census reasons about.
type Entity struct {
	Identifier  string
	UniqueID    int64
	Dimension   Dimension
	X, Y, Z     float64
	CustomName  string
	NameVisible bool
	Persistent  bool
	Health      int
}

// EntityFromNBT reduces a decoded actor record. It reports false when the
// record carries no usable position: such an actor cannot be assigned to a
// region, which is the whole point of the census.
//
// The input must come from a map decode. gophertunnel's NBT decoder aborts on
// any tag missing from a target struct, and actor records carry dozens of
// varying tags, so struct decoding fails on nearly every entity.
func EntityFromNBT(m map[string]any) (Entity, bool) {
	x, y, z, ok := nbtPos(m)
	if !ok {
		return Entity{}, false
	}
	return Entity{
		Identifier:  strings.TrimPrefix(nbtString(m, "identifier"), "minecraft:"),
		UniqueID:    nbtInt64(m, "UniqueID"),
		Dimension:   UnknownDimension,
		X:           x,
		Y:           y,
		Z:           z,
		CustomName:  nbtString(m, "CustomName"),
		NameVisible: nbtFlag(m, "CustomNameVisible"),
		Persistent:  nbtFlag(m, "Persistent"),
		Health:      nbtInt(m, "Health"),
	}, true
}

func nbtString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func nbtInt64(m map[string]any, key string) int64 {
	v, _ := m[key].(int64)
	return v
}

// nbtFlag reads a TAG_Byte used as a boolean. Bedrock writes these as uint8.
func nbtFlag(m map[string]any, key string) bool {
	v, _ := m[key].(uint8)
	return v != 0
}

// nbtInt reads a small integer tag. Health is TAG_Short, so int16.
func nbtInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int16:
		return int(v)
	case int32:
		return int(v)
	case uint8:
		return int(v)
	}
	return 0
}

// nbtPos reads the three-element Pos list. A map decode yields []interface{}
// holding float32, so neither []float32 nor float64 elements will assert.
func nbtPos(m map[string]any) (x, y, z float64, ok bool) {
	list, isList := m["Pos"].([]any)
	if !isList || len(list) != 3 {
		return 0, 0, 0, false
	}
	out := make([]float64, 3)
	for i, v := range list {
		f, isFloat := v.(float32)
		if !isFloat {
			return 0, 0, 0, false
		}
		out[i] = float64(f)
	}
	return out[0], out[1], out[2], true
}
