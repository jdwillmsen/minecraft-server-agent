package census

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReportOptions bounds the report. A world holds thousands of regions and
// dozens of entity types; a report nobody can read gets skimmed instead.
type ReportOptions struct {
	TopRegions int
	TopTypes   int
}

func DefaultReportOptions() ReportOptions {
	return ReportOptions{TopRegions: 15, TopTypes: 20}
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
	fmt.Fprintf(&b, "records %d, decoded %d, unparsable %d, unplaced %d\n",
		c.Stats.Records, c.Stats.Decoded, c.Stats.Unparsable, c.Stats.Unplaced)
	if c.Stats.FirstUnparsableErr != "" {
		fmt.Fprintf(&b, "first decode failure: %s\n", c.Stats.FirstUnparsableErr)
	}
	fmt.Fprintf(&b, "\n")

	byDimension := map[Dimension]int{}
	for _, t := range c.Totals {
		byDimension[t.Dimension] += t.Count
	}
	fmt.Fprintf(&b, "entities by dimension\n")
	for _, d := range []Dimension{Overworld, Nether, End, UnknownDimension} {
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
	fmt.Fprintf(&b, "  surface and cave caps differ and the save does not record which applies,\n")
	fmt.Fprintf(&b, "  so each region is graded against the range rather than one number\n")
	// Ranked within each dimension rather than globally. The End is full of
	// end-city shulkers whose counts dwarf everything else, and a single
	// global ranking buries the overworld and nether regions a player can
	// actually do something about.
	for _, d := range []Dimension{Overworld, Nether, End, UnknownDimension} {
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
			caps, _ := CapsFor(r.Key.Dimension, r.Category)
			lower, upper := caps.Range()
			fmt.Fprintf(&b, "    x %6d..%-6d z %6d..%-6d %-12s %4d / %d..%d  %s\n",
				minX, maxX, minZ, maxZ, r.Category, r.Count, lower, upper, r.Status)
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
		for i, con := range c.Concentrations {
			if i >= opts.TopTypes {
				break
			}
			fmt.Fprintf(&b, "  %6d  %-22s %-10s x=%.0f y=%.0f z=%.0f\n",
				con.Cluster.Count, con.Identifier, con.Dimension,
				con.Cluster.CentreX, con.Cluster.CentreY, con.Cluster.CentreZ)
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

func sourceKindOrUnknown(kind string) string {
	if kind == "" {
		return "unknown source"
	}
	return kind
}
