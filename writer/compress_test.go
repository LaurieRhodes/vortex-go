package writer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/encoding"
)

// TestDictStringRoundTrip writes low-cardinality strings and verifies dict encoding.
func TestDictStringRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"status"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypeUtf8},
		},
	}

	// 100 values, only 3 unique → ratio = 0.03, well below 0.5 threshold.
	strs := make([]string, 100)
	values := []string{"active", "pending", "inactive"}
	for i := range strs {
		strs[i] = values[i%3]
	}

	arr := makeStringArray(strs)

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(map[string]*vortex.Array{"status": arr}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Verify the flat segment root encoding is dict.
	colLayout := vf.Layout().Children[0]
	if colLayout.IsFlat() {
		node, _, err := vf.ReadFlatSegment(colLayout)
		if err != nil {
			t.Fatal(err)
		}
		if node.EncodingID != "vortex.dict" {
			t.Errorf("encoding = %q, want vortex.dict", node.EncodingID)
		}
	}

	// Read via Scan API.
	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"status"}})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for iter.Next() {
		res := iter.Result()
		a := res.Columns["status"]
		if a == nil {
			continue
		}
		s, err := encoding.StringValues(a)
		if err != nil {
			t.Fatalf("StringValues: %v", err)
		}
		got = append(got, s...)
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != 100 {
		t.Fatalf("got %d strings, want 100", len(got))
	}
	for i, want := range strs {
		if got[i] != want {
			t.Errorf("status[%d] = %q, want %q", i, got[i], want)
		}
	}
	t.Logf("dict string round-trip: 100 rows, 3 unique, %d bytes ✓", len(data))
}

// TestDictStringHighCardinality verifies dict encoding is NOT applied for high-cardinality.
func TestDictStringHighCardinality(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"name"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypeUtf8},
		},
	}

	// All unique strings → ratio = 1.0, dict encoding should NOT apply.
	strs := make([]string, 20)
	for i := range strs {
		strs[i] = fmt.Sprintf("user_%04d", i)
	}

	arr := makeStringArray(strs)

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(map[string]*vortex.Array{"name": arr}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Read back and verify.
	r := bytes.NewReader(out.Bytes())
	vf, err := vortex.Open(r, int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}

	// Should NOT be dict-encoded.
	colLayout := vf.Layout().Children[0]
	if colLayout.IsFlat() {
		node, _, err := vf.ReadFlatSegment(colLayout)
		if err != nil {
			t.Fatal(err)
		}
		if node.EncodingID == "vortex.dict" {
			t.Errorf("should NOT be dict-encoded with 100%% unique strings")
		}
	}

	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"name"}})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for iter.Next() {
		a := iter.Result().Columns["name"]
		if a == nil {
			continue
		}
		s, err := encoding.StringValues(a)
		if err != nil {
			t.Fatalf("StringValues: %v", err)
		}
		got = append(got, s...)
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	for i, want := range strs {
		if got[i] != want {
			t.Errorf("name[%d] = %q, want %q", i, got[i], want)
		}
	}
	t.Logf("high-cardinality: %d bytes, dict not applied ✓", out.Len())
}

