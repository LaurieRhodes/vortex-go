package encoding

import (
	"encoding/binary"
	"math"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// buildALPRDNode constructs a synthetic ALPRD-encoded array for testing.
// This simulates what a vortex file would contain: left_parts (primitive u16 codes),
// right_parts (primitive uint), plus metadata with the dictionary.
func buildALPRDNode(values []float64, rightBW int) (*vortex.ArrayNode, [][]byte) {
	n := len(values)

	// Split each float64 into left and right parts.

	// Build dictionary from the left patterns.
	patternToCode := make(map[uint16]int)
	var dict []uint16
	leftCodes := make([]uint16, n)
	rightParts := make([]uint64, n)

	for i, v := range values {
		bits := math.Float64bits(v)
		rightParts[i] = bits & ((1 << uint(rightBW)) - 1)
		leftPattern := uint16(bits >> uint(rightBW))

		if code, ok := patternToCode[leftPattern]; ok {
			leftCodes[i] = uint16(code)
		} else {
			code := len(dict)
			patternToCode[leftPattern] = code
			dict = append(dict, leftPattern)
			leftCodes[i] = uint16(code)
		}
	}

	// Encode left_parts as primitive u16 buffer.
	leftBuf := make([]byte, n*2)
	for i, c := range leftCodes {
		binary.LittleEndian.PutUint16(leftBuf[i*2:], c)
	}

	// Encode right_parts as primitive u64 buffer.
	rightBuf := make([]byte, n*8)
	for i, r := range rightParts {
		binary.LittleEndian.PutUint64(rightBuf[i*8:], r)
	}

	// Build dict as []uint32.
	dictU32 := make([]uint32, len(dict))
	for i, d := range dict {
		dictU32[i] = uint32(d)
	}

	meta := &encodings.ALPRDMetadata{
		RightBitWidth:  uint32(rightBW),
		DictLen:        uint32(len(dict)),
		Dict:           dictU32,
		LeftPartsPtype: int32(vortex.PTypeU16),
	}
	metaBytes, _ := proto.Marshal(meta)

	node := &vortex.ArrayNode{
		EncodingID: "vortex.alprd",
		Metadata:   metaBytes,
		Children: []*vortex.ArrayNode{
			{EncodingID: "vortex.primitive", BufferIndices: []uint16{0}}, // left
			{EncodingID: "vortex.primitive", BufferIndices: []uint16{1}}, // right
		},
	}

	return node, [][]byte{leftBuf, rightBuf}
}

func TestALPRDBasic(t *testing.T) {
	// Values with shared exponent bits — typical ALPRD case.
	values := []float64{
		1.23, 1.24, 1.25, 1.26, 1.27, 1.28, 1.29, 1.30,
		1.31, 1.32, 1.33, 1.34, 1.35, 1.36, 1.37, 1.38,
	}

	// Use rightBW = 48 (typical for f64: 64 - 16 = 48 right bits)
	node, bufs := buildALPRDNode(values, 48)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64}

	arr, err := DecodeArray(node, bufs, dtype, uint64(len(values)))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	if arr.Length != len(values) {
		t.Fatalf("Length = %d, want %d", arr.Length, len(values))
	}

	buf := arr.Buffers[0]
	for i, want := range values {
		got := math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
		if got != want {
			t.Errorf("[%d] = %.20f, want %.20f", i, got, want)
		}
	}
	t.Logf("alprd basic: all %d f64 values verified ✓", len(values))
}

func TestALPRDVariedExponents(t *testing.T) {
	// Mix of values with different exponents — more dictionary entries.
	values := make([]float64, 200)
	for i := range values {
		switch i % 5 {
		case 0:
			values[i] = 1.0 + float64(i)*0.001
		case 1:
			values[i] = 100.0 + float64(i)*0.01
		case 2:
			values[i] = 0.001 + float64(i)*0.0001
		case 3:
			values[i] = 1000.0 + float64(i)*0.1
		case 4:
			values[i] = 10000.0 + float64(i)*1.0
		}
	}

	node, bufs := buildALPRDNode(values, 48)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64}

	arr, err := DecodeArray(node, bufs, dtype, uint64(len(values)))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for i, want := range values {
		got := math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
		if got != want {
			t.Errorf("[%d] = %v, want %v", i, got, want)
		}
	}
	t.Logf("alprd varied: all %d values verified ✓", len(values))
}

