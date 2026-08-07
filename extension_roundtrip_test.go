package vortex_test

import (
	"bytes"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/encoding"
	"github.com/LaurieRhodes/vortex-go/writer"
)

// TestExtensionDTypeRoundTrip writes a file whose schema carries an
// Extension dtype over i64 storage, then verifies (a) the logical
// extension identity survives in the file schema and (b) scan +
// accessor decode operate on the storage representation.
func TestExtensionDTypeRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"Ts", "V"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypeExtension, ExtensionID: "kql.datetime",
				StorageDType: &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64}},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
		},
	}

	var buf bytes.Buffer
	w, err := writer.NewWriter(&buf, schema, writer.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	n := 1000
	ts := make([]byte, n*8)
	vs := make([]byte, n*8)
	tsArr := &vortex.Array{DType: &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64}, Length: n, Buffers: [][]byte{ts}}
	vArr := &vortex.Array{DType: &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64}, Length: n, Buffers: [][]byte{vs}}
	for i := 0; i < n; i++ {
		putLE64(ts, i, uint64(1700000000000000000+int64(i)*1_000_000_000))
		putLE64(vs, i, uint64(i))
	}
	if err := w.WriteChunk(map[string]*vortex.Array{"Ts": tsArr, "V": vArr}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := vortex.Open(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	dt := f.DType()
	if dt.FieldTypes[0].Kind != vortex.DTypeExtension || dt.FieldTypes[0].ExtensionID != "kql.datetime" {
		t.Fatalf("extension identity lost: %+v", dt.FieldTypes[0])
	}
	if dt.FieldTypes[0].StorageDType == nil || dt.FieldTypes[0].StorageDType.PType != vortex.PTypeI64 {
		t.Fatalf("storage dtype lost: %+v", dt.FieldTypes[0].StorageDType)
	}

	iter, err := f.Scan(vortex.ScanOptions{Columns: []string{"Ts"}})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for iter.Next() {
		arr := iter.Result().Columns["Ts"]
		vals, err := encoding.Int64Values(arr)
		if err != nil {
			t.Fatalf("accessor on extension column: %v", err)
		}
		for _, v := range vals {
			if v != 1700000000000000000+int64(total)*1_000_000_000 {
				t.Fatalf("value mismatch at %d: %d", total, v)
			}
			total++
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Fatalf("rows: got %d want %d", total, n)
	}
}

func putLE64(b []byte, i int, v uint64) {
	for j := 0; j < 8; j++ {
		b[i*8+j] = byte(v >> (8 * j))
	}
}
