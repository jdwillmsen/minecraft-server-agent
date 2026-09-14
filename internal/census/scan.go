package census

import (
	"context"
	"fmt"

	"github.com/df-mc/goleveldb/leveldb"
	"github.com/df-mc/goleveldb/leveldb/opt"
	"github.com/df-mc/goleveldb/leveldb/util"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// actorPrefix marks a saved entity. The 8 bytes after it are the actor id
// that digp records refer to.
const actorPrefix = "actorprefix"

// ScanStats records what the scan saw, so a census can state how much of the
// world it failed to read instead of quietly reporting a short count.
type ScanStats struct {
	Records            int    // actorprefix keys seen
	Decoded            int    // records that became entities
	Unparsable         int    // records whose NBT would not decode
	Unplaced           int    // records decoded but carrying no usable position
	FirstUnparsableErr string // first decode failure seen, or empty if none
}

// MaxUnparsableRatio is how much of a world may fail to decode before the
// records that did survive stop being a census.
//
// A backup is taken while the server runs, so a few torn records are normal
// and failing the nightly job over them would only teach operators to
// ignore it. A Bedrock release that moves the actor NBT layout takes every
// record with it. The two cases sit orders of magnitude apart, so the exact
// line matters far less than drawing one: 5% of a 400,000-record world is
// 20,000 records, far past torn-write noise and far short of a layout
// change.
const MaxUnparsableRatio = 0.05

// Unreadable reports whether so much of the world failed to decode that the
// rest cannot be reported as a census. A world with no actor records at all
// is not unreadable: an empty world is a fact about the world, and a report
// saying so must stay distinguishable from one built out of nothing.
func (s ScanStats) Unreadable() bool {
	return s.Records > 0 && float64(s.Unparsable) > MaxUnparsableRatio*float64(s.Records)
}

// Scan reads every entity out of a Bedrock world's LevelDB.
//
// The database is opened read-only: the census must never be able to modify
// a world, and the archive it usually reads is the only copy of that day's
// backup.
//
// The walk covers several hundred megabytes, so it stops at the first
// cancellation rather than holding a terminating pod open to the end of it.
func Scan(ctx context.Context, dbPath string) ([]Entity, ScanStats, error) {
	db, err := leveldb.OpenFile(dbPath, &opt.Options{ReadOnly: true})
	if err != nil {
		return nil, ScanStats{}, fmt.Errorf("open world %s: %w", dbPath, err)
	}
	defer db.Close()

	// Two passes: digp records must all be read before any actor can be
	// placed, because an actor's dimension lives in the chunk record rather
	// than in the actor itself, and the iteration order gives no guarantee
	// that a chunk is seen before the actors it owns.
	index := dimensionIndex{}
	if err := iterate(ctx, db, []byte(digpPrefix), func(k, v []byte) {
		index.addDigp(k, v)
	}); err != nil {
		return nil, ScanStats{}, err
	}

	var (
		entities []Entity
		stats    ScanStats
	)
	if err := iterate(ctx, db, []byte(actorPrefix), func(k, v []byte) {
		stats.Records++
		var m map[string]any
		if err := nbt.UnmarshalEncoding(v, &m, nbt.LittleEndian); err != nil {
			stats.Unparsable++
			if stats.FirstUnparsableErr == "" {
				stats.FirstUnparsableErr = err.Error()
			}
			return
		}
		e, ok := EntityFromNBT(m)
		if !ok {
			stats.Unplaced++
			return
		}
		e.Dimension = index.lookup(k[len(actorPrefix):])
		stats.Decoded++
		entities = append(entities, e)
	}); err != nil {
		return nil, stats, err
	}
	return entities, stats, nil
}

// iterate seeks the prefix range and calls fn for each key in it. The callback
// must not retain k or v: the iterator reuses their backing arrays between steps.
func iterate(ctx context.Context, db *leveldb.DB, prefix []byte, fn func(k, v []byte)) error {
	it := db.NewIterator(util.BytesPrefix(prefix), nil)
	defer it.Release()
	for it.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("iterate %s: %w", prefix, err)
		}
		fn(it.Key(), it.Value())
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate %s: %w", prefix, err)
	}
	return nil
}
