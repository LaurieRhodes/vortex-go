package vortex

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Stat represents a zone map statistic type, matching the Rust Stat enum.
type Stat uint8

const (
	StatIsConstant            Stat = 0
	StatIsSorted              Stat = 1
	StatIsStrictSorted        Stat = 2
	StatMax                   Stat = 3
	StatMin                   Stat = 4
	StatSum                   Stat = 5
	StatNullCount             Stat = 6
	StatUncompressedSizeBytes Stat = 7
	StatNaNCount              Stat = 8
)

// StatName returns the field name used in the stats table for this stat.
func (s Stat) Name() string {
	switch s {
	case StatIsConstant:
		return "is_constant"
	case StatIsSorted:
		return "is_sorted"
	case StatIsStrictSorted:
		return "is_strict_sorted"
	case StatMax:
		return "max"
	case StatMin:
		return "min"
	case StatSum:
		return "sum"
	case StatNullCount:
		return "null_count"
	case StatUncompressedSizeBytes:
		return "uncompressed_size_in_bytes"
	case StatNaNCount:
		return "nan_count"
	default:
		return fmt.Sprintf("stat_%d", s)
	}
}

// ZoneColumnStats holds per-zone typed statistics for one column.
// Slices are indexed by zone number.
type ZoneColumnStats struct {
	Min       []interface{} // typed values per zone (int32, int64, float64, etc.), nil = null
	Max       []interface{} // typed values per zone, nil = null
	NullCount []uint64      // null count per zone (may be empty if stat not present)
	NumZones  int
}

// ZoneMapResult holds decoded zone map statistics for a column.
type ZoneMapResult struct {
	ZoneLen  uint32
	NumZones int
	// Columns holds per-column stats. For a single column layout,
	// the key is "" (empty string).
	Columns map[string]*ZoneColumnStats
}

// ParseZonedMeta parses the Zoned layout's metadata bytes.
// Format: 4 bytes zone_len (LE uint32) + stat bitset bytes.
func ParseZonedMeta(metadata []byte) (zoneLen uint32, presentStats []Stat, err error) {
	if len(metadata) < 4 {
		return 0, nil, fmt.Errorf("zoned metadata too short: %d bytes", len(metadata))
	}

	zoneLen = binary.LittleEndian.Uint32(metadata[0:4])

	// Parse stat bitset (same format as Rust).
	for byteIdx, b := range metadata[4:] {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<uint(bit)) != 0 {
				presentStats = append(presentStats, Stat(byteIdx*8+bit))
			}
		}
	}

	return zoneLen, presentStats, nil
}

// statsTableDType constructs the expected DType for the zone map stats table
// given the column DType and which stats are present.
// This mirrors ZoneMap::dtype_for_stats_table in Rust.
func statsTableDType(colDType *DType, presentStats []Stat) *DType {
	var fieldNames []string
	var fieldTypes []*DType

	for _, stat := range presentStats {
		statDT := statFieldDType(colDType, stat)
		if statDT == nil {
			continue
		}

		// Make nullable.
		nullableDT := *statDT
		nullableDT.Nullable = true

		fieldNames = append(fieldNames, stat.Name())
		fieldTypes = append(fieldTypes, &nullableDT)

		// Max and Min have an additional is_truncated bool field.
		if stat == StatMax {
			fieldNames = append(fieldNames, "max_is_truncated")
			fieldTypes = append(fieldTypes, &DType{Kind: DTypeBool, Nullable: false})
		} else if stat == StatMin {
			fieldNames = append(fieldNames, "min_is_truncated")
			fieldTypes = append(fieldTypes, &DType{Kind: DTypeBool, Nullable: false})
		}
	}

	return &DType{
		Kind:       DTypeStruct,
		Nullable:   false,
		FieldNames: fieldNames,
		FieldTypes: fieldTypes,
	}
}

// statFieldDType returns the DType of a stat column for a given data column DType.
// Returns nil if the stat doesn't apply.
func statFieldDType(colDType *DType, stat Stat) *DType {
	switch stat {
	case StatMin, StatMax:
		return colDType
	case StatSum:
		// Sum type depends on column type.
		switch colDType.Kind {
		case DTypePrimitive:
			switch colDType.PType {
			case PTypeU8, PTypeU16, PTypeU32, PTypeU64:
				return &DType{Kind: DTypePrimitive, PType: PTypeU64}
			case PTypeI8, PTypeI16, PTypeI32, PTypeI64:
				return &DType{Kind: DTypePrimitive, PType: PTypeI64}
			case PTypeF32:
				return &DType{Kind: DTypePrimitive, PType: PTypeF64}
			case PTypeF64:
				return &DType{Kind: DTypePrimitive, PType: PTypeF64}
			}
		case DTypeBool:
			return &DType{Kind: DTypePrimitive, PType: PTypeU64}
		}
		return nil
	case StatNullCount, StatUncompressedSizeBytes, StatNaNCount:
		return &DType{Kind: DTypePrimitive, PType: PTypeU64}
	case StatIsConstant, StatIsSorted, StatIsStrictSorted:
		return &DType{Kind: DTypeBool}
	default:
		return nil
	}
}

