package census

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
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
	var b bytes.Buffer
	if err := writeExposition(&b, register(c, scrapedAt, opts)); err != nil {
		// Unreachable in practice: the encoder's only failure is the writer's,
		// and this one is a bytes.Buffer. Returned as a payload rather than
		// panicking because the caller writes this to a file a scraper reads,
		// and a comment is inert there where a panic would take the whole
		// census run down after it had already produced its report.
		return fmt.Sprintf("# census metrics could not be encoded: %v\n", err)
	}
	return b.String()
}

// WriteMetricsFile publishes the payload at path.
//
// client_golang writes through a uniquely-named temporary file in the same
// directory and renames it into place, so a scraper never reads a partial
// payload and two runs cannot collide on a staging name.
func WriteMetricsFile(path string, c Census, scrapedAt time.Time, opts MetricsOptions) error {
	if err := prometheus.WriteToTextfile(path, register(c, scrapedAt, opts)); err != nil {
		return fmt.Errorf("publish census metrics %s: %w", path, err)
	}
	return nil
}

// register builds the payload's registry.
//
// Built on client_golang rather than printed by hand: it owns the escaping,
// rejects a duplicate label set, and refuses an invalid metric name — three
// classes of mistake whose symptom is Prometheus dropping the entire scrape,
// every gauge here included, rather than the one bad line.
func register(c Census, scrapedAt time.Time, opts MetricsOptions) *prometheus.Registry {
	reg := prometheus.NewRegistry()

	gauge := func(name, help string, labels ...string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
		reg.MustRegister(g)
		return g
	}

	// Provenance first, because every number below it is only as current as
	// the world it was read from. A census taken from last night's archive is
	// a legitimate reading and a misleading one if its age is not published
	// beside it.
	takenAt := gauge("mc_census_world_taken_at_timestamp_seconds",
		"Unix time the world this census read was captured, which is not when the scan ran. 0 when the source carried no capture time.")
	// A zero time is the report's "unknown", and Unix() renders it as
	// -62135596800 -- a timestamp from year 1 that reads as a bug rather than
	// as a missing value. Zero is the epoch, which any age calculation already
	// renders as impossibly stale.
	if c.TakenAt.IsZero() {
		takenAt.WithLabelValues().Set(0)
	} else {
		takenAt.WithLabelValues().Set(float64(c.TakenAt.Unix()))
	}

	gauge("mc_census_scan_timestamp_seconds",
		"Unix time this census was taken.").
		WithLabelValues().Set(float64(scrapedAt.Unix()))

	fromSnapshot := 0.0
	if c.SourceKind == KindSnapshot {
		fromSnapshot = 1
	}
	gauge("mc_census_world_from_snapshot",
		"1 when the world was copied fresh from the running server, 0 when it came from a backup archive.").
		WithLabelValues().Set(fromSnapshot)

	byDimension := map[Dimension]int{}
	for _, t := range c.Totals {
		byDimension[t.Dimension] += t.Count
	}
	entities := gauge("mc_census_entities",
		"Entities stored in the world, by dimension. The world total is their sum.", "dimension")
	for _, d := range []Dimension{Overworld, Nether, End} {
		// Zero-filled for the three real dimensions, so an emptied nether
		// reports 0 rather than dropping its series -- an absent series and a
		// zero are different facts and only one of them would be true.
		entities.WithLabelValues(d.String()).Set(float64(byDimension[d]))
	}
	for _, d := range []Dimension{UnknownDimension, UnrecognisedDimension} {
		// The two diagnostic buckets are the opposite case: they mean the scan
		// could not place an entity, so a permanent zero would be a series
		// that exists only to say nothing is wrong.
		if n, ok := byDimension[d]; ok {
			entities.WithLabelValues(d.String()).Set(float64(n))
		}
	}

	// Totals arrive sorted by count, so the top N is a prefix. Labelled with
	// the dimension as well as the identifier because 4,000 items in the
	// nether and 4,000 in the overworld are different problems.
	entityType := gauge("mc_census_entity_type",
		"Entities of one type in one dimension, for the largest types only.",
		"identifier", "dimension", "category")
	for i, t := range c.Totals {
		if i >= opts.TopTypes {
			break
		}
		entityType.WithLabelValues(t.Identifier, t.Dimension.String(), t.Category.String()).
			Set(float64(t.Count))
	}

	// Counts of regions per grade, never the regions themselves: the world
	// holds thousands, their keys change every night, and the question this
	// answers is "how much of the world is at its cap", not "which cells".
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
	regions := gauge("mc_census_regions",
		"Population-control regions holding at least one entity of a capped category, graded against that category's cap.",
		"dimension", "category", "status")
	for _, k := range keys {
		// Every grade the grader can return, including the zeros and including
		// StatusUnknown: a status missing from this list would count into a
		// region total that no series carries, so the export would disagree
		// with the report by exactly the regions nobody thought about.
		for _, status := range []Status{Headroom, AtRisk, Capped, StatusUnknown} {
			regions.WithLabelValues(k.dimension.String(), k.category.String(), status.String()).
				Set(float64(graded[k][status]))
		}
	}

	// Only the three climate-bearing animals, zero-filled across the known
	// climates: twelve series whose label set does not move night to night.
	// A climate the game adds later is still exported, so the series sum to
	// the herd's total rather than silently dropping the newcomer.
	variant := gauge("mc_census_variant",
		"Animals of one identifier and climate variant, in every dimension. legacy is a record saved before variants existed.",
		"identifier", "climate")
	climates := map[string]map[string]int{}
	for _, id := range ClimateIdentifiers {
		climates[id] = map[string]int{}
		for _, climate := range Climates {
			climates[id][climate] = 0
		}
	}
	for _, g := range c.Variants {
		if byClimate, ok := climates[g.Identifier]; ok {
			byClimate[g.Variant] = g.Count
		}
	}
	for id, byClimate := range climates {
		for climate, n := range byClimate {
			variant.WithLabelValues(id, climate).Set(float64(n))
		}
	}

	persistent := 0
	for _, n := range c.PersistentByIdentifier {
		persistent += n
	}
	gauge("mc_census_persistent_entities",
		"Entities flagged as never despawning, each of which holds a cap slot permanently.").
		WithLabelValues().Set(float64(persistent))
	gauge("mc_census_named_entities",
		"Name-tagged entities. A name tag forces persistence in game, but this is counted from the name rather than from the persistence flag, so it is not a strict subset of the gauge above.").
		WithLabelValues().Set(float64(len(c.Named)))

	// The scan's own quality, because a census built from records that mostly
	// failed to decode renders as a quiet world rather than as a broken read.
	gauge("mc_census_scan_records",
		"Actor records read out of the world database.").
		WithLabelValues().Set(float64(c.Stats.Records))
	gauge("mc_census_scan_unusable_records",
		"Records that did not decode into a usable entity, for any reason.").
		WithLabelValues().Set(float64(c.Stats.Unusable()))

	return reg
}

func writeExposition(b *bytes.Buffer, reg *prometheus.Registry) error {
	families, err := reg.Gather()
	if err != nil {
		return err
	}
	enc := expfmt.NewEncoder(b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range families {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	return nil
}
