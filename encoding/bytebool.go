package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// ByteBoolDecoder decodes vortex.bytebool arrays.
//
// ByteBool encoding stores one byte per boolean value (0x00=false, 0x01=true).
// This is more compute-friendly than bitpacked bools for certain operations.
// Single buffer: one byte per element.
type ByteBoolDecoder struct{}

func (d *ByteBoolDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(buffers) < 1 {
		return nil, fmt.Errorf("bytebool: expected 1 buffer, got %d", len(buffers))
	}

	buf := buffers[0]
	count := int(rowCount)

	if len(buf) < count {
		return nil, fmt.Errorf("bytebool: buffer too small (%d bytes for %d values)", len(buf), count)
	}

	// Convert byte-per-bool to bitpacked bool (canonical form).
	outLen := (count + 7) / 8
	outBuf := make([]byte, outLen)
	for i := 0; i < count; i++ {
		if buf[i] != 0 {
			outBuf[i/8] |= 1 << (uint(i) % 8)
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
	Register("vortex.bytebool", &ByteBoolDecoder{})
}
