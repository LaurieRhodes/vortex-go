package encoding

import (
	"os"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// openTestFile opens a real Vortex reference file from testdata/.
func openTestFile(t *testing.T, name string) *vortex.File {
	t.Helper()
	path := "../testdata/" + name
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("reference file not available: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatalf("vortex.Open(%s): %v", name, err)
	}
	return vf
}

// readColumnData navigates the layout tree for a column and decodes all flat segments.
// Returns decoded arrays (one per flat chunk).
func readColumnData(t *testing.T, vf *vortex.File, colName string) []*vortex.Array {
	t.Helper()

	colLayout, colDType, _, err := vf.FindColumnLayout(colName)
	if err != nil {
		t.Fatalf("FindColumnLayout(%q): %v", colName, err)
	}

	dataLayout, dictLayout := vortex.NavigateToData(colLayout)

	// If there's a Dict layout, decode values + codes and reconstruct.
	if dictLayout != nil {
		return readDictLayoutData(t, vf, colLayout, dataLayout, dictLayout, colDType)
	}

	return readFlatLayoutData(t, vf, dataLayout, colDType)
}

// readFlatLayoutData reads flat segments from a data layout.
func readFlatLayoutData(t *testing.T, vf *vortex.File, dataLayout *vortex.Layout, colDType *vortex.DType) []*vortex.Array {
	t.Helper()

	flats := vortex.CollectFlatLayouts(dataLayout)
	if len(flats) == 0 {
		t.Fatal("no flat layouts found")
	}

	var arrays []*vortex.Array
	for _, flat := range flats {
		node, buffers, err := vf.ReadFlatSegment(flat)
		if err != nil {
			t.Fatalf("ReadFlatSegment: %v", err)
		}

		arr, err := DecodeArray(node, buffers, colDType, flat.RowCount)
		if err != nil {
			t.Fatalf("DecodeArray (encoding=%s): %v", node.EncodingID, err)
		}
		arrays = append(arrays, arr)
	}
	return arrays
}

// readDictLayoutData handles Dict layout: decode values + codes from separate segments,
// then reconstruct the full column by looking up codes in the values dictionary.
func readDictLayoutData(t *testing.T, vf *vortex.File, colLayout *vortex.Layout, codesLayout, valuesLayout *vortex.Layout, colDType *vortex.DType) []*vortex.Array {
	t.Helper()

	// Try to extract the codes PType from the Dict layout metadata (protobuf).
	dictCodesPType := vortex.ParseDictLayoutCodesPType(colLayout)

	// Decode dictionary values.
	valFlats := vortex.CollectFlatLayouts(valuesLayout)
	if len(valFlats) == 0 {
		t.Fatal("no flat layouts for dict values")
	}
	var valuesArrays []*vortex.Array
	for _, flat := range valFlats {
		node, buffers, err := vf.ReadFlatSegment(flat)
		if err != nil {
			t.Fatalf("ReadFlatSegment(values): %v", err)
		}
		arr, err := DecodeArray(node, buffers, colDType, flat.RowCount)
		if err != nil {
			t.Fatalf("DecodeArray(values, encoding=%s): %v", node.EncodingID, err)
		}
		valuesArrays = append(valuesArrays, arr)
	}

	// Decode codes. Detect the actual PType from the segment data.
	codeFlats := vortex.CollectFlatLayouts(codesLayout)
	if len(codeFlats) == 0 {
		t.Fatal("no flat layouts for dict codes")
	}
	var codesArrays []*vortex.Array
	for _, flat := range codeFlats {
		node, buffers, err := vf.ReadFlatSegment(flat)
		if err != nil {
			t.Fatalf("ReadFlatSegment(codes): %v", err)
		}

		// Use dict layout metadata for codes PType if available,
		// otherwise fall back to heuristic detection.
		var codesDType *vortex.DType
		if dictCodesPType >= 0 {
			codesDType = &vortex.DType{Kind: vortex.DTypePrimitive, PType: dictCodesPType}
		} else {
			codesDType = detectCodesPType(node, buffers, flat.RowCount, valuesLayout.RowCount)
		}

		arr, err := DecodeArray(node, buffers, codesDType, flat.RowCount)
		if err != nil {
			t.Fatalf("DecodeArray(codes, encoding=%s): %v", node.EncodingID, err)
		}
		codesArrays = append(codesArrays, arr)
	}

	// Reconstruct: combine values + codes into the final array.
	// For now handle the common case of a single values array and single codes array.
	if len(valuesArrays) == 0 || len(codesArrays) == 0 {
		t.Fatal("empty values or codes arrays")
	}

	valuesArr := valuesArrays[0]
	codesArr := codesArrays[0]

	result := &vortex.Array{
		DType:    colDType,
		Length:   codesArr.Length,
		Children: []*vortex.Array{codesArr, valuesArr},
		Stats:    codesArr.Stats,
	}

	return []*vortex.Array{result}
}

// detectCodesPType determines the PType for dictionary codes by examining the segment data.
// For primitive arrays, the buffer size / row count gives the element width directly.
// For bitpacked or other encodings, we fall back to the smallest PType that fits the dict size.
func detectCodesPType(node *vortex.ArrayNode, buffers [][]byte, rowCount, dictSize uint64) *vortex.DType {
	if node.EncodingID == "vortex.primitive" && len(node.BufferIndices) > 0 {
		idx := node.BufferIndices[0]
		if int(idx) < len(buffers) && rowCount > 0 {
			bytesPerElem := uint64(len(buffers[idx])) / rowCount
			switch bytesPerElem {
			case 1:
				return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU8}
			case 2:
				return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU16}
			case 4:
				return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
			case 8:
				return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU64}
			}
		}
	}

	// Fallback: infer from dictionary size.
	ptype := vortex.PTypeU8
	if dictSize > 255 {
		ptype = vortex.PTypeU16
	}
	if dictSize > 65535 {
		ptype = vortex.PTypeU32
	}
	return &vortex.DType{Kind: vortex.DTypePrimitive, PType: ptype}
}

