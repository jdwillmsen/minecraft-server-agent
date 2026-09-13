package census

import (
	"strings"
	"testing"
	"time"
)

func sampleCensus() Census {
	return Aggregate([]Entity{
		{Identifier: "zombie", Dimension: Overworld, X: 0, Y: 64, Z: 0},
		{Identifier: "zombie", Dimension: Overworld, X: 1, Y: 64, Z: 0},
		{Identifier: "piglin_brute", Dimension: Nether, X: 600, Y: 40, Z: 10, Persistent: true},
		{Identifier: "cat", Dimension: Overworld, X: 173, Y: 66, Z: 263, CustomName: "OJ", Persistent: true},
	}, ScanStats{Records: 4, Decoded: 4}, time.Date(2026, 9, 13, 20, 31, 0, 0, time.UTC), "archive")
}

func TestRenderStatesProvenanceProminently(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	if !strings.Contains(out, "2026-09-13T20:31:00Z") {
		t.Error("report does not state the world timestamp it was taken from")
	}
	if !strings.Contains(out, "archive") {
		t.Error("report does not state its source kind")
	}
}

func TestRenderShowsTotalsPerDimension(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	for _, want := range []string{"overworld", "nether", "zombie", "piglin_brute"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

func TestRenderNamesTaggedMobsAndTheirCost(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	if !strings.Contains(out, "OJ") || !strings.Contains(out, "cat") {
		t.Error("report does not list the name-tagged cat")
	}
}

func TestRenderReportsUnreadableRecordsRatherThanHidingThem(t *testing.T) {
	c := sampleCensus()
	c.Stats.Unparsable = 3
	c.Stats.Unplaced = 2
	out := Render(c, DefaultReportOptions())
	if !strings.Contains(out, "3") || !strings.Contains(out, "unparsable") {
		t.Error("report does not surface unparsable records")
	}
	if !strings.Contains(out, "unplaced") {
		t.Error("report does not surface unplaced records")
	}
}

func TestRenderHonoursTopLimits(t *testing.T) {
	var entities []Entity
	for i := 0; i < 40; i++ {
		entities = append(entities, Entity{
			Identifier: "zombie", Dimension: Overworld,
			X: float64(i * RegionSize), Z: 0,
		})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0).UTC(), "archive")
	out := Render(c, ReportOptions{TopRegions: 5, TopTypes: 5})
	if got := strings.Count(out, "x "); got > 6 {
		t.Errorf("report rendered %d region rows, want at most 5 plus a header", got)
	}
}

func TestRenderLocatesConcentrations(t *testing.T) {
	var entities []Entity
	for i := 0; i < 40; i++ {
		entities = append(entities, Entity{
			Identifier: "item", Dimension: Nether,
			X: -24, Y: 14, Z: 1320,
		})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0).UTC(), "archive")
	out := Render(c, DefaultReportOptions())
	if !strings.Contains(out, "concentrations") {
		t.Fatal("report has no concentrations section")
	}
	for _, want := range []string{"item", "z=1320"} {
		if !strings.Contains(out, want) {
			t.Errorf("concentration section is missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderAnEmptyCensusDoesNotPanic(t *testing.T) {
	out := Render(Census{PersistentByIdentifier: map[string]int{}}, DefaultReportOptions())
	if out == "" {
		t.Error("Render returned an empty string for an empty census")
	}
}
