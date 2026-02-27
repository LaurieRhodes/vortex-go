package vortex

import (
	"bytes"
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/LaurieRhodes/vortex-go/internal/flatbuf/vortexfb"
)

// buildFlatBuffer is a helper that builds a FlatBuffer with the given root offset
// and returns the finished bytes. FlatBuffers are built back-to-front, so we
// call builder.Finish(root) to get the final bytes.
func buildFlatBuffer(build func(b *flatbuffers.Builder) flatbuffers.UOffsetT) []byte {
	b := flatbuffers.NewBuilder(256)
	root := build(b)
	b.Finish(root)
	return b.FinishedBytes()
}

// buildDTypePrimitiveI32 builds a DType FlatBuffer for Primitive(I32, non-nullable).
func buildDTypePrimitiveI32() []byte {
	return buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		// Build the Primitive table.
		vortexfb.PrimitiveStart(b)
		vortexfb.PrimitiveAddPtype(b, vortexfb.PTypeI32)
		vortexfb.PrimitiveAddNullable(b, false)
		primOff := vortexfb.PrimitiveEnd(b)

		// Build the DType wrapping it.
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypePrimitive)
		vortexfb.DTypeAddType(b, primOff)
		return vortexfb.DTypeEnd(b)
	})
}

// buildDTypeStruct builds a DType FlatBuffer for Struct({fieldName: Primitive(I32)}).
func buildDTypeStruct(fieldNames []string, fieldDTypes []func(b *flatbuffers.Builder) flatbuffers.UOffsetT) []byte {
	return buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		// Build field name strings.
		nameOffsets := make([]flatbuffers.UOffsetT, len(fieldNames))
		for i := len(fieldNames) - 1; i >= 0; i-- {
			nameOffsets[i] = b.CreateString(fieldNames[i])
		}

		// Build child DType tables.
		dtypeOffsets := make([]flatbuffers.UOffsetT, len(fieldDTypes))
		for i := len(fieldDTypes) - 1; i >= 0; i-- {
			dtypeOffsets[i] = fieldDTypes[i](b)
		}

		// Build names vector.
		vortexfb.Struct_StartNamesVector(b, len(nameOffsets))
		for i := len(nameOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(nameOffsets[i])
		}
		namesVec := b.EndVector(len(nameOffsets))

		// Build dtypes vector.
		vortexfb.Struct_StartDtypesVector(b, len(dtypeOffsets))
		for i := len(dtypeOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(dtypeOffsets[i])
		}
		dtypesVec := b.EndVector(len(dtypeOffsets))

		// Build the Struct_ table.
		vortexfb.Struct_Start(b)
		vortexfb.Struct_AddNames(b, namesVec)
		vortexfb.Struct_AddDtypes(b, dtypesVec)
		vortexfb.Struct_AddNullable(b, false)
		structOff := vortexfb.Struct_End(b)

		// Wrap in DType.
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeStruct_)
		vortexfb.DTypeAddType(b, structOff)
		return vortexfb.DTypeEnd(b)
	})
}

// primitiveI32DType builds a Primitive(I32) DType inline in a builder (for struct fields).
func primitiveI32DType(b *flatbuffers.Builder) flatbuffers.UOffsetT {
	vortexfb.PrimitiveStart(b)
	vortexfb.PrimitiveAddPtype(b, vortexfb.PTypeI32)
	vortexfb.PrimitiveAddNullable(b, false)
	primOff := vortexfb.PrimitiveEnd(b)

	vortexfb.DTypeStart(b)
	vortexfb.DTypeAddTypeType(b, vortexfb.TypePrimitive)
	vortexfb.DTypeAddType(b, primOff)
	return vortexfb.DTypeEnd(b)
}

