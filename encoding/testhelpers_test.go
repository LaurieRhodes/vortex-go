package encoding

// Test helpers for constructing minimal Vortex files in memory.
// These duplicate the logic in vortex_test.go but are accessible from the
// encoding package's test scope.

import (
	"bytes"
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/flatbuf/vortexfb"
)

func fbBuild(build func(b *flatbuffers.Builder) flatbuffers.UOffsetT) []byte {
	b := flatbuffers.NewBuilder(256)
	root := build(b)
	b.Finish(root)
	return b.FinishedBytes()
}

// buildTestFile constructs a minimal Vortex file with a single Flat layout
// containing a Primitive(I32) array of the given values.
func buildTestFile(values []int32) []byte {
	var buf bytes.Buffer

	// Leading magic.
	buf.Write(vortex.Magic[:])

	// Data segment: Array FlatBuffer + raw i32 buffer.
	dataSegment := buildArraySeg(values, 0)
	dataOff := uint64(buf.Len())
	buf.Write(dataSegment)

	// DType: Primitive(I32).
	dtypeBytes := fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		vortexfb.PrimitiveStart(b)
		vortexfb.PrimitiveAddPtype(b, vortexfb.PTypeI32)
		vortexfb.PrimitiveAddNullable(b, false)
		primOff := vortexfb.PrimitiveEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypePrimitive)
		vortexfb.DTypeAddType(b, primOff)
		return vortexfb.DTypeEnd(b)
	})
	dtypeOff := uint64(buf.Len())
	buf.Write(dtypeBytes)

	// Layout: Flat with segment 0.
	layoutBytes := fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		vortexfb.LayoutStartSegmentsVector(b, 1)
		b.PrependUint32(0)
		segsVec := b.EndVector(1)
		vortexfb.LayoutStart(b)
		vortexfb.LayoutAddEncoding(b, 0) // layout_specs[0] = "vortex.flat"
		vortexfb.LayoutAddRowCount(b, uint64(len(values)))
		vortexfb.LayoutAddSegments(b, segsVec)
		return vortexfb.LayoutEnd(b)
	})
	layoutOff := uint64(buf.Len())
	buf.Write(layoutBytes)

	// Footer.
	footerBytes := buildFooterFB(
		[]string{"vortex.primitive"},
		[]string{"vortex.flat"},
		[]segSpec{{dataOff, uint32(len(dataSegment)), 0}},
	)
	footerOff := uint64(buf.Len())
	buf.Write(footerBytes)

	// Postscript.
	psBytes := buildPostscriptFB(
		&psSeg{dtypeOff, uint32(len(dtypeBytes)), 0},
		&psSeg{layoutOff, uint32(len(layoutBytes)), 0},
		&psSeg{footerOff, uint32(len(footerBytes)), 0},
	)
	buf.Write(psBytes)

	// EOF trailer.
	var trailer [vortex.EndOfFileSize]byte
	binary.LittleEndian.PutUint16(trailer[0:2], 1)
	binary.LittleEndian.PutUint16(trailer[2:4], uint16(len(psBytes)))
	copy(trailer[4:8], vortex.Magic[:])
	buf.Write(trailer[:])

	return buf.Bytes()
}

