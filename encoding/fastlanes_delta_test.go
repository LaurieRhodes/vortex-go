package encoding

import (
	"encoding/binary"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// referenceTranspose implements the Rust fastlanes transpose permutation.
// transpose(idx) = (idx%16)*64 + FL_ORDER[(idx/16)%8]*8 + idx/128
func referenceTranspose(input []uint32) []uint32 {
	output := make([]uint32, 1024)
	for i := 0; i < 1024; i++ {
		output[i] = input[deltaTransposeIdx(i)]
	}
	return output
}

// referenceDeltaCompress performs Rust-compatible delta compression on u32 values.
// Returns (bases, deltas) where both are in transposed FL_ORDER layout.
func referenceDeltaCompress(values []uint32) (bases, deltas []uint32) {
	const LANES = 32 // u32 LANES
	numChunks := len(values) / 1024
	remainder := len(values) % 1024

	bases = make([]uint32, 0, numChunks*LANES+1)
	deltas = make([]uint32, 0, len(values))

	for c := 0; c < numChunks; c++ {
		chunk := values[c*1024 : (c+1)*1024]

		// Transpose
		transposed := referenceTranspose(chunk)

		// Extract bases (first LANES elements of transposed)
		bases = append(bases, transposed[:LANES]...)

		// Compute deltas per lane
		chunkDeltas := make([]uint32, 1024)
		for lane := 0; lane < LANES; lane++ {
			prev := transposed[lane] // base for this lane = transposed[lane]
			tBits := 32              // u32 = 32 rows
			for row := 0; row < tBits; row++ {
				idx := deltaTransposedIndex(row, lane)
				val := transposed[idx]
				chunkDeltas[idx] = val - prev // wrapping sub
				prev = val
			}
		}
		deltas = append(deltas, chunkDeltas...)
	}

	// Remainder: scalar delta
	if remainder > 0 {
		remValues := values[numChunks*1024:]
		bases = append(bases, remValues[0])
		prev := remValues[0]
		for _, v := range remValues {
			deltas = append(deltas, v-prev)
			prev = v
		}
	}

	return bases, deltas
}

// u32SliceToBytes converts a []uint32 to little-endian bytes.
func u32SliceToBytes(vals []uint32) []byte {
	buf := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(buf[i*4:], v)
	}
	return buf
}

// buildDeltaArrayNode constructs an ArrayNode + buffers that simulates
// what a vortex file would contain for a fastlanes.delta encoded array.
func buildDeltaArrayNode(values []uint32) (*vortex.ArrayNode, [][]byte) {
	bases, deltas := referenceDeltaCompress(values)

	basesBuf := u32SliceToBytes(bases)
	deltasBuf := u32SliceToBytes(deltas)

	// Metadata
	meta := &encodings.DeltaMetadata{
		DeltasLen: uint64(len(deltas)),
		Offset:    0,
	}
	metaBytes, _ := proto.Marshal(meta)

	// Build the node tree: delta -> [bases(primitive), deltas(primitive)]
	node := &vortex.ArrayNode{
		EncodingID: "fastlanes.delta",
		Metadata:   metaBytes,
		Children: []*vortex.ArrayNode{
			{
				EncodingID:    "vortex.primitive",
				BufferIndices: []uint16{0},
			},
			{
				EncodingID:    "vortex.primitive",
				BufferIndices: []uint16{1},
			},
		},
	}

	return node, [][]byte{basesBuf, deltasBuf}
}

func TestDeltaBasic(t *testing.T) {
	// Simple monotonic u32 sequence: 0, 3, 6, 9, ...
	n := 2048 // exactly 2 chunks
	values := make([]uint32, n)
	for i := range values {
		values[i] = uint32(i) * 3
	}

	node, bufs := buildDeltaArrayNode(values)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	arr, err := DecodeArray(node, bufs, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	if arr.Length != n {
		t.Fatalf("Length = %d, want %d", arr.Length, n)
	}

	buf := arr.Buffers[0]
	for _, idx := range []int{0, 1, 2, 100, 511, 512, 1023, 1024, 1025, 2047} {
		got := binary.LittleEndian.Uint32(buf[idx*4:])
		want := uint32(idx) * 3
		if got != want {
			t.Errorf("[%d] = %d, want %d", idx, got, want)
		}
	}
	t.Logf("delta basic: [0]=%d [1023]=%d [2047]=%d ✓",
		binary.LittleEndian.Uint32(buf[0:]),
		binary.LittleEndian.Uint32(buf[1023*4:]),
		binary.LittleEndian.Uint32(buf[2047*4:]))
}

func TestDeltaWithRemainder(t *testing.T) {
	// 1500 elements = 1 full chunk + 476 remainder
	n := 1500
	values := make([]uint32, n)
	for i := range values {
		values[i] = uint32(i*i) % 100000 // non-monotonic but deterministic
	}

	node, bufs := buildDeltaArrayNode(values)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	arr, err := DecodeArray(node, bufs, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for _, idx := range []int{0, 1, 100, 500, 1023, 1024, 1025, 1499} {
		got := binary.LittleEndian.Uint32(buf[idx*4:])
		want := values[idx]
		if got != want {
			t.Errorf("[%d] = %d, want %d", idx, got, want)
		}
	}
	t.Logf("delta remainder: [0]=%d [1024]=%d [1499]=%d ✓",
		values[0], values[1024], values[1499])
}

func TestDeltaSmallRemainder(t *testing.T) {
	// Only 100 elements — no full chunks, pure remainder
	n := 100
	values := make([]uint32, n)
	for i := range values {
		values[i] = uint32(i) * 7
	}

	node, bufs := buildDeltaArrayNode(values)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	arr, err := DecodeArray(node, bufs, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for i := 0; i < n; i++ {
		got := binary.LittleEndian.Uint32(buf[i*4:])
		want := values[i]
		if got != want {
			t.Errorf("[%d] = %d, want %d", i, got, want)
		}
	}
	t.Logf("delta small: all %d values verified ✓", n)
}

func TestDeltaWrapping(t *testing.T) {
	// Test wrapping arithmetic: descending values cause underflow
	n := 1024
	values := make([]uint32, n)
	for i := range values {
		// Oscillating values that cause wrapping: 0xFFFFFFF0, 0x10, 0xFFFFFFF0, 0x10, ...
		if i%2 == 0 {
			values[i] = 0xFFFFFFF0
		} else {
			values[i] = 0x10
		}
	}

	node, bufs := buildDeltaArrayNode(values)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	arr, err := DecodeArray(node, bufs, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for _, idx := range []int{0, 1, 2, 3, 100, 101, 1022, 1023} {
		got := binary.LittleEndian.Uint32(buf[idx*4:])
		want := values[idx]
		if got != want {
			t.Errorf("[%d] = 0x%08X, want 0x%08X", idx, got, want)
		}
	}
	t.Logf("delta wrapping: verified ✓")
}

func TestDeltaExact3Chunks(t *testing.T) {
	// 3072 = exactly 3 chunks, no remainder
	n := 3072
	values := make([]uint32, n)
	for i := range values {
		values[i] = uint32(i)*5 + uint32((i*i)%13)
	}

	node, bufs := buildDeltaArrayNode(values)
	dtype := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	arr, err := DecodeArray(node, bufs, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	buf := arr.Buffers[0]
	for _, idx := range []int{0, 1, 1023, 1024, 2047, 2048, 3071} {
		got := binary.LittleEndian.Uint32(buf[idx*4:])
		want := values[idx]
		if got != want {
			t.Errorf("[%d] = %d, want %d", idx, got, want)
		}
	}
	t.Logf("delta 3 chunks: verified ✓")
}