// buildArraySegment builds a Flat layout segment containing a Primitive i32 array.
// Returns the segment bytes in the Vortex format:
//
//	[buffer data] [Array FlatBuffer] [fb_length: u32 LE]
func buildArraySegment(values []int32, arraySpecIndex uint16) []byte {
	// Build the raw buffer data first.
	bufData := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(bufData[i*4:], uint32(v))
	}

	// Build the Array FlatBuffer.
	arrayFB := buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		bufDataLen := uint32(len(values) * 4)

		// Build the Buffer struct (packed inline).
		vortexfb.ArrayStartBuffersVector(b, 1)
		vortexfb.CreateBuffer(b, 0, 0, vortexfb.CompressionNone, bufDataLen)
		buffersVec := b.EndVector(1)

		// Build the ArrayNode (root of the array tree).
		vortexfb.ArrayNodeStartBuffersVector(b, 1)
		b.PrependUint16(0) // buffer index 0
		bufsVec := b.EndVector(1)

		vortexfb.ArrayNodeStart(b)
		vortexfb.ArrayNodeAddEncoding(b, arraySpecIndex)
		vortexfb.ArrayNodeAddBuffers(b, bufsVec)
		nodeOff := vortexfb.ArrayNodeEnd(b)

		// Build the Array table.
		vortexfb.ArrayStart(b)
		vortexfb.ArrayAddRoot(b, nodeOff)
		vortexfb.ArrayAddBuffers(b, buffersVec)
		return vortexfb.ArrayEnd(b)
	})

	// Assemble: [buffer data] [Array FlatBuffer] [fb_length: u32 LE]
	fbLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(fbLen, uint32(len(arrayFB)))

	segment := make([]byte, 0, len(bufData)+len(arrayFB)+4)
	segment = append(segment, bufData...)
	segment = append(segment, arrayFB...)
	segment = append(segment, fbLen...)
	return segment
}

// buildLayout builds a Layout FlatBuffer.
func buildLayout(layoutSpecIndex uint16, rowCount uint64, segmentIndices []uint32, children [][]byte) []byte {
	return buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		return buildLayoutInline(b, layoutSpecIndex, rowCount, segmentIndices, children)
	})
}

// buildLayoutInline builds a Layout node inside an existing builder (for nesting).
func buildLayoutInline(b *flatbuffers.Builder, layoutSpecIndex uint16, rowCount uint64, segmentIndices []uint32, children [][]byte) flatbuffers.UOffsetT {
	// Build child layouts (if any). We need to decode the child FlatBuffer bytes
	// and re-serialize them inline. For simplicity, build children inline.
	// Actually, the children parameter here will be pre-built UOffsetTs... let me rethink.
	// For a flat approach, we'll just build everything inline.

	// Build segments vector.
	var segsVec flatbuffers.UOffsetT
	if len(segmentIndices) > 0 {
		vortexfb.LayoutStartSegmentsVector(b, len(segmentIndices))
		for i := len(segmentIndices) - 1; i >= 0; i-- {
			b.PrependUint32(segmentIndices[i])
		}
		segsVec = b.EndVector(len(segmentIndices))
	}

	vortexfb.LayoutStart(b)
	vortexfb.LayoutAddEncoding(b, layoutSpecIndex)
	vortexfb.LayoutAddRowCount(b, rowCount)
	if len(segmentIndices) > 0 {
		vortexfb.LayoutAddSegments(b, segsVec)
	}
	return vortexfb.LayoutEnd(b)
}

// buildFooter builds a Footer FlatBuffer.
func buildFooter(arraySpecs []string, layoutSpecs []string, segmentSpecs []SegmentSpec) []byte {
	return buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		// Build ArraySpec entries.
		aspecOffsets := make([]flatbuffers.UOffsetT, len(arraySpecs))
		for i := len(arraySpecs) - 1; i >= 0; i-- {
			idOff := b.CreateString(arraySpecs[i])
			vortexfb.ArraySpecStart(b)
			vortexfb.ArraySpecAddId(b, idOff)
			aspecOffsets[i] = vortexfb.ArraySpecEnd(b)
		}
		vortexfb.FooterStartArraySpecsVector(b, len(aspecOffsets))
		for i := len(aspecOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(aspecOffsets[i])
		}
		arraySpecsVec := b.EndVector(len(aspecOffsets))

		// Build LayoutSpec entries.
		lspecOffsets := make([]flatbuffers.UOffsetT, len(layoutSpecs))
		for i := len(layoutSpecs) - 1; i >= 0; i-- {
			idOff := b.CreateString(layoutSpecs[i])
			vortexfb.LayoutSpecStart(b)
			vortexfb.LayoutSpecAddId(b, idOff)
			lspecOffsets[i] = vortexfb.LayoutSpecEnd(b)
		}
		vortexfb.FooterStartLayoutSpecsVector(b, len(lspecOffsets))
		for i := len(lspecOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(lspecOffsets[i])
		}
		layoutSpecsVec := b.EndVector(len(lspecOffsets))

		// Build SegmentSpec structs (packed, inline).
		vortexfb.FooterStartSegmentSpecsVector(b, len(segmentSpecs))
		for i := len(segmentSpecs) - 1; i >= 0; i-- {
			s := segmentSpecs[i]
			vortexfb.CreateSegmentSpec(b, s.Offset, s.Length, s.AlignmentExponent, 0, 0)
		}
		segSpecsVec := b.EndVector(len(segmentSpecs))

		// Build Footer.
		vortexfb.FooterStart(b)
		vortexfb.FooterAddArraySpecs(b, arraySpecsVec)
		vortexfb.FooterAddLayoutSpecs(b, layoutSpecsVec)
		vortexfb.FooterAddSegmentSpecs(b, segSpecsVec)
		return vortexfb.FooterEnd(b)
	})
}

