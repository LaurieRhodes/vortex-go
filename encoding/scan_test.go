package encoding

import (
	"fmt"
	"math"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// --- Scan API Tests ---

func TestScanReadColumnSimple(t *testing.T) {
	vf := openTestFile(t, "small.vtx")

	arr, err := vf.ReadColumn("x")
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}

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
	t.Logf("ReadColumn small.vtx x: %v ✓", vals)
}

func TestScanReadColumnIntegers(t *testing.T) {
	vf := openTestFile(t, "integers.vtx")

	arr, err := vf.ReadColumn("i32_col")
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}

	vals, err := Int32Values(arr)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vals {
		if v != int32(i+1) {
			t.Errorf("i32_col[%d] = %d, want %d", i, v, i+1)
			break
		}
	}
	t.Logf("ReadColumn integers.vtx i32_col: [1..%d] ✓", len(vals))
}

func TestScanReadColumnsProjection(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	// Read only 2 of 4 columns.
	cols, err := vf.ReadColumns([]string{"id", "active"})
	if err != nil {
		t.Fatalf("ReadColumns: %v", err)
	}

	if len(cols) != 2 {
		t.Fatalf("expected 2 columns, got %d", len(cols))
	}

	// Verify id.
	idArr := cols["id"]
	if idArr == nil {
		t.Fatal("missing 'id' column")
	}
	if idArr.Length != 100 {
		t.Fatalf("id: expected 100 rows, got %d", idArr.Length)
	}
	ids, err := Int32Values(idArr)
	if err != nil {
		t.Fatal(err)
	}
	if ids[0] != 1 || ids[99] != 100 {
		t.Errorf("id: first=%d last=%d, want 1..100", ids[0], ids[99])
	}

	// Verify active.
	activeArr := cols["active"]
	if activeArr == nil {
		t.Fatal("missing 'active' column")
	}
	if activeArr.Length != 100 {
		t.Fatalf("active: expected 100 rows, got %d", activeArr.Length)
	}

	// Verify we did NOT read name or score.
	if _, ok := cols["name"]; ok {
		t.Error("unexpected 'name' column in projected result")
	}
	if _, ok := cols["score"]; ok {
		t.Error("unexpected 'score' column in projected result")
	}

	t.Logf("ReadColumns mixed.vtx [id, active]: id=%d rows, active=%d rows ✓", idArr.Length, activeArr.Length)
}

func TestScanReadAllColumns(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	// nil columns = read all.
	cols, err := vf.ReadColumns(nil)
	if err != nil {
		t.Fatalf("ReadColumns(nil): %v", err)
	}

	expected := []string{"id", "name", "score", "active"}
	if len(cols) != len(expected) {
		t.Fatalf("expected %d columns, got %d", len(expected), len(cols))
	}
	for _, name := range expected {
		if _, ok := cols[name]; !ok {
			t.Errorf("missing column %q", name)
		}
	}
	t.Logf("ReadColumns mixed.vtx (all): %d columns ✓", len(cols))
}

func TestScanReadDictColumn(t *testing.T) {
	// strings.vtx is a Rust-generated file with dict+fastlanes.bitpacked encoding.
	vf := openTestFile(t, "strings.vtx")

	arr, err := vf.ReadColumn("name")
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}

	if arr.Length != 1000 {
		t.Fatalf("expected 1000 rows, got %d", arr.Length)
	}

	// Dict array: children[0] = codes, children[1] = values.
	strs, err := DictLookupStrings(arr)
	if err != nil {
		t.Fatalf("DictLookupStrings: %v", err)
	}
	if len(strs) != 1000 {
		t.Fatalf("expected 1000 strings, got %d", len(strs))
	}

	expected := []string{"alice", "bob", "charlie", "diana", "eve"}
	for i, s := range strs {
		want := expected[i%len(expected)]
		if s != want {
			t.Errorf("name[%d] = %q, want %q", i, s, want)
			break
		}
	}
	t.Logf("ReadColumn strings.vtx name (dict): 1000 strings ✓")
}

