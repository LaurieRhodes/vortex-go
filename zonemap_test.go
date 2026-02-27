package vortex

import (
	"testing"
)

func TestParseZonedMeta(t *testing.T) {
	// Simulate metadata: zone_len=8192, stats bitset with Max(3), Min(4), Sum(5), NullCount(6)
	meta := []byte{
		0x00, 0x20, 0x00, 0x00, // zone_len = 8192
		0x78, 0x00, // bits 3,4,5,6 set = 0b01111000 = 0x78
	}

	zoneLen, stats, err := ParseZonedMeta(meta)
	if err != nil {
		t.Fatal(err)
	}
	if zoneLen != 8192 {
		t.Errorf("zone_len = %d, want 8192", zoneLen)
	}

	expected := []Stat{StatMax, StatMin, StatSum, StatNullCount}
	if len(stats) != len(expected) {
		t.Fatalf("got %d stats, want %d", len(stats), len(expected))
	}
	for i, s := range stats {
		if s != expected[i] {
			t.Errorf("stat[%d] = %d, want %d", i, s, expected[i])
		}
	}
}

func TestCanPruneZone(t *testing.T) {
	// 2 zones: zone 0 has [1000, 9191], zone 1 has [9192, 10999]
	stats := map[string]*ZoneColumnStats{
		"x": {
			Min:      []interface{}{int32(1000), int32(9192)},
			Max:      []interface{}{int32(9191), int32(10999)},
			NumZones: 2,
		},
	}

	tests := []struct {
		name   string
		pred   ColumnPredicate
		zone   int
		pruned bool
	}{
		// x > 10000 → zone 0 max=9191 ≤ 10000 → prune; zone 1 max=10999 > 10000 → keep
		{"GT 10000 zone0", GT("x", int32(10000)), 0, true},
		{"GT 10000 zone1", GT("x", int32(10000)), 1, false},
		// x < 5000 → zone 0 min=1000 < 5000 → keep; zone 1 min=9192 ≥ 5000 → prune
		{"LT 5000 zone0", LT("x", int32(5000)), 0, false},
		{"LT 5000 zone1", LT("x", int32(5000)), 1, true},
		// x == 500 → zone 0 min=1000 > 500 → prune; zone 1 min=9192 > 500 → prune
		{"EQ 500 zone0", EQ("x", int32(500)), 0, true},
		{"EQ 500 zone1", EQ("x", int32(500)), 1, true},
		// x == 5000 → zone 0 range includes 5000 → keep; zone 1 range excludes → prune
		{"EQ 5000 zone0", EQ("x", int32(5000)), 0, false},
		{"EQ 5000 zone1", EQ("x", int32(5000)), 1, true},
		// x >= 9192 → zone 0 max=9191 < 9192 → prune; zone 1 min=9192 → keep
		{"GTE 9192 zone0", GTE("x", int32(9192)), 0, true},
		{"GTE 9192 zone1", GTE("x", int32(9192)), 1, false},
		// x <= 1000 → zone 0 min=1000 → keep; zone 1 min=9192 > 1000 → prune
		{"LTE 1000 zone0", LTE("x", int32(1000)), 0, false},
		{"LTE 1000 zone1", LTE("x", int32(1000)), 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pruned := CanPruneZone([]ColumnPredicate{tt.pred}, stats, tt.zone)
			if pruned != tt.pruned {
				t.Errorf("CanPruneZone(%s, zone %d) = %v, want %v",
					tt.pred.Op, tt.zone, pruned, tt.pruned)
			}
		})
	}
}