// TestBitPackedIntRoundTrip writes integers with a narrow range and verifies FoR+BitPacking.
func TestBitPackedIntRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"val"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		},
	}

	// 1000 values in range [1000..1099] → range=99, bw=7 bits, 78% compression.
	n := 1000
	buf := make([]byte, n*4)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(1000+i%100))
	}
	arr := &vortex.Array{
		DType:   schema.FieldTypes[0],
		Length:  n,
		Buffers: [][]byte{buf},
	}

	// Write with compression.
	var compressed bytes.Buffer
	w, err := NewWriter(&compressed, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(map[string]*vortex.Array{"val": arr}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Write without compression.
	var uncompressed bytes.Buffer
	w2, err := NewWriter(&uncompressed, schema, UncompressedOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.WriteChunk(map[string]*vortex.Array{"val": arr}); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	compSize := compressed.Len()
	rawSize := uncompressed.Len()
	savings := float64(rawSize-compSize) / float64(rawSize) * 100

	t.Logf("bitpacked: %d bytes vs uncompressed: %d bytes (%.1f%% savings)", compSize, rawSize, savings)

	if compSize >= rawSize {
		t.Errorf("compressed (%d) should be smaller than uncompressed (%d)", compSize, rawSize)
	}

	// Verify the layout uses FoR encoding.
	r := bytes.NewReader(compressed.Bytes())
	vf, err := vortex.Open(r, int64(compSize))
	if err != nil {
		t.Fatal(err)
	}

	colLayout := vf.Layout().Children[0]
	if colLayout.IsFlat() {
		node, _, err := vf.ReadFlatSegment(colLayout)
		if err != nil {
			t.Fatal(err)
		}
		if node.EncodingID != "fastlanes.for" {
			t.Errorf("encoding = %q, want fastlanes.for", node.EncodingID)
		}
	}

	// Read back via Scan and verify values.
	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"val"}})
	if err != nil {
		t.Fatal(err)
	}

	var got []int32
	for iter.Next() {
		a := iter.Result().Columns["val"]
		if a == nil {
			continue
		}
		vals, err := encoding.Int32Values(a)
		if err != nil {
			t.Fatalf("Int32Values: %v", err)
		}
		got = append(got, vals...)
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != n {
		t.Fatalf("got %d values, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		want := int32(1000 + i%100)
		if got[i] != want {
			t.Errorf("val[%d] = %d, want %d", i, got[i], want)
		}
	}
}

// TestBitPackedMultipleTypes tests FoR+BitPacking for various integer widths.
func TestBitPackedMultipleTypes(t *testing.T) {
	tests := []struct {
		name  string
		ptype vortex.PType
		min   int64
		max   int64
	}{
		{"i16", vortex.PTypeI16, 100, 200},
		{"i32", vortex.PTypeI32, 5000, 5100},
		{"i64", vortex.PTypeI64, 1_000_000, 1_000_100},
		{"u16", vortex.PTypeU16, 0, 50},
		{"u32", vortex.PTypeU32, 10000, 10200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := &vortex.DType{
				Kind:       vortex.DTypeStruct,
				FieldNames: []string{"x"},
				FieldTypes: []*vortex.DType{
					{Kind: vortex.DTypePrimitive, PType: tt.ptype},
				},
			}

			n := 500
			elemSize := vortex.PTypeSize(tt.ptype)
			buf := make([]byte, n*elemSize)
			for i := 0; i < n; i++ {
				v := tt.min + int64(i)%(tt.max-tt.min+1)
				writeIntLE(buf, i*elemSize, elemSize, v)
			}

			arr := &vortex.Array{
				DType:   schema.FieldTypes[0],
				Length:  n,
				Buffers: [][]byte{buf},
			}

			var out bytes.Buffer
			w, err := NewWriter(&out, schema, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := w.WriteChunk(map[string]*vortex.Array{"x": arr}); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			// Read back via Scan.
			r := bytes.NewReader(out.Bytes())
			vf, err := vortex.Open(r, int64(out.Len()))
			if err != nil {
				t.Fatal(err)
			}

			iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"x"}})
			if err != nil {
				t.Fatal(err)
			}

			totalRows := 0
			for iter.Next() {
				totalRows += iter.Result().RowCount
			}
			if err := iter.Err(); err != nil {
				t.Fatal(err)
			}

			if totalRows != n {
				t.Errorf("got %d rows, want %d", totalRows, n)
			}
			t.Logf("%s: %d rows, %d bytes ✓", tt.name, totalRows, out.Len())
		})
	}
}

// TestDictStringLongValues tests dictionary encoding with strings > 12 bytes (out-of-line).
func TestDictStringLongValues(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"msg"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypeUtf8},
		},
	}

	// 50 values, 3 unique long strings.
	templates := []string{
		"this is a longer string that exceeds twelve bytes",
		"another example of a long dictionary value here",
		"and yet another that is also longer than twelve characters",
	}
	strs := make([]string, 50)
	for i := range strs {
		strs[i] = templates[i%3]
	}

	arr := makeStringArray(strs)

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(map[string]*vortex.Array{"msg": arr}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Read back and verify every value.
	r := bytes.NewReader(out.Bytes())
	vf, err := vortex.Open(r, int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}

	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"msg"}})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for iter.Next() {
		a := iter.Result().Columns["msg"]
		if a == nil {
			continue
		}
		s, err := encoding.StringValues(a)
		if err != nil {
			t.Fatalf("StringValues: %v", err)
		}
		got = append(got, s...)
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != 50 {
		t.Fatalf("got %d, want 50", len(got))
	}
	for i, want := range strs {
		if got[i] != want {
			t.Errorf("msg[%d] = %q, want %q", i, got[i], want)
		}
	}
	t.Logf("dict long strings: 50 rows, 3 unique, %d bytes ✓", out.Len())
}

