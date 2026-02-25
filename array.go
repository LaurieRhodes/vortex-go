package vortex

// Array represents decoded columnar data.
type Array struct {
	// DType is the logical type of this array.
	DType *DType

	// Length is the number of elements.
	Length int

	// Buffers holds the raw data buffers for this array.
	Buffers [][]byte

	// Children holds child arrays (for struct, dict, masked, etc.).
	Children []*Array

	// Stats holds optional statistics.
	Stats *ArrayStats
}

// ArrayNode represents an encoded array node within a Flat layout segment.
// This is the in-file representation before decoding.
type ArrayNode struct {
	// EncodingID is the encoding type, resolved from the footer's array_specs.
	EncodingID string

	// Metadata is protobuf-encoded, encoding-specific metadata.
	Metadata []byte

	// Children are child array nodes.
	Children []*ArrayNode

	// BufferIndices index into the segment's buffer table.
	BufferIndices []uint16

	// Stats holds per-array statistics.
	Stats *ArrayStats
}

// BufferDesc describes a data buffer within an array segment.
type BufferDesc struct {
	Padding           uint16
	AlignmentExponent uint8
	Compression       uint8 // 0=None, 1=LZ4
	Length            uint32
}
