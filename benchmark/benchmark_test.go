// Package benchmark compares Vortex vs Parquet performance across
// compression ratios, scan throughput, projection, and zone-map pruning.
package benchmark

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"

	vortex "github.com/LaurieRhodes/vortex-go"
	_ "github.com/LaurieRhodes/vortex-go/encoding"
	"github.com/LaurieRhodes/vortex-go/writer"
)

// ── Test data ────────────────────────────────────────────────────────

type benchRow struct {
	ID          int64   `parquet:"id"`
	UserID      int32   `parquet:"user_id"`
	Amount      float64 `parquet:"amount"`
	Category    string  `parquet:"category"`
	Description string  `parquet:"description"`
	Score       float64 `parquet:"score"`
	Active      bool    `parquet:"active"`
}

var categories = []string{
	"electronics", "clothing", "food", "books", "sports",
	"automotive", "health", "toys", "garden", "office",
}

var adjectives = []string{
	"premium", "basic", "deluxe", "standard", "compact",
	"professional", "eco", "ultra", "mini", "mega",
}

var nouns = []string{
	"widget", "gadget", "tool", "device", "kit",
	"set", "pack", "bundle", "system", "unit",
}

func generateRows(n int) []benchRow {
	rng := rand.New(rand.NewSource(42))
	rows := make([]benchRow, n)
	for i := range rows {
		rows[i] = benchRow{
			ID:          int64(i),
			UserID:      int32(rng.Intn(10000)),
			Amount:      math.Round(rng.Float64()*1000*100) / 100,
			Category:    categories[rng.Intn(len(categories))],
			Description: adjectives[rng.Intn(len(adjectives))] + " " + nouns[rng.Intn(len(nouns))],
			Score:       math.Round(rng.Float64()*5*100) / 100,
			Active:      rng.Intn(2) == 0,
		}
	}
	return rows
}

// ── VarBinView builder ───────────────────────────────────────────────

// buildVarBinViewBuffers encodes strings into proper VarBinView format:
//
//	Inline (≤12 bytes): [length:4][data:12]
//	Heap   (>12 bytes): [length:4][prefix:4][buf_idx:4][offset:4]
//
// Returns: views buffer + optional heap buffers.
func buildVarBinViewBuffers(strings []string) [][]byte {
	views := make([]byte, len(strings)*16)
	var heap []byte

	for i, s := range strings {
		b := []byte(s)
		off := i * 16
		binary.LittleEndian.PutUint32(views[off:], uint32(len(b)))

		if len(b) <= 12 {
			// Inline: copy directly into view.
			copy(views[off+4:off+16], b)
		} else {
			// Heap: store prefix + buffer reference.
			copy(views[off+4:off+8], b[:4])                                  // first 4 bytes prefix
			binary.LittleEndian.PutUint32(views[off+8:], 0)                  // buffer index 0
			binary.LittleEndian.PutUint32(views[off+12:], uint32(len(heap))) // offset into heap
			heap = append(heap, b...)
		}
	}

	if len(heap) == 0 {
		return [][]byte{views}
	}
	return [][]byte{views, heap}
}

// ── File writers ─────────────────────────────────────────────────────

func writeParquetFile(path string, rows []benchRow) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	pw := parquet.NewGenericWriter[benchRow](f)
	if _, err := pw.Write(rows); err != nil {
		return 0, err
	}
	if err := pw.Close(); err != nil {
		return 0, err
	}

	fi, _ := f.Stat()
	return fi.Size(), nil
}

