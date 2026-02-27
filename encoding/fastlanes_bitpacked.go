package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// BitPackedDecoder decodes fastlanes.bitpacked arrays.
//
// FastLanes bitpacking stores N-bit integers in a dense buffer using a transposed
// layout designed for SIMD parallelism. The packed buffer is typed — it's an array
// of T-sized elements (where T matches the logical PType width) arranged across
// "lanes".
//
// For a 1024-element block with element type T (having tBits = sizeof(T)*8 bits):
//
//	nLanes = 1024 / tBits
//	rowsPerLane = tBits
//
// The spiraldb/fastlanes Rust crate uses a reordered transposition (not the
// original FastLanes paper order). The mapping from (row, lane) → logical index is:
//
//	FL_ORDER = [0, 4, 2, 6, 1, 5, 3, 7]
//	logIdx = FL_ORDER[row/8]*16 + (row%8)*128 + lane
//
// The inverse (logical index → row, lane) is:
//
//	lane = logIdx % nLanes
//	s = logIdx / 128
//	fl = (logIdx - s*128 - lane) / 16
//	o = FL_ORDER[fl]          // FL_ORDER is its own inverse
//	row = o*8 + s
//
// Then the physical packed position for a value at (row, lane) with bit_width W:
//
//	startBit = row * W
//	word = startBit / tBits
//	shift = startBit % tBits
//	physIdx = word * nLanes + lane
type BitPackedDecoder struct{}

// flOrder is the FastLanes transposition order used by the spiraldb/fastlanes Rust
// crate. It is its own inverse: flOrder[flOrder[i]] == i for all i in 0..8.
var flOrder = [8]int{0, 4, 2, 6, 1, 5, 3, 7}

const fastlanesBlockSize = 1024

func (d *BitPackedDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return nil, fmt.Errorf("bitpacked: expected primitive dtype, got %v", dtype.Kind)
	}

	// Parse metadata.
	var meta encodings.BitPackedMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("bitpacked: parsing metadata: %w", err)
		}
	}

	bw := int(meta.BitWidth)
	offset := int(meta.Offset)

	if bw == 0 {
		// bit_width 0 means all values are zero (or handled by patches).
		return makeZeroArray(dtype, int(rowCount), node)
	}

	if len(buffers) < 1 {
		return nil, fmt.Errorf("bitpacked: expected at least 1 buffer, got %d", len(buffers))
	}

	buf := buffers[0]
	elemSize := vortex.PTypeSize(dtype.PType)

	outBuf, err := unpackFastLanesTyped(buf, bw, offset, int(rowCount), elemSize)
	if err != nil {
		return nil, fmt.Errorf("bitpacked: %w", err)
	}

	arr := &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: [][]byte{outBuf},
		Stats:   node.Stats,
	}

	// Handle patches (outlier values stored separately).
	if meta.Patches != nil && len(node.Children) > 0 {
		// TODO: implement full patch application.
	}

	return arr, nil
}

