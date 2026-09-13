package census

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/df-mc/goleveldb/leveldb"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// fixtureActor is one entity to write into a throwaway world. Dimension nil
// writes an 8-byte digp key (overworld); non-nil writes the 12-byte form.
type fixtureActor struct {
	ID        uint64
	Dimension *int32
	NBT       map[string]any
}

// writeFixtureWorld builds a real LevelDB holding the given actors and
// returns its path. Building the world through the same encoding the game
// uses is what makes the scan test meaningful — a hand-rolled byte fixture
// would only prove the parser agrees with itself.
func writeFixtureWorld(t *testing.T, actors []fixtureActor) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatalf("open fixture world: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close fixture world: %v", err)
		}
	}()

	byChunk := map[int32][]byte{}
	for _, a := range actors {
		id := make([]byte, 8)
		binary.LittleEndian.PutUint64(id, a.ID)

		payload, err := nbt.MarshalEncoding(a.NBT, nbt.LittleEndian)
		if err != nil {
			t.Fatalf("marshal actor %d: %v", a.ID, err)
		}
		if err := db.Put(append([]byte("actorprefix"), id...), payload, nil); err != nil {
			t.Fatalf("put actor %d: %v", a.ID, err)
		}

		var dim int32
		if a.Dimension != nil {
			dim = *a.Dimension
		}
		byChunk[dim] = append(byChunk[dim], id...)
	}

	for dim, ids := range byChunk {
		key := append([]byte("digp"), make([]byte, 8)...)
		binary.LittleEndian.PutUint32(key[4:], uint32(dim)) // chunk X, arbitrary
		if dim != 0 {
			key = append(key, 0, 0, 0, 0)
			binary.LittleEndian.PutUint32(key[12:], uint32(dim))
		}
		if err := db.Put(key, ids, nil); err != nil {
			t.Fatalf("put digp for dimension %d: %v", dim, err)
		}
	}
	return path
}

func pos(x, y, z float32) []any { return []any{x, y, z} }