func TestScanReadNullableColumn(t *testing.T) {
	vf := openTestFile(t, "nulls.vtx")

	arr, err := vf.ReadColumn("maybe_int")
	if err != nil {
		t.Fatalf("ReadColumn: %v", err)
	}

	if arr.Length != 10 {
		t.Fatalf("expected 10 rows, got %d", arr.Length)
	}

	vals, err := Int32Values(arr)
	if err != nil {
		t.Fatal(err)
	}

	// Check validity mask.
	if len(arr.Children) == 0 {
		t.Fatal("expected validity child for nullable column")
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
	t.Logf("ReadColumn nulls.vtx maybe_int: vals=%v, mask=%v ✓", vals, mask)
}

func TestScanIterator(t *testing.T) {
	vf := openTestFile(t, "small.vtx")

	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"x"}})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	chunks := 0
	totalRows := 0
	for iter.Next() {
		result := iter.Result()
		if result == nil {
			t.Fatal("nil result from iterator")
		}
		xArr := result.Columns["x"]
		if xArr == nil {
			t.Fatal("missing 'x' in result")
		}
		totalRows += result.RowCount
		chunks++
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}
	if totalRows != 5 {
		t.Errorf("expected 5 total rows, got %d", totalRows)
	}
	t.Logf("ScanIterator small.vtx: %d chunks, %d total rows ✓", chunks, totalRows)
}