// buildPostscript builds a Postscript FlatBuffer.
func buildPostscript(dtype, layout, footer *PostscriptSegment) []byte {
	return buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		var dtypeOff, layoutOff, footerOff flatbuffers.UOffsetT

		if dtype != nil {
			vortexfb.PostscriptSegmentStart(b)
			vortexfb.PostscriptSegmentAddOffset(b, dtype.Offset)
			vortexfb.PostscriptSegmentAddLength(b, dtype.Length)
			vortexfb.PostscriptSegmentAddAlignmentExponent(b, dtype.AlignmentExponent)
			dtypeOff = vortexfb.PostscriptSegmentEnd(b)
		}

		vortexfb.PostscriptSegmentStart(b)
		vortexfb.PostscriptSegmentAddOffset(b, layout.Offset)
		vortexfb.PostscriptSegmentAddLength(b, layout.Length)
		vortexfb.PostscriptSegmentAddAlignmentExponent(b, layout.AlignmentExponent)
		layoutOff = vortexfb.PostscriptSegmentEnd(b)

		vortexfb.PostscriptSegmentStart(b)
		vortexfb.PostscriptSegmentAddOffset(b, footer.Offset)
		vortexfb.PostscriptSegmentAddLength(b, footer.Length)
		vortexfb.PostscriptSegmentAddAlignmentExponent(b, footer.AlignmentExponent)
		footerOff = vortexfb.PostscriptSegmentEnd(b)

		vortexfb.PostscriptStart(b)
		if dtype != nil {
			vortexfb.PostscriptAddDtype(b, dtypeOff)
		}
		vortexfb.PostscriptAddLayout(b, layoutOff)
		vortexfb.PostscriptAddFooter(b, footerOff)
		return vortexfb.PostscriptEnd(b)
	})
}

// buildMinimalVortexFile constructs a complete valid Vortex file in memory.
// The file contains a single Flat layout with one Primitive(I32) segment.
func buildMinimalVortexFile(values []int32) []byte {
	var buf bytes.Buffer

	// 1. Write leading magic.
	buf.Write(Magic[:])

	// 2. Write data segment (Array FlatBuffer + raw buffer data).
	dataSegment := buildArraySegment(values, 0) // array_spec index 0 = "vortex.primitive"
	dataSegOffset := uint64(buf.Len())
	buf.Write(dataSegment)

	segSpec := SegmentSpec{
		Offset:            dataSegOffset,
		Length:            uint32(len(dataSegment)),
		AlignmentExponent: 0,
	}

	// 3. Build and write the DType FlatBuffer.
	dtypeBytes := buildDTypePrimitiveI32()
	dtypeOffset := uint64(buf.Len())
	buf.Write(dtypeBytes)

	// 4. Build and write the Layout FlatBuffer.
	// Simple: one Flat layout pointing at segment 0.
	layoutBytes := buildLayout(0, uint64(len(values)), []uint32{0}, nil) // layout_spec index 0 = "vortex.flat"
	layoutOffset := uint64(buf.Len())
	buf.Write(layoutBytes)

	// 5. Build and write the Footer FlatBuffer.
	footerBytes := buildFooter(
		[]string{"vortex.primitive"}, // array_specs
		[]string{"vortex.flat"},      // layout_specs
		[]SegmentSpec{segSpec},       // segment_specs
	)
	footerOffset := uint64(buf.Len())
	buf.Write(footerBytes)

	// 6. Build and write the Postscript FlatBuffer.
	psBytes := buildPostscript(
		&PostscriptSegment{Offset: dtypeOffset, Length: uint32(len(dtypeBytes))},
		&PostscriptSegment{Offset: layoutOffset, Length: uint32(len(layoutBytes))},
		&PostscriptSegment{Offset: footerOffset, Length: uint32(len(footerBytes))},
	)
	buf.Write(psBytes)

	// 7. Write the 8-byte EOF trailer.
	var trailer [EndOfFileSize]byte
	binary.LittleEndian.PutUint16(trailer[0:2], 1) // version = 1
	binary.LittleEndian.PutUint16(trailer[2:4], uint16(len(psBytes)))
	copy(trailer[4:8], Magic[:])
	buf.Write(trailer[:])

	return buf.Bytes()
}

