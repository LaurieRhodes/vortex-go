package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// ALPRDDecoder decodes vortex.alprd (ALP Right-Digit / "Real Doubles") arrays.
//
// ALPRD splits each float's bit representation into:
//   - left_parts: most-significant bits, dictionary-coded to u16 codes, then bitpacked
//   - right_parts: least-significant bits, bitpacked as uint (u32 for f32, u64 for f64)
//
// Decoding:
//  1. Decode left_parts child → u16 codes
//  2. Decode right_parts child → uint values
//  3. Dict-lookup left codes → u16 bit patterns
//  4. Apply patches on left parts (exceptions not in the dictionary)
//  5. Reconstruct: float = from_bits((uint(left_u16) << right_bit_width) | right)
//
// Children: [left_parts, right_parts, patch_indices?, patch_values?]
type ALPRDDecoder struct{}

func (d *ALPRDDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return nil, fmt.Errorf("alprd: expected primitive float dtype, got %v", dtype.Kind)
	}
	if len(node.Children) < 2 {
		return nil, fmt.Errorf("alprd: expected at least 2 children, got %d", len(node.Children))
	}

	// Parse metadata.
	var meta encodings.ALPRDMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("alprd: parsing metadata: %w", err)
		}
	}

	rightBW := int(meta.RightBitWidth)
	dictLen := int(meta.DictLen)

	// Build the dictionary: dict codes (index → u16 bit pattern).
	dict := make([]uint16, dictLen)
	for i := 0; i < dictLen && i < len(meta.Dict); i++ {
		dict[i] = uint16(meta.Dict[i])
	}

	// Determine left_parts dtype from metadata.
	leftPType := vortex.PTypeFromProto(meta.LeftPartsPtype)
	leftDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: leftPType}

	// Determine right_parts dtype.
	var rightDType *vortex.DType
	switch dtype.PType {
	case vortex.PTypeF32:
		rightDType = &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU32}
	case vortex.PTypeF64:
		rightDType = &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU64}
	default:
		return nil, fmt.Errorf("alprd: unsupported float ptype %v", dtype.PType)
	}

	// Decode left_parts (dictionary-coded, bitpacked u16 codes).
	leftArr, err := DecodeChild(node, 0, leftDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("alprd: decoding left_parts: %w", err)
	}

	// Decode right_parts (bitpacked uint).
	rightArr, err := DecodeChild(node, 1, rightDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("alprd: decoding right_parts: %w", err)
	}

	if len(leftArr.Buffers) == 0 || len(rightArr.Buffers) == 0 {
		return &vortex.Array{DType: dtype, Length: int(rowCount), Stats: node.Stats}, nil
	}

	leftBuf := leftArr.Buffers[0]
	rightBuf := rightArr.Buffers[0]
	n := int(rowCount)
	leftElemSize := vortex.PTypeSize(leftPType)

	// Step 1: Dictionary-decode left codes → u16 bit patterns.
	leftPatterns := make([]uint16, n)
	for i := 0; i < n; i++ {
		var code int
		switch leftElemSize {
		case 1:
			code = int(leftBuf[i])
		case 2:
			code = int(binary.LittleEndian.Uint16(leftBuf[i*2:]))
		case 4:
			code = int(binary.LittleEndian.Uint32(leftBuf[i*4:]))
		case 8:
			code = int(binary.LittleEndian.Uint64(leftBuf[i*8:]))
		}
		if code < dictLen {
			leftPatterns[i] = dict[code]
		}
		// Exceptions (code out of dict) will be patched below.
	}

	// Step 2: Apply patches on left_parts if present.
	if meta.Patches != nil && meta.Patches.Len > 0 && len(node.Children) >= 4 {
		patchLen := meta.Patches.Len
		patchOffset := meta.Patches.Offset

		idxPType := vortex.PTypeFromProto(meta.Patches.IndicesPtype)
		idxDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: idxPType}
		idxArr, err := DecodeChild(node, 2, idxDType, patchLen, ctx)
		if err != nil {
			return nil, fmt.Errorf("alprd: decoding patch indices: %w", err)
		}

		// Patch values are u16 (left bit patterns).
		valDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeU16}
		valArr, err := DecodeChild(node, 3, valDType, patchLen, ctx)
		if err != nil {
			return nil, fmt.Errorf("alprd: decoding patch values: %w", err)
		}

		if len(idxArr.Buffers) > 0 && len(valArr.Buffers) > 0 {
			idxBuf := idxArr.Buffers[0]
			valBuf := valArr.Buffers[0]
			idxElemSize := vortex.PTypeSize(idxPType)

			for i := uint64(0); i < patchLen; i++ {
				var idx uint64
				switch idxElemSize {
				case 1:
					idx = uint64(idxBuf[i])
				case 2:
					idx = uint64(binary.LittleEndian.Uint16(idxBuf[i*2:]))
				case 4:
					idx = uint64(binary.LittleEndian.Uint32(idxBuf[i*4:]))
				case 8:
					idx = binary.LittleEndian.Uint64(idxBuf[i*8:])
				}
				idx -= patchOffset // subtract the patches offset
				if idx < uint64(n) {
					leftPatterns[idx] = binary.LittleEndian.Uint16(valBuf[i*2:])
				}
			}
		}
	}

	// Step 3: Reconstruct floats: from_bits((uint(left) << right_bit_width) | right)
	outBuf := make([]byte, n*vortex.PTypeSize(dtype.PType))

	switch dtype.PType {
	case vortex.PTypeF32:
		for i := 0; i < n; i++ {
			left := uint32(leftPatterns[i])
			right := binary.LittleEndian.Uint32(rightBuf[i*4:])
			bits := (left << uint(rightBW)) | right
			binary.LittleEndian.PutUint32(outBuf[i*4:], math.Float32bits(math.Float32frombits(bits)))
		}
	case vortex.PTypeF64:
		for i := 0; i < n; i++ {
			left := uint64(leftPatterns[i])
			right := binary.LittleEndian.Uint64(rightBuf[i*8:])
			bits := (left << uint(rightBW)) | right
			binary.LittleEndian.PutUint64(outBuf[i*8:], bits)
		}
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  n,
		Buffers: [][]byte{outBuf},
		Stats:   node.Stats,
	}, nil
}

func init() {
	Register("vortex.alprd", &ALPRDDecoder{})
}