// ReadZoneMap reads and decodes zone map statistics from a Zoned layout.
// The layout must be a Zoned layout (IsZoned() == true).
// colDType is the data type of the column this zone map covers.
func (f *File) ReadZoneMap(layout *Layout, colDType *DType) (*ZoneMapResult, error) {
	colDType = ResolveStorageDType(colDType)
	if !layout.IsZoned() {
		return nil, fmt.Errorf("layout is not Zoned: %s", layout.EncodingID)
	}
	if len(layout.Children) < 2 {
		return nil, fmt.Errorf("Zoned layout needs 2 children, has %d", len(layout.Children))
	}

	// Parse metadata.
	zoneLen, presentStats, err := ParseZonedMeta(layout.Metadata)
	if err != nil {
		return nil, fmt.Errorf("parsing zoned metadata: %w", err)
	}

	// Read the stats table (child[1]).
	statsLayout := layout.Children[1]

	// Build expected dtype for the stats table.
	statsDType := statsTableDType(colDType, presentStats)

	// Read and decode the stats flat segment.
	statsArr, err := f.readAndDecodeLayout(statsLayout, statsDType)
	if err != nil {
		return nil, fmt.Errorf("reading zone stats: %w", err)
	}

	numZones := statsArr.Length

	// Extract per-zone statistics from the struct array.
	colStats := &ZoneColumnStats{
		NumZones: numZones,
	}

	// Find min, max, null_count fields by name.
	for i, name := range statsDType.FieldNames {
		if i >= len(statsArr.Children) {
			break
		}
		child := statsArr.Children[i]

		switch name {
		case "min":
			colStats.Min = extractTypedValues(child, colDType)
		case "max":
			colStats.Max = extractTypedValues(child, colDType)
		case "null_count":
			colStats.NullCount = extractUint64Values(child)
		}
	}

	return &ZoneMapResult{
		ZoneLen:  zoneLen,
		NumZones: numZones,
		Columns:  map[string]*ZoneColumnStats{"": colStats},
	}, nil
}

// readAndDecodeLayout reads and decodes a layout subtree (flat or chunked).
func (f *File) readAndDecodeLayout(layout *Layout, dtype *DType) (*Array, error) {
	if layout.IsFlat() {
		node, buffers, err := f.ReadFlatSegment(layout)
		if err != nil {
			return nil, err
		}
		if decodeArrayInternal == nil {
			return nil, fmt.Errorf("decoder not registered; import _ \"github.com/LaurieRhodes/vortex-go/encoding\"")
		}
		return decodeArrayInternal(node, buffers, dtype, layout.RowCount)
	}

	if layout.IsChunked() {
		// For chunked stats tables, decode each chunk and concatenate.
		var totalRows int
		var allChildren []*Array

		for _, child := range layout.Children {
			arr, err := f.readAndDecodeLayout(child, dtype)
			if err != nil {
				return nil, err
			}
			allChildren = append(allChildren, arr)
			totalRows += arr.Length
		}

		if len(allChildren) == 1 {
			return allChildren[0], nil
		}

		// Concatenate struct children by merging field arrays.
		return concatStructArrays(allChildren, dtype, totalRows)
	}

	return nil, fmt.Errorf("unsupported layout type for stats: %s", layout.EncodingID)
}

// concatStructArrays concatenates multiple struct arrays into one.
func concatStructArrays(arrays []*Array, dtype *DType, totalRows int) (*Array, error) {
	if len(arrays) == 0 {
		return &Array{DType: dtype, Length: 0}, nil
	}

	numFields := len(dtype.FieldNames)
	merged := &Array{
		DType:    dtype,
		Length:   totalRows,
		Children: make([]*Array, numFields),
	}

	for fi := 0; fi < numFields; fi++ {
		fieldDType := dtype.FieldTypes[fi]
		elemSize := ptypeByteSize(fieldDType)

		var buf []byte
		for _, arr := range arrays {
			if fi < len(arr.Children) {
				child := arr.Children[fi]
				if len(child.Buffers) > 0 {
					buf = append(buf, child.Buffers[0]...)
				}
			}
		}

		merged.Children[fi] = &Array{
			DType:   fieldDType,
			Length:  totalRows,
			Buffers: [][]byte{buf},
		}
		_ = elemSize // used for validation if needed
	}

	return merged, nil
}

// ptypeByteSize returns the byte size for a primitive dtype, or 0 for non-primitive.
func ptypeByteSize(dt *DType) int {
	if dt.Kind == DTypePrimitive {
		return PTypeSize(dt.PType)
	}
	if dt.Kind == DTypeBool {
		return 0 // bit-packed
	}
	return 0
}

