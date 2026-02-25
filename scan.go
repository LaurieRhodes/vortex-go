package vortex

import "fmt"

// ScanOptions controls how a file is scanned.
type ScanOptions struct {
	// Columns lists the column names to read. If nil, all columns are read.
	Columns []string

	// Filter is an optional zone-level predicate. If it returns false for a zone's
	// statistics, that zone is skipped entirely.
	Filter func(stats ZoneStats) bool

	// Limit is the maximum number of rows to return. Zero means no limit.
	Limit int
}

// ZoneStats holds per-zone statistics used for filter pushdown.
type ZoneStats struct {
	// ColumnStats maps column name → stats for this zone.
	ColumnStats map[string]*ArrayStats
	RowCount    uint64
}

// Scan reads data from the file with optional column projection and zone pruning.
func (f *File) Scan(opts ScanOptions) (*ArrayIterator, error) {
	// TODO: implement layout tree navigation with projection and zone pruning
	return nil, fmt.Errorf("vortex: Scan not yet implemented")
}

// ArrayIterator iterates over chunks of decoded arrays.
type ArrayIterator struct {
	// TODO: implement
}

// Next advances to the next chunk. Returns false when exhausted.
func (it *ArrayIterator) Next() bool {
	return false
}

// Array returns the current chunk's decoded data.
func (it *ArrayIterator) Array() *Array {
	return nil
}

// Err returns any error encountered during iteration.
func (it *ArrayIterator) Err() error {
	return nil
}
