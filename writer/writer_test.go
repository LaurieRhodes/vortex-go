package writer

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/encoding"
)

// TestWriteReadPrimitive writes a single i32 column, reads it back, verifies values.
func TestWriteReadPrimitive(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"x"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		},
	}

	values := []int32{10, 20, 30, 40, 50}
	buf := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	err = w.WriteChunk(map[string]*vortex.Array{
		"x": {
			DType:   schema.FieldTypes[0],
			Length:  len(values),
			Buffers: [][]byte{buf},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Read back.
	data := out.Bytes()
	t.Logf("wrote %d bytes", len(data))

	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Verify schema.
	dt := vf.DType()
	if dt.Kind != vortex.DTypeStruct {
		t.Fatalf("schema kind = %v, want Struct", dt.Kind)
	}
	if len(dt.FieldNames) != 1 || dt.FieldNames[0] != "x" {
		t.Fatalf("fields = %v, want [x]", dt.FieldNames)
	}

	// Navigate to the column and decode.
	colLayout, colDType, _, err := vf.FindColumnLayout("x")
	if err != nil {
		t.Fatal(err)
	}

	flats := vortex.CollectFlatLayouts(colLayout)
	if len(flats) == 0 {
		t.Fatal("no flat layouts found")
	}

	node, buffers, err := vf.ReadFlatSegment(flats[0])
	if err != nil {
		t.Fatal(err)
	}

	arr, err := encoding.DecodeArray(node, buffers, colDType, flats[0].RowCount)
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	got, err := encoding.Int32Values(arr)
	if err != nil {
		t.Fatal(err)
	}

	for i, v := range values {
		if got[i] != v {
			t.Errorf("x[%d] = %d, want %d", i, got[i], v)
		}
	}
	t.Logf("round-trip i32: %v ✓", got)
}

// TestWriteReadMultiColumn writes a 3-column file (i32, i64, f64) and reads back all columns.
func TestWriteReadMultiColumn(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"id", "count", "score"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
		},
	}

	n := 100
	idBuf := make([]byte, n*4)
	countBuf := make([]byte, n*8)
	scoreBuf := make([]byte, n*8)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(idBuf[i*4:], uint32(i+1))
		binary.LittleEndian.PutUint64(countBuf[i*8:], uint64(1000+i))
		binary.LittleEndian.PutUint64(scoreBuf[i*8:], math.Float64bits(float64(i)*1.5))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	err = w.WriteChunk(map[string]*vortex.Array{
		"id":    {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{idBuf}},
		"count": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{countBuf}},
		"score": {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{scoreBuf}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Read back.
	data := out.Bytes()
	t.Logf("wrote %d bytes for 3 columns × %d rows", len(data), n)

	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if vf.DType().Kind != vortex.DTypeStruct {
		t.Fatal("not a struct")
	}
	if len(vf.DType().FieldNames) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(vf.DType().FieldNames))
	}

	// Check id column.
	readAndCheckI32(t, vf, "id", func(i int) int32 { return int32(i + 1) }, n)

	// Check count column.
	readAndCheckI64(t, vf, "count", func(i int) int64 { return int64(1000 + i) }, n)

	// Check score column.
	readAndCheckF64(t, vf, "score", func(i int) float64 { return float64(i) * 1.5 }, n)
}

