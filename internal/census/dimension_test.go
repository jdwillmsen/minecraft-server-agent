package census

import (
	"encoding/binary"
	"testing"
)

func digpKey(chunkX, chunkZ int32, dim *int32) []byte {
	k := append([]byte("digp"), make([]byte, 8)...)
	binary.LittleEndian.PutUint32(k[4:], uint32(chunkX))
	binary.LittleEndian.PutUint32(k[8:], uint32(chunkZ))
	if dim != nil {
		k = append(k, 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(k[12:], uint32(*dim))
	}
	return k
}

func actorID(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

func TestAddDigpTreatsAnEightByteSuffixAsOverworld(t *testing.T) {
	ix := dimensionIndex{}
	ix.addDigp(digpKey(1, 2, nil), actorID(7))
	if got := ix.lookup(actorID(7)); got != Overworld {
		t.Errorf("lookup = %v, want overworld", got)
	}
}

func TestAddDigpReadsTheTrailingDimensionOnATwelveByteSuffix(t *testing.T) {
	nether := int32(1)
	end := int32(2)
	ix := dimensionIndex{}
	ix.addDigp(digpKey(1, 2, &nether), actorID(8))
	ix.addDigp(digpKey(3, 4, &end), actorID(9))
	if got := ix.lookup(actorID(8)); got != Nether {
		t.Errorf("nether lookup = %v, want nether", got)
	}
	if got := ix.lookup(actorID(9)); got != End {
		t.Errorf("end lookup = %v, want end", got)
	}
}

func TestAddDigpSplitsAValueHoldingSeveralActors(t *testing.T) {
	ix := dimensionIndex{}
	value := append(actorID(11), actorID(12)...)
	ix.addDigp(digpKey(0, 0, nil), value)
	for _, id := range []uint64{11, 12} {
		if got := ix.lookup(actorID(id)); got != Overworld {
			t.Errorf("lookup(%d) = %v, want overworld", id, got)
		}
	}
}

func TestLookupOfAnUnknownActorIsUnknown(t *testing.T) {
	if got := (dimensionIndex{}).lookup(actorID(99)); got != UnknownDimension {
		t.Errorf("lookup = %v, want unknown", got)
	}
}

func TestAddDigpIgnoresMalformedKeysAndValues(t *testing.T) {
	ix := dimensionIndex{}
	ix.addDigp([]byte("digp"), actorID(1))          // no chunk coords
	ix.addDigp(digpKey(0, 0, nil), []byte{1, 2, 3}) // value not a multiple of 8
	if len(ix) != 0 {
		t.Errorf("index has %d entries after malformed input, want 0", len(ix))
	}
}

func TestAddDigpFoldsADimensionItDoesNotKnowIntoUnrecognised(t *testing.T) {
	// A dimension int outside 0..2 used to be stored verbatim, and the
	// report iterates a fixed list of dimensions - so those actors were
	// counted in "decoded" and then vanished from every section below it.
	for _, raw := range []int32{3, 7, -9, 1 << 20} {
		dim := raw
		ix := dimensionIndex{}
		ix.addDigp(digpKey(1, 2, &dim), actorID(5))
		if got := ix.lookup(actorID(5)); got != UnrecognisedDimension {
			t.Errorf("dimension %d resolved to %v, want unrecognised", raw, got)
		}
	}
}

func TestAnUnrecognisedDimensionIsNotAMissingDigpRecord(t *testing.T) {
	// -1 is UnknownDimension's own value, so a digp record naming it used
	// to be indistinguishable from an actor whose chunk carried no record
	// at all. They call for different work: one is a code gap, the other a
	// chunk the scan could not place.
	minusOne := int32(-1)
	ix := dimensionIndex{}
	ix.addDigp(digpKey(1, 2, &minusOne), actorID(6))
	if got := ix.lookup(actorID(6)); got != UnrecognisedDimension {
		t.Errorf("a digp record naming -1 resolved to %v, want unrecognised", got)
	}
	if got := ix.lookup(actorID(7)); got != UnknownDimension {
		t.Errorf("an actor with no digp record resolved to %v, want unknown", got)
	}
}