// buildStructVortexFile builds a file with a Struct layout containing one i32 column.
func buildStructVortexFile(fieldName string, values []int32) []byte {
	var buf bytes.Buffer

	// 1. Leading magic.
	buf.Write(Magic[:])

	// 2. Data segment for the column.
	dataSegment := buildArraySegment(values, 0)
	dataSegOffset := uint64(buf.Len())
	buf.Write(dataSegment)

	segSpec := SegmentSpec{
		Offset:            dataSegOffset,
		Length:            uint32(len(dataSegment)),
		AlignmentExponent: 0,
	}

	// 3. DType: Struct({fieldName: Primitive(I32)})
	dtypeBytes := buildDTypeStruct(
		[]string{fieldName},
		[]func(b *flatbuffers.Builder) flatbuffers.UOffsetT{primitiveI32DType},
	)
	dtypeOffset := uint64(buf.Len())
	buf.Write(dtypeBytes)

	// 4. Layout: Struct(encoding=1) → Flat(encoding=0, segment=0)
	// layout_specs[0] = "vortex.flat", layout_specs[1] = "vortex.struct"
	layoutBytes := buildFlatBuffer(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		// Build child Flat layout first.
		vortexfb.LayoutStartSegmentsVector(b, 1)
		b.PrependUint32(0) // segment index 0
		flatSegsVec := b.EndVector(1)

		vortexfb.LayoutStart(b)
		vortexfb.LayoutAddEncoding(b, 0) // layout_specs[0] = "vortex.flat"
		vortexfb.LayoutAddRowCount(b, uint64(len(values)))
		vortexfb.LayoutAddSegments(b, flatSegsVec)
		flatOff := vortexfb.LayoutEnd(b)

		// Build parent Struct layout with one child.
		vortexfb.LayoutStartChildrenVector(b, 1)
		b.PrependUOffsetT(flatOff)
		childrenVec := b.EndVector(1)

		vortexfb.LayoutStart(b)
		vortexfb.LayoutAddEncoding(b, 1) // layout_specs[1] = "vortex.struct"
		vortexfb.LayoutAddRowCount(b, uint64(len(values)))
		vortexfb.LayoutAddChildren(b, childrenVec)
		return vortexfb.LayoutEnd(b)
	})
	layoutOffset := uint64(buf.Len())
	buf.Write(layoutBytes)

	// 5. Footer.
	footerBytes := buildFooter(
		[]string{"vortex.primitive"},
		[]string{"vortex.flat", "vortex.struct"},
		[]SegmentSpec{segSpec},
	)
	footerOffset := uint64(buf.Len())
	buf.Write(footerBytes)

	// 6. Postscript.
	psBytes := buildPostscript(
		&PostscriptSegment{Offset: dtypeOffset, Length: uint32(len(dtypeBytes))},
		&PostscriptSegment{Offset: layoutOffset, Length: uint32(len(layoutBytes))},
		&PostscriptSegment{Offset: footerOffset, Length: uint32(len(footerBytes))},
	)
	buf.Write(psBytes)

	// 7. EOF trailer.
	var trailer [EndOfFileSize]byte
	binary.LittleEndian.PutUint16(trailer[0:2], 1)
	binary.LittleEndian.PutUint16(trailer[2:4], uint16(len(psBytes)))
	copy(trailer[4:8], Magic[:])
	buf.Write(trailer[:])

	return buf.Bytes()
}

// --- Tests ---

func TestOpenMinimalFile(t *testing.T) {
	values := []int32{1, 2, 3, 4, 5}
	data := buildMinimalVortexFile(values)
	r := bytes.NewReader(data)

	f, err := Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Verify version.
	if f.Version() != 1 {
		t.Errorf("version = %d, want 1", f.Version())
	}

	// Verify DType parsed correctly.
	dt := f.DType()
	if dt == nil {
		t.Fatal("DType is nil")
	}
	if dt.Kind != DTypePrimitive {
		t.Errorf("DType.Kind = %v, want DTypePrimitive", dt.Kind)
	}
	if dt.PType != PTypeI32 {
		t.Errorf("DType.PType = %v, want PTypeI32", dt.PType)
	}
	if dt.Nullable {
		t.Error("DType.Nullable = true, want false")
	}

	// Verify Footer.
	footer := f.Footer()
	if footer == nil {
		t.Fatal("Footer is nil")
	}
	if len(footer.ArraySpecs) != 1 || footer.ArraySpecs[0] != "vortex.primitive" {
		t.Errorf("ArraySpecs = %v, want [vortex.primitive]", footer.ArraySpecs)
	}
	if len(footer.LayoutSpecs) != 1 || footer.LayoutSpecs[0] != "vortex.flat" {
		t.Errorf("LayoutSpecs = %v, want [vortex.flat]", footer.LayoutSpecs)
	}
	if len(footer.SegmentSpecs) != 1 {
		t.Fatalf("SegmentSpecs length = %d, want 1", len(footer.SegmentSpecs))
	}
	seg := footer.SegmentSpecs[0]
	expectedLen := uint32(len(buildArraySegment(values, 0)))
	if seg.Length != expectedLen {
		t.Errorf("SegmentSpec.Length = %d, want %d", seg.Length, expectedLen)
	}

	// Verify Layout.
	layout := f.Layout()
	if layout == nil {
		t.Fatal("Layout is nil")
	}
	if layout.EncodingID != LayoutFlat {
		t.Errorf("Layout.EncodingID = %q, want %q", layout.EncodingID, LayoutFlat)
	}
	if layout.RowCount != 5 {
		t.Errorf("Layout.RowCount = %d, want 5", layout.RowCount)
	}
	if len(layout.Segments) != 1 || layout.Segments[0] != 0 {
		t.Errorf("Layout.Segments = %v, want [0]", layout.Segments)
	}
}