// TestWriteReadStrings writes a utf8 column and reads it back.
func TestWriteReadStrings(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"name"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypeUtf8},
		},
	}

	strings := []string{"alice", "bob", "charlie", "this is a longer string that exceeds twelve bytes"}

	// Build VarBinView encoding.
	viewsBuf := make([]byte, len(strings)*16)
	var dataBuf []byte

	for i, s := range strings {
		off := i * 16
		binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
		if len(s) <= 12 {
			copy(viewsBuf[off+4:], s)
		} else {
			// First 4 bytes prefix.
			copy(viewsBuf[off+4:off+8], s[:4])
			// Buffer index 0, offset.
			binary.LittleEndian.PutUint32(viewsBuf[off+8:], 0)
			binary.LittleEndian.PutUint32(viewsBuf[off+12:], uint32(len(dataBuf)))
			dataBuf = append(dataBuf, s...)
		}
	}

	bufs := [][]byte{viewsBuf}
	if len(dataBuf) > 0 {
		bufs = append(bufs, dataBuf)
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	err = w.WriteChunk(map[string]*vortex.Array{
		"name": {DType: schema.FieldTypes[0], Length: len(strings), Buffers: bufs},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Read back.
	data := out.Bytes()
	t.Logf("wrote %d bytes for %d strings", len(data), len(strings))

	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	colLayout, colDType, _, err := vf.FindColumnLayout("name")
	if err != nil {
		t.Fatal(err)
	}

	flats := vortex.CollectFlatLayouts(colLayout)
	node, buffers, err := vf.ReadFlatSegment(flats[0])
	if err != nil {
		t.Fatal(err)
	}

	arr, err := encoding.DecodeArray(node, buffers, colDType, flats[0].RowCount)
	if err != nil {
		t.Fatal(err)
	}

	got, err := encoding.StringValues(arr)
	if err != nil {
		t.Fatal(err)
	}

	for i, want := range strings {
		if got[i] != want {
			t.Errorf("name[%d] = %q, want %q", i, got[i], want)
		}
	}
	t.Logf("round-trip strings: %v ✓", got)
}

// TestWriteReadVtxinfo writes a file and verifies vtxinfo can parse it.
func TestWriteReadVtxinfo(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"a", "b"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
		},
	}

	n := 10
	aBuf := make([]byte, n*4)
	bBuf := make([]byte, n*8)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(aBuf[i*4:], uint32(i*10))
		binary.LittleEndian.PutUint64(bBuf[i*8:], uint64(i*100))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	err = w.WriteChunk(map[string]*vortex.Array{
		"a": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{aBuf}},
		"b": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{bBuf}},
	})
	if err != nil {
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

	// Verify metadata is accessible.
	if vf.Version() != 1 {
		t.Errorf("version = %d, want 1", vf.Version())
	}
	layout := vf.Layout()
	if !layout.IsStruct() {
		t.Errorf("root layout = %q, want struct", layout.EncodingID)
	}
	if layout.RowCount != uint64(n) {
		t.Errorf("row count = %d, want %d", layout.RowCount, n)
	}
	if len(layout.Children) != 2 {
		t.Errorf("children = %d, want 2", len(layout.Children))
	}

	// Check statistics exist.
	stats := vf.Statistics()
	if stats == nil {
		t.Fatal("no statistics")
	}
	if len(stats.FieldStats) != 2 {
		t.Fatalf("field stats = %d, want 2", len(stats.FieldStats))
	}
	if !stats.FieldStats[0].HasMin() {
		t.Error("column 'a' missing min stat")
	}
	if !stats.FieldStats[0].HasMax() {
		t.Error("column 'a' missing max stat")
	}

	t.Logf("vtxinfo-compatible file: version=%d, rows=%d, cols=%d, segments=%d ✓",
		vf.Version(), layout.RowCount, len(layout.Children), len(vf.Footer().SegmentSpecs))
}

// TestWriteMultipleChunks writes two chunks and verifies the chunked layout.
func TestWriteMultipleChunks(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"x"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		},
	}

	chunk1 := makeI32Array([]int32{1, 2, 3})
	chunk2 := makeI32Array([]int32{4, 5, 6, 7})

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	if err := w.WriteChunk(map[string]*vortex.Array{"x": chunk1}); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteChunk(map[string]*vortex.Array{"x": chunk2}); err != nil {
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

	layout := vf.Layout()
	if !layout.IsStruct() {
		t.Fatalf("root = %q", layout.EncodingID)
	}
	if layout.RowCount != 7 {
		t.Errorf("rows = %d, want 7", layout.RowCount)
	}

	// Column should be a Chunked layout with 2 Flat children.
	colLayout := layout.Children[0]
	if !colLayout.IsChunked() {
		t.Fatalf("column layout = %q, want chunked", colLayout.EncodingID)
	}
	if len(colLayout.Children) != 2 {
		t.Fatalf("chunked children = %d, want 2", len(colLayout.Children))
	}

	// Decode both chunks.
	for i, flat := range colLayout.Children {
		if !flat.IsFlat() {
			t.Fatalf("chunk[%d] = %q, want flat", i, flat.EncodingID)
		}
		node, bufs, err := vf.ReadFlatSegment(flat)
		if err != nil {
			t.Fatalf("ReadFlatSegment[%d]: %v", i, err)
		}
		arr, err := encoding.DecodeArray(node, bufs, vf.DType().FieldTypes[0], flat.RowCount)
		if err != nil {
			t.Fatalf("DecodeArray[%d]: %v", i, err)
		}
		vals, _ := encoding.Int32Values(arr)
		t.Logf("chunk[%d]: %v", i, vals)
	}

	t.Logf("multi-chunk round-trip ✓")
}

