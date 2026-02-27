package vortex

// Layout represents a node in the Vortex file's layout tree.
// The layout tree describes how data is physically arranged in the file.
type Layout struct {
	// EncodingID is the layout type, resolved from the footer's layout_specs.
	// Known values: "vortex.struct", "vortex.chunked", "vortex.flat", "vortex.stats", "vortex.dict"
	EncodingID string

	// RowCount is the number of rows represented by this layout node.
	RowCount uint64

	// Metadata is opaque layout-specific metadata.
	Metadata []byte

	// Children are the child layout nodes.
	Children []*Layout

	// Segments are indices into the footer's segment_specs table.
	// These identify the byte ranges in the file that hold this layout's data.
	Segments []uint32
}

// Known layout encoding IDs.
const (
	LayoutFlat    = "vortex.flat"
	LayoutChunked = "vortex.chunked"
	LayoutStruct  = "vortex.struct"
	LayoutZoned   = "vortex.stats" // historical name; implements zone map pruning
	LayoutDict    = "vortex.dict"
)

// IsFlat returns true if this is a leaf layout containing a serialized array.
func (l *Layout) IsFlat() bool {
	return l.EncodingID == LayoutFlat
}

// IsStruct returns true if this layout splits data by columns.
func (l *Layout) IsStruct() bool {
	return l.EncodingID == LayoutStruct
}

// IsChunked returns true if this layout splits data by row groups.
func (l *Layout) IsChunked() bool {
	return l.EncodingID == LayoutChunked
}

// IsZoned returns true if this layout contains zone map statistics.
func (l *Layout) IsZoned() bool {
	return l.EncodingID == LayoutZoned
}
