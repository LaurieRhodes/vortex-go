package vortex

import (
	"fmt"
)

// ScanOptions controls how a file is scanned.
type ScanOptions struct {
	// Columns lists the column names to read. If nil, all columns are read.
	Columns []string

	// Filter is an optional zone-level predicate callback. If it returns false
	// for a zone's statistics, that zone is skipped entirely.
	// The callback receives per-zone typed statistics for all projected columns.
	Filter func(stats ZoneStats) bool

	// RowFilter is a declarative zone-level filter using column predicates.
	// When set, zone map statistics are decoded and zones that cannot contain
	// matching rows are skipped entirely. Predicates are ANDed together.
	RowFilter *RowFilter

	// Limit is the maximum number of rows to return. Zero means no limit.
	Limit int
}

// ZoneStats holds per-zone statistics for the Filter callback.
type ZoneStats struct {
	// ColumnStats maps column name → typed min/max stats for this zone.
	ColumnStats map[string]*ColumnZoneStats
	RowCount    uint64
	ZoneIndex   int
}

// ColumnZoneStats holds typed statistics for one column in one zone.
type ColumnZoneStats struct {
	Min interface{} // typed value: int32, int64, uint32, uint64, float32, float64, or nil
	Max interface{} // typed value, or nil
}

// ScanResult holds decoded column data for one chunk of a scan.
type ScanResult struct {
	// Columns maps column name → decoded Array for this chunk.
	Columns map[string]*Array

	// RowCount is the number of rows in this chunk.
	RowCount int
}

// scanColumn holds pre-computed navigation info for one column.
type scanColumn struct {
	name  string
	dtype *DType

	// For non-Dict columns: flat layouts to iterate.
	flatLayouts []*Layout

	// For Dict columns: separate dict values layout and codes flat layouts.
	isDict         bool
	dictValuesFlats []*Layout // flat layouts for dictionary values
	dictCodesFlats  []*Layout // flat layouts for dictionary codes
	dictValuesRows  uint64    // row count of values layout (= dict size)
	dictCodesPType  PType     // PType of the codes array; -1 = not known from metadata

	// Zone map support: the Zoned layout node for this column (nil if not Zoned).
	zonedLayout *Layout
}

// Scan reads data from the file with optional column projection and zone pruning.
//
// The returned ScanIterator yields chunks of decoded data. Each call to Next()
// populates the iterator with the next chunk across all projected columns.
//
// Example:
//
//	iter, err := f.Scan(ScanOptions{Columns: []string{"id", "name"}})
//	for iter.Next() {
//	    result := iter.Result()
//	    // result.Columns["id"], result.Columns["name"]
//	}
//	if err := iter.Err(); err != nil { ... }
func (f *File) Scan(opts ScanOptions) (*ScanIterator, error) {
	if f.dtype == nil || f.dtype.Kind != DTypeStruct {
		return nil, fmt.Errorf("vortex: file schema is not a struct")
	}

	// Determine which columns to read.
	colNames := opts.Columns
	if len(colNames) == 0 {
		colNames = f.dtype.FieldNames
	}

	// Pre-compute layout navigation for each column.
	columns := make([]scanColumn, len(colNames))
	for i, name := range colNames {
		sc, err := f.prepareScanColumn(name)
		if err != nil {
			return nil, fmt.Errorf("vortex: preparing column %q: %w", name, err)
		}
		columns[i] = sc
	}

	it := &ScanIterator{
		file:    f,
		opts:    opts,
		columns: columns,
	}

	// Initialize zone pruning if RowFilter or Filter callback is set.
	hasRowFilter := opts.RowFilter != nil && len(opts.RowFilter.Predicates) > 0
	hasCallbackFilter := opts.Filter != nil
	if hasRowFilter || hasCallbackFilter {
		if err := it.initZonePruning(); err != nil {
			// Non-fatal: if zone stats can't be read, proceed without pruning.
			_ = err
		}
	}

	return it, nil
}

// readZoneStatsForFilter reads zone map statistics needed for filtering.
// Returns per-column zone stats, the zone length, and number of zones.
func (f *File) readZoneStatsForFilter(columns []scanColumn) (map[string]*ZoneColumnStats, uint32, int, error) {
	allStats := make(map[string]*ZoneColumnStats)
	var zoneLen uint32
	var numZones int

	for i := range columns {
		sc := &columns[i]
		if sc.zonedLayout == nil {
			continue
		}

		zm, err := f.ReadZoneMap(sc.zonedLayout, sc.dtype)
		if err != nil {
			// Non-fatal: can't read stats for this column, skip pruning for it.
			continue
		}

		// Use the first column's zone metadata as canonical.
		if zoneLen == 0 {
			zoneLen = zm.ZoneLen
			numZones = zm.NumZones
		}

		// Map the stats from the empty key to the column name.
		if cs, ok := zm.Columns[""]; ok {
			allStats[sc.name] = cs
		}
	}

	return allStats, zoneLen, numZones, nil
}

