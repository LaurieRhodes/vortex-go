package vortex

// Footer holds the dictionary-encoded registries parsed from the file footer.
type Footer struct {
	// ArraySpecs maps encoding index → string ID (e.g., "fastlanes.bitpacked", "vortex.dict").
	ArraySpecs []string

	// LayoutSpecs maps layout index → string ID (e.g., "vortex.flat", "vortex.chunked").
	LayoutSpecs []string

	// SegmentSpecs maps segment index → byte range in the file.
	SegmentSpecs []SegmentSpec

	// CompressionSpecs maps compression index → scheme.
	CompressionSpecs []CompressionScheme
}

// SegmentSpec describes the location and properties of a data segment in the file.
type SegmentSpec struct {
	Offset            uint64
	Length            uint32
	AlignmentExponent uint8
}

// CompressionScheme identifies a segment compression algorithm.
type CompressionScheme uint8

const (
	CompressionNone CompressionScheme = 0
	CompressionLZ4  CompressionScheme = 1
	CompressionZLib CompressionScheme = 2
	CompressionZStd CompressionScheme = 3
)

func (c CompressionScheme) String() string {
	switch c {
	case CompressionNone:
		return "none"
	case CompressionLZ4:
		return "lz4"
	case CompressionZLib:
		return "zlib"
	case CompressionZStd:
		return "zstd"
	default:
		return "unknown"
	}
}
