package census

import (
	"sort"
	"time"
)

// Total is one entity type's count within one dimension.
type Total struct {
	Dimension  Dimension
	Identifier string
	Category   Category
	Count      int
}

// Region is one population-control region's occupancy for one category,
// graded against that category's cap range.
type Region struct {
	Key      RegionKey
	Category Category
	Count    int
	Status   Status
}

// Named is a name-tagged entity. Name tags force persistence, so each of
// these permanently occupies a slot in its region's cap.
type Named struct {
	Name       string
	Identifier string
	Dimension  Dimension
	X, Y, Z    float64
	Persistent bool
}

// Concentration is the largest cluster of one entity type in one dimension.
// It is what turns "6,515 items in the nether" into a place to stand.
type Concentration struct {
	Dimension  Dimension
	Identifier string
	Cluster    Cluster
}

// ConcentrationThreshold is how many of one type in one dimension it takes
// before the census bothers clustering them, and ConcentrationRadius is the
// chaining distance used. Below the threshold a "pile" is just a herd.
// ConcentrationsPerType bounds how many clusters of one type are kept.
// Keeping only the largest hides the second pile, and the second pile is
// often the interesting one: on FWB the base village outranks the iron farm,
// so reporting one cluster per type would have made the farm invisible.
const (
	ConcentrationThreshold = 25
	ConcentrationRadius    = 24.0
	ConcentrationsPerType  = 3
)

// Census is one complete reading of a world.
type Census struct {
	// TakenAt is when the world data was captured, not when the scan ran.
	// Every consumer renders it: a report that cannot say how old it is
	// will eventually be believed when it should not be.
	TakenAt    time.Time
	SourceKind string
	Stats      ScanStats

	Totals                 []Total
	Regions                []Region
	Named                  []Named
	PersistentByIdentifier map[string]int
	Concentrations         []Concentration
}

