package encoding

import (
	"encoding/binary"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

func TestDecimalByteParts(t *testing.T) {
	// Decimal(8, 2): values like 1.23 are stored as int32 123.
	// Build a primitive i32 child with values [100, 200, 350, -150].
	values := []int32{100, 200, 350, -150}
	n := len(values)

	buf := make([]byte, n*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}

	meta := &encodings.DecimalBytesPartsMetadata{
		ZerothChildPtype: int32(vortex.PTypeI32),
		LowerPartCount:   0,
	}
	metaBytes, _ := proto.Marshal(meta)

	node := &vortex.ArrayNode{
		EncodingID: "vortex.decimal_byte_parts",
		Metadata:   metaBytes,
		Children: []*vortex.ArrayNode{
			{EncodingID: "vortex.primitive", BufferIndices: []uint16{0}},
		},
	}

	dtype := &vortex.DType{
		Kind:      vortex.DTypeDecimal,
		Precision: 8,
		Scale:     2,
	}

	arr, err := DecodeArray(node, [][]byte{buf}, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	if arr.Length != n {
		t.Fatalf("Length = %d, want %d", arr.Length, n)
	}
	if arr.DType.Kind != vortex.DTypeDecimal {
		t.Fatalf("DType.Kind = %v, want DTypeDecimal", arr.DType.Kind)
	}
	if arr.DType.Precision != 8 || arr.DType.Scale != 2 {
		t.Fatalf("DType = decimal(%d,%d), want decimal(8,2)", arr.DType.Precision, arr.DType.Scale)
	}

	// Verify the raw integer values are preserved.
	outBuf := arr.Buffers[0]
	for i, want := range values {
		got := int32(binary.LittleEndian.Uint32(outBuf[i*4:]))
		if got != want {
			t.Errorf("[%d] = %d, want %d", i, got, want)
		}
	}
	t.Logf("decimal_byte_parts: decimal(8,2) with %d values, int repr [100,200,350,-150] ✓", n)
}

func TestDecimalBytePartsI64(t *testing.T) {
	// Decimal(18, 6): large precision with i64 backing.
	values := []int64{1234567890, -9876543210, 0, 42}
	n := len(values)

	buf := make([]byte, n*8)
	for i, v := range values {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
	}

	meta := &encodings.DecimalBytesPartsMetadata{
		ZerothChildPtype: int32(vortex.PTypeI64),
		LowerPartCount:   0,
	}
	metaBytes, _ := proto.Marshal(meta)

	node := &vortex.ArrayNode{
		EncodingID: "vortex.decimal_byte_parts",
		Metadata:   metaBytes,
		Children: []*vortex.ArrayNode{
			{EncodingID: "vortex.primitive", BufferIndices: []uint16{0}},
		},
	}

	dtype := &vortex.DType{
		Kind:      vortex.DTypeDecimal,
		Precision: 18,
		Scale:     6,
	}

	arr, err := DecodeArray(node, [][]byte{buf}, dtype, uint64(n))
	if err != nil {
		t.Fatalf("DecodeArray: %v", err)
	}

	outBuf := arr.Buffers[0]
	for i, want := range values {
		got := int64(binary.LittleEndian.Uint64(outBuf[i*8:]))
		if got != want {
			t.Errorf("[%d] = %d, want %d", i, got, want)
		}
	}
	t.Logf("decimal_byte_parts: decimal(18,6) i64 backing ✓")
}
