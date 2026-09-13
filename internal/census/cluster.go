package census

import (
	"math"
	"sort"
)

// Cluster is a group of entities that sit within a chained radius of each
// other. Clusters are what turn a flat count into a location: "4,147 items"
// is a number, "4,147 items at x=-24 y=14 z=1320" is a place to go.
type Cluster struct {
	Count                     int
	CentreX, CentreY, CentreZ float64
	MinX, MaxX                float64
	MinY, MaxY                float64
	MinZ, MaxZ                float64
}

// ClusterEntities groups entities by proximity and returns the groups
// largest first.
//
// Grouping is transitive: two entities further apart than the radius still
// share a cluster if a chain of neighbours links them. A spatial hash keeps
// the comparison local, so this stays practical on the tens of thousands of
// entities a real world holds rather than degrading to every-pair.
func ClusterEntities(entities []Entity, radius float64) []Cluster {
	n := len(entities)
	if n == 0 {
		return nil
	}

	type cell struct{ x, y, z int }
	grid := map[cell][]int{}
	// Floor, not truncation. Truncating toward zero makes the cell
	// straddling an axis twice as wide as the others, which is survivable
	// here but makes the neighbour argument depend on a coincidence rather
	// than on every cell being exactly one radius across.
	cellOf := func(e Entity) cell {
		return cell{
			int(math.Floor(e.X / radius)),
			int(math.Floor(e.Y / radius)),
			int(math.Floor(e.Z / radius)),
		}
	}
	for i, e := range entities {
		c := cellOf(e)
		grid[c] = append(grid[c], i)
	}

	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(a int) int {
		for parent[a] != a {
			parent[a] = parent[parent[a]]
			a = parent[a]
		}
		return a
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	r2 := radius * radius
	for c, members := range grid {
		for dx := -1; dx <= 1; dx++ {
			for dy := -1; dy <= 1; dy++ {
				for dz := -1; dz <= 1; dz++ {
					neighbours, ok := grid[cell{c.x + dx, c.y + dy, c.z + dz}]
					if !ok {
						continue
					}
					for _, i := range members {
						for _, j := range neighbours {
							if j <= i {
								continue
							}
							ex, ey, ez := entities[i].X-entities[j].X,
								entities[i].Y-entities[j].Y,
								entities[i].Z-entities[j].Z
							if ex*ex+ey*ey+ez*ez <= r2 {
								union(i, j)
							}
						}
					}
				}
			}
		}
	}

	members := map[int][]int{}
	for i := 0; i < n; i++ {
		root := find(i)
		members[root] = append(members[root], i)
	}

	out := make([]Cluster, 0, len(members))
	for _, group := range members {
		first := entities[group[0]]
		c := Cluster{
			Count: len(group),
			MinX:  first.X, MaxX: first.X,
			MinY: first.Y, MaxY: first.Y,
			MinZ: first.Z, MaxZ: first.Z,
		}
		var sx, sy, sz float64
		for _, i := range group {
			e := entities[i]
			sx, sy, sz = sx+e.X, sy+e.Y, sz+e.Z
			c.MinX, c.MaxX = min(c.MinX, e.X), max(c.MaxX, e.X)
			c.MinY, c.MaxY = min(c.MinY, e.Y), max(c.MaxY, e.Y)
			c.MinZ, c.MaxZ = min(c.MinZ, e.Z), max(c.MaxZ, e.Z)
		}
		count := float64(len(group))
		c.CentreX, c.CentreY, c.CentreZ = sx/count, sy/count, sz/count
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
