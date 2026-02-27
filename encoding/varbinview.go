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

// StringValues extracts string values from a VarBinView or Dict string array.
func StringValues(arr *vortex.Array) ([]string, error) {
	if arr.DType.Kind != vortex.DTypeUtf8 && arr.DType.Kind != vortex.DTypeBinary {
		return nil, fmt.Errorf("not a utf8/binary array (kind=%v)", arr.DType.Kind)
	}

	// Handle Dict arrays: children[0]=codes, children[1]=values.
	if len(arr.Buffers) == 0 && len(arr.Children) == 2 {
		return stringValuesFromDict(arr)
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

// stringValuesFromDict handles Dict-style string arrays.
func stringValuesFromDict(arr *vortex.Array) ([]string, error) {
	codesArr := arr.Children[0]
	valuesArr := arr.Children[1]

	// Extract dictionary strings from the values child.
	dictStrings, err := StringValues(valuesArr)
	if err != nil {
		return nil, fmt.Errorf("dict string values: %w", err)
	}
	dictLen := len(dictStrings)

	// Extract codes as integer indices.
	codes, err := readIntCodes(codesArr)
	if err != nil {
		return nil, fmt.Errorf("dict codes: %w", err)
	}

	// Look up each code in the dictionary.
	result := make([]string, len(codes))
	for i, idx := range codes {
		if idx >= dictLen {
			return nil, fmt.Errorf("dict code[%d]=%d exceeds dictionary size %d", i, idx, dictLen)
		}
		result[i] = dictStrings[idx]
	}
	return result, nil
}

// readIntCodes extracts integer indices from a codes array.
// Supports u8, u16, u32, u64 PTypes.
func readIntCodes(arr *vortex.Array) ([]int, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || len(arr.Buffers) == 0 {
		return nil, fmt.Errorf("codes array is not a buffered primitive (kind=%v, bufs=%d)",
			arr.DType.Kind, len(arr.Buffers))
	}
	buf := arr.Buffers[0]
	n := arr.Length
	codes := make([]int, n)

	switch arr.DType.PType {
	case vortex.PTypeU8:
		if len(buf) < n {
			return nil, fmt.Errorf("u8 codes buffer too small")
		}
		for i := 0; i < n; i++ {
			codes[i] = int(buf[i])
		}
	case vortex.PTypeU16:
		if len(buf) < n*2 {
			return nil, fmt.Errorf("u16 codes buffer too small")
		}
		for i := 0; i < n; i++ {
			codes[i] = int(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case vortex.PTypeU32:
		if len(buf) < n*4 {
			return nil, fmt.Errorf("u32 codes buffer too small")
		}
		for i := 0; i < n; i++ {
			codes[i] = int(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case vortex.PTypeU64:
		if len(buf) < n*8 {
			return nil, fmt.Errorf("u64 codes buffer too small")
		}
		for i := 0; i < n; i++ {
			codes[i] = int(binary.LittleEndian.Uint64(buf[i*8:]))
		}
	default:
		return nil, fmt.Errorf("unsupported codes PType: %v", arr.DType.PType)
	}
	return codes, nil
}

func init() {
	Register("vortex.varbinview", &VarBinViewDecoder{})
}
