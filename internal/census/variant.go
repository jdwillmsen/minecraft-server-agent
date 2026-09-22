package census

import (
	"math"
	"sort"
	"strconv"
)

// ClimateIdentifiers are the animals whose climate variant the census
// reports. They are the only mobs that carry one, and the metric that labels
// by climate is bounded by this list.
var ClimateIdentifiers = []string{"chicken", "cow", "pig"}

// Climates are the climate variants the game writes, plus ClimateLegacy. The
// metric zero-fills these so a variant that dies out reports 0 rather than a
// series that vanishes.
var Climates = []string{"temperate", "cold", "warm", ClimateLegacy}

// ClimateUnrecorded is reported for a record that has a properties compound
// but no climate in it. Folding it into legacy would claim the record has no
// properties at all, which it demonstrably does, and would hide a layout
// change that moved the climate to another key.
const ClimateUnrecorded = "unrecorded"

// HerdsPerVariant bounds how many herds of one variant the report lists.
// They are listed nearest first, because the question this section answers
// is "where is the closest brown mooshroom", not "where is the biggest".
const HerdsPerVariant = 3

// VariantGroup is every animal of one identifier and one variant.
//
// Herds holds only overworld clusters, because the reference point they are
// measured from is an overworld position and a nether coordinate is a
// different place with the same numbers. Elsewhere counts what was left out
// of them, so Count still agrees with the totals.
type VariantGroup struct {
	Identifier string
	Variant    string
	Count      int
	Elsewhere  int
	Herds      []Cluster
}

// variantOf names an entity's variant, or reports false for a mob the
// census does not break down by variant.
func variantOf(e Entity) (string, bool) {
	if e.Identifier == "mooshroom" {
		switch e.Variant {
		case MooshroomRed:
			return "red", true
		case MooshroomBrown:
			return "brown", true
		default:
			return "variant " + strconv.Itoa(e.Variant), true
		}
	}
	for _, id := range ClimateIdentifiers {
		if e.Identifier == id {
			if e.Climate == "" {
				return ClimateUnrecorded, true
			}
			return e.Climate, true
		}
	}
	return "", false
}

// aggregateVariants groups the variant-bearing animals and locates their
// herds. Herds are clusters rather than individuals because a pen of twenty
// cows is one place to go, and listing it twenty times would push every other
// herd off the report.
func aggregateVariants(entities []Entity) []VariantGroup {
	type key struct{ identifier, variant string }
	groups := map[key]*VariantGroup{}
	overworld := map[key][]Entity{}
	for _, e := range entities {
		variant, ok := variantOf(e)
		if !ok {
			continue
		}
		k := key{e.Identifier, variant}
		g := groups[k]
		if g == nil {
			g = &VariantGroup{Identifier: e.Identifier, Variant: variant}
			groups[k] = g
		}
		g.Count++
		if e.Dimension == Overworld {
			overworld[k] = append(overworld[k], e)
		} else {
			g.Elsewhere++
		}
	}

	out := make([]VariantGroup, 0, len(groups))
	for k, g := range groups {
		g.Herds = ClusterEntities(overworld[k], ConcentrationRadius)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identifier != out[j].Identifier {
			return out[i].Identifier < out[j].Identifier
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Variant < out[j].Variant
	})
	return out
}

// Point is a flat x/z position in the overworld.
type Point struct {
	X, Z float64
}

// Distance is the flat distance from p to a herd's centre. Height is left out
// because it is the one axis nobody travels along to reach a herd.
func (p Point) Distance(c Cluster) float64 {
	return math.Hypot(c.CentreX-p.X, c.CentreZ-p.Z)
}

// NearestHerds returns up to n herds ordered by distance from p, ties broken
// by the cluster order ClusterEntities already made deterministic.
func (g VariantGroup) NearestHerds(p Point, n int) []Cluster {
	herds := append([]Cluster(nil), g.Herds...)
	sort.SliceStable(herds, func(i, j int) bool {
		return p.Distance(herds[i]) < p.Distance(herds[j])
	})
	if len(herds) > n {
		herds = herds[:n]
	}
	return herds
}
