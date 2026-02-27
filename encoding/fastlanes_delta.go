package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// DeltaDecoder decodes fastlanes.delta arrays.
//
// Delta encoding stores values as prefix-sums over a FastLanes-transposed layout.
// Each 1024-element chunk has LANES base values; the deltas are stored in transposed
// FL_ORDER. Decompression applies a per-lane prefix-sum (undelta) in transposed order,
// then untransposes to recover the original logical order.
//
// For remainders (< 1024 elements), scalar prefix-sum is used.
//
// Metadata: DeltaMetadata { deltas_len: u64, offset: u32 }
// Children: [bases, deltas] — both same unsigned integer ptype
type DeltaDecoder struct{}

func (d *DeltaDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return nil, fmt.Errorf("delta: expected primitive dtype, got %v", dtype.Kind)
	}
	if len(node.Children) < 2 {
		return nil, fmt.Errorf("delta: expected 2 children (bases, deltas), got %d", len(node.Children))
	}

	// Parse metadata.
	var meta encodings.DeltaMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("delta: parsing metadata: %w", err)
		}
	}
	deltasLen := int(meta.DeltasLen)
	offset := int(meta.Offset)

	elemSize := vortex.PTypeSize(dtype.PType)
	nLanes := deltaLanes(elemSize)

	// Compute expected bases length.
	numChunks := deltasLen / 1024
	remainderBaseSz := 0
	if deltasLen%1024 > 0 {
		remainderBaseSz = 1
	}
	basesLen := numChunks*nLanes + remainderBaseSz

	// Decode children.
	basesArr, err := DecodeChild(node, 0, dtype, uint64(basesLen), ctx)
	if err != nil {
		return nil, fmt.Errorf("delta: decoding bases: %w", err)
	}
	deltasArr, err := DecodeChild(node, 1, dtype, uint64(deltasLen), ctx)
	if err != nil {
		return nil, fmt.Errorf("delta: decoding deltas: %w", err)
	}

	if len(basesArr.Buffers) == 0 || len(deltasArr.Buffers) == 0 {
		return &vortex.Array{
			DType:  dtype,
			Length: int(rowCount),
			Stats:  node.Stats,
		}, nil
	}

	basesBuf := basesArr.Buffers[0]
	deltasBuf := deltasArr.Buffers[0]

	// Allocate output.
	outBuf := make([]byte, deltasLen*elemSize)

	// Process full 1024-element chunks.
	for c := 0; c < numChunks; c++ {
		basesOff := c * nLanes * elemSize
		deltasOff := c * 1024 * elemSize
		outOff := c * 1024 * elemSize
		deltaUndeltaUntranspose(
			basesBuf[basesOff:],
			deltasBuf[deltasOff:],
			outBuf[outOff:],
			nLanes, elemSize,
		)
	}

	// Process remainder with scalar prefix-sum.
	remainder := deltasLen % 1024
	if remainder > 0 {
		remBaseOff := numChunks * nLanes * elemSize
		remDeltaOff := numChunks * 1024 * elemSize
		remOutOff := numChunks * 1024 * elemSize
		deltaScalarPrefixSum(
			basesBuf[remBaseOff:],
			deltasBuf[remDeltaOff:],
			outBuf[remOutOff:],
			remainder, elemSize,
		)
	}

	// Apply offset/length slicing.
	startByte := offset * elemSize
	endByte := (offset + int(rowCount)) * elemSize
	slicedBuf := outBuf[startByte:endByte]

	return &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: [][]byte{slicedBuf},
		Stats:   node.Stats,
	}, nil
}

// deltaLanes returns the number of SIMD lanes for a given element size.
func deltaLanes(elemSize int) int {
	return 1024 / (elemSize * 8)
}

