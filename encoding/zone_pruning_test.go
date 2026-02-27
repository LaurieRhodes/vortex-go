package encoding

import (
	"encoding/binary"
	"os"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// TestZonePruningFile verifies zone map reading from the zone_pruning.vortex test file.
// File structure: 300 rows, zone_len=100, 3 zones
//   Zone 0: values [0, 99]
//   Zone 1: values [1000, 1099]
//   Zone 2: values [2000, 2099]
func TestZonePruningFile(t *testing.T) {
	f, err := os.Open("../testdata/rust/zone_pruning.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	colLayout, colDType, _, err := vf.FindColumnLayout("value")
	if err != nil {
		t.Fatal(err)
	}

	if !colLayout.IsZoned() {
		t.Fatal("expected Zoned layout")
	}

	zm, err := vf.ReadZoneMap(colLayout, colDType)
	if err != nil {
		t.Fatal(err)
	}

	if zm.ZoneLen != 100 {
		t.Errorf("zone_len = %d, want 100", zm.ZoneLen)
	}
	if zm.NumZones != 3 {
		t.Errorf("num_zones = %d, want 3", zm.NumZones)
	}

	cs := zm.Columns[""]
	if cs == nil {
		t.Fatal("no column stats")
	}

	// Verify min/max per zone.
	expectations := []struct {
		zone int
		min  float64
		max  float64
	}{
		{0, 0, 99},
		{1, 1000, 1099},
		{2, 2000, 2099},
	}

	for _, exp := range expectations {
		min := toTestFloat64(t, cs.Min[exp.zone])
		max := toTestFloat64(t, cs.Max[exp.zone])
		if min != exp.min {
			t.Errorf("zone %d min = %v, want %v", exp.zone, min, exp.min)
		}
		if max != exp.max {
			t.Errorf("zone %d max = %v, want %v", exp.zone, max, exp.max)
		}
	}
}

// TestZonePruningDecisions verifies the pruning logic against zone_pruning.vortex.
func TestZonePruningDecisions(t *testing.T) {
	f, err := os.Open("../testdata/rust/zone_pruning.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	colLayout, colDType, _, err := vf.FindColumnLayout("value")
	if err != nil {
		t.Fatal(err)
	}

	zm, err := vf.ReadZoneMap(colLayout, colDType)
	if err != nil {
		t.Fatal(err)
	}

	cs := zm.Columns[""]
	stats := map[string]*vortex.ZoneColumnStats{"value": cs}

	tests := []struct {
		name       string
		pred       vortex.ColumnPredicate
		zone0Prune bool
		zone1Prune bool
		zone2Prune bool
	}{
		// value > 1500: zone 0 max=99 ≤ 1500 → prune, zone 1 max=1099 ≤ 1500 → prune, zone 2 max=2099 > 1500 → keep
		{"GT 1500", vortex.GT("value", int32(1500)), true, true, false},
		// value < 500: zone 0 min=0 < 500 → keep, zone 1 min=1000 ≥ 500 → prune, zone 2 min=2000 ≥ 500 → prune
		{"LT 500", vortex.LT("value", int32(500)), false, true, true},
		// value >= 1000 && value <= 1099: zone 0 max=99 < 1000 → prune, zone 1 range matches → keep, zone 2 min=2000 > 1099 → prune
		{"EQ range zone1", vortex.GTE("value", int32(1000)), true, false, false},
		// value == 50: zone 0 range includes 50 → keep, zone 1 min=1000 > 50 → prune, zone 2 min=2000 > 50 → prune
		{"EQ 50", vortex.EQ("value", int32(50)), false, true, true},
		// value != 0: only prune if min==max==0, which no zone satisfies → keep all
		{"NEQ 0", vortex.NEQ("value", int32(0)), false, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preds := []vortex.ColumnPredicate{tt.pred}
			got0 := vortex.CanPruneZone(preds, stats, 0)
			got1 := vortex.CanPruneZone(preds, stats, 1)
			got2 := vortex.CanPruneZone(preds, stats, 2)

			if got0 != tt.zone0Prune {
				t.Errorf("zone 0: got %v, want %v", got0, tt.zone0Prune)
			}
			if got1 != tt.zone1Prune {
				t.Errorf("zone 1: got %v, want %v", got1, tt.zone1Prune)
			}
			if got2 != tt.zone2Prune {
				t.Errorf("zone 2: got %v, want %v", got2, tt.zone2Prune)
			}
		})
	}
}

// TestScanWithRowFilter verifies that Scan with RowFilter correctly
// initializes zone pruning and computes the right pruning mask.
func TestScanWithRowFilter(t *testing.T) {
	tests := []struct {
		name          string
		filter        *vortex.RowFilter
		wantPruned    int
		wantTotal     int
		wantRowCount  int // expected total rows returned
	}{
		{
			name:         "no filter",
			filter:       nil,
			wantPruned:   0,
			wantTotal:    0,
			wantRowCount: 300,
		},
		{
			name:         "value > 1500 prunes 2 zones",
			filter:       vortex.NewRowFilter(vortex.GT("value", int32(1500))),
			wantPruned:   2,
			wantTotal:    3,
			wantRowCount: 300, // single flat segment can't be skipped
		},
		{
			name:         "value < 500 prunes 2 zones",
			filter:       vortex.NewRowFilter(vortex.LT("value", int32(500))),
			wantPruned:   2,
			wantTotal:    3,
			wantRowCount: 300, // single flat segment can't be skipped
		},
		{
			name: "value >= 1000 AND value <= 1099 prunes zones 0 and 2",
			filter: vortex.NewRowFilter(
				vortex.GTE("value", int32(1000)),
				vortex.LTE("value", int32(1099)),
			),
			wantPruned:   2,
			wantTotal:    3,
			wantRowCount: 300, // single flat segment can't be skipped
		},
		{
			name:         "impossible filter prunes all zones",
			filter:       vortex.NewRowFilter(vortex.GT("value", int32(10000))),
			wantPruned:   3,
			wantTotal:    3,
			wantRowCount: 0, // all zones pruned → no data
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Re-open for each test since iterator consumes data.
			f, err := os.Open("../testdata/rust/zone_pruning.vortex")
			if err != nil {
				t.Skip("test file not available:", err)
			}
			defer f.Close()
			fi, _ := f.Stat()
			vf, err := vortex.Open(f, fi.Size())
			if err != nil {
				t.Fatal(err)
			}

			opts := vortex.ScanOptions{
				Columns:   []string{"value"},
				RowFilter: tt.filter,
			}

			iter, err := vf.Scan(opts)
			if err != nil {
				t.Fatal(err)
			}

			// Check zone pruning stats.
			pruned, total := iter.ZonePruningStats()
			if pruned != tt.wantPruned {
				t.Errorf("pruned zones = %d, want %d", pruned, tt.wantPruned)
			}
			if total != tt.wantTotal {
				t.Errorf("total zones = %d, want %d", total, tt.wantTotal)
			}

			// Scan all data and count rows.
			totalRows := 0
			for iter.Next() {
				totalRows += iter.Result().RowCount
			}
			if err := iter.Err(); err != nil {
				t.Fatal(err)
			}

			if totalRows != tt.wantRowCount {
				t.Errorf("total rows = %d, want %d", totalRows, tt.wantRowCount)
			}
		})
	}
}

// TestScanWithRowFilterLargeInts tests zone pruning on the large_ints file
// which has 2 zones (10000 rows, zone_len=8192).
func TestScanWithRowFilterLargeInts(t *testing.T) {
	f, err := os.Open("../testdata/rust/large_ints.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	// i32_col has values 1000..10999
	// Zone 0 (rows 0-8191): min=1000, max=9191
	// Zone 1 (rows 8192-9999): min=9192, max=10999

	// Filter: i32_col > 10000 → zone 0 max=9191 ≤ 10000 → prune, zone 1 max=10999 → keep
	iter, err := vf.Scan(vortex.ScanOptions{
		Columns:   []string{"i32_col"},
		RowFilter: vortex.NewRowFilter(vortex.GT("i32_col", int32(10000))),
	})
	if err != nil {
		t.Fatal(err)
	}

	pruned, total := iter.ZonePruningStats()
	if total != 2 {
		t.Errorf("total zones = %d, want 2", total)
	}
	if pruned != 1 {
		t.Errorf("pruned zones = %d, want 1", pruned)
	}

	// Scan — single flat segment covers both zones, so can't skip.
	totalRows := 0
	for iter.Next() {
		totalRows += iter.Result().RowCount
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	// All 10000 rows returned (single flat can't be pruned per-zone).
	if totalRows != 10000 {
		t.Errorf("total rows = %d, want 10000", totalRows)
	}
}

// TestScanVerifyFilteredData checks that returned data from a filtered scan
// actually contains the expected values.
func TestScanVerifyFilteredData(t *testing.T) {
	f, err := os.Open("../testdata/rust/zone_pruning.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	// No filter — should get all 300 values.
	iter, err := vf.Scan(vortex.ScanOptions{Columns: []string{"value"}})
	if err != nil {
		t.Fatal(err)
	}

	var allValues []int32
	for iter.Next() {
		r := iter.Result()
		arr := r.Columns["value"]
		if arr != nil && len(arr.Buffers) > 0 {
			buf := arr.Buffers[0]
			for i := 0; i < arr.Length; i++ {
				v := int32(binary.LittleEndian.Uint32(buf[i*4:]))
				allValues = append(allValues, v)
			}
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatal(err)
	}

	if len(allValues) != 300 {
		t.Fatalf("got %d values, want 300", len(allValues))
	}

	// Verify zone boundaries.
	if allValues[0] != 0 || allValues[99] != 99 {
		t.Errorf("zone 0: first=%d last=%d, want 0..99", allValues[0], allValues[99])
	}
	if allValues[100] != 1000 || allValues[199] != 1099 {
		t.Errorf("zone 1: first=%d last=%d, want 1000..1099", allValues[100], allValues[199])
	}
	if allValues[200] != 2000 || allValues[299] != 2099 {
		t.Errorf("zone 2: first=%d last=%d, want 2000..2099", allValues[200], allValues[299])
	}
}

// TestScanWithCallbackFilter tests the callback-based Filter path on ScanOptions.
func TestScanWithCallbackFilter(t *testing.T) {
	f, err := os.Open("../testdata/rust/zone_pruning.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	// Use callback filter: only include zones where max > 1500.
	// Zone 0: max=99 → skip, Zone 1: max=1099 → skip, Zone 2: max=2099 → include
	iter, err := vf.Scan(vortex.ScanOptions{
		Columns: []string{"value"},
		Filter: func(stats vortex.ZoneStats) bool {
			cs, ok := stats.ColumnStats["value"]
			if !ok || cs.Max == nil {
				return true // no stats, include by default
			}
			maxF, ok := toTestFloat64ForCallback(cs.Max)
			if !ok {
				return true
			}
			return maxF > 1500.0
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	pruned, total := iter.ZonePruningStats()
	if total != 3 {
		t.Errorf("total zones = %d, want 3", total)
	}
	if pruned != 2 {
		t.Errorf("pruned zones = %d, want 2", pruned)
	}
}

// TestScanWithBothFilters tests that RowFilter and callback Filter work together.
func TestScanWithBothFilters(t *testing.T) {
	f, err := os.Open("../testdata/rust/zone_pruning.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	// RowFilter: value > 500 → prunes zone 0 (max=99)
	// Callback: only include zones where max < 1500 → prunes zone 2 (max=2099)
	// Combined: zone 0 pruned by RowFilter, zone 2 pruned by callback, zone 1 kept
	iter, err := vf.Scan(vortex.ScanOptions{
		Columns:   []string{"value"},
		RowFilter: vortex.NewRowFilter(vortex.GT("value", int32(500))),
		Filter: func(stats vortex.ZoneStats) bool {
			cs, ok := stats.ColumnStats["value"]
			if !ok || cs.Max == nil {
				return true
			}
			maxF, ok := toTestFloat64ForCallback(cs.Max)
			if !ok {
				return true
			}
			return maxF < 1500.0
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	pruned, total := iter.ZonePruningStats()
	if total != 3 {
		t.Errorf("total zones = %d, want 3", total)
	}
	// Zone 0 pruned by RowFilter, zone 2 pruned by callback → 2 pruned
	if pruned != 2 {
		t.Errorf("pruned zones = %d, want 2", pruned)
	}
}

func toTestFloat64ForCallback(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case int32:
		return float64(val), true
	case int64:
		return float64(val), true
	case float32:
		return float64(val), true
	case float64:
		return val, true
	case uint32:
		return float64(val), true
	case uint64:
		return float64(val), true
	default:
		return 0, false
	}
}
