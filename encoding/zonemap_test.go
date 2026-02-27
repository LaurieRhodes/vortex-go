package encoding

import (
	"os"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

func TestReadZoneMapPcoInts(t *testing.T) {
	f, err := os.Open("../testdata/rust/pco_ints.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	// Read zone stats for "counter" column.
	colLayout, colDType, _, err := vf.FindColumnLayout("counter")
	if err != nil {
		t.Fatal(err)
	}

	if !colLayout.IsZoned() {
		t.Fatal("expected Zoned layout for counter column")
	}

	zm, err := vf.ReadZoneMap(colLayout, colDType)
	if err != nil {
		t.Fatal(err)
	}

	if zm.ZoneLen != 8192 {
		t.Errorf("zone_len = %d, want 8192", zm.ZoneLen)
	}
	if zm.NumZones != 1 {
		t.Errorf("num_zones = %d, want 1", zm.NumZones)
	}

	cs := zm.Columns[""]
	if cs == nil {
		t.Fatal("no column stats found")
	}

	// counter is 0..1999, so min=0, max=1999
	t.Logf("min=%v max=%v null_count=%v", cs.Min, cs.Max, cs.NullCount)

	if len(cs.Min) != 1 || len(cs.Max) != 1 {
		t.Fatalf("expected 1 zone, got min=%d max=%d", len(cs.Min), len(cs.Max))
	}

	minVal := toTestFloat64(t, cs.Min[0])
	maxVal := toTestFloat64(t, cs.Max[0])

	if minVal != 0 {
		t.Errorf("min = %v, want 0", minVal)
	}
	if maxVal != 1999 {
		t.Errorf("max = %v, want 1999", maxVal)
	}
}

func TestReadZoneMapLargeInts(t *testing.T) {
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

	// large_ints has 10000 rows with zone_len=8192 → 2 zones
	colLayout, colDType, _, err := vf.FindColumnLayout("i32_col")
	if err != nil {
		t.Fatal(err)
	}

	zm, err := vf.ReadZoneMap(colLayout, colDType)
	if err != nil {
		t.Fatal(err)
	}

	if zm.NumZones != 2 {
		t.Errorf("num_zones = %d, want 2", zm.NumZones)
	}

	cs := zm.Columns[""]
	if cs == nil {
		t.Fatal("no column stats found")
	}

	t.Logf("zone 0: min=%v max=%v", cs.Min[0], cs.Max[0])
	t.Logf("zone 1: min=%v max=%v", cs.Min[1], cs.Max[1])

	// i32_col values are 1000..10999
	// Zone 0 (rows 0-8191): min=1000, max=9191
	// Zone 1 (rows 8192-9999): min=9192, max=10999
	min0 := toTestFloat64(t, cs.Min[0])
	max0 := toTestFloat64(t, cs.Max[0])
	min1 := toTestFloat64(t, cs.Min[1])
	max1 := toTestFloat64(t, cs.Max[1])

	if min0 != 1000 {
		t.Errorf("zone 0 min = %v, want 1000", min0)
	}
	if max0 != 9191 {
		t.Errorf("zone 0 max = %v, want 9191", max0)
	}
	if min1 != 9192 {
		t.Errorf("zone 1 min = %v, want 9192", min1)
	}
	if max1 != 10999 {
		t.Errorf("zone 1 max = %v, want 10999", max1)
	}
}

func TestReadZoneMapAllColumns(t *testing.T) {
	// Verify zone map reading works across different data types.
	f, err := os.Open("../testdata/rust/pco_ints.vortex")
	if err != nil {
		t.Skip("test file not available:", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	for _, colName := range vf.ColumnNames() {
		colLayout, colDType, _, err := vf.FindColumnLayout(colName)
		if err != nil {
			t.Errorf("FindColumnLayout(%s): %v", colName, err)
			continue
		}

		if !colLayout.IsZoned() {
			t.Logf("column %s: not Zoned, skipping", colName)
			continue
		}

		zm, err := vf.ReadZoneMap(colLayout, colDType)
		if err != nil {
			t.Errorf("ReadZoneMap(%s): %v", colName, err)
			continue
		}

		cs := zm.Columns[""]
		t.Logf("column %s: zones=%d zoneLen=%d min=%v max=%v nulls=%v",
			colName, zm.NumZones, zm.ZoneLen, cs.Min, cs.Max, cs.NullCount)
	}
}

// toTestFloat64 converts a value to float64, failing the test if not numeric.
func toTestFloat64(t *testing.T, v interface{}) float64 {
	t.Helper()
	switch val := v.(type) {
	case int8:
		return float64(val)
	case int16:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case uint8:
		return float64(val)
	case uint16:
		return float64(val)
	case uint32:
		return float64(val)
	case uint64:
		return float64(val)
	case float32:
		return float64(val)
	case float64:
		return val
	default:
		t.Fatalf("value not numeric: %T %v", v, v)
		return 0
	}
}
