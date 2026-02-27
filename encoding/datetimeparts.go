package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// DateTimePartsDecoder decodes vortex.datetimeparts arrays.
//
// DateTimeParts splits timestamps into three components for better compression:
//
//	days     — number of days since the Unix epoch
//	seconds  — seconds within the day (0–86399)
//	subseconds — fractional part in the target time unit
//
// Children: [0] days, [1] seconds, [2] subseconds
// Metadata: DateTimePartsMetadata { days_ptype, seconds_ptype, subseconds_ptype }
//
// The time unit is extracted from the Extension DType metadata:
//
//	byte 0:   TimeUnit tag (0=ns, 1=µs, 2=ms, 3=s)
//	bytes 1-2: timezone string length (u16 LE)
//	bytes 3+:  timezone string (UTF-8)
//
// Reconstruction: timestamp[i] = days[i]*86400*divisor + seconds[i]*divisor + subseconds[i]
type DateTimePartsDecoder struct{}

const secondsPerDay int64 = 86400

func (d *DateTimePartsDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) < 3 {
		return nil, fmt.Errorf("datetimeparts: expected 3 children, got %d", len(node.Children))
	}

	var meta encodings.DateTimePartsMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("datetimeparts: parsing metadata: %w", err)
		}
	}

	daysPType := vortex.PType(meta.DaysPtype)
	secondsPType := vortex.PType(meta.SecondsPtype)
	subsecondsPType := vortex.PType(meta.SubsecondsPtype)

	// Decode the three children.
	daysDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: daysPType, Nullable: dtype.Nullable}
	daysArr, err := DecodeChild(node, 0, daysDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("datetimeparts: decoding days: %w", err)
	}

	secondsDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: secondsPType}
	secondsArr, err := DecodeChild(node, 1, secondsDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("datetimeparts: decoding seconds: %w", err)
	}

	subsecondsDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: subsecondsPType}
	subsecondsArr, err := DecodeChild(node, 2, subsecondsDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("datetimeparts: decoding subseconds: %w", err)
	}

	// Determine the time unit divisor from Extension metadata.
	divisor := parseDivisor(dtype)

	// Reconstruct timestamps as i64 values.
	count := int(rowCount)
	outBuf := make([]byte, count*8)

	days := readAsInt64Slice(daysArr, daysPType)
	secs := readAsInt64Slice(secondsArr, secondsPType)
	subsecs := readAsInt64Slice(subsecondsArr, subsecondsPType)

	for i := 0; i < count; i++ {
		var d, s, ss int64
		if i < len(days) {
			d = days[i]
		}
		if i < len(secs) {
			s = secs[i]
		}
		if i < len(subsecs) {
			ss = subsecs[i]
		}
		ts := d*secondsPerDay*divisor + s*divisor + ss
		binary.LittleEndian.PutUint64(outBuf[i*8:], uint64(ts))
	}

	// Output dtype: if Extension, use the storage dtype (i64); otherwise use as-is.
	outDType := dtype
	if dtype.Kind == vortex.DTypeExtension && dtype.StorageDType != nil {
		outDType = dtype.StorageDType
	}
	if outDType.Kind != vortex.DTypePrimitive {
		outDType = &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64, Nullable: dtype.Nullable}
	}

	return &vortex.Array{
		DType:   outDType,
		Length:  count,
		Buffers: [][]byte{outBuf},
		Stats:   node.Stats,
	}, nil
}

// parseDivisor extracts the time unit divisor from Extension DType metadata.
// Returns 1_000_000_000 (nanoseconds) as default.
func parseDivisor(dtype *vortex.DType) int64 {
	if dtype.Kind != vortex.DTypeExtension || len(dtype.ExtMetadata) < 3 {
		return 1_000_000_000 // default: nanoseconds
	}

	tag := dtype.ExtMetadata[0]
	switch tag {
	case 0:
		return 1_000_000_000 // nanoseconds
	case 1:
		return 1_000_000 // microseconds
	case 2:
		return 1_000 // milliseconds
	case 3:
		return 1 // seconds
	default:
		return 1_000_000_000
	}
}

// readAsInt64Slice reads a primitive array's buffer as []int64 values.
func readAsInt64Slice(arr *vortex.Array, ptype vortex.PType) []int64 {
	if len(arr.Buffers) == 0 {
		return nil
	}
	buf := arr.Buffers[0]
	result := make([]int64, arr.Length)

	switch ptype {
	case vortex.PTypeI8:
		for i := 0; i < arr.Length && i < len(buf); i++ {
			result[i] = int64(int8(buf[i]))
		}
	case vortex.PTypeU8:
		for i := 0; i < arr.Length && i < len(buf); i++ {
			result[i] = int64(buf[i])
		}
	case vortex.PTypeI16:
		for i := 0; i < arr.Length && i*2+2 <= len(buf); i++ {
			result[i] = int64(int16(binary.LittleEndian.Uint16(buf[i*2:])))
		}
	case vortex.PTypeU16:
		for i := 0; i < arr.Length && i*2+2 <= len(buf); i++ {
			result[i] = int64(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case vortex.PTypeI32:
		for i := 0; i < arr.Length && i*4+4 <= len(buf); i++ {
			result[i] = int64(int32(binary.LittleEndian.Uint32(buf[i*4:])))
		}
	case vortex.PTypeU32:
		for i := 0; i < arr.Length && i*4+4 <= len(buf); i++ {
			result[i] = int64(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case vortex.PTypeI64:
		for i := 0; i < arr.Length && i*8+8 <= len(buf); i++ {
			result[i] = int64(binary.LittleEndian.Uint64(buf[i*8:]))
		}
	case vortex.PTypeU64:
		for i := 0; i < arr.Length && i*8+8 <= len(buf); i++ {
			result[i] = int64(binary.LittleEndian.Uint64(buf[i*8:]))
		}
	}
	return result
}

func init() {
	Register("vortex.datetimeparts", &DateTimePartsDecoder{})
}
