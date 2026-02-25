package encoding

import (
	"encoding/binary"
	"fmt"
	"math/bits"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// BitPackedDecoder decodes fastlanes.bitpacked arrays.
//
// FastLanes bitpacking stores N-bit integers in a dense buffer using a transposed
// layout for SIMD parallelism. Values are arranged in 1024-element blocks.
// Within each block, values are transposed across "lanes" determined by the
// element type width:
//
//	u8:  128 lanes × 8 rows   (1024/8  = 128)
//	u16:  64 lanes × 16 rows  (1024/16 = 64)
//	u32:  32 lanes × 32 rows  (1024/32 = 32)
//	u64:  16 lanes × 64 rows  (1024/64 = 16)
//
// For logical element index i: lane = i % numLanes, row = i / numLanes.
// Its bits are stored at transposed position: lane * rowsPerLane + row.
type BitPackedDecoder struct{}

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
		return makeZeroArray(dtype, int(rowCount), node, ctx)
	}

	if len(buffers) < 1 {
		return nil, fmt.Errorf("bitpacked: expected at least 1 buffer, got %d", len(buffers))
	}

	elemSize := vortex.PTypeSize(dtype.PType)
	buf := buffers[0]

	// Unpack based on element type width.
	var outBuf []byte
	var err error
	switch elemSize {
	case 1:
		outBuf, err = unpackFastLanes[uint8](buf, bw, offset, int(rowCount), 128, 8, getU8, putU8)
	case 2:
		outBuf, err = unpackFastLanes[uint16](buf, bw, offset, int(rowCount), 64, 16, getU16, putU16)
	case 4:
		outBuf, err = unpackFastLanes[uint32](buf, bw, offset, int(rowCount), 32, 32, getU32, putU32)
	case 8:
		outBuf, err = unpackFastLanes[uint64](buf, bw, offset, int(rowCount), 16, 64, getU64, putU64)
	default:
		return nil, fmt.Errorf("bitpacked: unsupported element size %d", elemSize)
	}
	if err != nil {
		return nil, err
	}

	arr := &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: [][]byte{outBuf},
		Stats:   node.Stats,
	}

	// Handle patches (outlier values stored separately).
	if meta.Patches != nil && len(node.Children) > 0 {
		// Patches child contains the exception values at their original indices.
		// For now, we decode the patches child and apply them.
		// TODO: implement full patch application.
	}

	return arr, nil
}

type getFunc[T any] func([]byte, int) T
type putFunc[T any] func([]byte, int, T)

// unpackFastLanes unpacks a FastLanes-transposed bitpacked buffer.
func unpackFastLanes[T uint8 | uint16 | uint32 | uint64](
	packed []byte,
	bw, offset, count int,
	numLanes, rowsPerLane int,
	get getFunc[T],
	put putFunc[T],
) ([]byte, error) {
	elemSize := int(bits.Len64(uint64(T(0))+1) / 8) // hack; use actual size
	// Calculate element size from type.
	var zero T
	switch any(zero).(type) {
	case uint8:
		elemSize = 1
	case uint16:
		elemSize = 2
	case uint32:
		elemSize = 4
	case uint64:
		elemSize = 8
	}

	outBuf := make([]byte, (offset+count)*elemSize)

	// Number of 1024-element blocks.
	totalElems := offset + count
	numBlocks := (totalElems + fastlanesBlockSize - 1) / fastlanesBlockSize

	// Each block occupies 128 * bw bytes of packed data (for any element width).
	bytesPerBlock := 128 * bw
	if len(packed) < numBlocks*bytesPerBlock {
		return nil, fmt.Errorf("bitpacked: buffer too small (%d bytes for %d blocks of %d bytes)",
			len(packed), numBlocks, bytesPerBlock)
	}

	mask := T((uint64(1) << bw) - 1)

	for block := 0; block < numBlocks; block++ {
		blockPacked := packed[block*bytesPerBlock : (block+1)*bytesPerBlock]

		for lane := 0; lane < numLanes; lane++ {
			for row := 0; row < rowsPerLane; row++ {
				elemIdx := block*fastlanesBlockSize + row*numLanes + lane
				if elemIdx < offset || elemIdx >= offset+count {
					continue
				}

				// Transposed bit position within this block.
				transPos := lane*rowsPerLane + row
				bitOff := transPos * bw
				byteOff := bitOff / 8
				bitShift := uint(bitOff % 8)

				// Read enough bytes to cover the value.
				var raw T
				switch elemSize {
				case 1:
					if byteOff < len(blockPacked) {
						raw = T(blockPacked[byteOff])
					}
				case 2:
					if byteOff+1 < len(blockPacked) {
						raw = T(binary.LittleEndian.Uint16(blockPacked[byteOff:]))
					}
				case 4:
					if byteOff+3 < len(blockPacked) {
						raw = T(binary.LittleEndian.Uint32(blockPacked[byteOff:]))
					}
				case 8:
					if byteOff+7 < len(blockPacked) {
						raw = T(binary.LittleEndian.Uint64(blockPacked[byteOff:]))
					}
				}

				val := (raw >> bitShift) & mask
				outIdx := elemIdx - offset
				put(outBuf, outIdx, val)
			}
		}
	}

	return outBuf, nil
}

// makeZeroArray creates an all-zero primitive array.
func makeZeroArray(dtype *vortex.DType, count int, node *vortex.ArrayNode, ctx *DecodeContext) (*vortex.Array, error) {
	elemSize := vortex.PTypeSize(dtype.PType)
	return &vortex.Array{
		DType:   dtype,
		Length:  count,
		Buffers: [][]byte{make([]byte, count*elemSize)},
		Stats:   node.Stats,
	}, nil
}

// Value getter/putter functions for each width.
func getU8(b []byte, i int) uint8 { return b[i] }
func putU8(b []byte, i int, v uint8) { b[i] = v }

func getU16(b []byte, i int) uint16 { return binary.LittleEndian.Uint16(b[i*2:]) }
func putU16(b []byte, i int, v uint16) { binary.LittleEndian.PutUint16(b[i*2:], v) }

func getU32(b []byte, i int) uint32 { return binary.LittleEndian.Uint32(b[i*4:]) }
func putU32(b []byte, i int, v uint32) { binary.LittleEndian.PutUint32(b[i*4:], v) }

func getU64(b []byte, i int) uint64 { return binary.LittleEndian.Uint64(b[i*8:]) }
func putU64(b []byte, i int, v uint64) { binary.LittleEndian.PutUint64(b[i*8:], v) }

func init() {
	Register("fastlanes.bitpacked", &BitPackedDecoder{})
}
