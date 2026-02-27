package writer

// Zone map (vortex.stats layout) writing support.
//
// When EnableZoneMaps is set, the writer wraps each column's layout in a
// vortex.stats (Zoned) layout that stores per-zone min/max statistics.
// Each zone corresponds to one chunk, enabling chunk-level pruning during scans.
//
// The Zoned layout has:
//   - Metadata: 4-byte zone_len (LE uint32) + stat bitset
//   - Child[0]: data layout (Flat or Chunked → Flat children)
//   - Child[1]: stats table layout (Flat segment containing struct array)
//
// The stats table is a Struct{max, max_is_truncated, min, min_is_truncated}
// with one row per zone.

import (
	"encoding/binary"
	"math"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// zoneEntry holds per-zone min/max for one column.
type zoneEntry struct {
	minI64 int64
	maxI64 int64
	minF64 float64
	maxF64 float64
}

// zoneColTracker accumulates per-zone stats for one column.
type zoneColTracker struct {
	ptype vortex.PType
	zones []zoneEntry
}

// addZone records stats for a new zone (chunk).
func (t *zoneColTracker) addZone(arr *vortex.Array, dtype *vortex.DType) {
	if dtype.Kind != vortex.DTypePrimitive || len(arr.Buffers) == 0 || arr.Length == 0 {
		return
	}

	buf := arr.Buffers[0]
	ze := zoneEntry{}

	switch dtype.PType {
	case vortex.PTypeI32:
		ze.minI64, ze.maxI64 = math.MaxInt32, math.MinInt32
		for i := 0; i < arr.Length; i++ {
			v := int64(int32(binary.LittleEndian.Uint32(buf[i*4:])))
			if v < ze.minI64 {
				ze.minI64 = v
			}
			if v > ze.maxI64 {
				ze.maxI64 = v
			}
		}
	case vortex.PTypeI64:
		ze.minI64, ze.maxI64 = math.MaxInt64, math.MinInt64
		for i := 0; i < arr.Length; i++ {
			v := int64(binary.LittleEndian.Uint64(buf[i*8:]))
			if v < ze.minI64 {
				ze.minI64 = v
			}
			if v > ze.maxI64 {
				ze.maxI64 = v
			}
		}
	case vortex.PTypeF64:
		ze.minF64, ze.maxF64 = math.MaxFloat64, -math.MaxFloat64
		for i := 0; i < arr.Length; i++ {
			v := math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
			if v < ze.minF64 {
				ze.minF64 = v
			}
			if v > ze.maxF64 {
				ze.maxF64 = v
			}
		}
	default:
		return // unsupported ptype for zone maps
	}

	t.ptype = dtype.PType
	t.zones = append(t.zones, ze)
}

// hasZones returns true if at least one zone has been tracked.
func (t *zoneColTracker) hasZones() bool {
	return len(t.zones) > 0
}

// serializeStatsTable produces a Flat segment containing the zone stats struct.
// Returns the segment bytes, alignment exponent, and whether it succeeded.
//
// The stats table is: Struct{max: <ptype> nullable, max_is_truncated: bool,
//
//	min: <ptype> nullable, min_is_truncated: bool}
func (w *Writer) serializeStatsTable(tracker *zoneColTracker) ([]byte, uint8, error) {
	numZones := len(tracker.zones)

	// Build buffer data for each field:
	// Field 0: max values (primitive, same ptype)
	// Field 1: max_is_truncated (bool, all false)
	// Field 2: min values (primitive, same ptype)
	// Field 3: min_is_truncated (bool, all false)

	var maxBuf, minBuf []byte
	elemSize := 0

	switch tracker.ptype {
	case vortex.PTypeI32:
		elemSize = 4
		maxBuf = make([]byte, numZones*4)
		minBuf = make([]byte, numZones*4)
		for i, ze := range tracker.zones {
			binary.LittleEndian.PutUint32(maxBuf[i*4:], uint32(int32(ze.maxI64)))
			binary.LittleEndian.PutUint32(minBuf[i*4:], uint32(int32(ze.minI64)))
		}
	case vortex.PTypeI64:
		elemSize = 8
		maxBuf = make([]byte, numZones*8)
		minBuf = make([]byte, numZones*8)
		for i, ze := range tracker.zones {
			binary.LittleEndian.PutUint64(maxBuf[i*8:], uint64(ze.maxI64))
			binary.LittleEndian.PutUint64(minBuf[i*8:], uint64(ze.minI64))
		}
	case vortex.PTypeF64:
		elemSize = 8
		maxBuf = make([]byte, numZones*8)
		minBuf = make([]byte, numZones*8)
		for i, ze := range tracker.zones {
			binary.LittleEndian.PutUint64(maxBuf[i*8:], math.Float64bits(ze.maxF64))
			binary.LittleEndian.PutUint64(minBuf[i*8:], math.Float64bits(ze.minF64))
		}
	}
	_ = elemSize

	// Boolean buffers: all false (bit-packed, ceil(n/8) bytes, all zeros).
	boolBuf := make([]byte, (numZones+7)/8)

	// Validity bitmap for nullable primitive fields: all valid (all 1s).
	validityBuf := make([]byte, (numZones+7)/8)
	for i := range validityBuf {
		validityBuf[i] = 0xFF
	}

	// Collect all buffers in order and build the encoding tree.
	// The segment format is: [buffers padded] [Array FlatBuffer] [fb_len u32]
	//
	// Encoding tree:
	//   vortex.struct (no buffers)
	//     vortex.masked (validity for max)                [buf: validity]
	//       vortex.primitive                              [buf: max values]
	//     vortex.bool                                     [buf: max_is_truncated]
	//     vortex.masked (validity for min)                [buf: validity]
	//       vortex.primitive                              [buf: min values]
	//     vortex.bool                                     [buf: min_is_truncated]
	//
	// Wait — actually, looking at how the Rust reader handles nullable stats,
	// and how the Go reader's extractTypedValues works... let me check.

	// Simpler approach: the Rust writer uses Sequence and Constant encodings
	// which our reader handles. But the simplest valid encoding is just
	// primitive with a validity bitmap using the masked encoding.
	//
	// Actually, looking at the reader's readAndDecodeLayout → DecodeArray path,
	// the stats table needs to go through the full decode pipeline.
	// The simplest encoding that works: primitive for the values.
	// For nullable fields, we need the masked wrapper.
	//
	// But wait — let me check: does the reader actually check nullable on the
	// stats values, or does it just read the primitive data?

	// Looking at extractTypedValues in zonemap.go, it reads child.Buffers[0]
	// directly as primitive data. It doesn't go through nullable/masked decode.
	// So we can just emit primitive arrays without masked wrappers, even though
	// the dtype says nullable. The reader accesses the raw buffer directly.

	// Build encoding tree:
	//   vortex.struct
	//     vortex.primitive [buf: max values]
	//     vortex.bool [buf: max_is_truncated]
	//     vortex.primitive [buf: min values]
	//     vortex.bool [buf: min_is_truncated]

	primEncIdx := w.registerArraySpec("vortex.primitive")
	boolEncIdx := w.registerArraySpec("vortex.bool")
	structEncIdx := w.registerArraySpec("vortex.struct")

	// Buffers in order: [maxBuf, boolBuf(max_trunc), minBuf, boolBuf(min_trunc)]
	buffers := [][]byte{maxBuf, boolBuf, minBuf, boolBuf}

	// Build the arrayNodeInfo tree.
	root := &arrayNodeInfo{
		encodingIdx: structEncIdx,
		children: []*arrayNodeInfo{
			{encodingIdx: primEncIdx, bufferLengths: []uint32{uint32(len(maxBuf))}},
			{encodingIdx: boolEncIdx, bufferLengths: []uint32{uint32(len(boolBuf))}},
			{encodingIdx: primEncIdx, bufferLengths: []uint32{uint32(len(minBuf))}},
			{encodingIdx: boolEncIdx, bufferLengths: []uint32{uint32(len(boolBuf))}},
		},
	}

	// Calculate padded sizes (8-byte alignment for all buffers).
	var pos int
	paddings := make([]uint16, len(buffers))
	for i, bd := range buffers {
		pad := (8 - (pos % 8)) % 8
		paddings[i] = uint16(pad)
		pos += pad + len(bd)
	}
	fbPad := (8 - (pos % 8)) % 8
	pos += fbPad

	// Build the Array FlatBuffer.
	bufAligns := make([]uint8, len(buffers)) // all 3 (= 8-byte alignment)
	for i := range bufAligns {
		bufAligns[i] = 3
	}
	arrayFB := w.buildArrayFB(root, buffers, paddings, bufAligns)

	segSize := pos + len(arrayFB) + 4
	seg := make([]byte, 0, segSize)

	zeros := make([]byte, 8)
	for i, bd := range buffers {
		if paddings[i] > 0 {
			seg = append(seg, zeros[:paddings[i]]...)
		}
		seg = append(seg, bd...)
	}
	if fbPad > 0 {
		seg = append(seg, zeros[:fbPad]...)
	}
	seg = append(seg, arrayFB...)

	fbLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(fbLen, uint32(len(arrayFB)))
	seg = append(seg, fbLen...)

	return seg, 3, nil // alignment exponent 3 = 8 bytes
}

// buildZonedMeta builds the metadata bytes for a vortex.stats layout node.
// Format: 4 bytes zone_len (LE) + stat bitset (StatMax=3, StatMin=4 → byte 0 = 0x18).
func buildZonedMeta(zoneLen uint32) []byte {
	meta := make([]byte, 5)
	binary.LittleEndian.PutUint32(meta[0:4], zoneLen)
	meta[4] = (1 << 3) | (1 << 4) // StatMax=3, StatMin=4
	return meta
}