// initZonePruning reads zone map stats for the RowFilter columns and computes
// which zones can be entirely skipped.
func (it *ScanIterator) initZonePruning() error {
	// Read zone stats for all projected columns.
	allStats, zoneLen, numZones, err := it.file.readZoneStatsForFilter(it.columns)
	if err != nil {
		return err
	}
	if numZones == 0 || zoneLen == 0 {
		return nil // No zone info available.
	}

	it.zoneLen = zoneLen
	it.numZones = numZones
	it.zoneStats = allStats

	// Compute pruning mask: which zones can be skipped.
	it.zonePruned = make([]bool, numZones)
	prunedCount := 0

	for z := 0; z < numZones; z++ {
		pruned := false

		// Apply declarative RowFilter predicates.
		if rf := it.opts.RowFilter; rf != nil && len(rf.Predicates) > 0 {
			if CanPruneZone(rf.Predicates, allStats, z) {
				pruned = true
			}
		}

		// Apply callback Filter: false return means skip this zone.
		if !pruned && it.opts.Filter != nil {
			zs := buildZoneStats(allStats, z, zoneLen, numZones)
			if !it.opts.Filter(zs) {
				pruned = true
			}
		}

		if pruned {
			it.zonePruned[z] = true
			prunedCount++
		}
	}

	// If no zones were pruned, disable pruning to avoid overhead.
	if prunedCount == 0 {
		it.zonePruned = nil
	}

	return nil
}

// buildZoneStats constructs a ZoneStats for the callback Filter interface.
func buildZoneStats(allStats map[string]*ZoneColumnStats, zoneIdx int, zoneLen uint32, numZones int) ZoneStats {
	cs := make(map[string]*ColumnZoneStats)
	for name, zcs := range allStats {
		if zoneIdx >= zcs.NumZones {
			continue
		}
		czs := &ColumnZoneStats{}
		if zcs.Min != nil && zoneIdx < len(zcs.Min) {
			czs.Min = zcs.Min[zoneIdx]
		}
		if zcs.Max != nil && zoneIdx < len(zcs.Max) {
			czs.Max = zcs.Max[zoneIdx]
		}
		cs[name] = czs
	}
	return ZoneStats{
		ColumnStats: cs,
		RowCount:    uint64(zoneLen),
		ZoneIndex:   zoneIdx,
	}
}

// computeChunkOffsets builds cumulative row offsets for each chunk.
// Returns a slice where chunkOffsets[i] is the starting row of chunk i.
func (it *ScanIterator) computeChunkOffsets() {
	if it.chunkOffsets != nil {
		return
	}

	// Use the first column to determine chunk row counts.
	if len(it.columns) == 0 {
		return
	}
	sc := &it.columns[0]

	var flats []*Layout
	if sc.isDict {
		flats = sc.dictCodesFlats
	} else {
		flats = sc.flatLayouts
	}

	offsets := make([]int64, len(flats)+1)
	var cum int64
	for i, flat := range flats {
		offsets[i] = cum
		cum += int64(flat.RowCount)
	}
	offsets[len(flats)] = cum
	it.chunkOffsets = offsets
}

// isChunkPruned returns true if all zones overlapping the given chunk
// are pruned, meaning the chunk can be entirely skipped.
func (it *ScanIterator) isChunkPruned(chunkIdx int) bool {
	if it.zonePruned == nil || it.zoneLen == 0 {
		return false
	}

	// Compute chunk offsets if not done yet.
	it.computeChunkOffsets()
	if chunkIdx+1 >= len(it.chunkOffsets) {
		return false
	}

	chunkStart := it.chunkOffsets[chunkIdx]
	chunkEnd := it.chunkOffsets[chunkIdx+1] // exclusive

	if chunkStart >= chunkEnd {
		return false
	}

	// Determine which zones this chunk overlaps.
	zl := int64(it.zoneLen)
	firstZone := int(chunkStart / zl)
	lastZone := int((chunkEnd - 1) / zl) // inclusive

	// Clamp to valid zone range.
	if firstZone >= it.numZones {
		return false
	}
	if lastZone >= it.numZones {
		lastZone = it.numZones - 1
	}

	// The chunk can be pruned only if ALL overlapping zones are pruned.
	for z := firstZone; z <= lastZone; z++ {
		if !it.zonePruned[z] {
			return false
		}
	}
	return true
}

