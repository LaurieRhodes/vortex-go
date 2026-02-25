package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// StructDecoder decodes vortex.struct arrays (array-level).
// A Struct array has one child per field, no buffers of its own.
type StructDecoder struct{}

func (d *StructDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypeStruct {
		return nil, fmt.Errorf("struct decoder: dtype is %v, not struct", dtype.Kind)
	}

	nFields := len(dtype.FieldTypes)
	if len(node.Children) != nFields {
		return nil, fmt.Errorf("struct: expected %d children (fields), got %d", nFields, len(node.Children))
	}

	children := make([]*vortex.Array, nFields)
	for i := 0; i < nFields; i++ {
		child, err := DecodeChild(node, i, dtype.FieldTypes[i], rowCount, ctx)
		if err != nil {
			return nil, fmt.Errorf("struct: decoding field %d (%s): %w", i, dtype.FieldNames[i], err)
		}
		children[i] = child
	}

	return &vortex.Array{
		DType:    dtype,
		Length:   int(rowCount),
		Children: children,
		Stats:    node.Stats,
	}, nil
}

func init() {
	Register("vortex.struct", &StructDecoder{})
}
