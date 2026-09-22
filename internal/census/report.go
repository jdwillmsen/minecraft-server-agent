package census

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ReportOptions bounds the report. A world holds thousands of regions and
// dozens of entity types; a report nobody can read gets skimmed instead.
type ReportOptions struct {
	TopRegions int
	TopTypes   int
	// Reference is where herd distances are measured from.
	Reference Point
}

// FWBBase is the reference point the report defaults to. The census is
// written for one world, and the question its variant section answers is how
// far a player standing at home has to go.
var FWBBase = Point{X: 168, Z: 248}

func DefaultReportOptions() ReportOptions {
	return ReportOptions{TopRegions: 15, TopTypes: 20, Reference: FWBBase}
}

// Render writes the census as plain text.
func Render(c Census, opts ReportOptions) string {
	var b strings.Builder

	taken := "unknown"
	if !c.TakenAt.IsZero() {
		taken = c.TakenAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(&b, "FWB mob census\n")
	fmt.Fprintf(&b, "world taken at %s via %s\n", taken, sourceKindOrUnknown(c.SourceKind))
	fmt.Fprintf(&b, "records %d, decoded %d, unparsable %d, unplaced %d, unidentified %d\n",
		c.Stats.Records, c.Stats.Decoded, c.Stats.Unparsable, c.Stats.Unplaced, c.Stats.Unidentified)
	if c.Stats.FirstUnparsableErr != "" {
		fmt.Fprintf(&b, "first decode failure: %s\n", c.Stats.FirstUnparsableErr)
	}
	// Stated only when it happened, and stated as a placement failure rather
	// than as an absence: these entities exist, and the per-dimension
	// sections below undercount by exactly this many.
	if c.Stats.UnresolvedDimension > 0 {
		fmt.Fprintf(&b, "dimension unresolved for %d entities; they are real and counted only under unknown\n",
			c.Stats.UnresolvedDimension)
		fmt.Fprintf(&b, "  chunk records skipped: %d bad key, %d bad value\n",
			c.Stats.DigpSkippedKey, c.Stats.DigpSkippedValue)
	}
	fmt.Fprintf(&b, "\n")

	byDimension := map[Dimension]int{}
	for _, t := range c.Totals {
		byDimension[t.Dimension] += t.Count
	}
	fmt.Fprintf(&b, "entities by dimension\n")
	for _, d := range []Dimension{Overworld, Nether, End, UnknownDimension, UnrecognisedDimension} {
		if n, ok := byDimension[d]; ok {
			fmt.Fprintf(&b, "  %-10s %6d\n", d, n)
		}
	}

	fmt.Fprintf(&b, "\ntop entity types\n")
	for i, t := range c.Totals {
		if i >= opts.TopTypes {
			break
		}
		fmt.Fprintf(&b, "  %-10s %-24s %-12s %6d\n", t.Dimension, t.Identifier, t.Category, t.Count)
	}

	fmt.Fprintf(&b, "\nregions closest to their spawn cap (%d blocks square)\n", RegionSize)
	fmt.Fprintf(&b, "  where a category spawns both above and below ground the two caps differ\n")
	fmt.Fprintf(&b, "  and the save does not record which applies, so those regions are graded\n")
	fmt.Fprintf(&b, "  against the range rather than one number\n")
	// Ranked within each dimension rather than globally. The End is full of
	// end-city shulkers whose counts dwarf everything else, and a single
	// global ranking buries the overworld and nether regions a player can
	// actually do something about.
	for _, d := range []Dimension{Overworld, Nether, End, UnknownDimension, UnrecognisedDimension} {
		shown := 0
		for _, r := range c.Regions {
			if r.Key.Dimension != d {
				continue
			}
			if shown >= opts.TopRegions {
				break
			}
			if shown == 0 {
				fmt.Fprintf(&b, "  %s\n", d)
			}
			minX, maxX, minZ, maxZ := r.Key.Bounds()
			fmt.Fprintf(&b, "    x %6d..%-6d z %6d..%-6d %-12s %4d / %-6s %s\n",
				minX, maxX, minZ, maxZ, r.Category, r.Count, capBounds(r.Caps), r.Status)
			shown++
		}
	}

	fmt.Fprintf(&b, "\nname-tagged entities (%d)\n", len(c.Named))
	fmt.Fprintf(&b, "  a name tag forces persistence, so each of these holds a cap slot forever\n")
	for _, n := range c.Named {
		fmt.Fprintf(&b, "  %-24s %-20s %-10s x=%.0f y=%.0f z=%.0f\n",
			n.Name, n.Identifier, n.Dimension, n.X, n.Y, n.Z)
	}

	if len(c.Concentrations) > 0 {
		fmt.Fprintf(&b, "\nlargest concentrations (%d or more of one type within %.0f blocks)\n",
			ConcentrationThreshold, ConcentrationRadius)
		// Grouping is transitive, so a cluster can be a trail hundreds of
		// blocks long rather than a pile, and its centre can sit where
		// nothing is. The extent is the part worth flying to; the centre is
		// kept because it is what every earlier report printed.
		fmt.Fprintf(&b, "  grouping chains neighbours, so a cluster may be a trail rather than a pile\n")
		fmt.Fprintf(&b, "  the extent is where to look; the centre may hold nothing at all\n")
		for i, con := range c.Concentrations {
			if i >= opts.TopTypes {
				break
			}
			fmt.Fprintf(&b, "  %6d  %-22s %-10s x=%.0f..%.0f y=%.0f..%.0f z=%.0f..%.0f  centre x=%.0f y=%.0f z=%.0f\n",
				con.Cluster.Count, con.Identifier, con.Dimension,
				con.Cluster.MinX, con.Cluster.MaxX,
				con.Cluster.MinY, con.Cluster.MaxY,
				con.Cluster.MinZ, con.Cluster.MaxZ,
				con.Cluster.CentreX, con.Cluster.CentreY, con.Cluster.CentreZ)
		}
	}

	if len(c.Variants) > 0 {
		fmt.Fprintf(&b, "\nanimal variants (herds within %.0f blocks, nearest %d to x=%.0f z=%.0f)\n",
			ConcentrationRadius, HerdsPerVariant, opts.Reference.X, opts.Reference.Z)
		fmt.Fprintf(&b, "  legacy is a record carrying no variant at all, which is not the same as temperate\n")
		for _, g := range c.Variants {
			fmt.Fprintf(&b, "  %-12s %-12s %6d", g.Identifier, g.Variant, g.Count)
			if g.Elsewhere > 0 {
				fmt.Fprintf(&b, "  (%d outside the overworld, not located)", g.Elsewhere)
			}
			fmt.Fprintf(&b, "\n")
			for _, h := range g.NearestHerds(opts.Reference, HerdsPerVariant) {
				fmt.Fprintf(&b, "    %6.0f blocks  %4d at x=%.0f..%.0f z=%.0f..%.0f  centre x=%.0f y=%.0f z=%.0f\n",
					opts.Reference.Distance(h), h.Count,
					h.MinX, h.MaxX, h.MinZ, h.MaxZ,
					h.CentreX, h.CentreY, h.CentreZ)
			}
		}
	}

	fmt.Fprintf(&b, "\npersistent entities by type\n")
	type kv struct {
		identifier string
		count      int
	}
	var persistent []kv
	for id, n := range c.PersistentByIdentifier {
		persistent = append(persistent, kv{id, n})
	}
	sort.Slice(persistent, func(i, j int) bool {
		if persistent[i].count != persistent[j].count {
			return persistent[i].count > persistent[j].count
		}
		return persistent[i].identifier < persistent[j].identifier
	})
	for i, p := range persistent {
		if i >= opts.TopTypes {
			break
		}
		fmt.Fprintf(&b, "  %-24s %6d\n", p.identifier, p.count)
	}

	return b.String()
}

// capBounds renders a cell's caps. A category that spawns in only one
// environment has one known ceiling, and printing "4..4" for it would
// advertise an ambiguity the save does not leave open.
func capBounds(c Caps) string {
	lower, upper := c.Range()
	if upper == NoSpawn {
		return "none"
	}
	if lower == upper {
		return strconv.Itoa(upper)
	}
	return fmt.Sprintf("%d..%d", lower, upper)
}

func sourceKindOrUnknown(kind string) string {
	if kind == "" {
		return "unknown source"
	}
	return kind
}
