package census

import (
	"encoding/binary"
	"testing"

	"github.com/df-mc/goleveldb/leveldb"
)

func TestScanReadsEntitiesWithTheirDimensions(t *testing.T) {
	nether := int32(1)
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 1, NBT: map[string]any{
			"identifier": "minecraft:zombie",
			"Pos":        pos(10, 64, 20),
			"UniqueID":   int64(1),
		}},
		{ID: 2, Dimension: &nether, NBT: map[string]any{
			"identifier": "minecraft:piglin_brute",
			"Pos":        pos(-5, 40, -5),
			"UniqueID":   int64(2),
			"Persistent": uint8(1),
		}},
	})

	entities, stats, err := Scan(path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("got %d entities, want 2", len(entities))
	}
	if stats.Decoded != 2 || stats.Unparsable != 0 {
		t.Errorf("stats = %+v, want 2 decoded and 0 unparsable", stats)
	}

	byID := map[int64]Entity{}
	for _, e := range entities {
		byID[e.UniqueID] = e
	}
	if got := byID[1]; got.Identifier != "zombie" || got.Dimension != Overworld {
		t.Errorf("actor 1 = %+v, want zombie in the overworld", got)
	}
	if got := byID[2]; got.Identifier != "piglin_brute" || got.Dimension != Nether || !got.Persistent {
		t.Errorf("actor 2 = %+v, want a persistent piglin_brute in the nether", got)
	}
}

func TestScanCountsUnplacedEntities(t *testing.T) {
	// An actor with no Pos cannot be assigned to a region. It must be
	// counted, not silently dropped — a census that quietly loses entities
	// is worse than one that says it lost them.
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 3, NBT: map[string]any{"identifier": "minecraft:zombie", "UniqueID": int64(3)}},
	})
	entities, stats, err := Scan(path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("got %d entities, want 0", len(entities))
	}
	if stats.Unplaced != 1 {
		t.Errorf("Unplaced = %d, want 1", stats.Unplaced)
	}
	if stats.Records != 1 {
		t.Errorf("Records = %d, want 1", stats.Records)
	}
}

func TestScanRejectsAMissingWorld(t *testing.T) {
	if _, _, err := Scan(t.TempDir() + "/does-not-exist"); err == nil {
		t.Error("Scan of a missing world returned nil error")
	}
}

func TestScanCapturesFirstUnparsableError(t *testing.T) {
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 1, NBT: map[string]any{
			"identifier": "minecraft:zombie",
			"Pos":        pos(10, 64, 20),
			"UniqueID":   int64(1),
		}},
	})

	// Bytes that are not valid NBT at all stand in for a torn write during
	// a backup snapshot - the kind of corruption FirstUnparsableErr exists
	// to let an operator diagnose.
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	corruptID := make([]byte, 8)
	binary.LittleEndian.PutUint64(corruptID, 999)
	if err := db.Put(append([]byte("actorprefix"), corruptID...), []byte{0xff, 0xff, 0xff}, nil); err != nil {
		db.Close()
		t.Fatalf("put corrupt record: %v", err)
	}
	db.Close()

	_, stats, err := Scan(path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stats.Unparsable != 1 {
		t.Errorf("Unparsable = %d, want 1", stats.Unparsable)
	}
	if stats.FirstUnparsableErr == "" {
		t.Error("FirstUnparsableErr is empty, want error message")
	}
}

func TestScanStatsSeparatesTornRecordsFromAWholesaleDecodeFailure(t *testing.T) {
	// A backup is taken while the server runs, so a handful of torn records
	// is normal and must not fail the job. A Bedrock release that moves the
	// actor NBT layout takes every record with it. The two live orders of
	// magnitude apart, and only the second one invalidates the report.
	for _, tc := range []struct {
		name  string
		stats ScanStats
		want  bool
	}{
		{"empty world", ScanStats{}, false},
		{"nothing failed", ScanStats{Records: 1000, Decoded: 1000}, false},
		{"a few torn records", ScanStats{Records: 1000, Decoded: 950, Unparsable: 50}, false},
		{"past the limit", ScanStats{Records: 1000, Decoded: 949, Unparsable: 51}, true},
		{"the layout moved", ScanStats{Records: 412000, Unparsable: 412000}, true},
	} {
		if got := tc.stats.Unreadable(); got != tc.want {
			t.Errorf("%s: Unreadable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
