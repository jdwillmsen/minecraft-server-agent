package census

import "math"

// RegionSize is the side of Bedrock's population-control region: 9 chunks,
// 144 blocks. Caps are counted per region, per category.
const RegionSize = 144

// RegionKey identifies one population-control region. Dimension is part of
// the key because the cap tables differ per dimension and regions at the
// same coordinates in different dimensions are unrelated.
type RegionKey struct {
	Dimension Dimension
	X, Z      int
}

// RegionOf places a position into its region. It floors rather than
// truncating: truncation rounds toward zero, which would merge the regions
// either side of an axis into one.
func RegionOf(d Dimension, x, z float64) RegionKey {
	return RegionKey{
		Dimension: d,
		X:         int(math.Floor(x / RegionSize)),
		Z:         int(math.Floor(z / RegionSize)),
	}
}

// Bounds returns the inclusive block bounds of the region, for reporting.
func (r RegionKey) Bounds() (minX, maxX, minZ, maxZ int) {
	return r.X * RegionSize, r.X*RegionSize + RegionSize - 1,
		r.Z * RegionSize, r.Z*RegionSize + RegionSize - 1
}
