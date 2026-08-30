package vortex

import (
	"encoding/binary"
	"fmt"

	"github.com/LaurieRhodes/vortex-go/internal/flatbuf/vortexfb"
)

// ParseArraySegment parses a Flat layout segment into an ArrayNode tree and
// extracted buffer byte slices.
//
// The Vortex segment format (as written by the Rust reference implementation):
//
//	[buffer data...] [Array FlatBuffer] [fb_length: u32 LE]
//
// The last 4 bytes of the segment are a little-endian uint32 giving the byte
// length of the Array FlatBuffer. The FlatBuffer sits immediately before those
// 4 bytes. Everything before the FlatBuffer is buffer data, laid out according
// to the Buffer descriptors in the FlatBuffer.
//
// The Array FlatBuffer contains:
//   - root: ArrayNode tree (encoding + metadata + children + buffer indices)
//   - buffers: [Buffer] descriptors (padding, alignment, compression, length)
func ParseArraySegment(data []byte, arraySpecs []string) (*ArrayNode, [][]byte, error) {
	if len(data) < 8 {
		return nil, nil, fmt.Errorf("segment too small (%d bytes)", len(data))
	}

	// Read the trailing 4-byte FlatBuffer length.
	fbLen := binary.LittleEndian.Uint32(data[len(data)-4:])
	if int(fbLen)+4 > len(data) {
		return nil, nil, fmt.Errorf("flatbuffer length %d exceeds segment size %d", fbLen, len(data))
	}

	// Extract the FlatBuffer bytes and the buffer data region.
	fbStart := len(data) - 4 - int(fbLen)
	fbBytes := data[fbStart : len(data)-4]
	bufRegion := data[:fbStart]

	return parseArrayFromFlatBufferAndBuffers(fbBytes, bufRegion, arraySpecs)
}

// ParseArraySegmentSplit parses an ArrayNode tree from a separate FlatBuffer
// (stored in layout metadata) and buffer data from a segment. This handles the
// "inline array node" mode where the Flat layout's metadata contains the Array
// FlatBuffer and the segment contains only raw buffer data.
func ParseArraySegmentSplit(fbBytes []byte, bufRegion []byte, arraySpecs []string) (*ArrayNode, [][]byte, error) {
	return parseArrayFromFlatBufferAndBuffers(fbBytes, bufRegion, arraySpecs)
}

// parseArrayFromFlatBufferAndBuffers does the actual parsing given separated
// FlatBuffer bytes and buffer data region.
func parseArrayFromFlatBufferAndBuffers(fbBytes []byte, bufRegion []byte, arraySpecs []string) (*ArrayNode, [][]byte, error) {
	if len(fbBytes) < 4 {
		return nil, nil, fmt.Errorf("flatbuffer too small (%d bytes)", len(fbBytes))
	}

	arr := vortexfb.GetRootAsArray(fbBytes, 0)

	// Extract the buffer descriptors.
	numBuffers := arr.BuffersLength()
	bufDescs := make([]BufferDesc, numBuffers)
	var bufSpec vortexfb.Buffer
	for i := 0; i < numBuffers; i++ {
		if arr.Buffers(&bufSpec, i) {
			bufDescs[i] = BufferDesc{
				Padding:           bufSpec.Padding(),
				AlignmentExponent: bufSpec.AlignmentExponent(),
				Compression:       uint8(bufSpec.Compression()),
				Length:            bufSpec.Length(),
			}
		}
	}

	// Extract buffer byte slices from the buffer data region.
	// Buffers are laid out sequentially with padding between them.
	buffers := make([][]byte, numBuffers)
	offset := 0
	for i, bd := range bufDescs {
		offset += int(bd.Padding) // skip padding
		end := offset + int(bd.Length)
		if end > len(bufRegion) {
			return nil, nil, fmt.Errorf("buffer %d overflows data region (offset %d + length %d > %d)",
				i, offset, bd.Length, len(bufRegion))
		}
		buffers[i] = bufRegion[offset:end]
		offset = end
	}

	// Parse the ArrayNode tree.
	var rootFB vortexfb.ArrayNode
	rootNode := arr.Root(&rootFB)
	if rootNode == nil {
		return nil, nil, fmt.Errorf("Array FlatBuffer has no root node")
	}

	node, err := parseFBArrayNode(rootNode, arraySpecs)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing array node: %w", err)
	}

	return node, buffers, nil
}

