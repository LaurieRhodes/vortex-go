// Package integration contains cross-format integration tests.
package integration

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/encoding"
	"github.com/LaurieRhodes/vortex-go/writer"
)

// ParquetRow is the test schema for Parquet files.
type ParquetRow struct {
	ID     int32   `parquet:"id"`
	Name   string  `parquet:"name"`
	Score  float64 `parquet:"score"`
	Active bool    `parquet:"active"`
}

// TestParquetToVortexRoundTrip writes a Parquet file, converts it to Vortex
// via the writer, reads it back, and verifies all values.
func TestParquetToVortexRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()

	// Step 1: Write a Parquet file.
	pqPath := filepath.Join(tmpDir, "test.parquet")
	writeParquet(t, pqPath, 1000)

	// Step 2: Read parquet and convert to Vortex.
	vtxPath := filepath.Join(tmpDir, "test.vtx")
	convertParquetToVortex(t, pqPath, vtxPath)

	// Step 3: Read back Vortex file and verify.
	verifyVortexFile(t, vtxPath, 1000)
}

// TestMultiChunkRoundTrip writes a multi-chunk Vortex file and reads it back.
func TestMultiChunkRoundTrip(t *testing.T) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"counter", "label"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
			{Kind: vortex.DTypeUtf8},
		},
	}

	var buf bytes.Buffer
	w, err := writer.NewWriter(&buf, schema, writer.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	labels := []string{"alpha", "beta", "gamma"}
	totalRows := 0

	// Write 5 chunks of 200 rows each.
	for chunk := 0; chunk < 5; chunk++ {
		n := 200
		base := chunk * n

		counterBuf := make([]byte, n*8)
		viewsBuf := make([]byte, n*16)

		for i := 0; i < n; i++ {
			row := base + i
			binary.LittleEndian.PutUint64(counterBuf[i*8:], uint64(int64(row)))
			s := labels[row%3]
			off := i * 16
			binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
			copy(viewsBuf[off+4:], s)
		}

		err := w.WriteChunk(map[string]*vortex.Array{
			"counter": {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{counterBuf}},
			"label":   {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{viewsBuf}},
		})
		if err != nil {
			t.Fatal(err)
		}
		totalRows += n
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	t.Logf("wrote %d bytes for %d rows in 5 chunks", buf.Len(), totalRows)

	// Read back and verify.
	data := buf.Bytes()
	r := bytes.NewReader(data)
	vf, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	iter, err := vf.Scan(vortex.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	readRows := 0
	for iter.Next() {
		result := iter.Result()
		n := result.RowCount

		counterArr := result.Columns["counter"]
		labelArr := result.Columns["label"]

		strs, err := encoding.StringValues(labelArr)
		if err != nil {
			t.Fatalf("StringValues: %v", err)
		}

		for i := 0; i < n; i++ {
			row := readRows + i
			gotCounter := int64(binary.LittleEndian.Uint64(counterArr.Buffers[0][i*8:]))
			if gotCounter != int64(row) {
				t.Errorf("row %d: counter=%d want %d", row, gotCounter, row)
			}
			wantLabel := labels[row%3]
			if strs[i] != wantLabel {
				t.Errorf("row %d: label=%q want %q", row, strs[i], wantLabel)
			}
		}
		readRows += n
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	if readRows != totalRows {
		t.Fatalf("read %d rows, want %d", readRows, totalRows)
	}
	t.Logf("verified %d rows across 5 chunks", readRows)
}

// TestGoWrittenRustReadable verifies that Go-written test files exist and
// can be read by Go (the Rust cross-compatibility test is run separately).
func TestGoWrittenRustReadable(t *testing.T) {
	files := []struct {
		path string
		rows int
	}{
		{"../testdata/go_written.vtx", 100},
		{"../testdata/go_written_large.vtx", 1500},
	}

	for _, f := range files {
		t.Run(filepath.Base(f.path), func(t *testing.T) {
			fh, err := os.Open(f.path)
			if err != nil {
				t.Skipf("file not found (run generate.sh first): %v", err)
			}
			defer fh.Close()

			fi, _ := fh.Stat()
			vf, err := vortex.Open(fh, fi.Size())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			iter, err := vf.Scan(vortex.ScanOptions{})
			if err != nil {
				t.Fatal(err)
			}

			rows := 0
			for iter.Next() {
				rows += iter.Result().RowCount
			}
			if err := iter.Err(); err != nil {
				t.Fatal(err)
			}
			if rows != f.rows {
				t.Fatalf("got %d rows, want %d", rows, f.rows)
			}
			t.Logf("%s: %d rows, %d bytes", filepath.Base(f.path), rows, fi.Size())
		})
	}
}

// --- helpers ---

func writeParquet(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := parquet.NewGenericWriter[ParquetRow](f)

	names := []string{"alice", "bob", "charlie", "diana", "eve"}
	rows := make([]ParquetRow, n)
	for i := range rows {
		rows[i] = ParquetRow{
			ID:     int32(i),
			Name:   names[i%5],
			Score:  float64(i) * 0.1,
			Active: i%3 != 0,
		}
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func convertParquetToVortex(t *testing.T, pqPath, vtxPath string) {
	t.Helper()

	// Open parquet file.
	pf, err := os.Open(pqPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	fi, _ := pf.Stat()

	pqFile, err := parquet.OpenFile(pf, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	// Build vortex schema.
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"id", "name", "score", "active"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
			{Kind: vortex.DTypeUtf8},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
			{Kind: vortex.DTypeBool},
		},
	}

	out, err := os.Create(vtxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	w, err := writer.NewWriter(out, schema, writer.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}

	// Read parquet row by row and batch into vortex chunks.
	reader := parquet.NewGenericReader[ParquetRow](pqFile)
	defer reader.Close()

	batchSize := 500
	rows := make([]ParquetRow, batchSize)
	totalRows := 0

	for {
		n, err := reader.Read(rows)
		if n == 0 {
			break
		}
		batch := rows[:n]

		idBuf := make([]byte, n*4)
		scoreBuf := make([]byte, n*8)
		boolBuf := make([]byte, (n+7)/8)
		viewsBuf := make([]byte, n*16)

		for i, r := range batch {
			binary.LittleEndian.PutUint32(idBuf[i*4:], uint32(r.ID))
			binary.LittleEndian.PutUint64(scoreBuf[i*8:], math.Float64bits(r.Score))
			if r.Active {
				boolBuf[i/8] |= 1 << uint(i%8)
			}
			off := i * 16
			binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(r.Name)))
			copy(viewsBuf[off+4:], r.Name)
		}

		writeErr := w.WriteChunk(map[string]*vortex.Array{
			"id":     {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{idBuf}},
			"name":   {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{viewsBuf}},
			"score":  {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{scoreBuf}},
			"active": {DType: schema.FieldTypes[3], Length: n, Buffers: [][]byte{boolBuf}},
		})
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		totalRows += n

		if err != nil {
			break
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("converted %d parquet rows to vortex", totalRows)
}

func verifyVortexFile(t *testing.T, vtxPath string, expectedRows int) {
	t.Helper()

	f, err := os.Open(vtxPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()

	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	iter, err := vf.Scan(vortex.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}

	names := []string{"alice", "bob", "charlie", "diana", "eve"}
	totalRows := 0

	for iter.Next() {
		result := iter.Result()
		n := result.RowCount

		idArr := result.Columns["id"]
		nameArr := result.Columns["name"]
		scoreArr := result.Columns["score"]
		activeArr := result.Columns["active"]

		strs, err := encoding.StringValues(nameArr)
		if err != nil {
			t.Fatalf("StringValues: %v", err)
		}

		for i := 0; i < n; i++ {
			row := totalRows + i

			gotID := int32(binary.LittleEndian.Uint32(idArr.Buffers[0][i*4:]))
			if gotID != int32(row) {
				t.Errorf("row %d: id=%d want %d", row, gotID, row)
			}

			if strs[i] != names[row%5] {
				t.Errorf("row %d: name=%q want %q", row, strs[i], names[row%5])
			}

			bits := binary.LittleEndian.Uint64(scoreArr.Buffers[0][i*8:])
			gotScore := math.Float64frombits(bits)
			wantScore := float64(row) * 0.1
			if math.Abs(gotScore-wantScore) > 1e-10 {
				t.Errorf("row %d: score=%.6f want %.6f", row, gotScore, wantScore)
			}

			byteIdx := i / 8
			bitIdx := i % 8
			gotActive := activeArr.Buffers[0][byteIdx]&(1<<uint(bitIdx)) != 0
			wantActive := row%3 != 0
			if gotActive != wantActive {
				t.Errorf("row %d: active=%v want %v", row, gotActive, wantActive)
			}
		}
		totalRows += n
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}
	if totalRows != expectedRows {
		t.Fatalf("read %d rows, want %d", totalRows, expectedRows)
	}
	t.Logf("verified %d rows (id, name, score, active)", totalRows)
}
