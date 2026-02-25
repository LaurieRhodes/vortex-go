package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// ChunkedDecoder decodes vortex.chunked arrays (array-level, not layout-level).
// A Chunked array has N children, each a sub-array. The decoded array is the
// concatenation of all children.
type ChunkedDecoder struct{}

func (d *ChunkedDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	children := make([]*vortex.Array, len(node.Children))
	totalLen := 0

	for i := range node.Children {
		child, err := DecodeChild(node, i, dtype, rowCount, ctx)
		if err != nil {
			return nil, fmt.Errorf("chunked: decoding child %d: %w", i, err)
		}
		children[i] = child
		totalLen += child.Length
	}

	return &vortex.Array{
		DType:    dtype,
		Length:   totalLen,
		Children: children,
		Stats:    node.Stats,
	}, nil
}

func init() {
	Register("vortex.chunked", &ChunkedDecoder{})
}