// prepareScanColumn navigates the layout tree for a single column and
// returns all the metadata needed to decode it on demand.
func (f *File) prepareScanColumn(name string) (scanColumn, error) {
	colLayout, colDType, _, err := f.FindColumnLayout(name)
	if err != nil {
		return scanColumn{}, err
	}

	// Navigate through Zoned (vortex.stats) layout to reach data.
	dataLayout, dictValuesLayout := NavigateToData(colLayout)

	sc := scanColumn{
		name:           name,
		dtype:          colDType,
		dictCodesPType: PType(-1), // sentinel: not known
	}

	// Capture Zoned layout reference for zone map reading.
	if colLayout.IsZoned() {
		sc.zonedLayout = colLayout
	}

	if dictValuesLayout != nil {
		// Dict layout: separate values and codes.
		sc.isDict = true
		sc.dictValuesFlats = CollectFlatLayouts(dictValuesLayout)
		sc.dictCodesFlats = CollectFlatLayouts(dataLayout)
		sc.dictValuesRows = dictValuesLayout.RowCount

		// Extract codes PType from the Dict layout metadata if available.
		sc.dictCodesPType = ParseDictLayoutCodesPType(colLayout)

		if len(sc.dictValuesFlats) == 0 {
			return scanColumn{}, fmt.Errorf("column %q: no flat layouts for dict values", name)
		}
		if len(sc.dictCodesFlats) == 0 {
			return scanColumn{}, fmt.Errorf("column %q: no flat layouts for dict codes", name)
		}
	} else {
		sc.flatLayouts = CollectFlatLayouts(dataLayout)
		if len(sc.flatLayouts) == 0 {
			return scanColumn{}, fmt.Errorf("column %q: no flat layouts found", name)
		}
	}

	return sc, nil
}

// ScanIterator iterates over chunks of decoded scan results.
type ScanIterator struct {
	file    *File
	opts    ScanOptions
	columns []scanColumn

	// Iteration state.
	chunkIdx  int
	maxChunks int  // computed lazily
	result    *ScanResult
	err       error
	started   bool
	done      bool
	rowsSoFar int

	// Zone pruning state. Nil when no filter is active.
	zonePruned   []bool  // zonePruned[i] == true means zone i should be skipped
	zoneLen      uint32  // rows per zone
	numZones     int     // total number of zones
	chunkOffsets []int64 // cumulative row offset for each chunk (computed lazily)
	zoneStats    map[string]*ZoneColumnStats // per-column zone stats (for callback filter)
}

// Next advances to the next chunk. Returns false when exhausted or on error.
func (it *ScanIterator) Next() bool {
	if it.done {
		return false
	}

	// Compute max chunks on first call.
	if !it.started {
		it.started = true
		it.maxChunks = it.computeMaxChunks()
		if it.maxChunks == 0 {
			it.done = true
			return false
		}
	}

	if it.chunkIdx >= it.maxChunks {
		it.done = true
		return false
	}

	// Check row limit.
	if it.opts.Limit > 0 && it.rowsSoFar >= it.opts.Limit {
		it.done = true
		return false
	}

	// Skip chunks that are entirely within pruned zones.
	for it.chunkIdx < it.maxChunks && it.isChunkPruned(it.chunkIdx) {
		// Advance past this chunk's rows for offset tracking.
		it.computeChunkOffsets()
		if it.chunkIdx+1 < len(it.chunkOffsets) {
			skippedRows := int(it.chunkOffsets[it.chunkIdx+1] - it.chunkOffsets[it.chunkIdx])
			_ = skippedRows // rows skipped, not counted toward limit
		}
		it.chunkIdx++
	}
	if it.chunkIdx >= it.maxChunks {
		it.done = true
		return false
	}

	// Decode this chunk for each column.
	result := &ScanResult{
		Columns: make(map[string]*Array, len(it.columns)),
	}

	chunkRowCount := 0
	for i := range it.columns {
		arr, rows, err := it.decodeColumnChunk(&it.columns[i], it.chunkIdx)
		if err != nil {
			it.err = err
			it.done = true
			return false
		}
		result.Columns[it.columns[i].name] = arr
		if rows > chunkRowCount {
			chunkRowCount = rows
		}
	}

	// Apply row limit: truncate if needed.
	if it.opts.Limit > 0 {
		remaining := it.opts.Limit - it.rowsSoFar
		if chunkRowCount > remaining {
			chunkRowCount = remaining
			// Truncate arrays.
			for name, arr := range result.Columns {
				result.Columns[name] = truncateArray(arr, chunkRowCount)
			}
		}
	}

	result.RowCount = chunkRowCount
	it.result = result
	it.rowsSoFar += chunkRowCount
	it.chunkIdx++
	return true
}

