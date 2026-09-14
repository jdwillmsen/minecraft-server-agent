package census

import (
	"context"
	"encoding/binary"
	"errors"
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

	entities, stats, err := Scan(context.Background(), path)
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
	entities, stats, err := Scan(context.Background(), path)
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

func TestScanCountsRecordsThatNameNoEntity(t *testing.T) {
	// A record that decodes and places but carries no identifier cannot be
	// categorised, so it presses against no cap and appears in the report
	// as a blank line. Counting it is what lets the run refuse a world
	// whose identifier tag has moved.
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 4, NBT: map[string]any{"Pos": pos(1, 64, 2), "UniqueID": int64(4)}},
	})
	entities, stats, err := Scan(context.Background(), path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("got %d entities, want 0", len(entities))
	}
	if stats.Unidentified != 1 || stats.Decoded != 0 {
		t.Errorf("stats = %+v, want 1 unidentified and 0 decoded", stats)
	}
	if stats.Records != 1 {
		t.Errorf("Records = %d, want 1", stats.Records)
	}
}

func TestScanStatsAccountForEveryRecordItSaw(t *testing.T) {
	// Records is the denominator of the unreadable ratio, so every record
	// must land in exactly one of the outcomes that make up its numerator
	// or in Decoded. A record that lands in none is a failure mode the
	// threshold cannot see.
	nether := int32(1)
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 1, NBT: map[string]any{"identifier": "minecraft:zombie", "Pos": pos(1, 64, 2)}},
		{ID: 2, Dimension: &nether, NBT: map[string]any{"identifier": "minecraft:ghast"}},
		{ID: 3, NBT: map[string]any{"Pos": pos(3, 64, 4)}},
	})
	_, stats, err := Scan(context.Background(), path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := stats.Decoded + stats.Unusable(); got != stats.Records {
		t.Errorf("decoded %d plus unusable %d = %d, want records %d", stats.Decoded, stats.Unusable(), got, stats.Records)
	}
}

func TestScanRejectsAMissingWorld(t *testing.T) {
	if _, _, err := Scan(context.Background(), t.TempDir()+"/does-not-exist"); err == nil {
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

	_, stats, err := Scan(context.Background(), path)
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
		// NBT that decodes is not NBT the census can use: a renamed or
		// retyped Pos leaves every record decoding and none of them
		// placeable, and a moved identifier leaves every record placed
		// and nothing named. Both render an empty report.
		{"Pos moved", ScanStats{Records: 412000, Unplaced: 412000}, true},
		{"identifier moved", ScanStats{Records: 412000, Unidentified: 412000}, true},
		{"failures spread across reasons", ScanStats{Records: 1000, Decoded: 940, Unparsable: 20, Unplaced: 20, Unidentified: 20}, true},
	} {
		if got := tc.stats.Unreadable(); got != tc.want {
			t.Errorf("%s: Unreadable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestScanStopsOnACancelledContext(t *testing.T) {
	// The walk covers several hundred megabytes on the real world, and the
	// CronJob's pod can be evicted part way through it.
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 1, NBT: map[string]any{
			"identifier": "minecraft:zombie",
			"Pos":        pos(10, 64, 20),
			"UniqueID":   int64(1),
		}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Scan(ctx, path); !errors.Is(err, context.Canceled) {
		t.Errorf("Scan of a cancelled context returned %v, want context.Canceled", err)
	}
}
