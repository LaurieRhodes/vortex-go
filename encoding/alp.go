package encoding

import (
	"encoding/binary"
	"fmt"
	"math"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// ALPDecoder decodes vortex.alp (Adaptive Lossless floating-Point) arrays.
//
// ALP encodes floating-point values as integers using powers-of-10 scaling:
//
//	encoded_int = round(float * 10^e / 10^f)
//	float       = int(encoded_int) * 10^f * 10^(-e)
//
// Children:
//   - child[0]: encoded integers (i32 for f32, i64 for f64)
//   - child[1..]: optional patches (indices, values, chunk_offsets) for
//     values that don't round-trip cleanly through the ALP transform
type ALPDecoder struct{}

// Pre-computed powers of 10 matching the Rust ALPFloat tables.
var f10_f32 = [11]float32{
	1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
}
var if10_f32 = [11]float32{
	1, 0.1, 0.01, 1e-3, 1e-4, 1e-5, 1e-6, 1e-7, 1e-8, 1e-9, 1e-10,
}

var f10_f64 = [24]float64{
	1, 10, 100, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9,
	1e10, 1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19,
	1e20, 1e21, 1e22, 1e23,
}
var if10_f64 = [24]float64{
	1, 0.1, 0.01, 1e-3, 1e-4, 1e-5, 1e-6, 1e-7, 1e-8, 1e-9,
	1e-10, 1e-11, 1e-12, 1e-13, 1e-14, 1e-15, 1e-16, 1e-17, 1e-18, 1e-19,
	1e-20, 1e-21, 1e-22, 1e-23,
}

func (d *ALPDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return nil, fmt.Errorf("alp: expected primitive dtype, got %v", dtype.Kind)
	}
	if len(node.Children) < 1 {
		return nil, fmt.Errorf("alp: expected at least 1 child (encoded), got %d", len(node.Children))
	}

	// Parse metadata.
	var meta encodings.ALPMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("alp: parsing metadata: %w", err)
		}
	}
	expE := int(meta.ExpE)
	expF := int(meta.ExpF)

	// Determine the encoded integer type from the target float type.
	var intDType *vortex.DType
	switch dtype.PType {
	case vortex.PTypeF32:
		intDType = &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32}
	case vortex.PTypeF64:
		intDType = &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64}
	default:
		return nil, fmt.Errorf("alp: unsupported float ptype %v", dtype.PType)
	}

	// Decode the encoded integer child.
	child, err := DecodeChild(node, 0, intDType, rowCount, ctx)
	if err != nil {
		return nil, fmt.Errorf("alp: decoding encoded child: %w", err)
	}

	if len(child.Buffers) == 0 || child.Length == 0 {
		return &vortex.Array{
			DType:  dtype,
			Length: child.Length,
			Stats:  node.Stats,
		}, nil
	}

	// Convert integers to floats in-place (same buffer width: i32↔f32, i64↔f64).
	buf := child.Buffers[0]

	switch dtype.PType {
	case vortex.PTypeF32:
		if expE >= len(f10_f32) || expF >= len(f10_f32) {
			return nil, fmt.Errorf("alp: f32 exponents out of range (e=%d, f=%d)", expE, expF)
		}
		fF := f10_f32[expF]
		ifE := if10_f32[expE]
		for i := 0; i < child.Length; i++ {
			off := i * 4
			intVal := int32(binary.LittleEndian.Uint32(buf[off:]))
			floatVal := float32(intVal) * fF * ifE
			binary.LittleEndian.PutUint32(buf[off:], math.Float32bits(floatVal))
		}

	case vortex.PTypeF64:
		if expE >= len(f10_f64) || expF >= len(f10_f64) {
			return nil, fmt.Errorf("alp: f64 exponents out of range (e=%d, f=%d)", expE, expF)
		}
		fF := f10_f64[expF]
		ifE := if10_f64[expE]
		for i := 0; i < child.Length; i++ {
			off := i * 8
			intVal := int64(binary.LittleEndian.Uint64(buf[off:]))
			floatVal := float64(intVal) * fF * ifE
			binary.LittleEndian.PutUint64(buf[off:], math.Float64bits(floatVal))
		}
	}

	// Apply patches if present.
	if meta.Patches != nil {
		if err := d.applyPatches(node, buf, dtype, rowCount, meta.Patches, ctx); err != nil {
			return nil, fmt.Errorf("alp: applying patches: %w", err)
		}
	}

	result := &vortex.Array{
		DType:   dtype,
		Length:  child.Length,
		Buffers: [][]byte{buf},
		Stats:   node.Stats,
	}

	return result, nil
}

// applyPatches decodes patch indices and values from children[1..] and overwrites
// the corresponding positions in the output buffer.
func (d *ALPDecoder) applyPatches(node *vortex.ArrayNode, buf []byte, dtype *vortex.DType, rowCount uint64, patchMeta *encodings.PatchesMetadata, ctx *DecodeContext) error {
	patchLen := patchMeta.Len
	if patchLen == 0 {
		return nil
	}

	// child[1] = patch indices, child[2] = patch values
	if len(node.Children) < 3 {
		return fmt.Errorf("patches metadata present but only %d children (need ≥3)", len(node.Children))
	}

	// Decode indices.
	idxPType := vortex.PTypeFromProto(patchMeta.IndicesPtype)
	idxDType := &vortex.DType{Kind: vortex.DTypePrimitive, PType: idxPType}
	idxArr, err := DecodeChild(node, 1, idxDType, patchLen, ctx)
	if err != nil {
		return fmt.Errorf("decoding patch indices: %w", err)
	}

	// Decode values (same dtype as the output float type).
	valArr, err := DecodeChild(node, 2, dtype, patchLen, ctx)
	if err != nil {
		return fmt.Errorf("decoding patch values: %w", err)
	}

	if len(idxArr.Buffers) == 0 || len(valArr.Buffers) == 0 {
		return nil
	}

	idxBuf := idxArr.Buffers[0]
	valBuf := valArr.Buffers[0]
	offset := patchMeta.Offset
	elemSize := vortex.PTypeSize(dtype.PType)

	for i := uint64(0); i < patchLen; i++ {
		// Read the index (accounting for offset).
		var idx uint64
		switch idxPType {
		case vortex.PTypeU8:
			idx = uint64(idxBuf[i])
		case vortex.PTypeU16:
			idx = uint64(binary.LittleEndian.Uint16(idxBuf[i*2:]))
		case vortex.PTypeU32:
			idx = uint64(binary.LittleEndian.Uint32(idxBuf[i*4:]))
		case vortex.PTypeU64:
			idx = binary.LittleEndian.Uint64(idxBuf[i*8:])
		default:
			return fmt.Errorf("unsupported index ptype %v", idxPType)
		}
		idx += offset

		if idx >= uint64(len(buf)/elemSize) {
			return fmt.Errorf("patch index %d out of range (len=%d)", idx, len(buf)/elemSize)
		}

		// Copy the patch value into the output buffer.
		dstOff := int(idx) * elemSize
		srcOff := int(i) * elemSize
		copy(buf[dstOff:dstOff+elemSize], valBuf[srcOff:srcOff+elemSize])
	}

	return nil
}

func init() {
	Register("vortex.alp", &ALPDecoder{})
}
