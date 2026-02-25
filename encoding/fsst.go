package encoding

import (
	"encoding/binary"
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// FSSTDecoder decodes vortex.fsst arrays.
//
// FSST (Fast Static Symbol Table) compresses strings by replacing common
// substrings (1–8 bytes) with single-byte codes. The encoding stores:
//
//	Buffer[0]: symbols — 8 bytes per symbol (up to 255 symbols, padded to 8 bytes)
//	Buffer[1]: symbol_lengths — 1 byte per symbol (actual length of each symbol, 1–8)
//	Buffer[2]: compressed_codes — concatenated compressed data for all strings
//	Child[0]:  uncompressed_lengths — integer array of expected decompressed lengths
//	Child[1]:  codes_offsets — integer array of N+1 VarBin offsets into buffer[2]
//	Child[2]:  (optional) validity mask — boolean array
//
// Decompression: for each byte in the compressed stream, if the byte value is
// less than the number of symbols, emit the corresponding symbol bytes.
// If the byte is 0xFF (escape code), emit the next byte literally.
type FSSTDecoder struct{}

const fsstEscapeByte = 0xFF

func (d *FSSTDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if len(buffers) < 3 {
		return nil, fmt.Errorf("fsst: expected at least 3 buffers, got %d", len(buffers))
	}
	if len(node.Children) < 2 {
		return nil, fmt.Errorf("fsst: expected at least 2 children, got %d", len(node.Children))
	}

	symbolsBuf := buffers[0]
	symbolLenBuf := buffers[1]
	compressedBuf := buffers[2]

	// Build symbol table.
	numSymbols := len(symbolLenBuf)
	symbols := make([][]byte, numSymbols)
	for i := 0; i < numSymbols; i++ {
		slen := int(symbolLenBuf[i])
		off := i * 8
		if off+slen > len(symbolsBuf) {
			return nil, fmt.Errorf("fsst: symbol %d overflows symbols buffer", i)
		}
		symbols[i] = symbolsBuf[off : off+slen]
	}

	// Decode child arrays: uncompressed_lengths and codes_offsets.
	ucLensDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU8}
	ucLensArr, err := DecodeChild(node, 0, ucLensDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("fsst: decoding uncompressed_lengths: %w", err)
	}

	offsetsDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU8}
	offsetsArr, err := DecodeChild(node, 1, offsetsDType, rowCount+1, ctx)
	if err != nil {
		return nil, fmt.Errorf("fsst: decoding codes_offsets: %w", err)
	}

	// Extract uncompressed lengths and offsets as integer slices.
	ucLens, err := readIntSlice(ucLensArr)
	if err != nil {
		return nil, fmt.Errorf("fsst: reading uncompressed_lengths: %w", err)
	}
	offsets, err := readIntSlice(offsetsArr)
	if err != nil {
		return nil, fmt.Errorf("fsst: reading codes_offsets: %w", err)
	}

	if len(offsets) < int(rowCount)+1 {
		return nil, fmt.Errorf("fsst: offsets length %d too small for %d rows", len(offsets), rowCount)
	}

	// Decompress each string and build a VarBinView array.
	n := int(rowCount)
	viewsBuf := make([]byte, n*16)
	var dataBufs [][]byte
	var currentData []byte

	for i := 0; i < n; i++ {
		start := offsets[i]
		end := offsets[i+1]
		expectedLen := ucLens[i]

		if end > len(compressedBuf) {
			return nil, fmt.Errorf("fsst: string %d compressed range [%d:%d] overflows buffer (len=%d)", i, start, end, len(compressedBuf))
		}

		// Decompress this string.
		decompressed := fsstDecompress(compressedBuf[start:end], symbols, expectedLen)

		strLen := len(decompressed)
		viewOff := i * 16
		binary.LittleEndian.PutUint32(viewsBuf[viewOff:], uint32(strLen))

		if strLen <= 12 {
			// Inline: store directly in the view.
			copy(viewsBuf[viewOff+4:viewOff+16], decompressed)
		} else {
			// Out-of-line: store in data buffer, reference from view.
			if currentData == nil || len(currentData)+strLen > 1<<30 {
				if currentData != nil {
					dataBufs = append(dataBufs, currentData)
				}
				currentData = make([]byte, 0, max(strLen, 4096))
			}
			bufIdx := len(dataBufs)
			bufOff := len(currentData)
			currentData = append(currentData, decompressed...)

			// View: prefix (first 4 bytes of string) + buffer index + offset.
			copy(viewsBuf[viewOff+4:viewOff+8], decompressed[:min(4, strLen)])
			binary.LittleEndian.PutUint32(viewsBuf[viewOff+8:], uint32(bufIdx))
			binary.LittleEndian.PutUint32(viewsBuf[viewOff+12:], uint32(bufOff))
		}
	}
	if currentData != nil {
		dataBufs = append(dataBufs, currentData)
	}

	// Build the result: views buffer + data buffers.
	allBufs := make([][]byte, 1+len(dataBufs))
	allBufs[0] = viewsBuf
	copy(allBufs[1:], dataBufs)

	arr := &vortex.Array{
		DType:   dtype,
		Length:  n,
		Buffers: allBufs,
		Stats:   node.Stats,
	}

	// Handle optional validity child (child[2]).
	if len(node.Children) > 2 {
		validityDType := &vortex.DType{Kind: vortex.DTypeBool}
		validity, err := DecodeChild(node, 2, validityDType, rowCount, ctx)
		if err != nil {
			return nil, fmt.Errorf("fsst: decoding validity: %w", err)
		}
		arr.Children = []*vortex.Array{validity}
	}

	return arr, nil
}

