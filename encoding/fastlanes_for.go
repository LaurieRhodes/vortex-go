package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/scalar"
	"google.golang.org/protobuf/proto"
)

// FoRDecoder decodes fastlanes.for (Frame-of-Reference) arrays.
//
// FoR encoding stores values as offsets from a reference (minimum) value.
// The metadata contains the reference value as a protobuf ScalarValue.
// The single child is the encoded offsets (typically bitpacked).
//
// Decoding: decoded[i] = child[i] + reference
type FoRDecoder struct{}

func (d *FoRDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return nil, fmt.Errorf("for: expected primitive dtype, got %v", dtype.Kind)
	}
	if len(node.Children) < 1 {
		return nil, fmt.Errorf("for: expected at least 1 child, got %d", len(node.Children))
	}

	// Parse the reference value from metadata (protobuf ScalarValue).
	var sv scalar.ScalarValue
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &sv); err != nil {
			return nil, fmt.Errorf("for: parsing reference scalar: %w", err)
		}
	}

	// Decode the child (offsets from reference).
	child, err := DecodeChild(node, 0, dtype, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("for: decoding child: %w", err)
	}

	if len(child.Buffers) == 0 || child.Length == 0 {
		return child, nil
	}

	// Add the reference value to each element.
	buf := child.Buffers[0]
	switch dtype.PType {
	case vortex.PTypeI8:
		ref := int8(scalarToInt64(&sv))
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				buf[i] = byte(int8(buf[i]) + ref)
			}
		}

	case vortex.PTypeI16:
		ref := int16(scalarToInt64(&sv))
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := int16(binary.LittleEndian.Uint16(buf[i*2:])) + ref
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
			}
		}

	case vortex.PTypeI32:
		ref := int32(scalarToInt64(&sv))
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := int32(binary.LittleEndian.Uint32(buf[i*4:])) + ref
				binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
			}
		}

	case vortex.PTypeI64:
		ref := scalarToInt64(&sv)
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := int64(binary.LittleEndian.Uint64(buf[i*8:])) + ref
				binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
			}
		}

	case vortex.PTypeU8:
		ref := uint8(scalarToUint64(&sv))
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				buf[i] = buf[i] + ref
			}
		}

	case vortex.PTypeU16:
		ref := uint16(scalarToUint64(&sv))
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := binary.LittleEndian.Uint16(buf[i*2:]) + ref
				binary.LittleEndian.PutUint16(buf[i*2:], v)
			}
		}

	case vortex.PTypeU32:
		ref := uint32(scalarToUint64(&sv))
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := binary.LittleEndian.Uint32(buf[i*4:]) + ref
				binary.LittleEndian.PutUint32(buf[i*4:], v)
			}
		}

	case vortex.PTypeU64:
		ref := scalarToUint64(&sv)
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := binary.LittleEndian.Uint64(buf[i*8:]) + ref
				binary.LittleEndian.PutUint64(buf[i*8:], v)
			}
		}

	case vortex.PTypeF32:
		ref := scalarToFloat32(&sv)
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:])) + ref
				binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
			}
		}

	case vortex.PTypeF64:
		ref := scalarToFloat64(&sv)
		if ref != 0 {
			for i := 0; i < child.Length; i++ {
				v := math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:])) + ref
				binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
			}
		}

	default:
		return nil, fmt.Errorf("for: unsupported ptype %v", dtype.PType)
	}

	child.Stats = node.Stats
	return child, nil
}

func init() {
	Register("fastlanes.for", &FoRDecoder{})
}
