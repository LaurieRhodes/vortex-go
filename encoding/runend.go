package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// RunEndDecoder decodes vortex.runend arrays.
//
// RunEnd encoding stores runs of identical values as (end_position, value) pairs.
// Children: [0] ends (integer array of run end positions), [1] values (one per run).
// Metadata: RunEndMetadata { ends_ptype, num_runs, offset }
//
// To expand: for each output position i, find the run where ends[run] > i+offset,
// then output values[run].
type RunEndDecoder struct{}

func (d *RunEndDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(node.Children) < 2 {
		return nil, fmt.Errorf("runend: expected 2 children, got %d", len(node.Children))
	}

	// Parse metadata.
	var meta encodings.RunEndMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("runend: parsing metadata: %w", err)
		}
	}

	numRuns := meta.NumRuns
	offset := meta.Offset

	// Decode ends child — integer array of run end positions.
	endsPType := vortex.PType(meta.EndsPtype)
	endsDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: endsPType}
	endsArr, err := DecodeChild(node, 0, endsDType, numRuns, ctx)
	if err != nil {
		return nil, fmt.Errorf("runend: decoding ends: %w", err)
	}

	// Decode values child — one value per run.
	valsArr, err := DecodeChild(node, 1, dtype, numRuns, ctx)
	if err != nil {
		return nil, fmt.Errorf("runend: decoding values: %w", err)
	}

	// Convert ends to uint64 slice for uniform processing.
	ends, err := endsToUint64(endsArr, endsPType)
	if err != nil {
		return nil, err
	}

	// Expand runs into the output array.
	return expandRuns(ends, valsArr, dtype, int(rowCount), int(offset))
}

// endsToUint64 converts an ends array to a []uint64 slice.
func endsToUint64(arr *vortex.Array, ptype vortex.PType) ([]uint64, error) {
	if len(arr.Buffers) == 0 {
		return nil, fmt.Errorf("runend: ends array has no buffers")
	}
	buf := arr.Buffers[0]
	result := make([]uint64, arr.Length)

	switch ptype {
	case vortex.PTypeU8, vortex.PTypeI8:
		for i := 0; i < arr.Length; i++ {
			result[i] = uint64(buf[i])
		}
	case vortex.PTypeU16, vortex.PTypeI16:
		for i := 0; i < arr.Length; i++ {
			result[i] = uint64(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case vortex.PTypeU32, vortex.PTypeI32:
		for i := 0; i < arr.Length; i++ {
			result[i] = uint64(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case vortex.PTypeU64, vortex.PTypeI64:
		for i := 0; i < arr.Length; i++ {
			result[i] = binary.LittleEndian.Uint64(buf[i*8:])
		}
	default:
		return nil, fmt.Errorf("runend: unsupported ends ptype %v", ptype)
	}
	return result, nil
}

// expandRuns expands run-end encoded data into a full array.
func expandRuns(ends []uint64, vals *vortex.Array, dtype *vortex.DType, count, offset int) (*vortex.Array, error) {
	elemSize := vortex.PTypeSize(dtype.PType)
	if elemSize == 0 {
		// Non-primitive (e.g. strings) — handle specially.
		return expandRunsGeneric(ends, vals, dtype, count, offset)
	}

	if len(vals.Buffers) == 0 {
		return nil, fmt.Errorf("runend: values array has no buffers")
	}
	valBuf := vals.Buffers[0]
	outBuf := make([]byte, count*elemSize)

	run := 0
	for i := 0; i < count; i++ {
		pos := uint64(i + offset)
		// Advance to the run that covers this position.
		for run < len(ends)-1 && ends[run] <= pos {
			run++
		}
		// Copy the value for this run.
		srcOff := run * elemSize
		dstOff := i * elemSize
		if srcOff+elemSize <= len(valBuf) {
			copy(outBuf[dstOff:dstOff+elemSize], valBuf[srcOff:srcOff+elemSize])
		}
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  count,
		Buffers: [][]byte{outBuf},
	}, nil
}

// expandRunsGeneric handles non-primitive run expansion (strings, bools, etc.)
func expandRunsGeneric(ends []uint64, vals *vortex.Array, dtype *vortex.DType, count, offset int) (*vortex.Array, error) {
	// For bool dtype, expand bit by bit.
	if dtype.Kind == vortex.DTypeBool && len(vals.Buffers) > 0 {
		outLen := (count + 7) / 8
		outBuf := make([]byte, outLen)
		valBuf := vals.Buffers[0]

		run := 0
		for i := 0; i < count; i++ {
			pos := uint64(i + offset)
			for run < len(ends)-1 && ends[run] <= pos {
				run++
			}
			// Read the bit for this run's value.
			if run/8 < len(valBuf) && (valBuf[run/8]>>(uint(run)%8))&1 == 1 {
				outBuf[i/8] |= 1 << (uint(i) % 8)
			}
		}
		return &vortex.Array{DType: dtype, Length: count, Buffers: [][]byte{outBuf}}, nil
	}

	return nil, fmt.Errorf("runend: unsupported dtype for run expansion: %v", dtype.Kind)
}

func init() {
	Register("vortex.runend", &RunEndDecoder{})
}