// unpackFastLanesTyped unpacks a FastLanes-transposed bitpacked buffer using the
// typed-element algorithm. The packed buffer is treated as an array of T-sized
// elements (where T has width elemSize bytes), accessed at stride nLanes.
//
// The logical-to-physical mapping uses the FL_ORDER transposition from the
// spiraldb/fastlanes Rust crate.
func unpackFastLanesTyped(packed []byte, bw, offset, count, elemSize int) ([]byte, error) {
	tBits := elemSize * 8
	nLanes := fastlanesBlockSize / tBits
	outBuf := make([]byte, count*elemSize)

	totalElems := offset + count
	numBlocks := (totalElems + fastlanesBlockSize - 1) / fastlanesBlockSize

	// Each block: nLanes lanes × bw packed T-elements per lane.
	// Total T-elements per block = nLanes * bw.
	bytesPerBlock := nLanes * bw * elemSize

	if len(packed) < numBlocks*bytesPerBlock {
		return nil, fmt.Errorf("buffer too small (%d bytes, need %d for %d blocks)",
			len(packed), numBlocks*bytesPerBlock, numBlocks)
	}

	mask := uint64((1 << bw) - 1)

	for block := 0; block < numBlocks; block++ {
		blockStart := block * bytesPerBlock
		blockPacked := packed[blockStart : blockStart+bytesPerBlock]

		for logIdx := 0; logIdx < fastlanesBlockSize; logIdx++ {
			globalIdx := block*fastlanesBlockSize + logIdx
			if globalIdx < offset || globalIdx >= offset+count {
				continue
			}

			// Compute (row, lane) from the logical index using the FL_ORDER
			// transposition. This is the inverse of:
			//   index(row, lane) = flOrder[row/8]*16 + (row%8)*128 + lane
			lane := logIdx % nLanes
			s := logIdx / 128
			flVal := (logIdx - s*128 - lane) / 16
			o := flOrder[flVal]
			row := o*8 + s

			// Now use (row, lane) to locate the bits in the packed buffer.
			startBit := row * bw
			word := startBit / tBits
			shift := startBit % tBits

			// Physical byte position within the block.
			physByte := (word*nLanes + lane) * elemSize

			// Read the T-element at the physical position.
			raw := readTypedElement(blockPacked, physByte, elemSize)

			// Extract the value, handling the case where it spans two T-elements.
			var val uint64
			if shift+bw > tBits {
				// Value straddles two T-sized words. Extract bits from each
				// separately to avoid overflow when tBits == 64.
				nextPhysByte := ((word+1)*nLanes + lane) * elemSize
				next := readTypedElement(blockPacked, nextPhysByte, elemSize)
				lowerBits := raw >> uint(shift) // (tBits - shift) bits from first word
				upperMask := uint64((1 << uint(shift+bw-tBits)) - 1)
				val = lowerBits | ((next & upperMask) << uint(tBits-shift))
			} else {
				val = (raw >> uint(shift)) & mask
			}

			// Write to output.
			outIdx := globalIdx - offset
			writeTypedElement(outBuf, outIdx*elemSize, elemSize, val)
		}
	}

	return outBuf, nil
}

// readTypedElement reads a little-endian element of the given size from buf at offset.
func readTypedElement(buf []byte, offset, elemSize int) uint64 {
	if offset+elemSize > len(buf) {
		return 0
	}
	switch elemSize {
	case 1:
		return uint64(buf[offset])
	case 2:
		return uint64(binary.LittleEndian.Uint16(buf[offset:]))
	case 4:
		return uint64(binary.LittleEndian.Uint32(buf[offset:]))
	case 8:
		return binary.LittleEndian.Uint64(buf[offset:])
	default:
		return 0
	}
}

// writeTypedElement writes a little-endian element of the given size to buf at offset.
func writeTypedElement(buf []byte, offset, elemSize int, val uint64) {
	if offset+elemSize > len(buf) {
		return
	}
	switch elemSize {
	case 1:
		buf[offset] = byte(val)
	case 2:
		binary.LittleEndian.PutUint16(buf[offset:], uint16(val))
	case 4:
		binary.LittleEndian.PutUint32(buf[offset:], uint32(val))
	case 8:
		binary.LittleEndian.PutUint64(buf[offset:], val)
	}
}

// makeZeroArray creates an all-zero primitive array.
func makeZeroArray(dtype *vortex.DType, count int, node *vortex.ArrayNode) (*vortex.Array, error) {
	elemSize := vortex.PTypeSize(dtype.PType)
	return &vortex.Array{
		DType:   dtype,
		Length:  count,
		Buffers: [][]byte{make([]byte, count*elemSize)},
		Stats:   node.Stats,
	}, nil
}

func init() {
	Register("fastlanes.bitpacked", &BitPackedDecoder{})
}

// ExportUnpackFastLanesTyped is an exported wrapper around unpackFastLanesTyped
// for use in cross-package tests (e.g. writer pack→unpack round-trip verification).
func ExportUnpackFastLanesTyped(packed []byte, bw, offset, count, elemSize int) ([]byte, error) {
	return unpackFastLanesTyped(packed, bw, offset, count, elemSize)
}
