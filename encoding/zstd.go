package encoding

import (
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

// ZstdDecoder decodes vortex.zstd arrays.
//
// Zstd compresses primitive or string data using the Zstandard algorithm.
// An optional shared dictionary improves compression across multiple frames.
//
// Buffers: [dictionary?] [frame0] [frame1] ...
//
//	If dictionary_size > 0, the first buffer is the Zstd dictionary.
//	Remaining buffers are compressed frames.
//
// Metadata: ZstdMetadata { dictionary_size(u32), frames: [ZstdFrameMetadata] }
//
//	Each frame has: uncompressed_size(u64), n_values(u64)
//
// Children: 0 or 1 (validity bitmap if nullable).
//
// For primitive dtypes: frames decompress to raw typed bytes.
// For string dtypes: frames decompress to length-prefixed strings
// (u32 LE length + bytes), which are reconstructed into VarBinView format.
type ZstdDecoder struct{}

func (d *ZstdDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	var meta encodings.ZstdMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("zstd: parsing metadata: %w", err)
		}
	}

	dictSize := int(meta.DictionarySize)

	// Separate dictionary buffer from frame buffers.
	var dictBuf []byte
	var frameBufs [][]byte

	if dictSize > 0 && len(buffers) > 0 {
		dictBuf = buffers[0]
		frameBufs = buffers[1:]
	} else {
		frameBufs = buffers
	}

	// Create decompressor with or without dictionary.
	var decoder *zstd.Decoder
	var err error
	if dictBuf != nil {
		decoder, err = zstd.NewReader(nil, zstd.WithDecoderDicts(dictBuf))
	} else {
		decoder, err = zstd.NewReader(nil)
	}
	if err != nil {
		return nil, fmt.Errorf("zstd: creating decoder: %w", err)
	}
	defer decoder.Close()

	// Calculate total uncompressed size from frame metadata.
	totalUncompressed := 0
	for _, fm := range meta.Frames {
		totalUncompressed += int(fm.UncompressedSize)
	}

	// Decompress all frames.
	decompressed := make([]byte, 0, totalUncompressed)
	for i, frame := range frameBufs {
		if i >= len(meta.Frames) {
			break
		}
		result, err := decoder.DecodeAll(frame, nil)
		if err != nil {
			return nil, fmt.Errorf("zstd: decompressing frame %d: %w", i, err)
		}
		decompressed = append(decompressed, result...)
	}

	// Reconstruct the array based on dtype.
	switch dtype.Kind {
	case vortex.DTypePrimitive:
		return decompressPrimitive(decompressed, dtype, int(rowCount), node)
	case vortex.DTypeUtf8, vortex.DTypeBinary:
		return decompressVarBinView(decompressed, dtype, int(rowCount), node)
	default:
		return nil, fmt.Errorf("zstd: unsupported dtype %v", dtype.Kind)
	}
}

// decompressPrimitive reconstructs a primitive array from decompressed bytes.
func decompressPrimitive(data []byte, dtype *vortex.DType, count int, node *vortex.ArrayNode) (*vortex.Array, error) {
	elemSize := vortex.PTypeSize(dtype.PType)
	if elemSize == 0 {
		return nil, fmt.Errorf("zstd: unknown element size for ptype %v", dtype.PType)
	}

	needed := count * elemSize
	if len(data) < needed {
		// Pad if shorter (e.g. due to null compaction).
		padded := make([]byte, needed)
		copy(padded, data)
		data = padded
	} else if len(data) > needed {
		data = data[:needed]
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  count,
		Buffers: [][]byte{data},
		Stats:   node.Stats,
	}, nil
}

// decompressVarBinView reconstructs a VarBinView string array from decompressed
// length-prefixed data. The format is interleaved: [u32 LE length][bytes]...
func decompressVarBinView(data []byte, dtype *vortex.DType, count int, node *vortex.ArrayNode) (*vortex.Array, error) {
	// Parse length-prefixed strings and build VarBinView format.
	viewsBuf := make([]byte, count*16)

	offset := 0
	for i := 0; i < count; i++ {
		if offset+4 > len(data) {
			break
		}
		strLen := int(binary.LittleEndian.Uint32(data[offset:]))
		offset += 4

		viewOff := i * 16
		binary.LittleEndian.PutUint32(viewsBuf[viewOff:], uint32(strLen))

		if strLen <= 12 {
			// Inline: copy up to 12 bytes directly into the view.
			if offset+strLen <= len(data) {
				copy(viewsBuf[viewOff+4:viewOff+16], data[offset:offset+strLen])
			}
		} else {
			// Out-of-line: store prefix + buffer index + offset.
			if offset+strLen <= len(data) {
				copy(viewsBuf[viewOff+4:viewOff+8], data[offset:offset+4]) // first 4 bytes prefix
			}
			binary.LittleEndian.PutUint32(viewsBuf[viewOff+8:], 0)               // buffer index
			binary.LittleEndian.PutUint32(viewsBuf[viewOff+12:], uint32(offset)) // offset into data
		}
		offset += strLen
	}

	return &vortex.Array{
		DType:   dtype,
		Length:  count,
		Buffers: [][]byte{viewsBuf, data},
		Stats:   node.Stats,
	}, nil
}

func init() {
	Register("vortex.zstd", &ZstdDecoder{})
}