// DictLookupStrings resolves dictionary codes to string values.
// The dict array must have Children: [codes, values] where codes is integer
// and values is a VarBinView string array.
func DictLookupStrings(dictArr *vortex.Array) ([]string, error) {
	if len(dictArr.Children) < 2 {
		return nil, nil
	}
	codesArr := dictArr.Children[0]
	valuesArr := dictArr.Children[1]

	// Get the dictionary strings.
	dictStrs, err := StringValues(valuesArr)
	if err != nil {
		return nil, err
	}

	// Get the codes as integers.
	codes, err := readIntSlice(codesArr)
	if err != nil {
		return nil, err
	}

	// Reconstruct.
	result := make([]string, len(codes))
	for i, code := range codes {
		if code >= 0 && code < len(dictStrs) {
			result[i] = dictStrs[code]
		}
	}
	return result, nil
}

// --- Tests against real reference files ---

func TestReadSmallVtx(t *testing.T) {
	vf := openTestFile(t, "small.vtx")

	dt := vf.DType()
	if dt.Kind != vortex.DTypeStruct {
		t.Fatalf("expected struct dtype, got %v", dt.Kind)
	}
	if len(dt.FieldNames) != 1 || dt.FieldNames[0] != "x" {
		t.Fatalf("expected field 'x', got %v", dt.FieldNames)
	}

	arrays := readColumnData(t, vf, "x")
	if len(arrays) == 0 {
		t.Fatal("no arrays returned")
	}

	arr := arrays[0]
	if arr.Length != 5 {
		t.Fatalf("expected 5 rows, got %d", arr.Length)
	}

	vals, err := Int32Values(arr)
	if err != nil {
		t.Fatalf("Int32Values: %v", err)
	}

	expected := []int32{10, 20, 30, 40, 50}
	for i, v := range expected {
		if vals[i] != v {
			t.Errorf("x[%d] = %d, want %d", i, vals[i], v)
		}
	}
	t.Logf("small.vtx x values: %v ✓", vals)
}