func TestALPRDf32(t *testing.T) {
	// Test f32 path.
	values32 := []float32{0.1, 0.2, 0.3, 1.5, 2.5, 3.5, 10.0, 100.0}

	n := len(values32)
	rightBW := 16 // 32 - 16 = 16 right bits for f32

	// Build manually for f32.
	patternToCode := make(map[uint16]int)
	var dict []uint16
	leftCodes := make([]uint16, n)
	rightParts := make([]uint32, n)

	for i, v := range values32 {
		bits := math.Float32bits(v)
		rightParts[i] = bits & ((1 << uint(rightBW)) - 1)
		leftPattern := uint16(bits >> uint(rightBW))

		if code, ok := patternToCode[leftPattern]; ok {
			leftCodes[i] = uint16(code)
		} else {
			code := len(dict)
			patternToCode[leftPattern] = code
			dict = append(dict, leftPattern)
			leftCodes[i] = uint16(code)
		}
	}

	leftBuf := make([]byte, n*2)
	for i, c := range leftCodes {
		binary.LittleEndian.PutUint16(leftBuf[i*2:], c)
	}
	rightBuf := make([]byte, n*4)
	for i, r := range rightParts {
		binary.LittleEndian.PutUint32(rightBuf[i*4:], r)
	}

	dictU32 := make([]uint32, len(dict))
	for i, d := range dict {
		dictU32[i] = uint32(d)
	}

	meta := &encodings.ALPRDMetadata{
		RightBitWidth:  uint32(rightBW),
		DictLen:        uint32(len(dict)),
		Dict:           dictU32,
		LeftPartsPtype: int32(vortex.PTypeU16),
	}
	metaBytes, _ := proto.Marshal(meta)

	node := &vortex.ArrayNode{
		EncodingID: "vortex.alprd",
		Metadata:   metaBytes,
		Children: []*vortex.ArrayNode{
			{EncodingID: "vortex.primitive", BufferIndices: []uint16{0}},
			{EncodingID: "vortex.primitive", BufferIndices: []uint16{1}},
		},
	}

	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF32}
	arr, err := DecodeArray(node, [][]byte{leftBuf, rightBuf}, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for i, want := range values32 {
		got := math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		if got != want {
			t.Errorf("[%d] = %v, want %v", i, got, want)
		}
	}
	t.Logf("alprd f32: all %d values verified ✓", n)
}

func TestALPRDWithPatches(t *testing.T) {
	// Create values where most share an exponent but a few are outliers.
	n := 100
	values := make([]float64, n)
	for i := range values {
		values[i] = 1.0 + float64(i)*0.001 // all share same high bits
	}
	// Insert many outliers with distinct exponents to exceed dict size (8).
	// Each outlier has a unique left-16-bit pattern.
	outlierPositions := []int{5, 10, 15, 20, 25, 30, 35, 40, 45, 50, 55, 60}
	for j, pos := range outlierPositions {
		values[pos] = math.Ldexp(1.0, (j+1)*10) // 2^10, 2^20, ..., 2^120
	}

	rightBW := 48

	// Build dict from the "normal" values, outliers become exceptions.
	patternToCode := make(map[uint16]int)
	var dict []uint16
	leftCodes := make([]uint16, n)
	rightParts := make([]uint64, n)

	type patchEntry struct {
		idx     int
		pattern uint16
	}
	var patches []patchEntry

	for i, v := range values {
		bits := math.Float64bits(v)
		rightParts[i] = bits & ((1 << uint(rightBW)) - 1)
		leftPattern := uint16(bits >> uint(rightBW))

		if code, ok := patternToCode[leftPattern]; ok {
			leftCodes[i] = uint16(code)
		} else if len(dict) < 8 { // max dict size
			code := len(dict)
			patternToCode[leftPattern] = code
			dict = append(dict, leftPattern)
			leftCodes[i] = uint16(code)
		} else {
			// Exception: not in dictionary.
			leftCodes[i] = 0 // fill with 0
			patches = append(patches, patchEntry{idx: i, pattern: leftPattern})
		}
	}

	leftBuf := make([]byte, n*2)
	for i, c := range leftCodes {
		binary.LittleEndian.PutUint16(leftBuf[i*2:], c)
	}
	rightBuf := make([]byte, n*8)
	for i, r := range rightParts {
		binary.LittleEndian.PutUint64(rightBuf[i*8:], r)
	}

	dictU32 := make([]uint32, len(dict))
	for i, d := range dict {
		dictU32[i] = uint32(d)
	}

	// Build patch buffers.
	var patchIdxBuf, patchValBuf []byte
	var patchesMeta *encodings.PatchesMetadata

	if len(patches) > 0 {
		patchIdxBuf = make([]byte, len(patches)*4) // u32 indices
		patchValBuf = make([]byte, len(patches)*2) // u16 values
		for i, p := range patches {
			binary.LittleEndian.PutUint32(patchIdxBuf[i*4:], uint32(p.idx))
			binary.LittleEndian.PutUint16(patchValBuf[i*2:], p.pattern)
		}
		patchesMeta = &encodings.PatchesMetadata{
			Len:          uint64(len(patches)),
			Offset:       0,
			IndicesPtype: int32(vortex.PTypeU32),
		}
	}

	meta := &encodings.ALPRDMetadata{
		RightBitWidth:  uint32(rightBW),
		DictLen:        uint32(len(dict)),
		Dict:           dictU32,
		LeftPartsPtype: int32(vortex.PTypeU16),
		Patches:        patchesMeta,
	}
	metaBytes, _ := proto.Marshal(meta)

	children := []*vortex.ArrayNode{
		{EncodingID: "vortex.primitive", BufferIndices: []uint16{0}},
		{EncodingID: "vortex.primitive", BufferIndices: []uint16{1}},
	}
	allBufs := [][]byte{leftBuf, rightBuf}

	if len(patches) > 0 {
		children = append(children,
			&vortex.ArrayNode{EncodingID: "vortex.primitive", BufferIndices: []uint16{2}},
			&vortex.ArrayNode{EncodingID: "vortex.primitive", BufferIndices: []uint16{3}},
		)
		allBufs = append(allBufs, patchIdxBuf, patchValBuf)
	}

	node := &vortex.ArrayNode{
		EncodingID: "vortex.alprd",
		Metadata:   metaBytes,
		Children:   children,
	}

	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64}
	arr, err := DecodeArray(node, allBufs, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for i, want := range values {
		got := math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
		if got != want {
			t.Errorf("[%d] = %v, want %v", i, got, want)
		}
	}
	t.Logf("alprd patches: all %d values verified (including %d patches) ✓", n, len(patches))
}
