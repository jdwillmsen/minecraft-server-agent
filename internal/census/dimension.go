package census

import "encoding/binary"

// digpPrefix marks the per-chunk records that list which actors belong to a
// chunk. The key carries the chunk's coordinates, and its length is the only
// thing that says which dimension the chunk is in.
const digpPrefix = "digp"

// dimensionIndex maps an actor's 8-byte id to the dimension of the chunk
// that owns it. Actor records themselves carry no dimension.
type dimensionIndex map[[8]byte]Dimension

// addDigp records every actor listed in one digp value.
//
// The key suffix is 8 bytes (chunk X and Z) for the overworld, or 12 bytes
// with a trailing dimension int for the nether and the end. The value is a
// packed array of 8-byte actor ids. Malformed records are skipped rather
// than guessed at: a wrong dimension silently moves entities between cap
// tables.
func (ix dimensionIndex) addDigp(key, value []byte) {
	suffix := key[len(digpPrefix):]
	var dim Dimension
	switch len(suffix) {
	case 8:
		dim = Overworld
	case 12:
		dim = knownDimension(int32(binary.LittleEndian.Uint32(suffix[8:])))
	default:
		return
	}
	if len(value)%8 != 0 {
		return
	}
	for i := 0; i+8 <= len(value); i += 8 {
		var id [8]byte
		copy(id[:], value[i:i+8])
		ix[id] = dim
	}
}

// knownDimension keeps the index closed over the dimensions the report can
// render. Bedrock has three; anything else is either a dimension added
// after this code or a corrupt record, and storing it verbatim dropped
// those actors out of every per-dimension section without a word.
func knownDimension(raw int32) Dimension {
	switch d := Dimension(raw); d {
	case Overworld, Nether, End:
		return d
	default:
		return UnrecognisedDimension
	}
}

// lookup resolves an actor id. An actor whose chunk carried no digp record
// resolves to UnknownDimension rather than defaulting to the overworld.
func (ix dimensionIndex) lookup(actorID []byte) Dimension {
	if len(actorID) != 8 {
		return UnknownDimension
	}
	var id [8]byte
	copy(id[:], actorID)
	if d, ok := ix[id]; ok {
		return d
	}
	return UnknownDimension
}
