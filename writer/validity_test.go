package writer

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/encoding"
)

func validityBits(valid []bool) *vortex.Array {
	buf := make([]byte, (len(valid)+7)/8)
	for i, v := range valid {
		if v {
			buf[i/8] |= 1 << uint(i%8)
		}
	}
	return &vortex.Array{DType: &vortex.DType{Kind: vortex.DTypeBool}, Length: len(valid), Buffers: [][]byte{buf}}
}

// TestNullablePrimitiveRoundTrip writes nullable i64, f64 and bool
// columns with validity children (one chunk with nulls, one without)
// and reads back values and validity.
func TestNullablePrimitiveRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"i", "f", "b"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64, Nullable: true},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64, Nullable: true},
			{Kind: vortex.DTypeBool, Nullable: true},
		},
	}
	n := 2000
	valid := make([]bool, n)
	ib := make([]byte, n*8)
	fb := make([]byte, n*8)
	bools := make([]bool, n)
	for i := 0; i < n; i++ {
		valid[i] = i%7 != 3
		binary.LittleEndian.PutUint64(ib[i*8:], uint64(int64(i%50)))
		binary.LittleEndian.PutUint64(fb[i*8:], math.Float64bits(float64(i)/4))
		bools[i] = i%3 == 0
	}
	boolArr := validityBits(bools)
	boolArr.DType = schema.FieldTypes[2]

	var out bytes.Buffer
	w, err := NewWriter(&out, schema, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	chunk := map[string]*vortex.Array{
		"i": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{ib}, Children: []*vortex.Array{validityBits(valid)}},
		"f": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{fb}, Children: []*vortex.Array{validityBits(valid)}},
		"b": {DType: schema.FieldTypes[2], Length: n, Buffers: boolArr.Buffers, Children: []*vortex.Array{validityBits(valid)}},
	}
	if err := w.WriteChunk(chunk); err != nil {
		t.Fatal(err)
	}
	// A second chunk with no nulls is written compressed, without a child.
	if err := w.WriteChunk(map[string]*vortex.Array{
		"i": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{ib}},
		"f": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{fb}},
		"b": {DType: schema.FieldTypes[2], Length: n, Buffers: boolArr.Buffers},
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	vf, err := vortex.Open(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"i", "f", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	chunkNo := 0
	rows := 0
	for iter.Next() {
		cols := iter.Result().Columns
		iv, err := encoding.Int64Values(cols["i"])
		if err != nil {
			t.Fatal(err)
		}
		fv, err := encoding.Float64Values(cols["f"])
		if err != nil {
			t.Fatal(err)
		}
		bv, err := encoding.BoolValues(cols["b"])
		if err != nil {
			t.Fatal(err)
		}
		for name, a := range cols {
			v, err := encoding.Validity(a)
			if err != nil {
				t.Fatal(err)
			}
			if chunkNo == 0 && rows == 0 {
				if v == nil {
					t.Fatalf("%s: chunk 0 lost its validity", name)
				}
				for i := range v {
					if v[i] != valid[i] {
						t.Fatalf("%s: validity[%d] = %v, want %v", name, i, v[i], valid[i])
					}
				}
			}
			if chunkNo == 1 && v != nil {
				t.Fatalf("%s: chunk 1 should have no validity", name)
			}
		}
		for i := range iv {
			if iv[i] != int64(i%50) || fv[i] != float64(i)/4 || bv[i] != (i%3 == 0) {
				t.Fatalf("chunk %d row %d: got %d %v %v", chunkNo, i, iv[i], fv[i], bv[i])
			}
		}
		rows += len(iv)
		chunkNo++
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	if rows != 2*n {
		t.Fatalf("rows = %d, want %d", rows, 2*n)
	}
}

// TestNullableZoneMaps: zone min/max cover valid values only (a null's
// placeholder 0 does not widen the range), and an all-null zone has
// null stats, so it is never pruned.
func TestNullableZoneMaps(t *testing.T) {
	schema := &vortex.DType{Kind: vortex.DTypeStruct, FieldNames: []string{"v"},
		FieldTypes: []*vortex.DType{{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64, Nullable: true}}}
	opts := DefaultOptions()
	opts.EnableZoneMaps = true
	var out bytes.Buffer
	w, err := NewWriter(&out, schema, opts)
	if err != nil {
		t.Fatal(err)
	}
	// zone 0: 1000..1099 with every 10th row null (placeholder 0)
	// zone 1: all null
	// zone 2: 2000..2099, no nulls
	for z := 0; z < 3; z++ {
		buf := make([]byte, 100*8)
		valid := make([]bool, 100)
		for i := 0; i < 100; i++ {
			valid[i] = (z == 0 && i%10 != 0) || z == 2
			if valid[i] {
				binary.LittleEndian.PutUint64(buf[i*8:], uint64(1000*(z/2+1)+i))
			}
		}
		arr := &vortex.Array{DType: schema.FieldTypes[0], Length: 100, Buffers: [][]byte{buf}}
		if z != 2 {
			arr.Children = []*vortex.Array{validityBits(valid)}
		}
		if err := w.WriteChunk(map[string]*vortex.Array{"v": arr}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	vf, err := vortex.Open(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	scan := func(op vortex.CompareOp, v int64) (pruned, zones int) {
		it, err := vf.Scan(vortex.ScanOptions{RowFilter: &vortex.RowFilter{
			Predicates: []vortex.ColumnPredicate{{Column: "v", Op: op, Value: v}}}})
		if err != nil {
			t.Fatal(err)
		}
		for it.Next() {
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
		return it.ZonePruningStats()
	}
	// v < 500: zone 0's valid minimum is 1001 (the nulls' 0 must not
	// count) and zone 2's is 2000, so both prune; the all-null zone 1
	// has null stats and is kept.
	if p, z := scan(vortex.OpLT, 500); z != 3 || p != 2 {
		t.Fatalf("v < 500: pruned %d of %d zones, want 2 of 3", p, z)
	}
	// v > 1500 prunes zone 0 only.
	if p, _ := scan(vortex.OpGT, 1500); p != 1 {
		t.Fatalf("v > 1500: pruned %d zones, want 1", p)
	}
}
