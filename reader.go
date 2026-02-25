package vortex

import (
	"fmt"
)

// ReadColumn reads and decodes a single column by name from the file.
// It navigates the layout tree: Struct → Zoned → Dict/Chunked → Flat,
// reading and decoding the array data from the appropriate segments.
//
// Returns the column's Layout and its DType for use with segment decoding.
func (f *File) FindColumnLayout(name string) (*Layout, *DType, int, error) {
	if f.dtype == nil || f.dtype.Kind != DTypeStruct {
		return nil, nil, -1, fmt.Errorf("file DType is not a struct")
	}

	// Find the column index.
	colIdx := -1
	for i, n := range f.dtype.FieldNames {
		if n == name {
			colIdx = i
			break
		}
	}
	if colIdx < 0 {
		return nil, nil, -1, fmt.Errorf("column %q not found in schema", name)
	}

	rootLayout := f.layout
	if !rootLayout.IsStruct() {
		return nil, nil, -1, fmt.Errorf("root layout is %q, expected vortex.struct", rootLayout.EncodingID)
	}
	if colIdx >= len(rootLayout.Children) {
		return nil, nil, -1, fmt.Errorf("column index %d out of range (have %d children)", colIdx, len(rootLayout.Children))
	}

	return rootLayout.Children[colIdx], f.dtype.FieldTypes[colIdx], colIdx, nil
}

// NavigateToData traverses Zoned and Dict layouts to find the data layout(s).
// For a typical column, the path is: Zoned → [Dict →] data (Chunked or Flat).
//
// Returns the data layout and an optional dictionary values layout.
// If the column uses a Dict layout, dictLayout will be non-nil and dataLayout
// will be the codes layout.
func NavigateToData(layout *Layout) (dataLayout *Layout, dictValuesLayout *Layout) {
	current := layout

	// Traverse through Zoned (stats) layout — child[0] is data.
	if current.IsZoned() && len(current.Children) >= 1 {
		current = current.Children[0]
	}

	// Check for Dict layout — child[0] is values, child[1] is codes.
	if current.EncodingID == LayoutDict && len(current.Children) >= 2 {
		return current.Children[1], current.Children[0]
	}

	return current, nil
}

// CollectFlatLayouts collects all Flat layout leaves from a layout subtree.
// This handles Chunked layouts (which contain multiple Flat children) and
// direct Flat layouts.
func CollectFlatLayouts(layout *Layout) []*Layout {
	if layout.IsFlat() {
		return []*Layout{layout}
	}
	if layout.IsChunked() {
		var flats []*Layout
		for _, child := range layout.Children {
			flats = append(flats, CollectFlatLayouts(child)...)
		}
		return flats
	}
	// For other layouts (shouldn't normally happen in data path), recurse.
	var flats []*Layout
	for _, child := range layout.Children {
		flats = append(flats, CollectFlatLayouts(child)...)
	}
	return flats
}

// ColumnNames returns the field names from the root schema, or nil if the
// schema is not a struct.
func (f *File) ColumnNames() []string {
	if f.dtype == nil || f.dtype.Kind != DTypeStruct {
		return nil
	}
	return f.dtype.FieldNames
}

// NumColumns returns the number of columns, or 0 if the schema is not a struct.
func (f *File) NumColumns() int {
	if f.dtype == nil || f.dtype.Kind != DTypeStruct {
		return 0
	}
	return len(f.dtype.FieldNames)
}
