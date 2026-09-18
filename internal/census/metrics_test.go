package census

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// scrapedAt is a fixed "now" so the age gauge is arithmetic rather than a
// race against the clock.
var scrapedAt = time.Date(2026, 9, 13, 21, 0, 0, 0, time.UTC)

// sample returns the metric's value, or "" when the sample is absent. The
// lookup is by the whole sample name including labels, because a metric that
// exports the right number under the wrong labels is wrong.
func sampleValue(t *testing.T, exposition, name string) string {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, " ")
		if found && key == name {
			return value
		}
	}
	return ""
}

func TestRenderMetricsCountsEntitiesPerDimension(t *testing.T) {
	out := RenderMetrics(sampleCensus(), scrapedAt, DefaultMetricsOptions())

	if got := sampleValue(t, out, `mc_census_entities{dimension="overworld"}`); got != "3" {
		t.Errorf("overworld entities = %q, want 3", got)
	}
	if got := sampleValue(t, out, `mc_census_entities{dimension="nether"}`); got != "1" {
		t.Errorf("nether entities = %q, want 1", got)
	}
}

// The report is the other half of this feature and the numbers have to agree:
// an export that disagrees with the printed report is worse than no export,
// because both look authoritative.
func TestRenderMetricsAgreesWithTheReport(t *testing.T) {
	c := sampleCensus()
	report := Render(c, DefaultReportOptions())
	out := RenderMetrics(c, scrapedAt, DefaultMetricsOptions())

	// Built with the report's own format rather than typed out, so this
	// guards the number rather than the column widths.
	if !strings.Contains(report, fmt.Sprintf("  %-10s %6d", Overworld, 3)) {
		t.Fatalf("report no longer prints the dimension total this compares against:\n%s", report)
	}
	if got := sampleValue(t, out, `mc_census_entities{dimension="overworld"}`); got != "3" {
		t.Errorf("metrics say %q overworld entities, report says 3", got)
	}
}

func TestRenderMetricsPublishesProvenance(t *testing.T) {
	out := RenderMetrics(sampleCensus(), scrapedAt, DefaultMetricsOptions())

	// 2026-09-13T20:31:00Z
	if got := sampleValue(t, out, "mc_census_world_taken_at_timestamp_seconds"); got != "1789331460" {
		t.Errorf("world taken-at = %q, want the sample census's 1789331460", got)
	}
	if got := sampleValue(t, out, "mc_census_scan_timestamp_seconds"); got != "1789333200" {
		t.Errorf("scan timestamp = %q, want the scrape time 1789333200", got)
	}
	// A reading taken from a day-old archive must not be readable as current,
	// and this is the gauge that says which it is.
	if got := sampleValue(t, out, "mc_census_world_from_snapshot"); got != "0" {
		t.Errorf("from_snapshot = %q, want 0 for an archive-sourced census", got)
	}
}

func TestRenderMetricsMarksASnapshotSource(t *testing.T) {
	c := sampleCensus()
	c.SourceKind = KindSnapshot
	out := RenderMetrics(c, scrapedAt, DefaultMetricsOptions())

	if got := sampleValue(t, out, "mc_census_world_from_snapshot"); got != "1" {
		t.Errorf("from_snapshot = %q, want 1 for a snapshot-sourced census", got)
	}
}

func TestRenderMetricsBoundsEntityTypeCardinality(t *testing.T) {
	var entities []Entity
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		entities = append(entities, Entity{Identifier: id, Dimension: Overworld})
	}
	c := Aggregate(entities, ScanStats{Records: 5, Decoded: 5}, scrapedAt, KindArchive)

	out := RenderMetrics(c, scrapedAt, MetricsOptions{TopTypes: 2})

	if n := strings.Count(out, "mc_census_entity_type{"); n != 2 {
		t.Errorf("exported %d entity type series, want the requested 2", n)
	}
}

// Per-region series are thousands of nightly-churning labels. The count of
// capped regions is the part that belongs in a time series.
func TestRenderMetricsExportsCappedRegionsWithoutPerRegionSeries(t *testing.T) {
	out := RenderMetrics(sampleCensus(), scrapedAt, DefaultMetricsOptions())

	if !strings.Contains(out, "mc_census_regions{") {
		t.Error("no graded-region gauge at all")
	}
	if strings.Contains(out, "region_x") || strings.Contains(out, "region_z") {
		t.Error("exported per-region coordinates, which is the cardinality this must not have")
	}
}

func TestRenderMetricsExportsScanQuality(t *testing.T) {
	c := Aggregate(nil, ScanStats{Records: 10, Decoded: 7, Unparsable: 2, Unplaced: 1}, scrapedAt, KindArchive)
	out := RenderMetrics(c, scrapedAt, DefaultMetricsOptions())

	if got := sampleValue(t, out, "mc_census_scan_records"); got != "10" {
		t.Errorf("scan records = %q, want 10", got)
	}
	if got := sampleValue(t, out, "mc_census_scan_unusable_records"); got != "3" {
		t.Errorf("unusable records = %q, want 3 (2 unparsable + 1 unplaced)", got)
	}
}

// A label value that ends the quoted string early makes Prometheus reject the
// whole scrape, taking every other gauge in the payload with it.
func TestRenderMetricsEscapesLabelValues(t *testing.T) {
	c := Aggregate([]Entity{
		{Identifier: `we"ird\one`, Dimension: Overworld},
	}, ScanStats{Records: 1, Decoded: 1}, scrapedAt, KindArchive)

	out := RenderMetrics(c, scrapedAt, DefaultMetricsOptions())

	if !strings.Contains(out, `identifier="we\"ird\\one"`) {
		t.Errorf("identifier was not escaped for the exposition format:\n%s", out)
	}
}

// Every metric family in the payload needs its HELP and TYPE, because these
// are read by whoever finds the panel a year from now with no access to this
// file.
func TestRenderMetricsDocumentsEveryFamily(t *testing.T) {
	out := RenderMetrics(sampleCensus(), scrapedAt, DefaultMetricsOptions())

	families := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, " ")
		name, _, _ = strings.Cut(name, "{")
		families[name] = true
	}
	if len(families) == 0 {
		t.Fatal("no samples in the payload at all")
	}
	for name := range families {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("%s has no HELP line", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s has no TYPE line", name)
		}
	}
}