// buildStructTestFile builds a file with Struct({fieldName: Primitive(I32)}).
func buildStructTestFile(fieldName string, values []int32) []byte {
	var buf bytes.Buffer

	buf.Write(vortex.Magic[:])

	dataSegment := buildArraySeg(values, 0)
	dataOff := uint64(buf.Len())
	buf.Write(dataSegment)

	// DType: Struct.
	dtypeBytes := fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		// Child DType: Primitive(I32).
		vortexfb.PrimitiveStart(b)
		vortexfb.PrimitiveAddPtype(b, vortexfb.PTypeI32)
		vortexfb.PrimitiveAddNullable(b, false)
		primOff := vortexfb.PrimitiveEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypePrimitive)
		vortexfb.DTypeAddType(b, primOff)
		childDT := vortexfb.DTypeEnd(b)

		// Struct.
		nameOff := b.CreateString(fieldName)
		vortexfb.Struct_StartNamesVector(b, 1)
		b.PrependUOffsetT(nameOff)
		namesVec := b.EndVector(1)
		vortexfb.Struct_StartDtypesVector(b, 1)
		b.PrependUOffsetT(childDT)
		dtypesVec := b.EndVector(1)
		vortexfb.Struct_Start(b)
		vortexfb.Struct_AddNames(b, namesVec)
		vortexfb.Struct_AddDtypes(b, dtypesVec)
		vortexfb.Struct_AddNullable(b, false)
		structOff := vortexfb.Struct_End(b)

		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeStruct_)
		vortexfb.DTypeAddType(b, structOff)
		return vortexfb.DTypeEnd(b)
	})
	dtypeOff := uint64(buf.Len())
	buf.Write(dtypeBytes)

	// Layout: Struct → Flat.
	layoutBytes := fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		// Child: Flat.
		vortexfb.LayoutStartSegmentsVector(b, 1)
		b.PrependUint32(0)
		flatSegsVec := b.EndVector(1)
		vortexfb.LayoutStart(b)
		vortexfb.LayoutAddEncoding(b, 0) // layout_specs[0] = "vortex.flat"
		vortexfb.LayoutAddRowCount(b, uint64(len(values)))
		vortexfb.LayoutAddSegments(b, flatSegsVec)
		flatOff := vortexfb.LayoutEnd(b)

		// Parent: Struct.
		vortexfb.LayoutStartChildrenVector(b, 1)
		b.PrependUOffsetT(flatOff)
		childrenVec := b.EndVector(1)
		vortexfb.LayoutStart(b)
		vortexfb.LayoutAddEncoding(b, 1) // layout_specs[1] = "vortex.struct"
		vortexfb.LayoutAddRowCount(b, uint64(len(values)))
		vortexfb.LayoutAddChildren(b, childrenVec)
		return vortexfb.LayoutEnd(b)
	})
	layoutOff := uint64(buf.Len())
	buf.Write(layoutBytes)

	footerBytes := buildFooterFB(
		[]string{"vortex.primitive"},
		[]string{"vortex.flat", "vortex.struct"},
		[]segSpec{{dataOff, uint32(len(dataSegment)), 0}},
	)
	footerOff := uint64(buf.Len())
	buf.Write(footerBytes)

	psBytes := buildPostscriptFB(
		&psSeg{dtypeOff, uint32(len(dtypeBytes)), 0},
		&psSeg{layoutOff, uint32(len(layoutBytes)), 0},
		&psSeg{footerOff, uint32(len(footerBytes)), 0},
	)
	buf.Write(psBytes)

	var trailer [vortex.EndOfFileSize]byte
	binary.LittleEndian.PutUint16(trailer[0:2], 1)
	binary.LittleEndian.PutUint16(trailer[2:4], uint16(len(psBytes)))
	copy(trailer[4:8], vortex.Magic[:])
	buf.Write(trailer[:])

	return buf.Bytes()
}

// --- Low-level helpers ---

type segSpec struct {
	offset uint64
	length uint32
	align  uint8
}

type psSeg struct {
	offset uint64
	length uint32
	align  uint8
}

func buildArraySeg(values []int32, arraySpecIdx uint16) []byte {
	bufData := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(bufData[i*4:], uint32(v))
	}

	arrayFB := fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		bufDataLen := uint32(len(values) * 4)
		vortexfb.ArrayStartBuffersVector(b, 1)
		vortexfb.CreateBuffer(b, 0, 0, vortexfb.CompressionNone, bufDataLen)
		buffersVec := b.EndVector(1)

		vortexfb.ArrayNodeStartBuffersVector(b, 1)
		b.PrependUint16(0)
		bufsVec := b.EndVector(1)
		vortexfb.ArrayNodeStart(b)
		vortexfb.ArrayNodeAddEncoding(b, arraySpecIdx)
		vortexfb.ArrayNodeAddBuffers(b, bufsVec)
		nodeOff := vortexfb.ArrayNodeEnd(b)

		vortexfb.ArrayStart(b)
		vortexfb.ArrayAddRoot(b, nodeOff)
		vortexfb.ArrayAddBuffers(b, buffersVec)
		return vortexfb.ArrayEnd(b)
	})

	// Vortex segment format: [buffer data] [Array FlatBuffer] [fb_length: u32 LE]
	fbLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(fbLen, uint32(len(arrayFB)))

	out := make([]byte, 0, len(bufData)+len(arrayFB)+4)
	out = append(out, bufData...)
	out = append(out, arrayFB...)
	out = append(out, fbLen...)
	return out
}

