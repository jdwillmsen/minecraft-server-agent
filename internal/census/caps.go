package census

// GlobalCap is Bedrock's server-wide ceiling on environmental spawning. It
// is not scaled by player count. The census reports world totals against it
// as context only: the figure the game checks is over loaded chunks, which a
// save cannot show.
const GlobalCap = 200

// Caps is one cell of the population-control table. Surface and Cave are
// separate ceilings for the same category.
type Caps struct {
	Surface, Cave int
}

// Range returns the caps as an ordered pair. The End inverts the usual
// relationship — its monster caps are 10 surface and 8 cave — so callers
// must never assume Surface is the lower bound.
func (c Caps) Range() (lower, upper int) {
	if c.Surface > c.Cave {
		return c.Cave, c.Surface
	}
	return c.Surface, c.Cave
}

var capTable = map[Dimension]map[Category]Caps{
	Overworld: {
		Monster:     {Surface: 8, Cave: 16},
		Animal:      {Surface: 4, Cave: 0},
		WaterAnimal: {Surface: 36, Cave: 0},
		Ambient:     {Surface: 0, Cave: 2},
		Pillager:    {Surface: 8, Cave: 8},
	},
	Nether: {
		Monster:     {Surface: 0, Cave: 16},
		Animal:      {Surface: 0, Cave: 4},
		WaterAnimal: {Surface: 0, Cave: 0},
		Ambient:     {Surface: 0, Cave: 0},
		Pillager:    {Surface: 0, Cave: 0},
	},
	End: {
		Monster:     {Surface: 10, Cave: 8},
		Animal:      {Surface: 4, Cave: 0},
		WaterAnimal: {Surface: 36, Cave: 0},
		Ambient:     {Surface: 0, Cave: 2},
		Pillager:    {Surface: 8, Cave: 8},
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
// graded against both bounds: at or below the lower bound there is headroom
// whichever way the mobs spawned, above the upper bound the region is
// saturated whichever way, and between them it depends on facts the save
// does not carry.
func StatusOf(d Dimension, c Category, count int) Status {
	caps, ok := CapsFor(d, c)
	if !ok {
		return StatusUnknown
	}
	lower, upper := caps.Range()
	switch {
	case count > upper:
		return Capped
	case count > lower:
		return AtRisk
	default:
		return Headroom
	}
}