// Aggregate turns scanned entities into the census.
//
// Only categories that consume a cap are graded into regions. Items,
// projectiles and vehicles are counted in the totals — they matter for tick
// cost and for spotting leaks — but they occupy no spawn slot, so grading
// them would invent pressure that does not exist.
func Aggregate(entities []Entity, stats ScanStats, takenAt time.Time, sourceKind string) Census {
	c := Census{
		TakenAt:                takenAt,
		SourceKind:             sourceKind,
		Stats:                  stats,
		PersistentByIdentifier: map[string]int{},
	}

	type totalKey struct {
		dimension  Dimension
		identifier string
	}
	totals := map[totalKey]int{}
	type regionCategory struct {
		key      RegionKey
		category Category
	}
	regions := map[regionCategory]int{}

	// Entities are retained per type so the largest concentration of each
	// can be located afterwards. Only types that pass the threshold are
	// clustered, so this never runs the pairwise pass over a long tail of
	// singletons.
	byType := map[totalKey][]Entity{}

	for _, e := range entities {
		category := CategoryOf(e.Identifier)
		key := totalKey{e.Dimension, e.Identifier}
		totals[key]++
		byType[key] = append(byType[key], e)

		if e.Persistent {
			c.PersistentByIdentifier[e.Identifier]++
		}
		if e.CustomName != "" {
			c.Named = append(c.Named, Named{
				Name:       e.CustomName,
				Identifier: e.Identifier,
				Dimension:  e.Dimension,
				X:          e.X, Y: e.Y, Z: e.Z,
				Persistent: e.Persistent,
			})
		}
		if _, counted := CapsFor(e.Dimension, category); counted {
			regions[regionCategory{RegionOf(e.Dimension, e.X, e.Z), category}]++
		}
	}

	for k, count := range totals {
		c.Totals = append(c.Totals, Total{
			Dimension:  k.dimension,
			Identifier: k.identifier,
			Category:   CategoryOf(k.identifier),
			Count:      count,
		})
	}
	sort.Slice(c.Totals, func(i, j int) bool {
		if c.Totals[i].Count != c.Totals[j].Count {
			return c.Totals[i].Count > c.Totals[j].Count
		}
		if c.Totals[i].Identifier != c.Totals[j].Identifier {
			return c.Totals[i].Identifier < c.Totals[j].Identifier
		}
		return c.Totals[i].Dimension < c.Totals[j].Dimension
	})

	for k, count := range regions {
		c.Regions = append(c.Regions, Region{
			Key:      k.key,
			Category: k.category,
			Count:    count,
			Status:   StatusOf(k.key.Dimension, k.category, count),
		})
	}
	sort.Slice(c.Regions, func(i, j int) bool {
		if c.Regions[i].Count != c.Regions[j].Count {
			return c.Regions[i].Count > c.Regions[j].Count
		}
		if c.Regions[i].Key.Dimension != c.Regions[j].Key.Dimension {
			return c.Regions[i].Key.Dimension < c.Regions[j].Key.Dimension
		}
		if c.Regions[i].Key.X != c.Regions[j].Key.X {
			return c.Regions[i].Key.X < c.Regions[j].Key.X
		}
		if c.Regions[i].Key.Z != c.Regions[j].Key.Z {
			return c.Regions[i].Key.Z < c.Regions[j].Key.Z
		}
		return c.Regions[i].Category < c.Regions[j].Category
	})

	for key, group := range byType {
		if len(group) < ConcentrationThreshold {
			continue
		}
		kept := 0
		for _, cluster := range ClusterEntities(group, ConcentrationRadius) {
			if cluster.Count < ConcentrationThreshold || kept >= ConcentrationsPerType {
				break // clusters are sorted largest first
			}
			c.Concentrations = append(c.Concentrations, Concentration{
				Dimension:  key.dimension,
				Identifier: key.identifier,
				Cluster:    cluster,
			})
			kept++
		}
	}
	sort.Slice(c.Concentrations, func(i, j int) bool {
		if c.Concentrations[i].Cluster.Count != c.Concentrations[j].Cluster.Count {
			return c.Concentrations[i].Cluster.Count > c.Concentrations[j].Cluster.Count
		}
		if c.Concentrations[i].Identifier != c.Concentrations[j].Identifier {
			return c.Concentrations[i].Identifier < c.Concentrations[j].Identifier
		}
		if c.Concentrations[i].Dimension != c.Concentrations[j].Dimension {
			return c.Concentrations[i].Dimension < c.Concentrations[j].Dimension
		}
		if c.Concentrations[i].Cluster.CentreX != c.Concentrations[j].Cluster.CentreX {
			return c.Concentrations[i].Cluster.CentreX < c.Concentrations[j].Cluster.CentreX
		}
		if c.Concentrations[i].Cluster.CentreZ != c.Concentrations[j].Cluster.CentreZ {
			return c.Concentrations[i].Cluster.CentreZ < c.Concentrations[j].Cluster.CentreZ
		}
		if c.Concentrations[i].Cluster.CentreY != c.Concentrations[j].Cluster.CentreY {
			return c.Concentrations[i].Cluster.CentreY < c.Concentrations[j].Cluster.CentreY
		}
		if c.Concentrations[i].Cluster.MinX != c.Concentrations[j].Cluster.MinX {
			return c.Concentrations[i].Cluster.MinX < c.Concentrations[j].Cluster.MinX
		}
		if c.Concentrations[i].Cluster.MinY != c.Concentrations[j].Cluster.MinY {
			return c.Concentrations[i].Cluster.MinY < c.Concentrations[j].Cluster.MinY
		}
		if c.Concentrations[i].Cluster.MinZ != c.Concentrations[j].Cluster.MinZ {
			return c.Concentrations[i].Cluster.MinZ < c.Concentrations[j].Cluster.MinZ
		}
		if c.Concentrations[i].Cluster.MaxX != c.Concentrations[j].Cluster.MaxX {
			return c.Concentrations[i].Cluster.MaxX < c.Concentrations[j].Cluster.MaxX
		}
		if c.Concentrations[i].Cluster.MaxY != c.Concentrations[j].Cluster.MaxY {
			return c.Concentrations[i].Cluster.MaxY < c.Concentrations[j].Cluster.MaxY
		}
		return c.Concentrations[i].Cluster.MaxZ < c.Concentrations[j].Cluster.MaxZ
	})

	sort.Slice(c.Named, func(i, j int) bool {
		if c.Named[i].Name != c.Named[j].Name {
			return c.Named[i].Name < c.Named[j].Name
		}
		if c.Named[i].Identifier != c.Named[j].Identifier {
			return c.Named[i].Identifier < c.Named[j].Identifier
		}
		if c.Named[i].Dimension != c.Named[j].Dimension {
			return c.Named[i].Dimension < c.Named[j].Dimension
		}
		if c.Named[i].X != c.Named[j].X {
			return c.Named[i].X < c.Named[j].X
		}
		if c.Named[i].Y != c.Named[j].Y {
			return c.Named[i].Y < c.Named[j].Y
		}
		return c.Named[i].Z < c.Named[j].Z
	})
	return c
}