func TestReadIntegersVtx(t *testing.T) {
	vf := openTestFile(t, "integers.vtx")

	dt := vf.DType()
	if len(dt.FieldNames) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(dt.FieldNames))
	}

	// i32_col — vortex.sequence.
	arrays := readColumnData(t, vf, "i32_col")
	arr := arrays[0]
	vals32, err := Int32Values(arr)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals32 {
		if v != int32(i+1) {
			t.Errorf("i32_col[%d] = %d, want %d", i, v, i+1)
			break
		}
	}
	t.Logf("integers.vtx i32_col: [1..100] ✓")

	// i64_col — vortex.sequence.
	arrays64 := readColumnData(t, vf, "i64_col")
	vals64, err := Int64Values(arrays64[0])
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals64 {
		if v != int64(1000+i) {
			t.Errorf("i64_col[%d] = %d, want %d", i, v, 1000+i)
			break
		}
	}
	t.Logf("integers.vtx i64_col: [1000..1099] ✓")
}

func TestReadMixedVtxId(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	arrays := readColumnData(t, vf, "id")
	vals, err := Int32Values(arrays[0])
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals {
		if v != int32(i+1) {
			t.Errorf("id[%d] = %d, want %d", i, v, i+1)
			break
		}
	}
	t.Logf("mixed.vtx id: [1..100] ✓")
}

func TestReadMixedVtxActive(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	arrays := readColumnData(t, vf, "active")
	arr := arrays[0]
	if arr.Length != 100 {
		t.Fatalf("expected 100 rows, got %d", arr.Length)
	}
	bools, err := BoolValues(arr)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mixed.vtx active: first=%v, len=%d ✓", bools[0], len(bools))
}

func TestReadNullsVtxMaybeInt(t *testing.T) {
	vf := openTestFile(t, "nulls.vtx")

	arrays := readColumnData(t, vf, "maybe_int")
	arr := arrays[0]
	if arr.Length != 10 {
		t.Fatalf("expected 10 rows, got %d", arr.Length)
	}

	vals, err := Int32Values(arr)
	if err != nil {
		t.Fatal(err)
	}

	if len(arr.Children) == 0 {
		t.Fatal("expected validity child")
	}
	mask, err := BoolValues(arr.Children[0])
	if err != nil {
		t.Fatal(err)
	}

	nullCount := 0
	for _, v := range mask {
		if !v {
			nullCount++
		}
	}
	if nullCount != 5 {
		t.Errorf("expected 5 nulls, got %d", nullCount)
	}
	t.Logf("nulls.vtx maybe_int: vals=%v, mask=%v ✓", vals, mask)
}

func TestReadStringsVtx(t *testing.T) {
	// strings.vtx is a Rust-generated file with dict+fastlanes.bitpacked encoding.
	// It contains 1000 rows cycling through 5 names.
	vf := openTestFile(t, "strings.vtx")

	arrays := readColumnData(t, vf, "name")
	if len(arrays) == 0 {
		t.Fatal("no arrays returned")
	}

	arr := arrays[0]
	if arr.Length != 1000 {
		t.Fatalf("expected 1000 rows, got %d", arr.Length)
	}

	// This is a dict array: children[0]=codes, children[1]=values.
	strs, err := DictLookupStrings(arr)
	if err != nil {
		t.Fatalf("DictLookupStrings: %v", err)
	}
	if len(strs) != 1000 {
		t.Fatalf("expected 1000 strings, got %d", len(strs))
	}

	// The data cycles: "alice", "bob", "charlie", "diana", "eve" × 200.
	expected := []string{"alice", "bob", "charlie", "diana", "eve"}
	for i, s := range strs {
		want := expected[i%len(expected)]
		if s != want {
			t.Errorf("name[%d] = %q, want %q", i, s, want)
			break
		}
	}
	t.Logf("strings.vtx name: 1000 strings cycling %v ✓", expected)
}

