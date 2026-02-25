package encoding

import (
	vortex "github.com/LaurieRhodes/vortex-go"
)

// ConstantDecoder decodes vortex.constant arrays.
// A Constant array has no buffers — the scalar value is in the metadata (protobuf ScalarValue).
// One child may be present (the validity array).
type ConstantDecoder struct{}

func (d *ConstantDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	// The metadata contains a protobuf-encoded ScalarValue.
	// Store it in Buffers[0] for downstream extraction.
	return &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: [][]byte{node.Metadata},
		Stats:   node.Stats,
	}, nil
}

func init() {
	Register("vortex.constant", &ConstantDecoder{})
}
