package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// BoolDecoder decodes vortex.bool arrays.
// A Bool array has one buffer containing bit-packed booleans (1 bit per value).
type BoolDecoder struct{}

func (d *BoolDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(buffers) != 1 {
		return nil, fmt.Errorf("bool: expected 1 buffer, got %d", len(buffers))
	}

	expectedBytes := (rowCount + 7) / 8
	if uint64(len(buffers[0])) < expectedBytes {
		return nil, fmt.Errorf("bool: buffer too small (%d bytes, need %d for %d rows)",
			len(buffers[0]), expectedBytes, rowCount)
	}

	arr := &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: buffers,
		Stats:   node.Stats,
	}

	// Optional validity child.
	if len(node.Children) > 0 {
		validityDType := &vortex.DType{Kind: vortex.DTypeBool}
		validity, err := DecodeChild(node, 0, validityDType, rowCount, ctx)
		if err != nil {
			return nil, fmt.Errorf("bool: decoding validity child: %w", err)
		}
		arr.Children = []*vortex.Array{validity}
	}

	return arr, nil
}

func init() {
	Register("vortex.bool", &BoolDecoder{})
}

// BoolValues extracts boolean values from a decoded Bool array.
func BoolValues(arr *vortex.Array) ([]bool, error) {
	if arr.DType.Kind != vortex.DTypeBool {
		return nil, fmt.Errorf("not a bool array")
	}
	if len(arr.Buffers) == 0 {
		return nil, nil
	}
	buf := arr.Buffers[0]
	values := make([]bool, arr.Length)
	for i := range values {
		byteIdx := i / 8
		bitIdx := uint(i % 8)
		values[i] = (buf[byteIdx]>>bitIdx)&1 == 1
	}
	return values, nil
}