func TestReadMixedVtxName(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	arrays := readColumnData(t, vf, "name")
	if len(arrays) == 0 {
		t.Fatal("no arrays returned")
	}

	arr := arrays[0]
	if arr.Length != 100 {
		t.Fatalf("expected 100 rows, got %d", arr.Length)
	}

	strs, err := DictLookupStrings(arr)
	if err != nil {
		t.Fatalf("DictLookupStrings: %v", err)
	}

	// mixed.vtx name cycles: "alice", "bob", "charlie", "diana", "eve" × 20.
	expected := []string{"alice", "bob", "charlie", "diana", "eve"}
	for i, s := range strs {
		want := expected[i%5]
		if s != want {
			t.Errorf("name[%d] = %q, want %q", i, s, want)
			break
		}
	}
	t.Logf("mixed.vtx name: 100 strings cycling 5 names ✓")
}

func TestReadMixedVtxScore(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	arrays := readColumnData(t, vf, "score")
	if len(arrays) == 0 {
		t.Fatal("no arrays returned")
	}

	arr := arrays[0]
	if arr.Length != 100 {
		t.Fatalf("expected 100 rows, got %d", arr.Length)
	}

	// Score is f64 via dict layout. The dict values should be f64 primitives.
	// Verify we can at least access the dict structure.
	if len(arr.Children) < 2 {
		t.Fatalf("expected dict children, got %d children", len(arr.Children))
	}

	codesArr := arr.Children[0]
	valuesArr := arr.Children[1]
	t.Logf("mixed.vtx score: codes.len=%d, values.len=%d", codesArr.Length, valuesArr.Length)

	// Get the dictionary f64 values.
	dictVals, err := Float64Values(valuesArr)
	if err != nil {
		t.Fatalf("Float64Values: %v", err)
	}
	t.Logf("  dict values: %v", dictVals)

	// Get codes and reconstruct.
	codes, err := readIntSlice(codesArr)
	if err != nil {
		t.Fatalf("readIntSlice(codes): %v", err)
	}

	scores := make([]float64, len(codes))
	for i, c := range codes {
		scores[i] = dictVals[c]
	}

	// mixed.vtx score cycles: 1.5, 2.7, 3.14, 4.0, 5.5 × 20.
	expectedScores := []float64{1.5, 2.7, 3.14, 4.0, 5.5}
	for i, s := range scores {
		want := expectedScores[i%5]
		if s != want {
			t.Errorf("score[%d] = %v, want %v", i, s, want)
			break
		}
	}
	t.Logf("  first 10 scores: %v ✓", scores[:10])
}

func TestReadNullsVtxMaybeStr(t *testing.T) {
	vf := openTestFile(t, "nulls.vtx")

	arrays := readColumnData(t, vf, "maybe_str")
	if len(arrays) == 0 {
		t.Fatal("no arrays returned")
	}

	arr := arrays[0]
	if arr.Length != 10 {
		t.Fatalf("expected 10 rows, got %d", arr.Length)
	}

	// maybe_str uses FSST encoding directly (not dict), with validity child.
	strs, err := StringValues(arr)
	if err != nil {
		t.Fatalf("StringValues: %v", err)
	}
	t.Logf("nulls.vtx maybe_str strings: %v", strs)

	// Check validity.
	if len(arr.Children) > 0 {
		mask, err := BoolValues(arr.Children[0])
		if err != nil {
			t.Fatalf("BoolValues(validity): %v", err)
		}
		nullCount := 0
		for _, v := range mask {
			if !v {
				nullCount++
			}
		}
		t.Logf("  validity: %v (%d nulls) ✓", mask, nullCount)
	}
}