// extractTypedValues reads typed values from a decoded primitive array.
// Returns a slice of interface{} values, one per element.
func extractTypedValues(arr *Array, colDType *DType) []interface{} {
	if arr == nil || arr.Length == 0 {
		return nil
	}

	// Handle nullable arrays: check for validity bitmap.
	// In decoded arrays, nullable primitives may have a validity child or
	// the buffers may directly contain values with a separate validity bitmap.
	var validityBuf []byte
	var dataBuf []byte

	if len(arr.Buffers) >= 2 {
		// Buffers[0] = validity, Buffers[1] = data (some decoders)
		validityBuf = arr.Buffers[0]
		dataBuf = arr.Buffers[1]
	} else if len(arr.Buffers) == 1 {
		dataBuf = arr.Buffers[0]
	}

	// For masked arrays, the child[0] is the values array.
	if dataBuf == nil && len(arr.Children) > 0 {
		inner := arr.Children[0]
		if len(inner.Buffers) > 0 {
			dataBuf = inner.Buffers[0]
		}
		// Check for validity child.
		if len(arr.Children) > 1 {
			v := arr.Children[1]
			if len(v.Buffers) > 0 {
				validityBuf = v.Buffers[0]
			}
		}
	}

	if dataBuf == nil {
		return make([]interface{}, arr.Length)
	}

	values := make([]interface{}, arr.Length)

	dt := colDType
	if dt == nil {
		dt = arr.DType
	}

	for i := 0; i < arr.Length; i++ {
		// Check validity.
		if len(validityBuf) > 0 {
			byteIdx := i / 8
			bitIdx := i % 8
			if byteIdx < len(validityBuf) && validityBuf[byteIdx]&(1<<uint(bitIdx)) == 0 {
				values[i] = nil
				continue
			}
		}

		values[i] = readTypedValue(dataBuf, i, dt)
	}

	return values
}

// readTypedValue reads a single typed value from a data buffer at the given index.
func readTypedValue(buf []byte, idx int, dt *DType) interface{} {
	switch dt.Kind {
	case DTypePrimitive:
		return readPrimitiveValue(buf, idx, dt.PType)
	case DTypeBool:
		byteIdx := idx / 8
		bitIdx := idx % 8
		if byteIdx < len(buf) {
			return buf[byteIdx]&(1<<uint(bitIdx)) != 0
		}
		return false
	case DTypeUtf8, DTypeBinary:
		// Stats for strings: these would be in VarBinView format.
		// For zone map filtering we'll extract them if possible,
		// but string stats are complex (truncation etc). Return nil for now.
		return nil
	default:
		return nil
	}
}

// readPrimitiveValue reads a primitive value from a byte buffer at the given index.
func readPrimitiveValue(buf []byte, idx int, ptype PType) interface{} {
	switch ptype {
	case PTypeI8:
		if idx < len(buf) {
			return int8(buf[idx])
		}
	case PTypeU8:
		if idx < len(buf) {
			return buf[idx]
		}
	case PTypeI16:
		off := idx * 2
		if off+2 <= len(buf) {
			return int16(binary.LittleEndian.Uint16(buf[off:]))
		}
	case PTypeU16:
		off := idx * 2
		if off+2 <= len(buf) {
			return binary.LittleEndian.Uint16(buf[off:])
		}
	case PTypeI32:
		off := idx * 4
		if off+4 <= len(buf) {
			return int32(binary.LittleEndian.Uint32(buf[off:]))
		}
	case PTypeU32:
		off := idx * 4
		if off+4 <= len(buf) {
			return binary.LittleEndian.Uint32(buf[off:])
		}
	case PTypeI64:
		off := idx * 8
		if off+8 <= len(buf) {
			return int64(binary.LittleEndian.Uint64(buf[off:]))
		}
	case PTypeU64:
		off := idx * 8
		if off+8 <= len(buf) {
			return binary.LittleEndian.Uint64(buf[off:])
		}
	case PTypeF32:
		off := idx * 4
		if off+4 <= len(buf) {
			bits := binary.LittleEndian.Uint32(buf[off:])
			return math.Float32frombits(bits)
		}
	case PTypeF64:
		off := idx * 8
		if off+8 <= len(buf) {
			bits := binary.LittleEndian.Uint64(buf[off:])
			return math.Float64frombits(bits)
		}
	}
	return nil
}

// extractUint64Values reads uint64 values from a decoded array.
func extractUint64Values(arr *Array) []uint64 {
	if arr == nil || arr.Length == 0 {
		return nil
	}

	var dataBuf []byte
	if len(arr.Buffers) > 0 {
		dataBuf = arr.Buffers[len(arr.Buffers)-1] // last buffer is typically data
	}
	if dataBuf == nil && len(arr.Children) > 0 {
		inner := arr.Children[0]
		if len(inner.Buffers) > 0 {
			dataBuf = inner.Buffers[0]
		}
	}

	if dataBuf == nil || len(dataBuf) < arr.Length*8 {
		return make([]uint64, arr.Length)
	}

	values := make([]uint64, arr.Length)
	for i := 0; i < arr.Length; i++ {
		values[i] = binary.LittleEndian.Uint64(dataBuf[i*8:])
	}
	return values
}