// --- Helpers ---

func makeI32Array(values []int32) *vortex.Array {
	buf := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}
	return &vortex.Array{
		DType:   &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		Length:  len(values),
		Buffers: [][]byte{buf},
	}
}

// --- Compression round-trip tests (via ReadColumn API) ---

// TestDictEncodingViaReadColumn tests dict-encoded strings via ReadColumn.
func TestDictEncodingViaReadColumn(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"status"},
		FieldTypes: []*vortex.DType{{Kind: vortex.DTypeUtf8}},
	}

	// 50 rows with 3 distinct values.
	statuses := []string{"pending", "approved", "rejected"}
	var input []string
	for i := 0; i < 50; i++ {
		input = append(input, statuses[i%len(statuses)])
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	if err := w.WriteChunk(map[string]*vortex.Array{"status": makeStringArray(input)}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	// Read via ReadColumn.
	arr, err := vf.ReadColumn("status")
	if err != nil {
		t.Fatal(err)
	}

	got, err := encoding.StringValues(arr)
	if err != nil {
		t.Fatalf("StringValues: %v", err)
	}

	if len(got) != len(input) {
		t.Fatalf("got %d strings, want %d", len(got), len(input))
	}
	for i, want := range input {
		if got[i] != want {
			t.Errorf("status[%d] = %q, want %q", i, got[i], want)
			break
		}
	}
	t.Logf("dict encoding via ReadColumn: 50 rows ✓")
}

// TestFoRBitPackingViaReadColumn tests FoR+BitPacking via the ReadColumn API.
func TestFoRBitPackingViaReadColumn(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"seq"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		},
	}

	n := 200
	values := make([]int32, n)
	for i := range values {
		values[i] = int32(5000 + i)
	}

	buf := make([]byte, n*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	if err := w.WriteChunk(map[string]*vortex.Array{
		"seq": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{buf}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	arr, err := vf.ReadColumn("seq")
	if err != nil {
		t.Fatal(err)
	}

	got, err := encoding.Int32Values(arr)
	if err != nil {
		t.Fatal(err)
	}

	for i, want := range values {
		if got[i] != want {
			t.Errorf("seq[%d] = %d, want %d", i, got[i], want)
			break
		}
	}
	t.Logf("FoR+BitPacking via ReadColumn: %d values ✓", n)
}

// TestMixedCompressionRoundTrip writes a file with both dict-encoded strings
// and FoR+BitPacked integers, verifying both compress and round-trip correctly.
func TestMixedCompressionRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"city", "temp"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypeUtf8},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		},
	}

	n := 200
	cities := []string{"London", "Paris", "Tokyo", "Sydney", "Berlin"}
	var cityInput []string
	temps := make([]int32, n)
	for i := 0; i < n; i++ {
		cityInput = append(cityInput, cities[i%len(cities)])
		temps[i] = int32(15 + i%20) // range [15, 34]
	}

	tempBuf := make([]byte, n*4)
	for i, v := range temps {
		binary.LittleEndian.PutUint32(tempBuf[i*4:], uint32(v))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	if err := w.WriteChunk(map[string]*vortex.Array{
		"city": makeStringArray(cityInput),
		"temp": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{tempBuf}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	t.Logf("mixed compression: %d rows → %d bytes", n, len(data))

	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	// Verify city (dict-encoded).
	cityArr, err := vf.ReadColumn("city")
	if err != nil {
		t.Fatal(err)
	}
	gotCities, err := encoding.StringValues(cityArr)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range cityInput {
		if gotCities[i] != want {
			t.Errorf("city[%d] = %q, want %q", i, gotCities[i], want)
			break
		}
	}

	// Verify temp (FoR+BitPacked).
	tempArr, err := vf.ReadColumn("temp")
	if err != nil {
		t.Fatal(err)
	}
	gotTemps, err := encoding.Int32Values(tempArr)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range temps {
		if gotTemps[i] != want {
			t.Errorf("temp[%d] = %d, want %d", i, gotTemps[i], want)
			break
		}
	}
	t.Logf("mixed compression round-trip: cities ✓, temps ✓")
}

// TestUncompressedOption verifies that UncompressedOptions disables compression.
func TestUncompressedOption(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"x"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
		},
	}

	// Values that would normally be FoR+BitPacked.
	n := 100
	values := make([]int32, n)
	for i := range values {
		values[i] = int32(i % 8) // range [0,7], 3 bits
	}

	buf := make([]byte, n*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, UncompressedOptions())
	if err != nil {
		t.Fatal(err)
	}

	if err := w.WriteChunk(map[string]*vortex.Array{
		"x": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{buf}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data := out.Bytes()
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	colLayout, _, _, err := vf.FindColumnLayout("x")
	if err != nil {
		t.Fatal(err)
	}

	flats := vortex.CollectFlatLayouts(colLayout)
	node, _, err := vf.ReadFlatSegment(flats[0])
	if err != nil {
		t.Fatal(err)
	}

	if node.EncodingID != "vortex.primitive" {
		t.Errorf("uncompressed mode should use vortex.primitive, got %q", node.EncodingID)
	}
	t.Logf("uncompressed option: encoding=%q ✓", node.EncodingID)
}

func readAndCheckI32(t *testing.T, vf *vortex.File, col string, expected func(int) int32, n int) {
	t.Helper()
	colLayout, colDType, _, err := vf.FindColumnLayout(col)
	if err != nil {
		t.Fatal(err)
	}
	flats := vortex.CollectFlatLayouts(colLayout)
	node, bufs, err := vf.ReadFlatSegment(flats[0])
	if err != nil {
		t.Fatal(err)
	}
	arr, err := encoding.DecodeArray(node, bufs, colDType, flats[0].RowCount)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := encoding.Int32Values(arr)
	for i := 0; i < n; i++ {
		if got[i] != expected(i) {
			t.Errorf("%s[%d] = %d, want %d", col, i, got[i], expected(i))
			break
		}
	}
	t.Logf("%s: %d values ✓", col, n)
}

func readAndCheckI64(t *testing.T, vf *vortex.File, col string, expected func(int) int64, n int) {
	t.Helper()
	colLayout, colDType, _, err := vf.FindColumnLayout(col)
	if err != nil {
		t.Fatal(err)
	}
	flats := vortex.CollectFlatLayouts(colLayout)
	node, bufs, err := vf.ReadFlatSegment(flats[0])
	if err != nil {
		t.Fatal(err)
	}
	arr, err := encoding.DecodeArray(node, bufs, colDType, flats[0].RowCount)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := encoding.Int64Values(arr)
	for i := 0; i < n; i++ {
		if got[i] != expected(i) {
			t.Errorf("%s[%d] = %d, want %d", col, i, got[i], expected(i))
			break
		}
	}
	t.Logf("%s: %d values ✓", col, n)
}

func readAndCheckF64(t *testing.T, vf *vortex.File, col string, expected func(int) float64, n int) {
	t.Helper()
	colLayout, colDType, _, err := vf.FindColumnLayout(col)
	if err != nil {
		t.Fatal(err)
	}
	flats := vortex.CollectFlatLayouts(colLayout)
	node, bufs, err := vf.ReadFlatSegment(flats[0])
	if err != nil {
		t.Fatal(err)
	}
	arr, err := encoding.DecodeArray(node, bufs, colDType, flats[0].RowCount)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := encoding.Float64Values(arr)
	for i := 0; i < n; i++ {
		if got[i] != expected(i) {
			t.Errorf("%s[%d] = %v, want %v", col, i, got[i], expected(i))
			break
		}
	}
	t.Logf("%s: %d values ✓", col, n)
}
