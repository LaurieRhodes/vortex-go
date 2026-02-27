package vortex

import "fmt"

// CompareOp represents a comparison operator for zone map predicates.
type CompareOp int

const (
	OpGT  CompareOp = iota // >
	OpGTE                  // >=
	OpLT                   // <
	OpLTE                  // <=
	OpEQ                   // ==
	OpNEQ                  // !=
)

func (op CompareOp) String() string {
	switch op {
	case OpGT:
		return ">"
	case OpGTE:
		return ">="
	case OpLT:
		return "<"
	case OpLTE:
		return "<="
	case OpEQ:
		return "=="
	case OpNEQ:
		return "!="
	default:
		return "?"
	}
}

// ColumnPredicate represents a single filter condition on a column.
type ColumnPredicate struct {
	Column string
	Op     CompareOp
	Value  interface{} // must be comparable: int32, int64, uint32, uint64, float32, float64
}

// RowFilter holds a set of column predicates ANDed together.
// All predicates must be satisfiable for a zone to be read.
type RowFilter struct {
	Predicates []ColumnPredicate
}

// GT creates a ColumnPredicate: column > value.
func GT(column string, value interface{}) ColumnPredicate {
	return ColumnPredicate{Column: column, Op: OpGT, Value: value}
}

// GTE creates a ColumnPredicate: column >= value.
func GTE(column string, value interface{}) ColumnPredicate {
	return ColumnPredicate{Column: column, Op: OpGTE, Value: value}
}

// LT creates a ColumnPredicate: column < value.
func LT(column string, value interface{}) ColumnPredicate {
	return ColumnPredicate{Column: column, Op: OpLT, Value: value}
}

// LTE creates a ColumnPredicate: column <= value.
func LTE(column string, value interface{}) ColumnPredicate {
	return ColumnPredicate{Column: column, Op: OpLTE, Value: value}
}

// EQ creates a ColumnPredicate: column == value.
func EQ(column string, value interface{}) ColumnPredicate {
	return ColumnPredicate{Column: column, Op: OpEQ, Value: value}
}

// NEQ creates a ColumnPredicate: column != value.
func NEQ(column string, value interface{}) ColumnPredicate {
	return ColumnPredicate{Column: column, Op: OpNEQ, Value: value}
}

// NewRowFilter creates a RowFilter from a list of predicates (ANDed together).
func NewRowFilter(preds ...ColumnPredicate) *RowFilter {
	return &RowFilter{Predicates: preds}
}

// ReferencedColumns returns the set of column names referenced by the filter.
func (rf *RowFilter) ReferencedColumns() []string {
	seen := make(map[string]bool)
	var cols []string
	for _, p := range rf.Predicates {
		if !seen[p.Column] {
			seen[p.Column] = true
			cols = append(cols, p.Column)
		}
	}
	return cols
}

// CanPruneZone determines if a zone can be entirely skipped based on its statistics.
// Returns true if the zone CANNOT contain any matching rows.
//
// Zone pruning logic for each predicate type:
//   - column > V  → prune if zone.max <= V  (all values ≤ V, none > V)
//   - column >= V → prune if zone.max < V   (all values < V, none ≥ V)
//   - column < V  → prune if zone.min >= V  (all values ≥ V, none < V)
//   - column <= V → prune if zone.min > V   (all values > V, none ≤ V)
//   - column == V → prune if zone.max < V or zone.min > V (V outside range)
//   - column != V → prune if zone.min == zone.max == V (all values equal to V)
func CanPruneZone(preds []ColumnPredicate, zoneStats map[string]*ZoneColumnStats, zoneIdx int) bool {
	for _, pred := range preds {
		cs, ok := zoneStats[pred.Column]
		if !ok {
			// No stats for this column — can't prune.
			continue
		}
		if zoneIdx >= cs.NumZones {
			continue
		}

		minVal := getZoneStat(cs.Min, zoneIdx)
		maxVal := getZoneStat(cs.Max, zoneIdx)

		if minVal == nil || maxVal == nil {
			// Null stats — can't prune (zone might contain anything).
			continue
		}

		if canPruneWithStats(pred, minVal, maxVal) {
			return true // Any predicate that prunes → entire AND is unsatisfiable.
		}
	}
	return false
}

func getZoneStat(stats []interface{}, idx int) interface{} {
	if stats == nil || idx >= len(stats) {
		return nil
	}
	return stats[idx]
}

func canPruneWithStats(pred ColumnPredicate, minVal, maxVal interface{}) bool {
	cmpMax, okMax := compareValues(maxVal, pred.Value)
	cmpMin, okMin := compareValues(minVal, pred.Value)

	switch pred.Op {
	case OpGT:
		// column > V → prune if max <= V
		return okMax && cmpMax <= 0
	case OpGTE:
		// column >= V → prune if max < V
		return okMax && cmpMax < 0
	case OpLT:
		// column < V → prune if min >= V
		return okMin && cmpMin >= 0
	case OpLTE:
		// column <= V → prune if min > V
		return okMin && cmpMin > 0
	case OpEQ:
		// column == V → prune if max < V or min > V
		return (okMax && cmpMax < 0) || (okMin && cmpMin > 0)
	case OpNEQ:
		// column != V → prune only if min == max == V
		if okMin && okMax {
			cmpMinMax, ok := compareValues(minVal, maxVal)
			return ok && cmpMinMax == 0 && cmpMin == 0
		}
		return false
	}
	return false
}

// compareValues compares two typed values.
// Returns (result, ok) where result is <0, 0, or >0 like strcmp.
// Both values must be numeric types. The function promotes to a common type.
func compareValues(a, b interface{}) (int, bool) {
	af, aOk := toFloat64(a)
	bf, bOk := toFloat64(b)

	if !aOk || !bOk {
		return 0, false
	}

	if af < bf {
		return -1, true
	}
	if af > bf {
		return 1, true
	}
	return 0, true
}

// toFloat64 converts a numeric value to float64 for comparison.
func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case int:
		return float64(val), true
	case int8:
		return float64(val), true
	case int16:
		return float64(val), true
	case int32:
		return float64(val), true
	case int64:
		return float64(val), true
	case uint8:
		return float64(val), true
	case uint16:
		return float64(val), true
	case uint32:
		return float64(val), true
	case uint64:
		return float64(val), true
	case float32:
		return float64(val), true
	case float64:
		return val, true
	default:
		return 0, false
	}
}

// String returns a human-readable representation of the filter.
func (rf *RowFilter) String() string {
	if rf == nil || len(rf.Predicates) == 0 {
		return "(no filter)"
	}
	s := ""
	for i, p := range rf.Predicates {
		if i > 0 {
			s += " AND "
		}
		s += fmt.Sprintf("%s %s %v", p.Column, p.Op, p.Value)
	}
	return s
}
