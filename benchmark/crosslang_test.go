// Cross-language benchmark: generates shared Vortex files and benchmarks
// Go reader performance. Run the Rust benchmark separately on the same files
// to compare.
package benchmark

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	vortex "github.com/LaurieRhodes/vortex-go"
	_ "github.com/LaurieRhodes/vortex-go/encoding"
	"github.com/LaurieRhodes/vortex-go/writer"
)

// generateBenchFile creates a Vortex file for cross-language benchmarking.
// Schema: id:i64, value:f64, category:utf8 (3 columns)
// Data: sequential ids, sine-wave values, 10 rotating category strings.
func generateBenchFile(nRows, chunkSize int) []byte {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"id", "value", "category"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
			{Kind: vortex.DTypeUtf8},
		},
	}
	cats := []string{"alpha", "bravo", "charlie", "delta", "echo",
		"foxtrot", "golf", "hotel", "india", "juliet"}

	var buf bytes.Buffer
	w, _ := writer.NewWriter(&buf, schema, writer.DefaultOptions())

	for offset := 0; offset < nRows; offset += chunkSize {
		n := chunkSize
		if offset+n > nRows {
			n = nRows - offset
		}
		idBuf := make([]byte, n*8)
		valBuf := make([]byte, n*8)
		viewsBuf := make([]byte, n*16)

		for i := 0; i < n; i++ {
			row := offset + i
			binary.LittleEndian.PutUint64(idBuf[i*8:], uint64(int64(row)))
			v := math.Sin(float64(row)*0.001) * 1000.0
			binary.LittleEndian.PutUint64(valBuf[i*8:], math.Float64bits(v))
			s := cats[row%10]
			off := i * 16
			binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
			copy(viewsBuf[off+4:], s)
		}
		w.WriteChunk(map[string]*vortex.Array{
			"id":       {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{idBuf}},
			"value":    {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{valBuf}},
			"category": {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{viewsBuf}},
		})
	}
	w.Close()
	return buf.Bytes()
}

func benchGoRead(data []byte, opts vortex.ScanOptions, iters int) (rows int64, avg time.Duration) {
	var total time.Duration
	for i := 0; i < iters; i++ {
		r := bytes.NewReader(data)
		f, err := vortex.Open(r, int64(len(data)))
		if err != nil {
			panic(err)
		}
		t0 := time.Now()
		it, err := f.Scan(opts)
		if err != nil {
			panic(err)
		}
		var n int64
		for it.Next() {
			n += int64(it.Result().RowCount)
		}
		total += time.Since(t0)
		rows = n
	}
	return rows, total / time.Duration(iters)
}

func TestCrossLanguageBenchmark(t *testing.T) {
	// Generate and save benchmark files.
	benchDir := filepath.Join(os.TempDir(), "vortex-bench")
	os.MkdirAll(benchDir, 0755)

	type benchCase struct {
		name      string
		nRows     int
		chunkSize int
		iters     int
	}
	cases := []benchCase{
		{"100K", 100_000, 10_000, 50},
		{"500K", 500_000, 50_000, 20},
		{"1M", 1_000_000, 50_000, 10},
	}

	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    Go vs Rust Reader Benchmark                             ║")
	fmt.Println("║  Schema: id:i64, value:f64, category:utf8  (3 columns)                     ║")
	fmt.Println("║  Run Rust benchmark: cd testdata/testgen && cargo run --bin bench --release ║")
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println()

	for _, c := range cases {
		data := generateBenchFile(c.nRows, c.chunkSize)

		// Save file for Rust benchmark.
		path := filepath.Join(benchDir, fmt.Sprintf("bench_%s.vtx", c.name))
		os.WriteFile(path, data, 0644)

		// Full scan.
		rows, fullAvg := benchGoRead(data, vortex.ScanOptions{}, c.iters)
		throughputMB := float64(len(data)) / (1 << 20) / fullAvg.Seconds()
		throughputRows := float64(rows) / fullAvg.Seconds() / 1e6

		// Projected scan (1/3 columns).
		_, projAvg := benchGoRead(data, vortex.ScanOptions{
			Columns: []string{"id"},
		}, c.iters)

		// Filtered scan (zone pruning).
		filter := &vortex.RowFilter{
			Predicates: []vortex.ColumnPredicate{
				{Column: "id", Op: vortex.OpGTE, Value: int64(0)},
				{Column: "id", Op: vortex.OpLTE, Value: int64(int64(c.nRows) / 10)},
			},
		}
		filtRows, filtAvg := benchGoRead(data, vortex.ScanOptions{RowFilter: filter}, c.iters)

		fmt.Printf("┌─── %s (%d chunks × %dK) ─── file: %s\n",
			c.name, c.nRows/c.chunkSize, c.chunkSize/1000, fmtBytes(int64(len(data))))
		fmt.Printf("│  Go full scan:      %9s  (%dK rows, %.0f MB/s, %.1fM rows/s)\n",
			fullAvg.Round(time.Microsecond), rows/1000, throughputMB, throughputRows)
		fmt.Printf("│  Go project 1/3:    %9s  (%.1f× vs full)\n",
			projAvg.Round(time.Microsecond), float64(fullAvg)/float64(projAvg))
		fmt.Printf("│  Go filter 10%%:     %9s  (%dK/%dK rows, %.1f× vs full)\n",
			filtAvg.Round(time.Microsecond), filtRows/1000, rows/1000,
			float64(fullAvg)/float64(filtAvg))
		fmt.Printf("│\n")
		fmt.Printf("│  Benchmark file: %s\n", path)
		fmt.Printf("└──────────────────────────────────────────────────────────────────\n\n")
	}

	fmt.Println("To run Rust benchmark on same files:")
	fmt.Printf("  cd testdata/testgen && cargo run --bin bench --release -- %s\n", benchDir)
	fmt.Println()
	fmt.Println("╚════════════════════════════════════════════════════════════════════════════╝")
}
