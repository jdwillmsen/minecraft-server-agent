package census

// GlobalCap is Bedrock's server-wide ceiling on environmental spawning. It
// is not scaled by player count. The census reports world totals against it
// as context only: the figure the game checks is over loaded chunks, which a
// save cannot show.
const GlobalCap = 200

// NoSpawn marks an environment a category cannot spawn in: animals do not
// spawn underground, and nothing in the nether spawns under open sky.
//
// It is deliberately not zero. A ceiling of zero and an environment with no
// ceiling behave identically for spawning - nothing spawns either way - but
// they differ entirely when grading mobs that are already there. Every mob
// present occupies its category's density regardless of how it got there, so
// the count is real; what is absent is a ceiling of this environment's to
// measure it against, because those mobs arrived under the other
// environment's ceiling, or were bred, spawned from an egg or led in. Zero
// would grade every one of them as over cap.
const NoSpawn = -1

// Caps is one cell of the population-control table. Surface and Cave are
// separate ceilings for the same category, either of which may be NoSpawn.
type Caps struct {
	Surface, Cave int
}

// Range returns the applicable caps as an ordered pair. The End inverts the
// usual relationship — its monster caps are 10 surface and 8 cave — so
// callers must never assume Surface is the lower bound.
//
// Where only one environment spawns the category, both bounds are that cap:
// the ambiguity the range exists to express is gone, and the count can be
// graded exactly.
func (c Caps) Range() (lower, upper int) {
	switch {
	// Neither environment spawns the category, so there are no bounds to
	// grade against - which is not the same as bounds of zero.
	case c.Surface == NoSpawn && c.Cave == NoSpawn:
		return NoSpawn, NoSpawn
	case c.Surface == NoSpawn:
		return c.Cave, c.Cave
	case c.Cave == NoSpawn:
		return c.Surface, c.Surface
	case c.Surface > c.Cave:
		return c.Cave, c.Surface
	default:
		return c.Surface, c.Cave
	}
}

// capTable holds a cell only where the category spawns in that dimension.
// An absent row is a category Bedrock never spawns there, which is a
// different statement from a cap it cannot exceed.
var capTable = map[Dimension]map[Category]Caps{
	Overworld: {
		Monster:     {Surface: 8, Cave: 16},
		Animal:      {Surface: 4, Cave: NoSpawn},
		WaterAnimal: {Surface: 36, Cave: NoSpawn},
		Ambient:     {Surface: NoSpawn, Cave: 2},
		Pillager:    {Surface: 8, Cave: 8},
	},
	// Nothing in the nether spawns under open sky, so every nether spawn is
	// a cave spawn. Water animals, bats and pillager patrols have no nether
	// spawning at all.
	Nether: {
		Monster: {Surface: NoSpawn, Cave: 16},
		Animal:  {Surface: NoSpawn, Cave: 4},
	},
	End: {
		Monster: {Surface: 10, Cave: 8},
	},
}

// CapsFor returns the population caps for a dimension and category. It
// reports false for categories that consume no cap and for entities whose
// dimension could not be resolved.
func CapsFor(d Dimension, c Category) (Caps, bool) {
	byCategory, ok := capTable[d]
	if !ok {
		return Caps{}, false
	}
	caps, ok := byCategory[c]
	return caps, ok
}

// Status is how close a region is to refusing new spawns.
type Status int

const (
	StatusUnknown Status = iota
	Headroom
	AtRisk
	Capped
)

func (s Status) String() string {
	switch s {
	case Headroom:
		return "headroom"
	case AtRisk:
		return "at_risk"
	case Capped:
		return "capped"
	default:
		return "unknown"
	}
}

// StatusOf grades a regional count against its cap range.
//
// Bedrock fixes whether a mob counts as a surface or a cave spawn at spawn
// time and does not write that to the save, so the exact applicable cap
// cannot be recovered. Rather than invent a single number, the count is
// graded against both bounds. Bedrock refuses a spawn once the count reaches
// the ceiling, so below the lower bound there is headroom whichever way the
// mobs spawned, at or above the upper bound the region is saturated whichever
// way, and between them it depends on facts the save does not carry.
func StatusOf(d Dimension, c Category, count int) Status {
	caps, ok := CapsFor(d, c)
	if !ok {
		return StatusUnknown
	}
	return caps.Status(count)
}

// Status grades a count against this cell's caps.
func (c Caps) Status(count int) Status {
	lower, upper := c.Range()
	if upper == NoSpawn {
		return StatusUnknown
	}
	switch {
	case count >= upper:
		return Capped
	case count >= lower:
		return AtRisk
	default:
		return Headroom
	}
}