// parseFBArrayNode recursively converts a FlatBuffer ArrayNode into our Go ArrayNode.
func parseFBArrayNode(fb *vortexfb.ArrayNode, arraySpecs []string) (*ArrayNode, error) {
	node := &ArrayNode{}

	// Resolve encoding index → string ID.
	encIdx := fb.Encoding()
	if int(encIdx) < len(arraySpecs) {
		node.EncodingID = arraySpecs[encIdx]
	} else {
		return nil, fmt.Errorf("encoding index %d out of range (have %d specs)", encIdx, len(arraySpecs))
	}

	// Extract metadata bytes.
	node.Metadata = fb.MetadataBytes()

	// Extract buffer indices.
	node.BufferIndices = make([]uint16, fb.BuffersLength())
	for i := 0; i < fb.BuffersLength(); i++ {
		node.BufferIndices[i] = fb.Buffers(i)
	}

	// Parse stats if present.
	var statsFB vortexfb.ArrayStats
	if fb.Stats(&statsFB) != nil {
		stats := parseArrayStats(&statsFB)
		node.Stats = &stats
	}

	// Recursively parse children.
	node.Children = make([]*ArrayNode, fb.ChildrenLength())
	for i := 0; i < fb.ChildrenLength(); i++ {
		var childFB vortexfb.ArrayNode
		if fb.Children(&childFB, i) {
			child, err := parseFBArrayNode(&childFB, arraySpecs)
			if err != nil {
				return nil, fmt.Errorf("parsing child %d: %w", i, err)
			}
			node.Children[i] = child
		}
	}

	return node, nil
}

// ReadFlatSegment reads a Flat layout's segment from the file and parses it into
// an ArrayNode tree and buffer slices.
//
// It handles both segment formats:
//   - Default: the Array FlatBuffer is embedded at the end of the segment
//   - Inline: the Array FlatBuffer is in the layout's metadata (FlatLayoutMetadata protobuf)
func (f *File) ReadFlatSegment(layout *Layout) (*ArrayNode, [][]byte, error) {
	if !layout.IsFlat() {
		return nil, nil, fmt.Errorf("expected Flat layout, got %q", layout.EncodingID)
	}
	if len(layout.Segments) == 0 {
		return nil, nil, fmt.Errorf("flat layout has no segments")
	}

	// Read the segment bytes.
	segIdx := layout.Segments[0]
	if int(segIdx) >= len(f.footer.SegmentSpecs) {
		return nil, nil, fmt.Errorf("segment index %d out of range", segIdx)
	}

	segData, err := f.readSegmentSpec(f.footer.SegmentSpecs[segIdx])
	if err != nil {
		return nil, nil, err
	}

	// Check if the layout metadata contains an inline array tree.
	// FlatLayoutMetadata is a protobuf message with optional field 1 (bytes)
	// containing the Array FlatBuffer.
	if fbBytes := extractInlineArrayTree(layout.Metadata); fbBytes != nil {
		return ParseArraySegmentSplit(fbBytes, segData, f.footer.ArraySpecs)
	}

	// Default: FlatBuffer is embedded at the end of the segment.
	return ParseArraySegment(segData, f.footer.ArraySpecs)
}

// extractInlineArrayTree attempts to extract the array_encoding_tree field
// from FlatLayoutMetadata protobuf. Returns nil if not present.
// The protobuf wire format for field 1 (bytes) is: tag=0x0a, then varint length, then bytes.
func extractInlineArrayTree(metadata []byte) []byte {
	if len(metadata) == 0 {
		return nil
	}

	// Simple protobuf parser for a single optional bytes field at tag 1.
	offset := 0
	for offset < len(metadata) {
		if offset >= len(metadata) {
			return nil
		}
		tag := metadata[offset]
		offset++

		fieldNum := tag >> 3
		wireType := tag & 0x07

		if fieldNum == 1 && wireType == 2 { // field 1, length-delimited
			// Read varint length.
			length, n := decodeVarint(metadata[offset:])
			if n == 0 {
				return nil
			}
			offset += n
			end := offset + int(length)
			if end > len(metadata) {
				return nil
			}
			return metadata[offset:end]
		}

		// Skip unknown field.
		switch wireType {
		case 0: // varint
			for offset < len(metadata) && metadata[offset]&0x80 != 0 {
				offset++
			}
			offset++ // final byte
		case 2: // length-delimited
			length, n := decodeVarint(metadata[offset:])
			offset += n + int(length)
		default:
			return nil // unknown wire type, bail
		}
	}
	return nil
}

// decodeVarint decodes a protobuf varint. Returns (value, bytes consumed).
func decodeVarint(data []byte) (uint64, int) {
	var val uint64
	for i, b := range data {
		val |= uint64(b&0x7F) << (7 * uint(i))
		if b&0x80 == 0 {
			return val, i + 1
		}
		if i >= 9 {
			return 0, 0 // overflow
		}
	}
	return 0, 0
}
