package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"github.com/LaurieRhodes/vortex-go/internal/proto/scalar"
	"google.golang.org/protobuf/proto"
)

// SparseDecoder decodes vortex.sparse arrays.
//
// Sparse encoding stores a fill value (in a buffer) plus patches at specific
// indices that override the fill value.
// Children: [0] patch_indices (integer array), [1] patch_values (same dtype as column).
// Buffers: [0] fill value as protobuf ScalarValue.
// Metadata: SparseMetadata { patches: PatchesMetadata }
type SparseDecoder struct{}

func (d *SparseDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) < 2 {
		return nil, fmt.Errorf("sparse: expected 2 children, got %d", len(node.Children))
	}

	// Parse metadata.
	var meta encodings.SparseMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("sparse: parsing metadata: %w", err)
		}
	}

	if meta.Patches == nil {
		return nil, fmt.Errorf("sparse: missing patches metadata")
	}

	patchLen := meta.Patches.Len
	indicesPType := vortex.PType(meta.Patches.IndicesPtype)

	// Decode fill value from buffer.
	var fillSV scalar.ScalarValue
	if len(buffers) > 0 && len(buffers[0]) > 0 {
		if err := proto.Unmarshal(buffers[0], &fillSV); err != nil {
			return nil, fmt.Errorf("sparse: parsing fill value: %w", err)
		}
	}

	// Decode indices child.
	indicesDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: indicesPType}
	indicesArr, err := DecodeChild(node, 0, indicesDType, patchLen, ctx)
	if err != nil {
		return nil, fmt.Errorf("sparse: decoding indices: %w", err)
	}

	// Decode values child.
	valuesArr, err := DecodeChild(node, 1, dtype, patchLen, ctx)
	if err != nil {
		return nil, fmt.Errorf("sparse: decoding values: %w", err)
	}

	// Build the output array filled with the fill value.
	outArr, err := makeFillArray(dtype, int(rowCount), &fillSV)
	if err != nil {
		return nil, err
	}

	// Apply patches: set values at the specified indices.
	indices, err := indicesToUint64(indicesArr, indicesPType)
	if err != nil {
		return nil, err
	}

	if err := applyPatches(outArr, indices, valuesArr, dtype); err != nil {
		return nil, err
	}

	outArr.Stats = node.Stats
	return outArr, nil
}

// makeFillArray creates an array of count elements filled with the scalar value.
func makeFillArray(dtype *vortex.DType, count int, sv *scalar.ScalarValue) (*vortex.Array, error) {
	if dtype.Kind == vortex.DTypePrimitive {
		elemSize := vortex.PTypeSize(dtype.PType)
		buf := make([]byte, count*elemSize)

		// Write the fill value into a template, then copy.
		var fillBytes [8]byte
		switch dtype.PType {
		case vortex.PTypeI8, vortex.PTypeU8:
			fillBytes[0] = byte(scalarToInt64(sv))
		case vortex.PTypeI16, vortex.PTypeU16:
			binary.LittleEndian.PutUint16(fillBytes[:], uint16(scalarToInt64(sv)))
		case vortex.PTypeI32, vortex.PTypeU32:
			binary.LittleEndian.PutUint32(fillBytes[:], uint32(scalarToInt64(sv)))
		case vortex.PTypeI64, vortex.PTypeU64:
			binary.LittleEndian.PutUint64(fillBytes[:], uint64(scalarToInt64(sv)))
		case vortex.PTypeF32:
			binary.LittleEndian.PutUint32(fillBytes[:], uint32(scalarToInt64(sv)))
		case vortex.PTypeF64:
			binary.LittleEndian.PutUint64(fillBytes[:], uint64(scalarToInt64(sv)))
		}

		// Fill all elements.
		for i := 0; i < count; i++ {
			copy(buf[i*elemSize:], fillBytes[:elemSize])
		}

		return &vortex.Array{DType: dtype, Length: count, Buffers: [][]byte{buf}}, nil
	}

	// For null fill values or non-primitive types, just return zeros.
	if dtype.Kind == vortex.DTypeBool {
		buf := make([]byte, (count+7)/8)
		return &vortex.Array{DType: dtype, Length: count, Buffers: [][]byte{buf}}, nil
	}

	return nil, fmt.Errorf("sparse: unsupported fill dtype %v", dtype.Kind)
}

// indicesToUint64 converts an indices array to a []uint64 slice.
func indicesToUint64(arr *vortex.Array, ptype vortex.PType) ([]uint64, error) {
	if len(arr.Buffers) == 0 {
		return nil, nil
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
		return nil, fmt.Errorf("sparse: unsupported indices ptype %v", ptype)
	}
	return result, nil
}

// applyPatches overwrites elements in outArr at the given indices with values from valuesArr.
func applyPatches(outArr *vortex.Array, indices []uint64, valuesArr *vortex.Array, dtype *vortex.DType) error {
	if dtype.Kind != vortex.DTypePrimitive || len(outArr.Buffers) == 0 || len(valuesArr.Buffers) == 0 {
		return nil
	}

	elemSize := vortex.PTypeSize(dtype.PType)
	outBuf := outArr.Buffers[0]
	valBuf := valuesArr.Buffers[0]

	for i, idx := range indices {
		outOff := int(idx) * elemSize
		valOff := i * elemSize
		if outOff+elemSize <= len(outBuf) && valOff+elemSize <= len(valBuf) {
			copy(outBuf[outOff:outOff+elemSize], valBuf[valOff:valOff+elemSize])
		}
	}
	return nil
}

func init() {
	Register("vortex.sparse", &SparseDecoder{})
}