// TestPackFastLanesRoundTrip directly tests pack → unpack for various configs.
func TestPackFastLanesRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		n        int
		elemSize int
		bw       int
	}{
		{"u32_bw7_1024", 1024, 4, 7},
		{"u32_bw3_100", 100, 4, 3},
		{"u32_bw15_2048", 2048, 4, 15},
		{"u16_bw5_1024", 1024, 2, 5},
		{"u64_bw10_1024", 1024, 8, 10},
		{"u8_bw4_1024", 1024, 1, 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mask := uint64((1 << uint(tt.bw)) - 1)
			values := make([]uint64, tt.n)
			for i := range values {
				values[i] = uint64(i*17+3) & mask
			}

			packed := packFastLanes(values, tt.bw, tt.elemSize)

			// Unpack using the decoder.
			unpacked, err := encoding.ExportUnpackFastLanesTyped(packed, tt.bw, 0, tt.n, tt.elemSize)
			if err != nil {
				t.Fatalf("unpack: %v", err)
			}

			// Compare.
			for i := 0; i < tt.n; i++ {
				var got uint64
				off := i * tt.elemSize
				switch tt.elemSize {
				case 1:
					got = uint64(unpacked[off])
				case 2:
					got = uint64(binary.LittleEndian.Uint16(unpacked[off:]))
				case 4:
					got = uint64(binary.LittleEndian.Uint32(unpacked[off:]))
				case 8:
					got = binary.LittleEndian.Uint64(unpacked[off:])
				}
				if got != values[i] {
					t.Fatalf("[%d] got %d, want %d", i, got, values[i])
				}
			}
			t.Logf("%s: pack → unpack round-trip ✓ (%d bytes packed)", tt.name, len(packed))
		})
	}
}

// --- Helpers ---

func makeStringArray(strings []string) *vortex.Array {
	viewsBuf := make([]byte, len(strings)*16)
	var dataBuf []byte

	for i, s := range strings {
		off := i * 16
		binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
		if len(s) <= 12 {
			copy(viewsBuf[off+4:], s)
		} else {
			copy(viewsBuf[off+4:off+8], s[:4])
			binary.LittleEndian.PutUint32(viewsBuf[off+8:], 0)
			binary.LittleEndian.PutUint32(viewsBuf[off+12:], uint32(len(dataBuf)))
			dataBuf = append(dataBuf, s...)
		}
	}

	bufs := [][]byte{viewsBuf}
	if len(dataBuf) > 0 {
		bufs = append(bufs, dataBuf)
	}

	return &vortex.Array{
		DType:   &vortex.DType{Kind: vortex.DTypeUtf8},
		Length:  len(strings),
		Buffers: bufs,
	}
}

func writeIntLE(buf []byte, off, size int, val int64) {
	switch size {
	case 2:
		binary.LittleEndian.PutUint16(buf[off:], uint16(val))
	case 4:
		binary.LittleEndian.PutUint32(buf[off:], uint32(val))
	case 8:
		binary.LittleEndian.PutUint64(buf[off:], uint64(val))
	}
}

// -----------------------------------------------------------------------
// Pcodec compression tests
// -----------------------------------------------------------------------

// TestPcoFloatRoundTrip verifies pcodec compression for float columns.
// Floats are the primary pcodec use case since BitPacking doesn't handle them.
func TestPcoFloatRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"f64_col"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
		},
	}

	// Sequential floats with narrow range compress well via pcodec baseline.
	n := 2000
	buf := make([]byte, n*8)
	for i := 0; i < n; i++ {
		v := 100.0 + float64(i)*0.01 // [100.00, 119.99]
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = w.WriteChunk(map[string]*vortex.Array{
		"f64_col": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{buf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	rawSize := n * 8
	ratio := float64(len(data)) / float64(rawSize) * 100
	t.Logf("wrote %d bytes for %d f64 values (raw=%d, ratio=%.1f%%)",
		len(data), n, rawSize, ratio)

	// Read back and verify values exactly.
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"f64_col"}})
	if err != nil {
		t.Fatal(err)
	}

	var allValues []float64
	for iter.Next() {
		arr := iter.Result().Columns["f64_col"]
		if arr != nil && len(arr.Buffers) > 0 {
			for j := 0; j < arr.Length; j++ {
				bits := binary.LittleEndian.Uint64(arr.Buffers[0][j*8:])
				allValues = append(allValues, math.Float64frombits(bits))
			}
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	if len(allValues) != n {
		t.Fatalf("got %d values, want %d", len(allValues), n)
	}
	for i, got := range allValues {
		want := 100.0 + float64(i)*0.01
		if got != want {
			t.Errorf("index %d: got %v, want %v", i, got, want)
			break
		}
	}
}

// TestPcoF32RoundTrip verifies pcodec compression for f32 columns.
func TestPcoF32RoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"f32_col"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF32},
		},
	}

	n := 1000
	buf := make([]byte, n*4)
	for i := 0; i < n; i++ {
		v := float32(50.0) + float32(i)*0.1 // [50.0, 149.9]
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = w.WriteChunk(map[string]*vortex.Array{
		"f32_col": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{buf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	rawSize := n * 4
	t.Logf("wrote %d bytes for %d f32 values (raw=%d, ratio=%.1f%%)",
		len(data), n, rawSize, float64(len(data))/float64(rawSize)*100)

	// Verify round-trip with exact value comparison.
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"f32_col"}})
	if err != nil {
		t.Fatal(err)
	}
	var allValues []float32
	for iter.Next() {
		arr := iter.Result().Columns["f32_col"]
		if arr != nil && len(arr.Buffers) > 0 {
			for j := 0; j < arr.Length; j++ {
				bits := binary.LittleEndian.Uint32(arr.Buffers[0][j*4:])
				allValues = append(allValues, math.Float32frombits(bits))
			}
		}
	}
	if len(allValues) != n {
		t.Fatalf("got %d values, want %d", len(allValues), n)
	}
	for i, got := range allValues {
		want := float32(50.0) + float32(i)*0.1
		if got != want {
			t.Errorf("index %d: got %v, want %v", i, got, want)
			break
		}
	}
}