// Result returns the current chunk's scan result. Only valid after Next() returns true.
func (it *ScanIterator) Result() *ScanResult {
	return it.result
}

// Err returns any error encountered during iteration.
func (it *ScanIterator) Err() error {
	return it.err
}

// ZonePruningStats returns zone pruning information for diagnostics.
// Returns the number of zones pruned and total zones, or (0, 0) if
// zone pruning is not active.
func (it *ScanIterator) ZonePruningStats() (pruned, total int) {
	if it.zonePruned == nil {
		return 0, 0
	}
	for _, p := range it.zonePruned {
		if p {
			pruned++
		}
	}
	return pruned, len(it.zonePruned)
}

// computeMaxChunks determines the number of chunks to iterate.
// For non-Dict columns this is the number of flat layouts;
// for Dict columns this is the number of codes flat layouts.
// All columns should have the same chunk count.
func (it *ScanIterator) computeMaxChunks() int {
	maxChunks := 0
	for _, sc := range it.columns {
		n := len(sc.flatLayouts)
		if sc.isDict {
			n = len(sc.dictCodesFlats)
		}
		if n > maxChunks {
			maxChunks = n
		}
	}
	return maxChunks
}

// decodeColumnChunk decodes one chunk of one column.
func (it *ScanIterator) decodeColumnChunk(sc *scanColumn, chunkIdx int) (*Array, int, error) {
	if sc.isDict {
		return it.decodeDictColumnChunk(sc, chunkIdx)
	}
	return it.decodeSimpleColumnChunk(sc, chunkIdx)
}

// decodeSimpleColumnChunk decodes a non-Dict column chunk.
func (it *ScanIterator) decodeSimpleColumnChunk(sc *scanColumn, chunkIdx int) (*Array, int, error) {
	if chunkIdx >= len(sc.flatLayouts) {
		// This column has fewer chunks; return an empty array.
		return &Array{DType: sc.dtype, Length: 0}, 0, nil
	}

	flat := sc.flatLayouts[chunkIdx]
	node, buffers, err := it.file.ReadFlatSegment(flat)
	if err != nil {
		return nil, 0, fmt.Errorf("reading flat segment for %q: %w", sc.name, err)
	}

	arr, err := decodeArrayInternal(node, buffers, sc.dtype, flat.RowCount)
	if err != nil {
		return nil, 0, fmt.Errorf("decoding %q (encoding=%s): %w", sc.name, node.EncodingID, err)
	}

	return arr, int(flat.RowCount), nil
}

// decodeDictColumnChunk decodes a Dict layout column chunk.
// It decodes the dictionary values (shared) and the codes for this chunk,
// then returns a combined Dict-style Array.
func (it *ScanIterator) decodeDictColumnChunk(sc *scanColumn, chunkIdx int) (*Array, int, error) {
	// Decode dictionary values (decode all values flats and use the first for now).
	// In typical files there's one values segment.
	var valuesArr *Array
	for _, flat := range sc.dictValuesFlats {
		node, buffers, err := it.file.ReadFlatSegment(flat)
		if err != nil {
			return nil, 0, fmt.Errorf("reading dict values for %q: %w", sc.name, err)
		}
		arr, err := decodeArrayInternal(node, buffers, sc.dtype, flat.RowCount)
		if err != nil {
			return nil, 0, fmt.Errorf("decoding dict values for %q: %w", sc.name, err)
		}
		valuesArr = arr
		break // typically one values segment
	}
	if valuesArr == nil {
		return nil, 0, fmt.Errorf("no dict values decoded for %q", sc.name)
	}

	// Decode codes for this chunk.
	if chunkIdx >= len(sc.dictCodesFlats) {
		return &Array{DType: sc.dtype, Length: 0}, 0, nil
	}

	codesFlat := sc.dictCodesFlats[chunkIdx]
	node, buffers, err := it.file.ReadFlatSegment(codesFlat)
	if err != nil {
		return nil, 0, fmt.Errorf("reading dict codes for %q: %w", sc.name, err)
	}

	// Determine codes PType: prefer metadata from Dict layout, fall back to heuristic.
	var codesDType *DType
	if sc.dictCodesPType >= 0 {
		// We have a known codes PType from the Dict layout metadata.
		codesDType = &DType{Kind: DTypePrimitive, PType: sc.dictCodesPType}
	} else {
		// Fall back to heuristic detection.
		codesDType = detectCodesPType(node, buffers, codesFlat.RowCount, sc.dictValuesRows)
	}
	codesArr, err := decodeArrayInternal(node, buffers, codesDType, codesFlat.RowCount)
	if err != nil {
		return nil, 0, fmt.Errorf("decoding dict codes for %q: %w", sc.name, err)
	}

	// Return as Dict-style array: children[0] = codes, children[1] = values.
	result := &Array{
		DType:    sc.dtype,
		Length:   codesArr.Length,
		Children: []*Array{codesArr, valuesArr},
		Stats:    codesArr.Stats,
	}

	return result, int(codesFlat.RowCount), nil
}

