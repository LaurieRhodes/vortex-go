package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
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

	// Read codes PType and values length from DictArrayMetadata.
	// Fall back to u32/rowCount if metadata is missing (backwards compatibility).
	codesPType := vortex.PTypeU32
	var valuesLen uint64
	if len(node.Metadata) > 0 {
		var meta encodings.DictArrayMetadata
		if err := proto.Unmarshal(node.Metadata, &meta); err == nil {
			codesPType = vortex.PTypeFromProto(meta.CodesPtype)
			if meta.ValuesLen > 0 {
				valuesLen = uint64(meta.ValuesLen)
			}
		}
	}

	codesDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: codesPType}
	codes, err := DecodeChild(node, 0, codesDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("dict: decoding codes: %w", err)
	}

	// Values child: the dictionary, decode with the target dtype.
	// The row count for values is the dictionary size (from metadata),
	// not the array row count.
	valuesNode := node.Children[1]
	valuesRowCount := rowCount // fallback
	if valuesLen > 0 {
		valuesRowCount = valuesLen
	}
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
