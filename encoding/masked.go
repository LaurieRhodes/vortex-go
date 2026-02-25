package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// MaskedDecoder decodes vortex.masked arrays.
// A Masked array has two children:
//   - Child 0: values — the actual data array
//   - Child 1: mask — boolean validity array (true = valid, false = null)
type MaskedDecoder struct{}

func (d *MaskedDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) != 2 {
		return nil, fmt.Errorf("masked: expected 2 children, got %d", len(node.Children))
	}

	// Values child: same dtype as the masked array.
	values, err := DecodeChild(node, 0, dtype, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("masked: decoding values: %w", err)
	}

	// Mask child: boolean array.
	maskDType := &vortex.DType{Kind: vortex.DTypeBool}
	mask, err := DecodeChild(node, 1, maskDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("masked: decoding mask: %w", err)
	}

	return &vortex.Array{
		DType:    dtype,
		Length:   int(rowCount),
		Children: []*vortex.Array{values, mask},
		Stats:    node.Stats,
	}, nil
}

func init() {
	Register("vortex.masked", &MaskedDecoder{})
}
