package encoding

import (
	vortex "github.com/LaurieRhodes/vortex-go"
)

// NullDecoder decodes vortex.null arrays.
// A Null array has no buffers — it represents rowCount null values.
type NullDecoder struct{}

func (d *NullDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	return &vortex.Array{
		DType:  dtype,
		Length: int(rowCount),
		Stats:  node.Stats,
	}, nil
}

func init() {
	Register("vortex.null", &NullDecoder{})
}
