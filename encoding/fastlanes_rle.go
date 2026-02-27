package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// FastLanesRLEDecoder decodes fastlanes.rle arrays.
//
// FastLanes RLE stores data as 1024-element chunks. Within each chunk, values
// are dictionary-encoded: an indices array maps each position to a value in the
// values array. Multiple chunks share a single values array, with per-chunk
// offsets stored in values_idx_offsets.
//
// Children: [0] values, [1] indices, [2] values_idx_offsets
// Metadata: RLEMetadata { values_len, indices_len, indices_ptype,
//
//	values_idx_offsets_len, values_idx_offsets_ptype, offset }
//
// For element i in chunk c:
//
//	global_val_idx = indices[c*1024 + i] + (offsets[c] - offsets[0])
//	output[i] = values[global_val_idx]
type FastLanesRLEDecoder struct{}

const flChunkSize = 1024

func (d *FastLanesRLEDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) < 3 {
		return nil, fmt.Errorf("fastlanes.rle: expected 3 children, got %d", len(node.Children))
	}

	var meta encodings.RLEMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("fastlanes.rle: parsing metadata: %w", err)
		}
	}

	indicesPType := vortex.PType(meta.IndicesPtype)
	offsetsPType := vortex.PType(meta.ValuesIdxOffsetsPtype)
	offset := int(meta.Offset)

	// Decode children.
	valuesArr, err := DecodeChild(node, 0, dtype, meta.ValuesLen, ctx)
	if err != nil {
		return nil, fmt.Errorf("fastlanes.rle: decoding values: %w", err)
	}

	indicesDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: indicesPType}
	indicesArr, err := DecodeChild(node, 1, indicesDType, meta.IndicesLen, ctx)
	if err != nil {
		return nil, fmt.Errorf("fastlanes.rle: decoding indices: %w", err)
	}

	offsetsDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: offsetsPType}
	offsetsArr, err := DecodeChild(node, 2, offsetsDType, meta.ValuesIdxOffsetsLen, ctx)
	if err != nil {
		return nil, fmt.Errorf("fastlanes.rle: decoding offsets: %w", err)
	}

	// Convert indices and offsets to uint64 slices.
	indices, err := toUint64Slice(indicesArr, indicesPType)
	if err != nil {
		return nil, fmt.Errorf("fastlanes.rle: converting indices: %w", err)
	}

	offsets, err := toUint64Slice(offsetsArr, offsetsPType)
	if err != nil {
		return nil, fmt.Errorf("fastlanes.rle: converting offsets: %w", err)
	}

	return expandRLE(valuesArr, indices, offsets, dtype, int(rowCount), offset, node)
}

// toUint64Slice converts a primitive array to []uint64.
func toUint64Slice(arr *vortex.Array, ptype vortex.PType) ([]uint64, error) {
	if len(arr.Buffers) == 0 {
		return nil, fmt.Errorf("array has no buffers")
	}
	buf := arr.Buffers[0]
	result := make([]uint64, arr.Length)

	switch ptype {
	case vortex.PTypeU8, vortex.PTypeI8:
		for i := 0; i < arr.Length; i++ {
			result[i] = uint64(buf[i])
		}
	case vortex.PTypeU16, vortex.PTypeI16:
		for i := 0; i < arr.Length; i++ {
			result[i] = uint64(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case vortex.PTypeU32, vortex.PTypeI32:
		for i := 0; i < arr.Length; i++ {
			result[i] = uint64(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case vortex.PTypeU64, vortex.PTypeI64:
		for i := 0; i < arr.Length; i++ {
			result[i] = binary.LittleEndian.Uint64(buf[i*8:])
		}
	default:
		return nil, fmt.Errorf("unsupported ptype %v", ptype)
	}
	return result, nil
}

// expandRLE expands FastLanes RLE-encoded data to a full array.
func expandRLE(valuesArr *vortex.Array, indices, offsets []uint64, dtype *vortex.DType, count, offset int, node *vortex.ArrayNode) (*vortex.Array, error) {
	elemSize := vortex.PTypeSize(dtype.PType)
	if elemSize == 0 {
		return nil, fmt.Errorf("fastlanes.rle: unsupported dtype %v for RLE expansion", dtype.Kind)
	}

	if len(valuesArr.Buffers) == 0 {
		return nil, fmt.Errorf("fastlanes.rle: values array has no buffers")
	}
	valBuf := valuesArr.Buffers[0]
	outBuf := make([]byte, count*elemSize)

	// The base offset to subtract from values_idx_offsets to make them relative.
	chunkStart := offset / flChunkSize
	baseOffset := offsets[0]

	for i := 0; i < count; i++ {
		globalIdx := offset + i
		chunkIdx := globalIdx/flChunkSize - chunkStart
		idxInChunk := globalIdx % flChunkSize

		// Get the chunk-relative index from the indices array.
		indicesPos := (chunkStart+chunkIdx)*flChunkSize + idxInChunk
		if indicesPos >= len(indices) {
			continue
		}
		localIdx := indices[indicesPos]

		// Add the chunk's offset into the values array.
		chunkValOffset := offsets[chunkIdx] - baseOffset
		globalValIdx := int(localIdx + chunkValOffset)

		// Copy the value.
		srcOff := globalValIdx * elemSize
		dstOff := i * elemSize
		if srcOff+elemSize <= len(valBuf) && dstOff+elemSize <= len(outBuf) {
			copy(outBuf[dstOff:dstOff+elemSize], valBuf[srcOff:srcOff+elemSize])
		}
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  count,
		Buffers: [][]byte{outBuf},
		Stats:   node.Stats,
	}, nil
}

func init() {
	Register("fastlanes.rle", &FastLanesRLEDecoder{})
}
