package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"github.com/LaurieRhodes/vortex-go/internal/proto/scalar"
	"google.golang.org/protobuf/proto"
)

// SequenceDecoder decodes vortex.sequence arrays.
// A Sequence array has no buffers and no children. It represents the formula:
//
//	A[i] = base + i * multiplier
//
// The base and multiplier are protobuf ScalarValues stored in the metadata.
type SequenceDecoder struct{}

func (d *SequenceDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return nil, fmt.Errorf("sequence: expected primitive dtype, got %v", dtype.Kind)
	}

	// Parse the protobuf metadata.
	var meta encodings.SequenceMetadata
	if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
		return nil, fmt.Errorf("sequence: parsing metadata: %w", err)
	}

	if meta.Base == nil || meta.Multiplier == nil {
		return nil, fmt.Errorf("sequence: metadata missing base or multiplier")
	}

	// Generate the values buffer.
	buf, err := generateSequence(meta.Base, meta.Multiplier, dtype.PType, int(rowCount))
	if err != nil {
		return nil, fmt.Errorf("sequence: generating values: %w", err)
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: [][]byte{buf},
		Stats:   node.Stats,
	}, nil
}

// generateSequence produces a raw byte buffer containing base + i * multiplier
// for i in 0..count, in the native format of the given PType.
func generateSequence(baseSV, mulSV *scalar.ScalarValue, ptype vortex.PType, count int) ([]byte, error) {
	elemSize := vortex.PTypeSize(ptype)
	buf := make([]byte, count*elemSize)

	switch ptype {
	case vortex.PTypeI32:
		base := scalarToInt64(baseSV)
		mul := scalarToInt64(mulSV)
		for i := 0; i < count; i++ {
			v := int32(base + int64(i)*mul)
			binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
		}
	case vortex.PTypeI64:
		base := scalarToInt64(baseSV)
		mul := scalarToInt64(mulSV)
		for i := 0; i < count; i++ {
			v := base + int64(i)*mul
			binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
		}
	case vortex.PTypeU32:
		base := scalarToUint64(baseSV)
		mul := scalarToUint64(mulSV)
		for i := 0; i < count; i++ {
			v := uint32(base + uint64(i)*mul)
			binary.LittleEndian.PutUint32(buf[i*4:], v)
		}
	case vortex.PTypeU64:
		base := scalarToUint64(baseSV)
		mul := scalarToUint64(mulSV)
		for i := 0; i < count; i++ {
			v := base + uint64(i)*mul
			binary.LittleEndian.PutUint64(buf[i*8:], v)
		}
	case vortex.PTypeI16:
		base := scalarToInt64(baseSV)
		mul := scalarToInt64(mulSV)
		for i := 0; i < count; i++ {
			v := int16(base + int64(i)*mul)
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
		}
	case vortex.PTypeI8:
		base := scalarToInt64(baseSV)
		mul := scalarToInt64(mulSV)
		for i := 0; i < count; i++ {
			buf[i] = byte(int8(base + int64(i)*mul))
		}
	case vortex.PTypeU16:
		base := scalarToUint64(baseSV)
		mul := scalarToUint64(mulSV)
		for i := 0; i < count; i++ {
			v := uint16(base + uint64(i)*mul)
			binary.LittleEndian.PutUint16(buf[i*2:], v)
		}
	case vortex.PTypeU8:
		base := scalarToUint64(baseSV)
		mul := scalarToUint64(mulSV)
		for i := 0; i < count; i++ {
			buf[i] = byte(base + uint64(i)*mul)
		}
	case vortex.PTypeF32:
		base := scalarToFloat64(baseSV)
		mul := scalarToFloat64(mulSV)
		for i := 0; i < count; i++ {
			v := float32(base + float64(i)*mul)
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
		}
	case vortex.PTypeF64:
		base := scalarToFloat64(baseSV)
		mul := scalarToFloat64(mulSV)
		for i := 0; i < count; i++ {
			v := base + float64(i)*mul
			binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
		}
	default:
		return nil, fmt.Errorf("unsupported PType for sequence: %v", ptype)
	}

	return buf, nil
}

func init() {
	Register("vortex.sequence", &SequenceDecoder{})
}