// TestPcoIntWideRange verifies pcodec handles integers with wide range
// (where BitPacking doesn't help because bw > 75% of type width).
func TestPcoIntWideRange(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"wide_col"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
		},
	}

	// i64 values in narrow range: BitPacking saves >25% but pcodec saves even more.
	// Range = 2000, needs 11 bits. For i64 (64 bits), bw=11 < 48 (75% of 64),
	// so BitPacking WOULD apply. Use EnableBitPacking=false to test pcodec path.
	n := 2000
	buf := make([]byte, n*8)
	for i := 0; i < n; i++ {
		v := int64(1000000 + i)
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
	}

	opts := DefaultOptions()
	opts.EnableBitPacking = false // Force pcodec path for integers.

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, opts)
	if err != nil {
		t.Fatal(err)
	}
	err = w.WriteChunk(map[string]*vortex.Array{
		"wide_col": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{buf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	rawSize := n * 8
	ratio := float64(len(data)) / float64(rawSize) * 100
	t.Logf("wrote %d bytes for %d i64 values (raw=%d, ratio=%.1f%%)",
		len(data), n, rawSize, ratio)

	// Pcodec with narrow-range i64 should compress well.
	if ratio > 50 {
		t.Errorf("expected significant compression, got ratio=%.1f%%", ratio)
	}

	// Verify round-trip.
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"wide_col"}})
	if err != nil {
		t.Fatal(err)
	}

	var allValues []int64
	for iter.Next() {
		arr := iter.Result().Columns["wide_col"]
		if arr != nil && len(arr.Buffers) > 0 {
			for j := 0; j < arr.Length; j++ {
				v := int64(binary.LittleEndian.Uint64(arr.Buffers[0][j*8:]))
				allValues = append(allValues, v)
			}
		}
	}
	if len(allValues) != n {
		t.Fatalf("got %d values, want %d", len(allValues), n)
	}
	for i, got := range allValues {
		want := int64(1000000 + i)
		if got != want {
			t.Errorf("index %d: got %d, want %d", i, got, want)
			break
		}
	}
}

// TestPcoMixedWithBitPacking verifies that BitPacking is preferred for narrow-range
// integers while pcodec handles floats.
func TestPcoMixedWithBitPacking(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"narrow", "floats"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
		},
	}

	n := 1000
	narrowBuf := make([]byte, n*4)
	floatBuf := make([]byte, n*8)

	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(narrowBuf[i*4:], uint32(int32(100+i%50)))
		binary.LittleEndian.PutUint64(floatBuf[i*8:], math.Float64bits(200.0+float64(i)*0.005))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	err = w.WriteChunk(map[string]*vortex.Array{
		"narrow": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{narrowBuf}},
		"floats": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{floatBuf}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	rawSize := n*4 + n*8
	t.Logf("wrote %d bytes for 2 columns (%d rows, raw=%d, ratio=%.1f%%)",
		len(data), n, rawSize, float64(len(data))/float64(rawSize)*100)

	// Verify round-trip for both columns.
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	iter, err := vf.Scan(vortex.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var totalRows int
	for iter.Next() {
		res := iter.Result()
		totalRows += res.RowCount

		// Verify narrow ints.
		if arr := res.Columns["narrow"]; arr != nil && len(arr.Buffers) > 0 {
			for j := 0; j < arr.Length; j++ {
				got := int32(binary.LittleEndian.Uint32(arr.Buffers[0][j*4:]))
				want := int32(100 + j%50)
				if got != want {
					t.Errorf("narrow[%d]: got %d, want %d", j, got, want)
					break
				}
			}
		}

		// Verify floats.
		if arr := res.Columns["floats"]; arr != nil && len(arr.Buffers) > 0 {
			for j := 0; j < arr.Length; j++ {
				bits := binary.LittleEndian.Uint64(arr.Buffers[0][j*8:])
				got := math.Float64frombits(bits)
				want := 200.0 + float64(j)*0.005
				if got != want {
					t.Errorf("floats[%d]: got %v, want %v", j, got, want)
					break
				}
			}
		}
	}
	if totalRows != n {
		t.Errorf("got %d rows, want %d", totalRows, n)
	}
}
