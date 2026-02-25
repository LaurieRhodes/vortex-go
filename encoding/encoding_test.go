package encoding

import (
	"bytes"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// TestDecodeFullRoundTrip builds a minimal Vortex file, opens it, reads the
// Flat segment, parses the ArrayNode, and decodes the Primitive values.
func TestDecodeFullRoundTrip(t *testing.T) {
	values := []int32{10, 20, 30, 40, 50}
	data := buildTestFile(values)
	r := bytes.NewReader(data)

	f, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Read the Flat layout segment.
	layout := f.Layout()
	if !layout.IsFlat() {
		t.Fatalf("expected Flat layout, got %q", layout.EncodingID)
	}

	node, buffers, err := f.ReadFlatSegment(layout)
	if err != nil {
		t.Fatalf("ReadFlatSegment failed: %v", err)
	}

	if node.EncodingID != "vortex.primitive" {
		t.Errorf("node.EncodingID = %q, want %q", node.EncodingID, "vortex.primitive")
	}

	// Decode the array.
	arr, err := DecodeArray(node, buffers, f.DType(), layout.RowCount)
	if err != nil {
		t.Fatalf("DecodeArray failed: %v", err)
	}

	if arr.Length != 5 {
		t.Errorf("arr.Length = %d, want 5", arr.Length)
	}

	// Extract values.
	got, err := Int32Values(arr)
	if err != nil {
		t.Fatalf("Int32Values failed: %v", err)
	}

	for i, v := range values {
		if got[i] != v {
			t.Errorf("value[%d] = %d, want %d", i, got[i], v)
		}
	}
}

// TestDecodeStructLayout tests reading a Struct layout with one column.
func TestDecodeStructLayout(t *testing.T) {
	values := []int32{100, 200, 300}
	data := buildStructTestFile("numbers", values)
	r := bytes.NewReader(data)

	f, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Navigate: Struct → child[0] (Flat).
	layout := f.Layout()
	if !layout.IsStruct() {
		t.Fatalf("expected Struct layout, got %q", layout.EncodingID)
	}
	if len(layout.Children) != 1 {
		t.Fatalf("expected 1 child, got %d", len(layout.Children))
	}

	childLayout := layout.Children[0]
	if !childLayout.IsFlat() {
		t.Fatalf("expected Flat child, got %q", childLayout.EncodingID)
	}

	node, buffers, err := f.ReadFlatSegment(childLayout)
	if err != nil {
		t.Fatalf("ReadFlatSegment failed: %v", err)
	}

	// Decode using the column's DType (not the struct DType).
	colDType := f.DType().FieldTypes[0]
	arr, err := DecodeArray(node, buffers, colDType, childLayout.RowCount)
	if err != nil {
		t.Fatalf("DecodeArray failed: %v", err)
	}

	got, err := Int32Values(arr)
	if err != nil {
		t.Fatalf("Int32Values failed: %v", err)
	}

	for i, v := range values {
		if got[i] != v {
			t.Errorf("value[%d] = %d, want %d", i, got[i], v)
		}
	}
}