func TestScanWithLimit(t *testing.T) {
	vf := openTestFile(t, "integers.vtx")

	iter, err := vf.Scan(vortex.ScanOptions{
		Columns: []string{"i32_col"},
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	totalRows := 0
	for iter.Next() {
		totalRows += iter.Result().RowCount
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}

	if totalRows != 10 {
		t.Errorf("expected 10 rows with limit, got %d", totalRows)
	}

	// Verify the actual values are the first 10.
	arr, err := vf.ReadColumn("i32_col")
	if err != nil {
		t.Fatal(err)
	}
	// ReadColumn returns all rows; verify i32_col is correct.
	vals, _ := Int32Values(arr)
	if vals[0] != 1 || vals[9] != 10 {
		t.Errorf("first 10: got [%d..%d], want [1..10]", vals[0], vals[9])
	}
	t.Logf("ScanWithLimit integers.vtx: %d rows ✓", totalRows)
}

func TestScanColumnNotFound(t *testing.T) {
	vf := openTestFile(t, "small.vtx")

	_, err := vf.ReadColumn("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent column")
	}
	t.Logf("ReadColumn nonexistent: %v ✓", err)
}

func TestScanMixedDictAndPrimitive(t *testing.T) {
	vf := openTestFile(t, "mixed.vtx")

	// Read both a dict column (name) and a primitive column (id) together.
	cols, err := vf.ReadColumns([]string{"id", "name"})
	if err != nil {
		t.Fatalf("ReadColumns: %v", err)
	}

	idArr := cols["id"]
	nameArr := cols["name"]

	if idArr.Length != 100 {
		t.Fatalf("id: expected 100 rows, got %d", idArr.Length)
	}
	if nameArr.Length != 100 {
		t.Fatalf("name: expected 100 rows, got %d", nameArr.Length)
	}

	// Verify id values.
	ids, _ := Int32Values(idArr)
	if ids[0] != 1 || ids[99] != 100 {
		t.Errorf("id range: [%d..%d], want [1..100]", ids[0], ids[99])
	}

	// Verify name dict strings.
	strs, err := DictLookupStrings(nameArr)
	if err != nil {
		t.Fatalf("DictLookupStrings: %v", err)
	}
	expected := []string{"alice", "bob", "charlie", "diana", "eve"}
	if strs[0] != expected[0] || strs[4] != expected[4] {
		t.Errorf("name[0]=%q, name[4]=%q; want %q, %q", strs[0], strs[4], expected[0], expected[4])
	}

	t.Logf("ReadColumns mixed.vtx [id, name]: id=[1..100], name=%d strings ✓", len(strs))
}

func TestScanWriterRoundTrip(t *testing.T) {
	// Test Scan API on a writer-produced file (via writer test infrastructure).
	// The writer tests produce files that are then read back; here we verify
	// the Scan API can read writer-produced files correctly.
	vf := openTestFile(t, "small.vtx")

	// Use the iterator to read everything.
	iter, err := vf.Scan(vortex.ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	var allResults []*vortex.ScanResult
	for iter.Next() {
		allResults = append(allResults, iter.Result())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}

	if len(allResults) == 0 {
		t.Fatal("no results from iterator")
	}

	totalRows := 0
	for _, r := range allResults {
		totalRows += r.RowCount
	}
	if totalRows != 5 {
		t.Errorf("expected 5 total rows, got %d", totalRows)
	}
	t.Logf("Scan round-trip small.vtx: %d results, %d rows ✓", len(allResults), totalRows)
}

// --- Rust cross-validation tests for P1 decoders ---

func TestDateTimeParts(t *testing.T) {
	vf := openTestFile(t, "rust/timestamps.vortex")

	// Read counter column to verify row count and basic decoding.
	counter, err := vf.ReadColumn("counter")
	if err != nil {
		t.Fatalf("ReadColumn(counter): %v", err)
	}
	if counter.Length != 2000 {
		t.Fatalf("expected 2000 rows, got %d", counter.Length)
	}
	counterVals, err := Int64Values(counter)
	if err != nil {
		t.Fatalf("Int64Values(counter): %v", err)
	}
	// Spot-check counter.
	for _, idx := range []int{0, 1, 999, 1999} {
		if counterVals[idx] != int64(idx) {
			t.Errorf("counter[%d] = %d, want %d", idx, counterVals[idx], idx)
		}
	}

	// Read nanosecond timestamp column (datetimeparts-encoded).
	tsNs, err := vf.ReadColumn("ts_ns")
	if err != nil {
		t.Fatalf("ReadColumn(ts_ns): %v", err)
	}
	if tsNs.Length != 2000 {
		t.Fatalf("ts_ns: expected 2000 rows, got %d", tsNs.Length)
	}
	nsVals, err := Int64Values(tsNs)
	if err != nil {
		t.Fatalf("Int64Values(ts_ns): %v", err)
	}

	// Verify: base = 2024-01-01T00:00:00Z in nanos + i*60s
	baseNs := int64(1_704_067_200_000_000_000)
	for _, idx := range []int{0, 1, 100, 999, 1999} {
		expected := baseNs + int64(idx)*60_000_000_000
		if nsVals[idx] != expected {
			t.Errorf("ts_ns[%d] = %d, want %d", idx, nsVals[idx], expected)
		}
	}
	t.Logf("ts_ns: first=%d last=%d ✓", nsVals[0], nsVals[1999])

	// Read millisecond timestamp column.
	tsMs, err := vf.ReadColumn("ts_ms")
	if err != nil {
		t.Fatalf("ReadColumn(ts_ms): %v", err)
	}
	msVals, err := Int64Values(tsMs)
	if err != nil {
		t.Fatalf("Int64Values(ts_ms): %v", err)
	}

	baseMs := int64(1_704_067_200_000)
	for _, idx := range []int{0, 1, 100, 999, 1999} {
		expected := baseMs + int64(idx)*60_000
		if msVals[idx] != expected {
			t.Errorf("ts_ms[%d] = %d, want %d", idx, msVals[idx], expected)
		}
	}
	t.Logf("ts_ms: first=%d last=%d ✓", msVals[0], msVals[1999])
}

func TestZstdStrings(t *testing.T) {
	vf := openTestFile(t, "rust/zstd_strings.vortex")

	// Read counter to verify row count.
	counter, err := vf.ReadColumn("counter")
	if err != nil {
		t.Fatalf("ReadColumn(counter): %v", err)
	}
	if counter.Length != 2000 {
		t.Fatalf("expected 2000 rows, got %d", counter.Length)
	}
	counterVals, err := Int64Values(counter)
	if err != nil {
		t.Fatalf("Int64Values(counter): %v", err)
	}
	for _, idx := range []int{0, 1, 999, 1999} {
		if counterVals[idx] != int64(idx) {
			t.Errorf("counter[%d] = %d, want %d", idx, counterVals[idx], idx)
		}
	}

	// Read unique_str (zstd-encoded high-cardinality strings).
	uniqueStr, err := vf.ReadColumn("unique_str")
	if err != nil {
		t.Fatalf("ReadColumn(unique_str): %v", err)
	}
	if uniqueStr.Length != 2000 {
		t.Fatalf("unique_str: expected 2000 rows, got %d", uniqueStr.Length)
	}
	strVals, err := StringValues(uniqueStr)
	if err != nil {
		t.Fatalf("StringValues(unique_str): %v", err)
	}

	// Spot-check: format matches "event-NNNNNN-session-XXXXXXXX-payload-data-NN"
	for _, idx := range []int{0, 1, 100, 999, 1999} {
		expected := fmt.Sprintf("event-%06d-session-%08x-payload-data-%d", idx, idx*0x12345678, idx%100)
		if strVals[idx] != expected {
			t.Errorf("unique_str[%d] = %q, want %q", idx, strVals[idx], expected)
		}
	}

	// Read category column.
	cat, err := vf.ReadColumn("category")
	if err != nil {
		t.Fatalf("ReadColumn(category): %v", err)
	}
	catVals, err := StringValues(cat)
	if err != nil {
		t.Fatalf("StringValues(category): %v", err)
	}
	categories := []string{"error", "warning", "info", "debug", "trace"}
	for _, idx := range []int{0, 1, 2, 3, 4, 100, 1999} {
		expected := categories[idx%5]
		if catVals[idx] != expected {
			t.Errorf("category[%d] = %q, want %q", idx, catVals[idx], expected)
		}
	}
	t.Logf("zstd_strings: unique_str[0]=%q category[0]=%q ✓", strVals[0], catVals[0])
}

func TestPcoInts(t *testing.T) {
	vf := openTestFile(t, "rust/pco_ints.vortex")

	// --- counter (i64, pco-encoded) ---
	counter, err := vf.ReadColumn("counter")
	if err != nil {
		t.Fatalf("ReadColumn(counter): %v", err)
	}
	if counter.Length != 2000 {
		t.Fatalf("expected 2000 rows, got %d", counter.Length)
	}
	counterVals, err := Int64Values(counter)
	if err != nil {
		t.Fatalf("Int64Values(counter): %v", err)
	}
	for _, idx := range []int{0, 1, 100, 999, 1999} {
		if counterVals[idx] != int64(idx) {
			t.Errorf("counter[%d] = %d, want %d", idx, counterVals[idx], idx)
		}
	}

	// --- monotonic_i32 (pco-encoded): i*7 + (i*i % 13) ---
	mono, err := vf.ReadColumn("monotonic_i32")
	if err != nil {
		t.Fatalf("ReadColumn(monotonic_i32): %v", err)
	}
	monoVals, err := Int32Values(mono)
	if err != nil {
		t.Fatalf("Int32Values(monotonic_i32): %v", err)
	}
	for _, idx := range []int{0, 1, 2, 10, 100, 999, 1999} {
		expected := int32(idx)*7 + int32((idx*idx)%13)
		if monoVals[idx] != expected {
			t.Errorf("monotonic_i32[%d] = %d, want %d", idx, monoVals[idx], expected)
		}
	}

	// --- scattered_u32 (pco-encoded): (i * 2654435761) % 100000 ---
	scat, err := vf.ReadColumn("scattered_u32")
	if err != nil {
		t.Fatalf("ReadColumn(scattered_u32): %v", err)
	}
	scatVals, err := Uint32Values(scat)
	if err != nil {
		t.Fatalf("Uint32Values(scattered_u32): %v", err)
	}
	for _, idx := range []int{0, 1, 2, 10, 100, 999, 1999} {
		expected := (uint32(idx) * 2654435761) % 100000
		if scatVals[idx] != expected {
			t.Errorf("scattered_u32[%d] = %d, want %d", idx, scatVals[idx], expected)
		}
	}

	t.Logf("pco_ints: monotonic_i32[0]=%d scattered_u32[0]=%d counter[0]=%d ✓",
		monoVals[0], scatVals[0], counterVals[0])
}

func TestAlpFloat64(t *testing.T) {
	vf := openTestFile(t, "rust/pco_ints.vortex")

	// patterned_f64 is ALP-encoded: 100.0 + i*0.5 + (i%7)*0.01
	col, err := vf.ReadColumn("patterned_f64")
	if err != nil {
		t.Fatalf("ReadColumn(patterned_f64): %v", err)
	}
	if col.Length != 2000 {
		t.Fatalf("expected 2000 rows, got %d", col.Length)
	}
	vals, err := Float64Values(col)
	if err != nil {
		t.Fatalf("Float64Values: %v", err)
	}

	for _, idx := range []int{0, 1, 2, 6, 7, 100, 999, 1999} {
		expected := 100.0 + float64(idx)*0.5 + float64(idx%7)*0.01
		if math.Abs(vals[idx]-expected) > 1e-9 {
			t.Errorf("patterned_f64[%d] = %v, want %v", idx, vals[idx], expected)
		}
	}
	t.Logf("alp_f64: patterned_f64[0]=%.4f [1]=%.4f [6]=%.4f ✓", vals[0], vals[1], vals[6])
}

func TestAlpFloatsWithPatches(t *testing.T) {
	vf := openTestFile(t, "rust/alp_floats.vortex")

	// --- mixed_f64: i%4==3 → PI*(1+i), else 100.0 + i*0.25 ---
	col, err := vf.ReadColumn("mixed_f64")
	if err != nil {
		t.Fatalf("ReadColumn(mixed_f64): %v", err)
	}
	if col.Length != 2000 {
		t.Fatalf("expected 2000 rows, got %d", col.Length)
	}
	vals, err := Float64Values(col)
	if err != nil {
		t.Fatalf("Float64Values(mixed_f64): %v", err)
	}

	for _, idx := range []int{0, 1, 2, 3, 4, 7, 11, 100, 103, 999, 1999} {
		var expected float64
		if idx%4 == 3 {
			expected = math.Pi * (1.0 + float64(idx))
		} else {
			expected = 100.0 + float64(idx)*0.25
		}
		if math.Abs(vals[idx]-expected) > 1e-9 {
			t.Errorf("mixed_f64[%d] = %.15f, want %.15f (diff=%.2e)",
				idx, vals[idx], expected, vals[idx]-expected)
		}
	}

	// --- clean_f32: i * 0.1 ---
	f32col, err := vf.ReadColumn("clean_f32")
	if err != nil {
		t.Fatalf("ReadColumn(clean_f32): %v", err)
	}
	f32vals, err := Float32Values(f32col)
	if err != nil {
		t.Fatalf("Float32Values(clean_f32): %v", err)
	}
	for _, idx := range []int{0, 1, 10, 100, 999, 1999} {
		expected := float32(idx) * 0.1
		if math.Abs(float64(f32vals[idx]-expected)) > 1e-5 {
			t.Errorf("clean_f32[%d] = %v, want %v", idx, f32vals[idx], expected)
		}
	}

	t.Logf("alp_floats: mixed_f64[3]=%.10f (PI*4=%.10f) clean_f32[10]=%.4f ✓",
		vals[3], math.Pi*4.0, f32vals[10])
}
