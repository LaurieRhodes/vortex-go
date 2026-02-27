package pcodec

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestCompressDecompressBaseline tests round-trip with baseline (level 0) compression.
func TestCompressDecompressBaseline(t *testing.T) {
	tests := []struct {
		name string
		nt   NumberType
		data []byte
	}{
		{"i32_simple", NumI32, i32ToBytes([]int32{1, 2, 3, 4, 5, 100, -50, 0, 42, 999})},
		{"u32_simple", NumU32, u32ToBytes([]uint32{0, 1, 2, 3, 100, 200, 300, 1000})},
		{"i64_simple", NumI64, i64ToBytes([]int64{-1000, 0, 1000, math.MaxInt64, math.MinInt64})},
		{"f64_simple", NumF64, f64ToBytes([]float64{1.0, 2.5, -3.14, 0.0, 100.0})},
		{"f32_simple", NumF32, f32ToBytes([]float32{1.0, 2.5, -3.14, 0.0, 100.0})},
		{"i32_constant", NumI32, i32ToBytes([]int32{42, 42, 42, 42, 42})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := CompressOptions{Level: 0}
			result, err := Compress(tt.data, tt.nt, opts)
			if err != nil {
				t.Fatal(err)
			}

			roundTrip(t, result, tt.nt, tt.data)
		})
	}
}

// TestCompressDecompressMultiBin tests round-trip with multi-bin compression.
func TestCompressDecompressMultiBin(t *testing.T) {
	tests := []struct {
		name string
		nt   NumberType
		data []byte
	}{
		{"i32_spread", NumI32, i32ToBytes(makeI32Spread(1000))},
		{"u32_narrow", NumU32, u32ToBytes(makeU32Narrow(500))},
		{"i64_mixed", NumI64, i64ToBytes([]int64{-1000, -999, 0, 1, 2, 3, 1000, 1001, 1002})},
		{"f64_mixed", NumF64, f64ToBytes([]float64{1.0, 1.1, 1.2, 100.0, 100.1, 100.2, -50.0, -50.1})},
		{"i32_large", NumI32, i32ToBytes(makeI32Spread(10000))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := DefaultCompressOptions()
			result, err := Compress(tt.data, tt.nt, opts)
			if err != nil {
				t.Fatal(err)
			}

			roundTrip(t, result, tt.nt, tt.data)
		})
	}
}

// TestCompressDecompressMultiPage tests multi-page chunks.
func TestCompressDecompressMultiPage(t *testing.T) {
	data := i32ToBytes(makeI32Spread(2000))
	opts := CompressOptions{Level: 0, ValuesPerPage: 500}
	result, err := Compress(data, NumI32, opts)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.PageCounts) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(result.PageCounts))
	}
	if result.PageCounts[0] != 4 {
		t.Errorf("expected 4 pages, got %d", result.PageCounts[0])
	}

	roundTrip(t, result, NumI32, data)
}

// TestCompressDecompressMultiChunk tests multi-chunk files.
func TestCompressDecompressMultiChunk(t *testing.T) {
	data := i32ToBytes(makeI32Spread(1000))
	opts := CompressOptions{Level: 0, ValuesPerChunk: 300}
	result, err := Compress(data, NumI32, opts)
	if err != nil {
		t.Fatal(err)
	}

	// 1000 values / 300 per chunk = 4 chunks (300+300+300+100).
	if len(result.ChunkMetas) != 4 {
		t.Fatalf("expected 4 chunks, got %d", len(result.ChunkMetas))
	}

	roundTrip(t, result, NumI32, data)
}

// TestLatentOrderedRoundTrip verifies toLatentOrdered/fromLatentOrdered are inverses.
func TestLatentOrderedRoundTrip(t *testing.T) {
	tests := []struct {
		nt  NumberType
		raw uint64
	}{
		{NumU32, 0}, {NumU32, 42}, {NumU32, math.MaxUint32},
		{NumI32, 0}, {NumI32, uint64(0xFFFFFFFF)}, // -1 as uint32 bits
		{NumI32, uint64(0x80000000)}, // MinInt32 as uint32 bits
		{NumI64, 0}, {NumI64, ^uint64(0)},
		{NumF64, math.Float64bits(1.0)}, {NumF64, math.Float64bits(-1.0)},
		{NumF32, uint64(math.Float32bits(3.14))},
	}

	for _, tt := range tests {
		lat := toLatentOrdered(tt.nt, tt.raw)
		got := fromLatentOrdered(tt.nt, lat)
		if got != tt.raw {
			t.Errorf("nt=%v raw=%d: toLatentOrdered→fromLatentOrdered = %d, want %d",
				tt.nt, tt.raw, got, tt.raw)
		}
	}
}

// roundTrip decompresses the CompressResult and compares to original data.
func roundTrip(t *testing.T, result *CompressResult, nt NumberType, original []byte) {
	t.Helper()

	lt := nt.LatentType()
	bs := nt.ByteSize()
	n := len(original) / bs

	// Build pagesPerChunk and pageNValues from result.
	pagesPerChunk := result.PageCounts
	pageNValues := result.PageNs

	latents, err := Decompress(
		result.Header,
		result.ChunkMetas,
		result.Pages,
		pagesPerChunk,
		pageNValues,
		lt,
	)
	if err != nil {
		t.Fatalf("decompression failed: %v", err)
	}

	if len(latents) != n {
		t.Fatalf("decompressed %d values, want %d", len(latents), n)
	}

	decoded := LatentsToBytes(nt, latents)
	if len(decoded) != len(original) {
		t.Fatalf("decoded %d bytes, want %d", len(decoded), len(original))
	}

	for i := 0; i < n; i++ {
		off := i * bs
		got := decoded[off : off+bs]
		want := original[off : off+bs]
		match := true
		for j := range got {
			if got[j] != want[j] {
				match = false
				break
			}
		}
		if !match {
			t.Errorf("mismatch at index %d: got %v, want %v", i, got, want)
			if i > 5 {
				t.Fatal("too many mismatches, stopping")
			}
		}
	}
}

// Helper functions.

func i32ToBytes(vals []int32) []byte {
	buf := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}
	return buf
}

func u32ToBytes(vals []uint32) []byte {
	buf := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(buf[i*4:], v)
	}
	return buf
}

func i64ToBytes(vals []int64) []byte {
	buf := make([]byte, len(vals)*8)
	for i, v := range vals {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
	}
	return buf
}

func f64ToBytes(vals []float64) []byte {
	buf := make([]byte, len(vals)*8)
	for i, v := range vals {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

func f32ToBytes(vals []float32) []byte {
	buf := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func makeI32Spread(n int) []int32 {
	vals := make([]int32, n)
	for i := range vals {
		vals[i] = int32(i*3 - n)
	}
	return vals
}

func makeU32Narrow(n int) []uint32 {
	vals := make([]uint32, n)
	for i := range vals {
		vals[i] = uint32(100 + i%50)
	}
	return vals
}

// TestDebugI64MultiBin isolates the i64 multi-bin failure.
func TestDebugI64MultiBin(t *testing.T) {
	// First test: larger i64 dataset to see if it's a small-data issue
	vals := make([]int64, 500)
	for i := range vals {
		vals[i] = int64(i*3 - 750)
	}
	data := i64ToBytes(vals)
	opts := DefaultCompressOptions()
	result, err := Compress(data, NumI64, opts)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, result, NumI64, data)
}