// deltaUndeltaUntranspose applies undelta (prefix-sum per lane in transposed order)
// and then untransposes a single 1024-element chunk.
func deltaUndeltaUntranspose(bases, deltas, output []byte, nLanes, elemSize int) {
	tBits := elemSize * 8 // number of rows per lane

	// Step 1: undelta in transposed order — prefix-sum within each lane.
	// The transposed buffer has values in FL_ORDER layout.
	// We produce a transposed output that has the cumulative sums.
	transposed := make([]byte, 1024*elemSize)

	mask := elemMask(elemSize)

	for lane := 0; lane < nLanes; lane++ {
		prev := readElem(bases, lane*elemSize, elemSize)

		for row := 0; row < tBits; row++ {
			// FL_ORDER iteration: same as the iterate! macro
			idx := deltaTransposedIndex(row, lane)
			delta := readElem(deltas, idx*elemSize, elemSize)
			val := (prev + delta) & mask // wrapping add at element width
			writeElem(transposed, idx*elemSize, elemSize, val)
			prev = val
		}
	}

	// Step 2: untranspose — map from transposed to logical order.
	for i := 0; i < 1024; i++ {
		ti := deltaTransposeIdx(i)
		copy(output[ti*elemSize:(ti+1)*elemSize], transposed[i*elemSize:(i+1)*elemSize])
	}
}

// deltaTransposedIndex computes the transposed index for a given (row, lane) pair.
// This matches the iterate! macro's index function:
//
//	index(row, lane) = FL_ORDER[row/8]*16 + (row%8)*128 + lane
func deltaTransposedIndex(row, lane int) int {
	o := row / 8
	s := row % 8
	return flOrder[o]*16 + s*128 + lane
}

// deltaTransposeIdx is the transpose permutation used by untranspose.
// Maps from sequential index to transposed index:
//
//	transpose(idx) = (idx%16)*64 + FL_ORDER[(idx/16)%8]*8 + idx/128
//
// Note: 16 = hardcoded for u64 lanes in the transpose crate, but the function
// is actually type-independent — it operates on the 1024-element permutation.
func deltaTransposeIdx(idx int) int {
	lane := idx % 16
	order := (idx / 16) % 8
	row := idx / 128
	return lane*64 + flOrder[order]*8 + row
}

// deltaScalarPrefixSum decodes a remainder chunk (< 1024 elements) using
// scalar prefix-sum from a single base value.
func deltaScalarPrefixSum(bases, deltas, output []byte, count, elemSize int) {
	mask := elemMask(elemSize)
	prev := readElem(bases, 0, elemSize)
	for i := 0; i < count; i++ {
		delta := readElem(deltas, i*elemSize, elemSize)
		val := (prev + delta) & mask // wrapping add at element width
		writeElem(output, i*elemSize, elemSize, val)
		prev = val
	}
}

// elemMask returns a bitmask for the given element size (wrapping mask).
func elemMask(elemSize int) uint64 {
	switch elemSize {
	case 1:
		return 0xFF
	case 2:
		return 0xFFFF
	case 4:
		return 0xFFFFFFFF
	case 8:
		return 0xFFFFFFFFFFFFFFFF
	default:
		return 0xFFFFFFFFFFFFFFFF
	}
}

// readElem reads an unsigned integer of the given byte width from buf at byte offset.
func readElem(buf []byte, off, elemSize int) uint64 {
	switch elemSize {
	case 1:
		return uint64(buf[off])
	case 2:
		return uint64(binary.LittleEndian.Uint16(buf[off:]))
	case 4:
		return uint64(binary.LittleEndian.Uint32(buf[off:]))
	case 8:
		return binary.LittleEndian.Uint64(buf[off:])
	default:
		return 0
	}
}

// writeElem writes an unsigned integer of the given byte width to buf at byte offset.
func writeElem(buf []byte, off, elemSize int, val uint64) {
	switch elemSize {
	case 1:
		buf[off] = byte(val)
	case 2:
		binary.LittleEndian.PutUint16(buf[off:], uint16(val))
	case 4:
		binary.LittleEndian.PutUint32(buf[off:], uint32(val))
	case 8:
		binary.LittleEndian.PutUint64(buf[off:], uint64(val))
	}
}

func init() {
	Register("fastlanes.delta", &DeltaDecoder{})
}
