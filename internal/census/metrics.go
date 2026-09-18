package census

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MetricsOptions bounds the exported series.
//
// The report can afford a long tail because a human reads it once; a time
// series cannot, because every distinct label set is kept for the retention
// period whether anyone looks at it or not.
type MetricsOptions struct {
	TopTypes int
}

func DefaultMetricsOptions() MetricsOptions {
	return MetricsOptions{TopTypes: DefaultReportOptions().TopTypes}
}

// RenderMetrics writes the census as a Prometheus text exposition payload.
//
// This is the same census the report renders, counted the same way — the two
// disagreeing would make both untrustworthy, so the numbers come from the
// aggregate rather than from a second pass over the entities.
//
// scrapedAt is passed in rather than read from the clock so that a caller can
// say when the scan ran, and so the output is a function of its inputs.
func RenderMetrics(c Census, scrapedAt time.Time, opts MetricsOptions) string {
	var b strings.Builder

	// Provenance first, because every number below it is only as current as
	// the world it was read from. A census taken from last night's archive is
	// a legitimate reading and a misleading one if its age is not published
	// beside it.
	gauge(&b, "mc_census_world_taken_at_timestamp_seconds",
		"Unix time the world this census read was captured, which is not when the scan ran.",
		nil, float64(c.TakenAt.Unix()))
	gauge(&b, "mc_census_scan_timestamp_seconds",
		"Unix time this census was taken.",
		nil, float64(scrapedAt.Unix()))
	fromSnapshot := 0.0
	if c.SourceKind == KindSnapshot {
		fromSnapshot = 1
	}
	gauge(&b, "mc_census_world_from_snapshot",
		"1 when the world was copied fresh from the running server, 0 when it came from a backup archive.",
		nil, fromSnapshot)

	byDimension := map[Dimension]int{}
	for _, t := range c.Totals {
		byDimension[t.Dimension] += t.Count
	}
	// Dimensions in a fixed order rather than map order: the payload is
	// diffed against the previous night's by whoever is chasing a change.
	head(&b, "mc_census_entities",
		"Entities stored in the world, by dimension. The world total is their sum.")
	for _, d := range allDimensions {
		if n, ok := byDimension[d]; ok {
			sample(&b, "mc_census_entities", labels{{"dimension", d.String()}}, float64(n))
		}
	}

	// Totals arrive sorted by count, so the top N is a prefix. Labelled with
	// the dimension as well as the identifier because 4,000 items in the
	// nether and 4,000 in the overworld are different problems.
	head(&b, "mc_census_entity_type",
		"Entities of one type in one dimension, for the largest types only.")
	for i, t := range c.Totals {
		if i >= opts.TopTypes {
			break
		}
		sample(&b, "mc_census_entity_type", labels{
			{"identifier", t.Identifier},
			{"dimension", t.Dimension.String()},
			{"category", t.Category.String()},
		}, float64(t.Count))
	}

	// Counts of regions per grade, never the regions themselves: the world
	// holds thousands, their keys change every night, and the question this
	// answers is "how much of the world is at its cap", not "which cells".
	//
	// Every grade is emitted for every dimension and category that appears,
	// including the zeros, so a series does not vanish on the night nothing
	// is capped -- an absent series and a zero mean different things, and
	// only one of them is true here.
	type dimCategory struct {
		dimension Dimension
		category  Category
	}
	graded := map[dimCategory]map[Status]int{}
	for _, r := range c.Regions {
		key := dimCategory{r.Key.Dimension, r.Category}
		if graded[key] == nil {
			graded[key] = map[Status]int{}
		}
		graded[key][r.Status]++
	}
	keys := make([]dimCategory, 0, len(graded))
	for k := range graded {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].dimension != keys[j].dimension {
			return keys[i].dimension < keys[j].dimension
		}
		return keys[i].category < keys[j].category
	})
	head(&b, "mc_census_regions",
		"Population-control regions holding at least one entity of a capped category, graded against that category's cap.")
	for _, k := range keys {
		for _, status := range []Status{Headroom, AtRisk, Capped} {
			sample(&b, "mc_census_regions", labels{
				{"dimension", k.dimension.String()},
				{"category", k.category.String()},
				{"status", status.String()},
			}, float64(graded[k][status]))
		}
	}

	persistent := 0
	for _, n := range c.PersistentByIdentifier {
		persistent += n
	}
	gauge(&b, "mc_census_persistent_entities",
		"Entities the game will never despawn, each of which holds a cap slot permanently.",
		nil, float64(persistent))
	gauge(&b, "mc_census_named_entities",
		"Name-tagged entities, a subset of the persistent ones.",
		nil, float64(len(c.Named)))

	// The scan's own quality, because a census built from records that mostly
	// failed to decode renders as a quiet world rather than as a broken read.
	gauge(&b, "mc_census_scan_records",
		"Actor records read out of the world database.",
		nil, float64(c.Stats.Records))
	gauge(&b, "mc_census_scan_unusable_records",
		"Records that did not decode into a usable entity, for any reason.",
		nil, float64(c.Stats.Unusable()))

	return b.String()
}

// allDimensions is the render order shared with the report.
var allDimensions = []Dimension{Overworld, Nether, End, UnknownDimension, UnrecognisedDimension}

type label struct{ name, value string }

type labels []label

// gauge writes a whole single-sample family. head and sample are the same
// thing split apart, for families with more than one sample.
func gauge(b *strings.Builder, name, help string, l labels, value float64) {
	head(b, name, help)
	sample(b, name, l, value)
}

func head(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s gauge\n", name)
}

func sample(b *strings.Builder, name string, l labels, value float64) {
	b.WriteString(name)
	if len(l) > 0 {
		b.WriteString("{")
		for i, kv := range l {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(b, `%s="%s"`, kv.name, escapeLabelValue(kv.value))
		}
		b.WriteString("}")
	}
	// 'f' with -1 precision rather than %g or %f: every value here is a whole
	// number, %f pads six zeros onto each one, and %g renders a Unix
	// timestamp as 1.78933146e+09 -- which Prometheus accepts and a human
	// reading the payload cannot check against a clock.
	fmt.Fprintf(b, " %s\n", strconv.FormatFloat(value, 'f', -1, 64))
}

// escapeLabelValue escapes what the exposition format reserves. An entity
// identifier is normally alphanumeric, but it comes out of a world file
// rather than out of this codebase, and one unescaped quote makes Prometheus
// reject the entire scrape -- every gauge above it included.
//
// The replacement is applied here rather than left to %q because %q escapes
// Go's way, which is close enough to be mistaken for correct and different
// enough to matter for non-ASCII.
func escapeLabelValue(v string) string {
	var out strings.Builder
	for _, r := range v {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case '"':
			out.WriteString(`\"`)
		case '\n':
			out.WriteString(`\n`)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
