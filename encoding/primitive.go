package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// PrimitiveDecoder decodes vortex.primitive arrays.
// A Primitive array has one buffer containing raw typed values (little-endian).
type PrimitiveDecoder struct{}

func (d *PrimitiveDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(buffers) != 1 {
		return nil, fmt.Errorf("primitive: expected 1 buffer, got %d", len(buffers))
	}

	expectedBytes := rowCount * uint64(vortex.PTypeSize(dtype.PType))
	if uint64(len(buffers[0])) < expectedBytes {
		return nil, fmt.Errorf("primitive: buffer too small (%d bytes, need %d for %d rows of %s)",
			len(buffers[0]), expectedBytes, rowCount, dtype.PType)
	}

	arr := &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: buffers,
		Stats:   node.Stats,
	}

	// If the primitive has a child, it's the validity mask (Bool array).
	// This is how nullable primitives are stored: data buffer + optional bool child.
	if len(node.Children) > 0 {
		validityDType := &vortex.DType{Kind: vortex.DTypeBool}
		validity, err := DecodeChild(node, 0, validityDType, rowCount, ctx)
		if err != nil {
			return nil, fmt.Errorf("primitive: decoding validity child: %w", err)
		}
		arr.Children = []*vortex.Array{validity}
	}

	return arr, nil
}

// --- Typed value extractors ---

// Int32Values returns the i32 values from a Primitive array.
func Int32Values(arr *vortex.Array) ([]int32, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeI32 {
		return nil, fmt.Errorf("not an i32 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	if v := primView[int32](buf, arr.Length); v != nil {
		return v, nil
	}
	values := make([]int32, arr.Length)
	for i := range values {
		values[i] = int32(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return values, nil
}

// Uint32Values returns the u32 values from a Primitive array.
func Uint32Values(arr *vortex.Array) ([]uint32, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeU32 {
		return nil, fmt.Errorf("not a u32 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	if v := primView[uint32](buf, arr.Length); v != nil {
		return v, nil
	}
	values := make([]uint32, arr.Length)
	for i := range values {
		values[i] = binary.LittleEndian.Uint32(buf[i*4:])
	}
	return values, nil
}

// Int64Values returns the i64 values from a Primitive array.
func Int64Values(arr *vortex.Array) ([]int64, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeI64 {
		return nil, fmt.Errorf("not an i64 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	if v := primView[int64](buf, arr.Length); v != nil {
		return v, nil
	}
	values := make([]int64, arr.Length)
	for i := range values {
		values[i] = int64(binary.LittleEndian.Uint64(buf[i*8:]))
	}
	return values, nil
}

// Uint64Values returns the u64 values from a Primitive array.
func Uint64Values(arr *vortex.Array) ([]uint64, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeU64 {
		return nil, fmt.Errorf("not a u64 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	if v := primView[uint64](buf, arr.Length); v != nil {
		return v, nil
	}
	values := make([]uint64, arr.Length)
	for i := range values {
		values[i] = binary.LittleEndian.Uint64(buf[i*8:])
	}
	return values, nil
}

// Float32Values returns the f32 values from a Primitive array.
func Float32Values(arr *vortex.Array) ([]float32, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeF32 {
		return nil, fmt.Errorf("not an f32 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	values := make([]float32, arr.Length)
	for i := range values {
		values[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return values, nil
}

// Float64Values returns the f64 values from a Primitive array.
func Float64Values(arr *vortex.Array) ([]float64, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeF64 {
		return nil, fmt.Errorf("not an f64 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	if v := primView[float64](buf, arr.Length); v != nil {
		return v, nil
	}
	values := make([]float64, arr.Length)
	for i := range values {
		values[i] = math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
	}
	return values, nil
}

// Uint8Values returns the u8 values from a Primitive array.
func Uint8Values(arr *vortex.Array) ([]uint8, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeU8 {
		return nil, fmt.Errorf("not a u8 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	values := make([]uint8, arr.Length)
	copy(values, buf[:arr.Length])
	return values, nil
}

func init() {
	Register("vortex.primitive", &PrimitiveDecoder{})
}

// Uint16Values returns the u16 values from a Primitive array.
func Uint16Values(arr *vortex.Array) ([]uint16, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || arr.DType.PType != vortex.PTypeU16 {
		return nil, fmt.Errorf("not a u16 primitive array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	values := make([]uint16, arr.Length)
	for i := range values {
		values[i] = binary.LittleEndian.Uint16(buf[i*2:])
	}
	return values, nil
}
