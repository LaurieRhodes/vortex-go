package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// DictDecoder decodes vortex.dict arrays.
// A Dict array has two children:
//   - Child 0: codes — integer array of indices into the values dictionary
//   - Child 1: values — the dictionary (e.g., a VarBinView string array)
type DictDecoder struct{}

func (d *DictDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) != 2 {
		return nil, fmt.Errorf("dict: expected 2 children, got %d", len(node.Children))
	}

	// Codes child: integers, decode as the codes' own PType.
	// The dtype for codes is typically u8/u16/u32 — we use a synthetic primitive dtype.
	codesDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	codes, err := DecodeChild(node, 0, codesDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("dict: decoding codes: %w", err)
	}

	// Values child: the dictionary, decode with the target dtype.
	// The row count for values is the dictionary size, not the array row count.
	// We'll use the child's own row count if available, otherwise fall back.
	valuesNode := node.Children[1]
	valuesRowCount := rowCount // fallback; in practice, BtrBlocks child metadata gives the real count
	values, err := decodeWithContext(valuesNode, dtype, valuesRowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("dict: decoding values: %w", err)
	}

	return &vortex.Array{
		DType:    dtype,
		Length:   int(rowCount),
		Children: []*vortex.Array{codes, values},
		Stats:    node.Stats,
	}, nil
}

func init() {
	Register("vortex.dict", &DictDecoder{})
}