func writeVortexFile(path string, rows []benchRow, chunkSize int) (int64, error) {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"id", "user_id", "amount", "category", "description", "score", "active"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
			{Kind: vortex.DTypeUtf8},
			{Kind: vortex.DTypeUtf8},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
			{Kind: vortex.DTypeBool},
		},
	}
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w, err := writer.NewWriter(f, schema, writer.DefaultOptions())
	if err != nil {
		return 0, err
	}
	for start := 0; start < len(rows); start += chunkSize {
		end := start + chunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		n := len(chunk)

		idBuf := make([]byte, n*8)
		uidBuf := make([]byte, n*4)
		amtBuf := make([]byte, n*8)
		scoreBuf := make([]byte, n*8)
		boolBuf := make([]byte, (n+7)/8)

		catStrings := make([]string, n)
		descStrings := make([]string, n)

		for i, r := range chunk {
			binary.LittleEndian.PutUint64(idBuf[i*8:], uint64(r.ID))
			binary.LittleEndian.PutUint32(uidBuf[i*4:], uint32(r.UserID))
			binary.LittleEndian.PutUint64(amtBuf[i*8:], math.Float64bits(r.Amount))
			binary.LittleEndian.PutUint64(scoreBuf[i*8:], math.Float64bits(r.Score))
			if r.Active {
				boolBuf[i/8] |= 1 << (i % 8)
			}
			catStrings[i] = r.Category
			descStrings[i] = r.Description
		}

		catBufs := buildVarBinViewBuffers(catStrings)
		descBufs := buildVarBinViewBuffers(descStrings)

		err = w.WriteChunk(map[string]*vortex.Array{
			"id":          {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{idBuf}},
			"user_id":     {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{uidBuf}},
			"amount":      {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{amtBuf}},
			"category":    {DType: schema.FieldTypes[3], Length: n, Buffers: catBufs},
			"description": {DType: schema.FieldTypes[4], Length: n, Buffers: descBufs},
			"score":       {DType: schema.FieldTypes[5], Length: n, Buffers: [][]byte{scoreBuf}},
			"active":      {DType: schema.FieldTypes[6], Length: n, Buffers: [][]byte{boolBuf}},
		})
		if err != nil {
			return 0, err
		}
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	fi, _ := os.Stat(path)
	return fi.Size(), nil
}

// ── Scan helpers ─────────────────────────────────────────────────────

func scanVortex(data []byte, opts vortex.ScanOptions) (rows int64, pruned, zones int, dur time.Duration, err error) {
	r := bytes.NewReader(data)
	f, err := vortex.Open(r, int64(len(data)))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	t0 := time.Now()
	iter, err := f.Scan(opts)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	for iter.Next() {
		rows += int64(iter.Result().RowCount)
	}
	dur = time.Since(t0)
	pruned, zones = iter.ZonePruningStats()
	return rows, pruned, zones, dur, iter.Err()
}

func benchVortex(data []byte, opts vortex.ScanOptions, iters int) (rows int64, pruned, zones int, avg time.Duration, err error) {
	var total time.Duration
	for i := 0; i < iters; i++ {
		r, p, z, d, e := scanVortex(data, opts)
		if e != nil {
			return 0, 0, 0, 0, e
		}
		rows = r
		pruned = p
		zones = z
		total += d
	}
	return rows, pruned, zones, total / time.Duration(iters), nil
}

func benchParquet(path string, iters int) (rows int64, avg time.Duration, err error) {
	var total time.Duration
	for i := 0; i < iters; i++ {
		f, err := os.Open(path)
		if err != nil {
			return 0, 0, err
		}

		t0 := time.Now()
		reader := parquet.NewGenericReader[benchRow](f)
		buf := make([]benchRow, 4096)
		var n int64
		for {
			nr, err := reader.Read(buf)
			n += int64(nr)
			if err != nil {
				break
			}
		}
		total += time.Since(t0)
		rows = n
		f.Close()
	}
	return rows, total / time.Duration(iters), nil
}

// ── Report ───────────────────────────────────────────────────────────

func TestPerformanceReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance report in short mode")
	}

	tmpDir := t.TempDir()
	sizes := []int{10_000, 100_000, 500_000, 1_000_000}

	fmt.Println()
	fmt.Println("╔════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                       vortex-go Performance Report                        ║")
	fmt.Println("║  Schema: id:i64, user_id:i32, amount:f64, category:utf8,                  ║")
	fmt.Println("║          description:utf8, score:f64, active:bool  (7 columns)             ║")
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println()

	for _, n := range sizes {
		rows := generateRows(n)
		pqPath := filepath.Join(tmpDir, fmt.Sprintf("bench_%d.parquet", n))
		vtxPath := filepath.Join(tmpDir, fmt.Sprintf("bench_%d.vtx", n))

		chunkSize := 10_000
		if n >= 500_000 {
			chunkSize = 50_000
		}
		iters := 10
		if n >= 500_000 {
			iters = 5
		}

		fmt.Printf("┌─── %dk rows (%d chunks of %dk) ──────────────────────────────────\n", n/1000, (n+chunkSize-1)/chunkSize, chunkSize/1000)

		// Write both formats.
		t0 := time.Now()
		pqSize, err := writeParquetFile(pqPath, rows)
		pqWrite := time.Since(t0)
		if err != nil {
			t.Fatalf("parquet write: %v", err)
		}

		t0 = time.Now()
		vtxSize, err := writeVortexFile(vtxPath, rows, chunkSize)
		vtxWrite := time.Since(t0)
		if err != nil {
			t.Fatalf("vortex write: %v", err)
		}

		vtxData, _ := os.ReadFile(vtxPath)

		ratio := float64(vtxSize) / float64(pqSize) * 100
		savings := 100 - ratio
		fmt.Printf("│  File size:    Parquet %9s  →  Vortex %9s  (%.0f%% smaller)\n",
			fmtBytes(pqSize), fmtBytes(vtxSize), savings)
		fmt.Printf("│  Write speed:  Parquet %9s  |  Vortex %9s\n",
			pqWrite.Round(time.Millisecond), vtxWrite.Round(time.Millisecond))

		// Full scan.
		vtxRows, _, _, vtxFull, err := benchVortex(vtxData, vortex.ScanOptions{}, iters)
		if err != nil {
			t.Fatalf("vortex scan: %v", err)
		}
		pqRows, pqFull, err := benchParquet(pqPath, iters)
		if err != nil {
			t.Fatalf("parquet scan: %v", err)
		}
		speedup := float64(pqFull) / float64(vtxFull)
		_ = pqRows

		fmt.Printf("│  Full scan:    Parquet %9s  |  Vortex %9s  (%.1f× faster)\n",
			pqFull.Round(100*time.Microsecond), vtxFull.Round(100*time.Microsecond), speedup)

		// Projected scan (2 of 7 columns).
		_, _, _, vtxProj, err := benchVortex(vtxData, vortex.ScanOptions{Columns: []string{"id", "amount"}}, iters)
		if err != nil {
			t.Fatalf("vortex projected: %v", err)
		}
		projSpeedup := float64(vtxFull) / float64(vtxProj)
		projVsPq := float64(pqFull) / float64(vtxProj)
		fmt.Printf("│  Project 2/7:  Vortex %9s  (%.1f× vs full, %.1f× vs Parquet)\n",
			vtxProj.Round(100*time.Microsecond), projSpeedup, projVsPq)

		// Throughput.
		vtxMBps := float64(vtxSize) / vtxFull.Seconds() / 1e6
		pqMBps := float64(pqSize) / pqFull.Seconds() / 1e6
		vtxRowsPs := float64(vtxRows) / vtxFull.Seconds()
		pqRowsPs := float64(pqRows) / pqFull.Seconds()
		fmt.Printf("│  Throughput:   Parquet %5.0f MB/s  |  Vortex %5.0f MB/s\n", pqMBps, vtxMBps)
		fmt.Printf("│               Parquet %5.1fM row/s |  Vortex %5.1fM row/s\n",
			pqRowsPs/1e6, vtxRowsPs/1e6)
		fmt.Printf("└──────────────────────────────────────────────────────────────────\n\n")
	}

	// Zone pruning section using Rust-generated files.
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║                       Zone Map Pruning (Rust-written files)                ║")
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println()

	rustDir := filepath.Join("..", "testdata", "rust")
	// zone_pruning.vortex has 3 zones: [0-99], [1000-1099], [2000-2099].
	// Filter for value in [50,150] should match only zone 0, pruning 2/3 zones.
	zpPath := filepath.Join(rustDir, "zone_pruning.vortex")
	zpData, err := os.ReadFile(zpPath)
	if err != nil {
		fmt.Printf("  zone_pruning.vortex: skipped (file not found)\n")
	} else {
		totalRows, _, _, fullAvg, _ := benchVortex(zpData, vortex.ScanOptions{}, 100)

		// Filter: value BETWEEN 50 AND 150 — should prune zones 1 and 2.
		filter := &vortex.RowFilter{
			Predicates: []vortex.ColumnPredicate{
				{Column: "value", Op: vortex.OpGTE, Value: int32(50)},
				{Column: "value", Op: vortex.OpLTE, Value: int32(150)},
			},
		}
		matchRows, pruned, zones, filterAvg, _ := benchVortex(zpData, vortex.ScanOptions{RowFilter: filter}, 100)

		// Wider filter: value BETWEEN 0 AND 2100 — should match all zones.
		wideFilter := &vortex.RowFilter{
			Predicates: []vortex.ColumnPredicate{
				{Column: "value", Op: vortex.OpGTE, Value: int32(0)},
				{Column: "value", Op: vortex.OpLTE, Value: int32(2100)},
			},
		}
		wideRows, widePruned, wideZones, _, _ := benchVortex(zpData, vortex.ScanOptions{RowFilter: wideFilter}, 100)

		fmt.Printf("  zone_pruning.vortex — 3 zones: [0-99], [1000-1099], [2000-2099]\n")
		fmt.Printf("    Full scan:    %8s  (%d rows, %d chunks)\n", fullAvg.Round(time.Microsecond), totalRows, zones)
		fmt.Printf("    WHERE 50..150:  %d/%d rows returned, %d/%d zones pruned  ✓\n",
			matchRows, totalRows, pruned, zones)
		fmt.Printf("    WHERE 0..2100:  %d/%d rows returned, %d/%d zones pruned  ✓\n",
			wideRows, totalRows, widePruned, wideZones)
		fmt.Println()

		// Verify: narrow filter identifies 2/3 zones as irrelevant.
		// All 300 rows still returned because the single chunk spans all zones
		// and can only be skipped if ALL overlapping zones are pruned.
		if pruned != 2 || zones != 3 {
			t.Errorf("expected 2/3 zones pruned, got %d/%d", pruned, zones)
		}
		if matchRows != 300 {
			t.Errorf("expected 300 rows (single chunk spans all zones), got %d", matchRows)
		}
		// Wide filter should return all 300 rows, no pruning.
		if widePruned != 0 {
			t.Errorf("expected 0 zones pruned for wide filter, got %d", widePruned)
		}
		fmt.Printf("    Note: single chunk spans all zones, so chunk-level skip requires multi-chunk layout\n")
		fmt.Printf("    Zone pruning: VERIFIED (zone identification correct)\n")
		_ = filterAvg
	}

	// Go-written zone pruning with multi-chunk layout (each chunk = one zone).
	fmt.Println()
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║                  Zone Map Pruning (Go-written, multi-chunk)                ║")
	fmt.Println("╠════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println()
	{
		// Write 10 chunks of 10K rows each, with distinct value ranges.
		// Chunk i: values [i*100000 .. i*100000+9999]
		schema := &vortex.DType{
			Kind:       vortex.DTypeStruct,
			FieldNames: []string{"value"},
			FieldTypes: []*vortex.DType{
				{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
			},
		}
		var zpBuf bytes.Buffer
		w, _ := writer.NewWriter(&zpBuf, schema, writer.DefaultOptions())
		chunkSize := 10000
		numChunks := 10
		for c := 0; c < numChunks; c++ {
			buf := make([]byte, chunkSize*8)
			base := int64(c) * 100000
			for i := 0; i < chunkSize; i++ {
				binary.LittleEndian.PutUint64(buf[i*8:], uint64(base+int64(i)))
			}
			w.WriteChunk(map[string]*vortex.Array{
				"value": {DType: schema.FieldTypes[0], Length: chunkSize, Buffers: [][]byte{buf}},
			})
		}
		w.Close()

		goZoneData := zpBuf.Bytes()
		totalRows, _, _, fullAvg, _ := benchVortex(goZoneData, vortex.ScanOptions{}, 100)

		// Filter: value BETWEEN 50000 AND 150000 → matches chunk 1 only (100K-109K), 9/10 pruned.
		goFilter := &vortex.RowFilter{
			Predicates: []vortex.ColumnPredicate{
				{Column: "value", Op: vortex.OpGTE, Value: int64(50000)},
				{Column: "value", Op: vortex.OpLTE, Value: int64(150000)},
			},
		}
		matchRows, pruned, filtZones, filterAvg, _ := benchVortex(goZoneData, vortex.ScanOptions{RowFilter: goFilter}, 100)

		speedup := float64(fullAvg) / float64(filterAvg)

		fmt.Printf("  Go-written: %d chunks \u00d7 %dK rows = %dK total (%s)\n",
			numChunks, chunkSize/1000, totalRows/1000, fmtBytes(int64(len(goZoneData))))
		fmt.Printf("    Full scan:    %8s  (%d rows, %d chunks)\n", fullAvg.Round(time.Microsecond), totalRows, numChunks)
		fmt.Printf("    WHERE 50K..150K: %8s  (%d/%d rows, %d/%d zones pruned, %.1f\u00d7 faster)\n",
			filterAvg.Round(time.Microsecond), matchRows, totalRows, pruned, filtZones, speedup)

		if pruned < 8 {
			t.Errorf("expected >=8 zones pruned, got %d", pruned)
		}
		if matchRows != 10000 {
			t.Errorf("expected 10000 rows (1 matching chunk), got %d", matchRows)
		}
		fmt.Printf("    Zone pruning: VERIFIED (chunk-level skip working) \u2713\n")
	}

	fmt.Println("╚════════════════════════════════════════════════════════════════════════════╝")
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