// detectCodesPType determines the PType for dictionary codes.
// For primitive encoding, derive from buffer size / row count.
// Otherwise fall back based on dictionary size.
func detectCodesPType(node *ArrayNode, buffers [][]byte, rowCount, dictSize uint64) *DType {
	if node.EncodingID == "vortex.primitive" && len(node.BufferIndices) > 0 {
		idx := node.BufferIndices[0]
		if int(idx) < len(buffers) && rowCount > 0 {
			bytesPerElem := uint64(len(buffers[idx])) / rowCount
			switch bytesPerElem {
			case 1:
				return &DType{Kind: DTypePrimitive, PType: PTypeU8}
			case 2:
				return &DType{Kind: DTypePrimitive, PType: PTypeU16}
			case 4:
				return &DType{Kind: DTypePrimitive, PType: PTypeU32}
			case 8:
				return &DType{Kind: DTypePrimitive, PType: PTypeU64}
			}
		}
	}

	// Fallback: smallest PType that can index the dictionary.
	ptype := PTypeU8
	if dictSize > 255 {
		ptype = PTypeU16
	}
	if dictSize > 65535 {
		ptype = PTypeU32
	}
	return &DType{Kind: DTypePrimitive, PType: ptype}
}

// decodeArrayInternal is the internal decode function. It delegates to the encoding
// package's DecodeArray via a package-level function variable to avoid import cycles.
// This is set by the encoding package's init.
var decodeArrayInternal func(node *ArrayNode, allBuffers [][]byte, dtype *DType, rowCount uint64) (*Array, error)

// RegisterDecoder registers the encoding package's DecodeArray function.
// Called from the encoding package to break the import cycle.
func RegisterDecoder(fn func(node *ArrayNode, allBuffers [][]byte, dtype *DType, rowCount uint64) (*Array, error)) {
	decodeArrayInternal = fn
}

// truncateArray returns a copy of the array with at most n rows.
func truncateArray(arr *Array, n int) *Array {
	if arr.Length <= n {
		return arr
	}

	result := &Array{
		DType:  arr.DType,
		Length: n,
		Stats:  arr.Stats,
	}

	// Truncate buffers.
	if len(arr.Buffers) > 0 && arr.DType != nil {
		switch arr.DType.Kind {
		case DTypePrimitive:
			elemSize := PTypeSize(arr.DType.PType)
			if elemSize > 0 && len(arr.Buffers) > 0 {
				newSize := n * elemSize
				if newSize <= len(arr.Buffers[0]) {
					result.Buffers = [][]byte{arr.Buffers[0][:newSize]}
				} else {
					result.Buffers = arr.Buffers
				}
			}
		case DTypeBool:
			// Bit-packed: ceil(n/8) bytes.
			if len(arr.Buffers) > 0 {
				newSize := (n + 7) / 8
				if newSize <= len(arr.Buffers[0]) {
					result.Buffers = [][]byte{arr.Buffers[0][:newSize]}
				} else {
					result.Buffers = arr.Buffers
				}
			}
		default:
			// For complex types (strings, etc.), keep full buffers but adjust Length.
			result.Buffers = arr.Buffers
		}
	}

	// Truncate children recursively.
	if len(arr.Children) > 0 {
		result.Children = make([]*Array, len(arr.Children))
		for i, child := range arr.Children {
			if child != nil && child.Length == arr.Length {
				// Same-length child (e.g., validity mask) — truncate too.
				result.Children[i] = truncateArray(child, n)
			} else {
				// Different-length child (e.g., dict values) — keep as-is.
				result.Children[i] = child
			}
		}
	}

	return result
}