func buildFooterFB(arraySpecs, layoutSpecs []string, segs []segSpec) []byte {
	return fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		aspecOffs := make([]flatbuffers.UOffsetT, len(arraySpecs))
		for i := len(arraySpecs) - 1; i >= 0; i-- {
			idOff := b.CreateString(arraySpecs[i])
			vortexfb.ArraySpecStart(b)
			vortexfb.ArraySpecAddId(b, idOff)
			aspecOffs[i] = vortexfb.ArraySpecEnd(b)
		}
		vortexfb.FooterStartArraySpecsVector(b, len(aspecOffs))
		for i := len(aspecOffs) - 1; i >= 0; i-- {
			b.PrependUOffsetT(aspecOffs[i])
		}
		aVec := b.EndVector(len(aspecOffs))

		lspecOffs := make([]flatbuffers.UOffsetT, len(layoutSpecs))
		for i := len(layoutSpecs) - 1; i >= 0; i-- {
			idOff := b.CreateString(layoutSpecs[i])
			vortexfb.LayoutSpecStart(b)
			vortexfb.LayoutSpecAddId(b, idOff)
			lspecOffs[i] = vortexfb.LayoutSpecEnd(b)
		}
		vortexfb.FooterStartLayoutSpecsVector(b, len(lspecOffs))
		for i := len(lspecOffs) - 1; i >= 0; i-- {
			b.PrependUOffsetT(lspecOffs[i])
		}
		lVec := b.EndVector(len(lspecOffs))

		vortexfb.FooterStartSegmentSpecsVector(b, len(segs))
		for i := len(segs) - 1; i >= 0; i-- {
			vortexfb.CreateSegmentSpec(b, segs[i].offset, segs[i].length, segs[i].align, 0, 0)
		}
		sVec := b.EndVector(len(segs))

		vortexfb.FooterStart(b)
		vortexfb.FooterAddArraySpecs(b, aVec)
		vortexfb.FooterAddLayoutSpecs(b, lVec)
		vortexfb.FooterAddSegmentSpecs(b, sVec)
		return vortexfb.FooterEnd(b)
	})
}

func buildPostscriptFB(dtype, layout, footer *psSeg) []byte {
	return fbBuild(func(b *flatbuffers.Builder) flatbuffers.UOffsetT {
		var dtOff, layOff, ftOff flatbuffers.UOffsetT

		if dtype != nil {
			vortexfb.PostscriptSegmentStart(b)
			vortexfb.PostscriptSegmentAddOffset(b, dtype.offset)
			vortexfb.PostscriptSegmentAddLength(b, dtype.length)
			vortexfb.PostscriptSegmentAddAlignmentExponent(b, dtype.align)
			dtOff = vortexfb.PostscriptSegmentEnd(b)
		}

		vortexfb.PostscriptSegmentStart(b)
		vortexfb.PostscriptSegmentAddOffset(b, layout.offset)
		vortexfb.PostscriptSegmentAddLength(b, layout.length)
		vortexfb.PostscriptSegmentAddAlignmentExponent(b, layout.align)
		layOff = vortexfb.PostscriptSegmentEnd(b)

		vortexfb.PostscriptSegmentStart(b)
		vortexfb.PostscriptSegmentAddOffset(b, footer.offset)
		vortexfb.PostscriptSegmentAddLength(b, footer.length)
		vortexfb.PostscriptSegmentAddAlignmentExponent(b, footer.align)
		ftOff = vortexfb.PostscriptSegmentEnd(b)

		vortexfb.PostscriptStart(b)
		if dtype != nil {
			vortexfb.PostscriptAddDtype(b, dtOff)
		}
		vortexfb.PostscriptAddLayout(b, layOff)
		vortexfb.PostscriptAddFooter(b, ftOff)
		return vortexfb.PostscriptEnd(b)
	})
}
