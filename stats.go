package vortex

// FileStatistics holds file-level per-field statistics.
type FileStatistics struct {
	FieldStats []ArrayStats
}

// ArrayStats holds statistics for an array or zone.
type ArrayStats struct {
	Min              []byte // protobuf-encoded ScalarValue
	Max              []byte // protobuf-encoded ScalarValue
	Sum              []byte // protobuf-encoded ScalarValue
	NullCount        *uint64
	IsSorted         *bool
	IsStrictSorted   *bool
	IsConstant       *bool
	UncompressedSize *uint64
	NanCount         *uint64
}

// HasMin returns true if minimum statistics are available.
func (s *ArrayStats) HasMin() bool {
	return len(s.Min) > 0
}

// HasMax returns true if maximum statistics are available.
func (s *ArrayStats) HasMax() bool {
	return len(s.Max) > 0
}
