package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// DecimalBytePartsDecoder decodes vortex.decimal_byte_parts arrays.
//
// This encoding stores decimal values as their fixed-point integer representation
// in a single child (the "most significant part" or MSP). The child is a signed
// integer primitive array whose values, when divided by 10^scale, give the actual
// decimal values.
//
// Metadata: zeroth_child_ptype (PType of the child), lower_part_count (always 0 currently)
// Children: [msp] — a single signed integer array
type DecimalBytePartsDecoder struct{}

func (d *DecimalBytePartsDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) < 1 {
		return nil, fmt.Errorf("decimal_byte_parts: expected at least 1 child, got %d", len(node.Children))
	}

	// Parse metadata to get the child PType.
	var meta encodings.DecimalBytesPartsMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("decimal_byte_parts: parsing metadata: %w", err)
		}
	}

	childPType := vortex.PTypeFromProto(meta.ZerothChildPtype)
	childDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: childPType}

	// Decode the MSP child (signed integer representation of the decimal values).
	child, err := DecodeChild(node, 0, childDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("decimal_byte_parts: decoding msp child: %w", err)
	}

	// Return the result with the decimal dtype but the primitive child's buffers.
	// The integer values ARE the decimal fixed-point representation.
	return &vortex.Array{
		DType:    dtype,
		Length:   child.Length,
		Buffers:  child.Buffers,
		Children: child.Children,
		Stats:    node.Stats,
	}, nil
}

func init() {
	Register("vortex.decimal_byte_parts", &DecimalBytePartsDecoder{})
}
