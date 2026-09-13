package census

import (
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

// Scan reads every entity out of a Bedrock world's LevelDB.
//
// The database is opened read-only: the census must never be able to modify
// a world, and the archive it usually reads is the only copy of that day's
// backup.
func Scan(dbPath string) ([]Entity, ScanStats, error) {
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
	if err := iterate(db, []byte(digpPrefix), func(k, v []byte) {
		index.addDigp(k, v)
	}); err != nil {
		return nil, ScanStats{}, err
	}

	var (
		entities []Entity
		stats    ScanStats
	)
	if err := iterate(db, []byte(actorPrefix), func(k, v []byte) {
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
func iterate(db *leveldb.DB, prefix []byte, fn func(k, v []byte)) error {
	it := db.NewIterator(util.BytesPrefix(prefix), nil)
	defer it.Release()
	for it.Next() {
		fn(it.Key(), it.Value())
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate %s: %w", prefix, err)
	}
	return nil
}
