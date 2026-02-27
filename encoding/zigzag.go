package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// ZigZagDecoder decodes vortex.zigzag arrays.
//
// ZigZag encoding maps signed integers to unsigned integers so that small
// absolute values have small encoded values (good for bitpacking).
// Inverse: signed = (unsigned >> 1) ^ -(unsigned & 1)
//
// Single child: the unsigned integer array.
type ZigZagDecoder struct{}

func (d *ZigZagDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) < 1 {
		return nil, fmt.Errorf("zigzag: expected 1 child, got %d", len(node.Children))
	}

	// Decode the child (unsigned values).
	child, err := DecodeChild(node, 0, dtype, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("zigzag: decoding child: %w", err)
	}

	if len(child.Buffers) == 0 || child.Length == 0 {
		return child, nil
	}

	buf := child.Buffers[0]
	elemSize := vortex.PTypeSize(dtype.PType)

	// Apply inverse zigzag: signed = (unsigned >> 1) ^ -(unsigned & 1)
	switch elemSize {
	case 1:
		for i := 0; i < child.Length; i++ {
			u := buf[i]
			buf[i] = byte((u >> 1) ^ -(u & 1))
		}
	case 2:
		for i := 0; i < child.Length; i++ {
			u := binary.LittleEndian.Uint16(buf[i*2:])
			binary.LittleEndian.PutUint16(buf[i*2:], (u>>1)^-(u&1))
		}
	case 4:
		for i := 0; i < child.Length; i++ {
			u := binary.LittleEndian.Uint32(buf[i*4:])
			binary.LittleEndian.PutUint32(buf[i*4:], (u>>1)^-(u&1))
		}
	case 8:
		for i := 0; i < child.Length; i++ {
			u := binary.LittleEndian.Uint64(buf[i*8:])
			binary.LittleEndian.PutUint64(buf[i*8:], (u>>1)^-(u&1))
		}
	default:
		return nil, fmt.Errorf("zigzag: unsupported element size %d", elemSize)
	}

	child.Stats = node.Stats
	return child, nil
}

func init() {
	Register("vortex.zigzag", &ZigZagDecoder{})
}