// fsstDecompress decompresses a single FSST-compressed byte sequence.
func fsstDecompress(compressed []byte, symbols [][]byte, expectedLen int) []byte {
	out := make([]byte, 0, expectedLen)
	numSymbols := len(symbols)

	for i := 0; i < len(compressed); i++ {
		b := compressed[i]
		if b == fsstEscapeByte {
			// Escape: next byte is literal.
			i++
			if i < len(compressed) {
				out = append(out, compressed[i])
			}
		} else if int(b) < numSymbols {
			// Symbol lookup.
			out = append(out, symbols[b]...)
		} else {
			// Code beyond symbol table and not escape — shouldn't happen
			// but treat as literal for robustness.
			out = append(out, b)
		}
	}
	return out
}

// readIntSlice reads an integer primitive array's values as []int.
func readIntSlice(arr *vortex.Array) ([]int, error) {
	if arr.DType.Kind != vortex.DTypePrimitive || len(arr.Buffers) == 0 {
		return nil, fmt.Errorf("not a primitive array with buffers")
	}

	buf := arr.Buffers[0]
	n := arr.Length
	result := make([]int, n)

	switch arr.DType.PType {
	case vortex.PTypeU8:
		for i := 0; i < n; i++ {
			result[i] = int(buf[i])
		}
	case vortex.PTypeU16:
		for i := 0; i < n; i++ {
			result[i] = int(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case vortex.PTypeU32:
		for i := 0; i < n; i++ {
			result[i] = int(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case vortex.PTypeU64:
		for i := 0; i < n; i++ {
			result[i] = int(binary.LittleEndian.Uint64(buf[i*8:]))
		}
	case vortex.PTypeI32:
		for i := 0; i < n; i++ {
			result[i] = int(int32(binary.LittleEndian.Uint32(buf[i*4:])))
		}
	case vortex.PTypeI64:
		for i := 0; i < n; i++ {
			result[i] = int(int64(binary.LittleEndian.Uint64(buf[i*8:])))
		}
	default:
		return nil, fmt.Errorf("unsupported PType for integer slice: %v", arr.DType.PType)
	}
	return result, nil
}

func init() {
	Register("vortex.fsst", &FSSTDecoder{})
}