func TestOpenStructFile(t *testing.T) {
	values := []int32{10, 20, 30}
	data := buildStructVortexFile("numbers", values)
	r := bytes.NewReader(data)

	f, err := Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Verify DType is a Struct.
	dt := f.DType()
	if dt == nil {
		t.Fatal("DType is nil")
	}
	if dt.Kind != DTypeStruct {
		t.Errorf("DType.Kind = %v, want DTypeStruct", dt.Kind)
	}
	if len(dt.FieldNames) != 1 || dt.FieldNames[0] != "numbers" {
		t.Errorf("FieldNames = %v, want [numbers]", dt.FieldNames)
	}
	if len(dt.FieldTypes) != 1 {
		t.Fatalf("FieldTypes length = %d, want 1", len(dt.FieldTypes))
	}
	if dt.FieldTypes[0].Kind != DTypePrimitive || dt.FieldTypes[0].PType != PTypeI32 {
		t.Errorf("field type = %v, want Primitive(I32)", dt.FieldTypes[0])
	}

	// Verify Layout is Struct → Flat.
	layout := f.Layout()
	if layout == nil {
		t.Fatal("Layout is nil")
	}
	if layout.EncodingID != LayoutStruct {
		t.Errorf("Layout.EncodingID = %q, want %q", layout.EncodingID, LayoutStruct)
	}
	if layout.RowCount != 3 {
		t.Errorf("Layout.RowCount = %d, want 3", layout.RowCount)
	}
	if len(layout.Children) != 1 {
		t.Fatalf("Layout.Children length = %d, want 1", len(layout.Children))
	}

	child := layout.Children[0]
	if child.EncodingID != LayoutFlat {
		t.Errorf("child.EncodingID = %q, want %q", child.EncodingID, LayoutFlat)
	}
	if child.RowCount != 3 {
		t.Errorf("child.RowCount = %d, want 3", child.RowCount)
	}
	if len(child.Segments) != 1 || child.Segments[0] != 0 {
		t.Errorf("child.Segments = %v, want [0]", child.Segments)
	}
}

func TestOpenTooSmall(t *testing.T) {
	data := []byte{1, 2, 3}
	_, err := Open(bytes.NewReader(data), 3)
	if err == nil {
		t.Fatal("expected error for file too small")
	}
}

func TestOpenBadMagic(t *testing.T) {
	data := make([]byte, 20)
	copy(data[0:4], "VTXF")   // leading magic OK
	copy(data[16:20], "XXXX") // trailing magic bad
	_, err := Open(bytes.NewReader(data), 20)
	if err == nil {
		t.Fatal("expected error for bad trailing magic")
	}
}

func TestReadSegmentSpec(t *testing.T) {
	values := []int32{100, 200, 300}
	data := buildMinimalVortexFile(values)
	r := bytes.NewReader(data)

	f, err := Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Read the data segment.
	seg := f.Footer().SegmentSpecs[0]
	segData, err := f.readSegmentSpec(seg)
	if err != nil {
		t.Fatalf("readSegmentSpec failed: %v", err)
	}

	// The segment should contain buffer data + Array FlatBuffer + 4 bytes of fb_length.
	if len(segData) != int(seg.Length) {
		t.Errorf("segment length = %d, want %d", len(segData), seg.Length)
	}

	// Buffer data is at the beginning of the segment (first 12 bytes = 3 x i32).
	for i, want := range values {
		got := int32(binary.LittleEndian.Uint32(segData[i*4:]))
		if got != want {
			t.Errorf("value[%d] = %d, want %d", i, got, want)
		}
	}
}