// --- Convenience functions ---

// ReadColumns reads and decodes all requested columns from the file.
// If columns is nil, all columns are read.
// Returns a map of column name → decoded Array with all rows.
//
// This is a convenience wrapper around Scan that collects all chunks into
// single arrays per column.
func (f *File) ReadColumns(columns []string) (map[string]*Array, error) {
	iter, err := f.Scan(ScanOptions{Columns: columns})
	if err != nil {
		return nil, err
	}

	result := make(map[string]*Array)
	for iter.Next() {
		chunk := iter.Result()
		for name, arr := range chunk.Columns {
			if existing, ok := result[name]; ok {
				// Append to existing.
				result[name] = appendArrays(existing, arr)
			} else {
				result[name] = arr
			}
		}
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadColumn reads and decodes a single column by name.
// Returns the decoded Array with all rows.
func (f *File) ReadColumn(name string) (*Array, error) {
	cols, err := f.ReadColumns([]string{name})
	if err != nil {
		return nil, err
	}
	arr, ok := cols[name]
	if !ok {
		return nil, fmt.Errorf("vortex: column %q not found in results", name)
	}
	return arr, nil
}

// appendArrays concatenates two arrays. For simple cases (single buffer),
// it appends the buffers. For complex cases it returns a wrapper.
// ParseDictLayoutCodesPType walks the column layout tree to find a Dict layout
// node and extracts the codes_ptype from its DictLayoutMetadata protobuf.
// Returns the PType if found, or 0 (PTypeU8) as a fallback which will be
// overridden by the heuristic in detectCodesPType.
//
// DictLayoutMetadata protobuf layout:
//
//	field 1 (tag=0x08, varint): codes_ptype (PType enum value)
//	field 2 (tag=0x10, varint): is_nullable_codes (optional bool)
//	field 3 (tag=0x18, varint): all_values_referenced (optional bool)
func ParseDictLayoutCodesPType(layout *Layout) PType {
	// Walk past Zoned layout to find the Dict layout.
	current := layout
	if current.IsZoned() && len(current.Children) > 0 {
		current = current.Children[0]
	}
	if current.EncodingID != LayoutDict || len(current.Metadata) == 0 {
		return PType(-1) // not found
	}

	// Parse the protobuf manually to extract field 1 (codes_ptype).
	data := current.Metadata
	offset := 0
	for offset < len(data) {
		tag := data[offset]
		offset++
		fieldNum := tag >> 3
		wireType := tag & 0x07

		if wireType != 0 { // only varint fields in this message
			break
		}

		val, n := decodeVarint(data[offset:])
		if n == 0 {
			break
		}
		offset += n

		if fieldNum == 1 {
			// codes_ptype: map from Rust PType enum to Go PType.
			// Both use the same ordering: U8=0, U16=1, U32=2, U64=3, etc.
			return PType(val)
		}
	}

	return PType(-1)
}

func appendArrays(a, b *Array) *Array {
	if a == nil {
		return b
	}
	if b == nil || b.Length == 0 {
		return a
	}

	result := &Array{
		DType:  a.DType,
		Length: a.Length + b.Length,
		Stats:  a.Stats,
	}

	// Concatenate buffers.
	if len(a.Buffers) > 0 && len(b.Buffers) > 0 && len(a.Buffers) == len(b.Buffers) {
		result.Buffers = make([][]byte, len(a.Buffers))
		for i := range a.Buffers {
			combined := make([]byte, len(a.Buffers[i])+len(b.Buffers[i]))
			copy(combined, a.Buffers[i])
			copy(combined[len(a.Buffers[i]):], b.Buffers[i])
			result.Buffers[i] = combined
		}
	} else if len(a.Buffers) > 0 {
		result.Buffers = a.Buffers
	}

	// Concatenate children (for validity masks, etc.).
	if len(a.Children) > 0 && len(b.Children) > 0 && len(a.Children) == len(b.Children) {
		result.Children = make([]*Array, len(a.Children))
		for i := range a.Children {
			result.Children[i] = appendArrays(a.Children[i], b.Children[i])
		}
	} else if len(a.Children) > 0 {
		result.Children = a.Children
	}

	return result
}
