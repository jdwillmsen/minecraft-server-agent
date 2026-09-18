package census

import (
	"strconv"
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
	c.Stats.Unidentified = 1
	out := Render(c, DefaultReportOptions())
	if !strings.Contains(out, "3") || !strings.Contains(out, "unparsable") {
		t.Error("report does not surface unparsable records")
	}
	if !strings.Contains(out, "unplaced") {
		t.Error("report does not surface unplaced records")
	}
	if !strings.Contains(out, "unidentified 1") {
		t.Errorf("report does not surface records that named no entity\n---\n%s", out)
	}
}

func TestRenderStatesTheFirstDecodeFailureWhenPresent(t *testing.T) {
	c := sampleCensus()
	c.Stats.Unparsable = 1
	c.Stats.FirstUnparsableErr = "unmarshal actorprefix 0102030405060708: unexpected EOF"
	out := Render(c, DefaultReportOptions())
	if !strings.Contains(out, "first decode failure: unmarshal actorprefix 0102030405060708: unexpected EOF") {
		t.Errorf("report does not state the first decode failure\n---\n%s", out)
	}
}

func TestRenderOmitsTheFirstDecodeFailureLineWhenThereIsNone(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	if strings.Contains(out, "first decode failure") {
		t.Errorf("report states a decode failure that never happened\n---\n%s", out)
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
	// Each region row contributes exactly one "x " (from "x %6d..."); the
	// dimension header line does not. 40 single-zombie regions exist, so
	// only TopRegions honouring the limit gives exactly 5.
	if got := strings.Count(out, "x "); got != 5 {
		t.Errorf("report rendered %d region rows, want exactly 5", got)
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

func TestRenderStatesOneNumberWhereOnlyOneEnvironmentSpawnsTheCategory(t *testing.T) {
	// Animals only spawn above ground, so the overworld's animal cap is a
	// single known number. Printing it as "4..4" reads like a bug and
	// suggests an ambiguity the save does not actually leave open.
	c := Aggregate([]Entity{
		{Identifier: "cow", Dimension: Overworld, X: 0, Y: 64, Z: 0},
	}, ScanStats{Records: 1, Decoded: 1}, time.Unix(0, 0).UTC(), "archive")

	out := Render(c, DefaultReportOptions())
	if strings.Contains(out, "4..4") {
		t.Errorf("report renders a range where the cap is exact\n---\n%s", out)
	}
	if !strings.Contains(out, "animal") || !strings.Contains(out, "1 / 4") {
		t.Errorf("report does not grade the cow against the animal cap of 4\n---\n%s", out)
	}
}

func TestRenderAccountsForEveryDecodedEntityByDimension(t *testing.T) {
	// The dimension breakdown iterates a fixed list. Any dimension missing
	// from it makes the section sum to less than the decoded count printed
	// two lines above, with nothing to say entities went missing.
	entities := []Entity{
		{Identifier: "zombie", Dimension: Overworld, X: 0, Y: 64, Z: 0},
		{Identifier: "zombie", Dimension: Nether, X: 0, Y: 64, Z: 0},
		{Identifier: "zombie", Dimension: End, X: 0, Y: 64, Z: 0},
		{Identifier: "zombie", Dimension: UnknownDimension, X: 0, Y: 64, Z: 0},
		{Identifier: "zombie", Dimension: UnrecognisedDimension, X: 0, Y: 64, Z: 0},
	}
	c := Aggregate(entities, ScanStats{Records: 5, Decoded: 5}, time.Unix(0, 0).UTC(), "archive")

	out := Render(c, DefaultReportOptions())
	section, _, ok := strings.Cut(out[strings.Index(out, "entities by dimension\n"):], "\n\n")
	if !ok {
		t.Fatalf("report has no dimension section\n---\n%s", out)
	}
	sum := 0
	for _, line := range strings.Split(section, "\n")[1:] {
		fields := strings.Fields(line)
		n, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("dimension line %q does not end in a count: %v", line, err)
		}
		sum += n
	}
	if sum != len(entities) {
		t.Errorf("dimension breakdown sums to %d, want %d\n---\n%s", sum, len(entities), out)
	}
}

func TestCapBoundsPrintsNoCeilingRatherThanMinusOne(t *testing.T) {
	// Caps is exported and NoSpawn is -1, so a cell nothing spawns in must
	// not render as a ceiling of "-1" in the one column an operator reads
	// the count against.
	if got := capBounds(Caps{Surface: NoSpawn, Cave: NoSpawn}); got != "none" {
		t.Errorf("capBounds(no spawnable environment) = %q, want %q", got, "none")
	}
}

func TestRenderGivesConcentrationExtentNotJustItsCentre(t *testing.T) {
	// A transitively chained cluster can be a trail rather than a pile, and
	// its centre can sit where no entity is. Reported live on 2026-09-18: an
	// operator flew to the printed coordinate of a 4,147-item "pile" twice
	// and found nothing, because the items were strung along ~970 blocks of
	// tunnel and the centre was empty corridor.
	var trail []Entity
	for i := 0; i < 30; i++ {
		trail = append(trail, Entity{
			Identifier: "item", Dimension: Nether,
			X: -24, Y: 14, Z: float64(i * 20),
		})
	}
	c := Aggregate(trail, ScanStats{Records: 30, Decoded: 30}, time.Time{}, "archive")
	out := Render(c, DefaultReportOptions())

	if !strings.Contains(out, "z=0..580") {
		t.Errorf("report does not give the cluster's z extent\n---\n%s", out)
	}
	if !strings.Contains(out, "centre") {
		t.Errorf("report does not label the centre it prints\n---\n%s", out)
	}
}

func TestRenderSurfacesUnresolvedDimensionsAndSkippedDigp(t *testing.T) {
	// An entity whose dimension will not resolve is still a real entity: it
	// was killed in game on 2026-09-18 after the report filed it as unknown.
	// Reporting the count, and why the chunk records were skipped, is what
	// separates "the census cannot place these" from "these do not exist".
	c := sampleCensus()
	c.Stats.UnresolvedDimension = 1897
	c.Stats.DigpSkippedValue = 12
	c.Stats.DigpSkippedKey = 3
	out := Render(c, DefaultReportOptions())

	if !strings.Contains(out, "1897") || !strings.Contains(out, "unresolved") {
		t.Errorf("report does not state how many entities it could not place\n---\n%s", out)
	}
	if !strings.Contains(out, "12") || !strings.Contains(out, "skipped") {
		t.Errorf("report does not state why chunk records were skipped\n---\n%s", out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("report does not state the bad-key skips\n---\n%s", out)
	}
}
