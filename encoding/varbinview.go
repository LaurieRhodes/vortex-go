package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// VarBinViewDecoder decodes vortex.varbinview arrays.
// This is the canonical encoding for Utf8 and Binary types.
//
// Buffer layout:
//   - Buffer 0: views — one 16-byte view descriptor per string
//   - Buffers 1..N: data buffers containing string bytes for long strings
//
// View descriptor (16 bytes):
//   - length: uint32 (byte length of the string)
//   - If length <= 12: next 12 bytes are the string data inline
//   - If length > 12: next 4 bytes are a prefix, then uint32 buffer_index + uint32 offset
type VarBinViewDecoder struct{}

func (d *VarBinViewDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(buffers) < 1 {
		return nil, fmt.Errorf("varbinview: expected at least 1 buffer (views), got %d", len(buffers))
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: buffers,
		Stats:   node.Stats,
	}, nil
}

// StringValues extracts string values from a VarBinView array.
func StringValues(arr *vortex.Array) ([]string, error) {
	if arr.DType.Kind != vortex.DTypeUtf8 && arr.DType.Kind != vortex.DTypeBinary {
		return nil, fmt.Errorf("not a utf8/binary array (kind=%v)", arr.DType.Kind)
	}
	if len(arr.Buffers) < 1 {
		return nil, nil
	}

	viewsBuf := arr.Buffers[0]
	dataBufs := arr.Buffers[1:]
	n := arr.Length

	if len(viewsBuf) < n*16 {
		return nil, fmt.Errorf("views buffer too small: %d bytes for %d rows (need %d)", len(viewsBuf), n, n*16)
	}

	values := make([]string, n)
	for i := 0; i < n; i++ {
		off := i * 16
		strLen := binary.LittleEndian.Uint32(viewsBuf[off : off+4])

		if strLen <= 12 {
			// Inline: bytes 4..4+strLen are the string data.
			values[i] = string(viewsBuf[off+4 : off+4+int(strLen)])
		} else {
			// Out-of-line: bytes 4..8 are prefix (ignored for extraction),
			// bytes 8..12 are buffer index, bytes 12..16 are offset within that buffer.
			bufIdx := binary.LittleEndian.Uint32(viewsBuf[off+8 : off+12])
			bufOff := binary.LittleEndian.Uint32(viewsBuf[off+12 : off+16])

			if int(bufIdx) >= len(dataBufs) {
				return nil, fmt.Errorf("string %d: buffer index %d out of range (have %d data buffers)", i, bufIdx, len(dataBufs))
			}
			db := dataBufs[bufIdx]
			end := bufOff + strLen
			if int(end) > len(db) {
				return nil, fmt.Errorf("string %d: offset %d + length %d overflows data buffer %d (size %d)", i, bufOff, strLen, bufIdx, len(db))
			}
			values[i] = string(db[bufOff:end])
		}
	}

	return values, nil
}

func init() {
	Register("vortex.varbinview", &VarBinViewDecoder{})
}
