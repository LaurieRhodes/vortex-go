package writer

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"sort"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"github.com/LaurieRhodes/vortex-go/internal/proto/scalar"
	"github.com/LaurieRhodes/vortex-go/pcodec"
	"google.golang.org/protobuf/proto"
)

// -----------------------------------------------------------------------
// Dictionary encoding for low-cardinality strings
// -----------------------------------------------------------------------

// canonicalizeDictString attempts dictionary encoding for a string column.
// Returns (nil, nil, nil) if the column isn't suitable for dictionary encoding.
func (w *Writer) canonicalizeDictString(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
	if arr.Length == 0 {
		return nil, nil, nil
	}

	// Extract strings from the VarBinView array.
	strings, err := extractStringsFromArray(arr)
	if err != nil || strings == nil {
		return nil, nil, nil // Can't extract, fall back to varbinview.
	}

	// Build dictionary: sorted unique values.
	dict, codes := buildDictionary(strings)
	if len(dict) == 0 {
		return nil, nil, nil
	}

	// Check cardinality ratio: only dict-encode if ratio < 0.5.
	ratio := float64(len(dict)) / float64(len(strings))
	if ratio >= 0.5 {
		return nil, nil, nil // Not worth dict-encoding.
	}

	// Choose codes PType based on dictionary size.
	var codesPType vortex.PType
	switch {
	case len(dict) <= 256:
		codesPType = vortex.PTypeU8
	case len(dict) <= 65536:
		codesPType = vortex.PTypeU16
	default:
		codesPType = vortex.PTypeU32
	}

	// Build codes buffer.
	codesBuf := encodeCodesBuf(codes, codesPType)

	// Build dictionary values as VarBinView.
	viewsBuf, dataBufs := encodeVarBinView(dict)

	// Assemble the node tree.
	// Dict metadata.
	allRef := true
	dictMeta := &encodings.DictArrayMetadata{
		ValuesLen:           uint32(len(dict)),
		CodesPtype:          int32(codesPType),
		AllValuesReferenced: &allRef,
	}
	dictMetaBytes, err := proto.Marshal(dictMeta)
	if err != nil {
		return nil, nil, fmt.Errorf("dict: marshalling metadata: %w", err)
	}

	dictEncIdx := w.registerArraySpec("vortex.dict")
	primEncIdx := w.registerArraySpec("vortex.primitive")
	vbvEncIdx := w.registerArraySpec("vortex.varbinview")

	// Codes child: primitive array.
	codesNode := &arrayNodeInfo{
		encodingIdx:   primEncIdx,
		bufferLengths: []uint32{uint32(len(codesBuf))},
	}

	// Values child: varbinview array.
	vbvLengths := make([]uint32, 1+len(dataBufs))
	vbvLengths[0] = uint32(len(viewsBuf))
	for i, db := range dataBufs {
		vbvLengths[i+1] = uint32(len(db))
	}
	// VarBinView views buffer requires 16-byte (2^4) alignment.
	vbvAligns := make([]uint8, len(vbvLengths))
	vbvAligns[0] = 4 // views buffer: 16-byte alignment
	for i := 1; i < len(vbvAligns); i++ {
		vbvAligns[i] = 3 // data buffers: 8-byte alignment
	}
	valuesNode := &arrayNodeInfo{
		encodingIdx:   vbvEncIdx,
		bufferLengths: vbvLengths,
		bufferAligns:  vbvAligns,
	}

	// Root dict node.
	dictNode := &arrayNodeInfo{
		encodingIdx: dictEncIdx,
		metadata:    dictMetaBytes,
		children:    []*arrayNodeInfo{codesNode, valuesNode},
	}

	// Assemble buffers in order: codes, views, data...
	var allBuffers [][]byte
	allBuffers = append(allBuffers, codesBuf)
	allBuffers = append(allBuffers, viewsBuf)
	allBuffers = append(allBuffers, dataBufs...)

	return dictNode, allBuffers, nil
}

// extractStringsFromArray reads strings from a VarBinView array.
func extractStringsFromArray(arr *vortex.Array) ([]string, error) {
	if len(arr.Buffers) == 0 {
		// Empty or zero-length views: all empty strings.
		result := make([]string, arr.Length)
		return result, nil
	}

	views := arr.Buffers[0]
	if len(views) < arr.Length*16 {
		return nil, fmt.Errorf("views buffer too short")
	}

	result := make([]string, arr.Length)
	for i := 0; i < arr.Length; i++ {
		off := i * 16
		strLen := binary.LittleEndian.Uint32(views[off:])
		if strLen <= 12 {
			// Inline string.
			result[i] = string(views[off+4 : off+4+int(strLen)])
		} else {
			// Out-of-line: bytes 4..7 = prefix, 8..11 = bufIdx, 12..15 = offset.
			bufIdx := binary.LittleEndian.Uint32(views[off+8:])
			dataOff := binary.LittleEndian.Uint32(views[off+12:])
			bi := int(bufIdx) + 1 // buffer[0] is views, buffer[1..] are data.
			if bi < len(arr.Buffers) {
				end := int(dataOff) + int(strLen)
				if end <= len(arr.Buffers[bi]) {
					result[i] = string(arr.Buffers[bi][dataOff:end])
				}
			}
		}
	}
	return result, nil
}

// buildDictionary creates a sorted dictionary and index codes from the input strings.
func buildDictionary(strings []string) (dict []string, codes []int) {
	// Find unique values.
	unique := make(map[string]struct{})
	for _, s := range strings {
		unique[s] = struct{}{}
	}

	// Sort dictionary.
	dict = make([]string, 0, len(unique))
	for s := range unique {
		dict = append(dict, s)
	}
	sort.Strings(dict)

	// Build reverse lookup.
	lookup := make(map[string]int, len(dict))
	for i, s := range dict {
		lookup[s] = i
	}

	// Map input to codes.
	codes = make([]int, len(strings))
	for i, s := range strings {
		codes[i] = lookup[s]
	}

	return dict, codes
}

// encodeCodesBuf encodes integer codes into a byte buffer.
func encodeCodesBuf(codes []int, ptype vortex.PType) []byte {
	switch ptype {
	case vortex.PTypeU8:
		buf := make([]byte, len(codes))
		for i, c := range codes {
			buf[i] = byte(c)
		}
		return buf
	case vortex.PTypeU16:
		buf := make([]byte, len(codes)*2)
		for i, c := range codes {
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(c))
		}
		return buf
	default: // U32
		buf := make([]byte, len(codes)*4)
		for i, c := range codes {
			binary.LittleEndian.PutUint32(buf[i*4:], uint32(c))
		}
		return buf
	}
}

// encodeVarBinView creates VarBinView buffers (views + data) from a list of strings.
func encodeVarBinView(strings []string) (viewsBuf []byte, dataBufs [][]byte) {
	viewsBuf = make([]byte, len(strings)*16)

	// Collect all out-of-line data into a single data buffer.
	var dataBuf []byte
	dataOffset := 0

	for i, s := range strings {
		off := i * 16
		sLen := len(s)
		binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(sLen))

		if sLen <= 12 {
			// Inline: copy string directly into view bytes 4..15.
			copy(viewsBuf[off+4:], s)
		} else {
			// Out-of-line: prefix (4 bytes), bufIdx (u32), offset (u32).
			copy(viewsBuf[off+4:off+8], s[:4]) // first 4 bytes as prefix
			binary.LittleEndian.PutUint32(viewsBuf[off+8:], 0) // bufIdx = 0
			binary.LittleEndian.PutUint32(viewsBuf[off+12:], uint32(dataOffset))
			dataBuf = append(dataBuf, s...)
			dataOffset += sLen
		}
	}

	if len(dataBuf) > 0 {
		dataBufs = [][]byte{dataBuf}
	}
	return viewsBuf, dataBufs
}

// -----------------------------------------------------------------------
// FoR + FastLanes BitPacking for integers
// -----------------------------------------------------------------------

// canonicalizeFoRBitPacked attempts FoR + BitPacking for an integer column.
// Returns (nil, nil, nil) if the column isn't suitable.
func (w *Writer) canonicalizeFoRBitPacked(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
	if arr.Length == 0 || dtype.Kind != vortex.DTypePrimitive {
		return nil, nil, nil
	}

	// Only handle integer types (not floats).
	switch dtype.PType {
	case vortex.PTypeI8, vortex.PTypeI16, vortex.PTypeI32, vortex.PTypeI64,
		vortex.PTypeU8, vortex.PTypeU16, vortex.PTypeU32, vortex.PTypeU64:
		// OK
	default:
		return nil, nil, nil
	}

	if len(arr.Buffers) == 0 {
		return nil, nil, nil
	}

	buf := arr.Buffers[0]
	elemSize := vortex.PTypeSize(dtype.PType)
	if len(buf) < arr.Length*elemSize {
		return nil, nil, nil
	}

	// Compute min/max as uint64.
	minVal, maxVal := computeMinMaxUint64(buf, arr.Length, dtype.PType)

	// Compute bit width needed for the range.
	rangeVal := maxVal - minVal
	bw := 0
	if rangeVal > 0 {
		bw = bits.Len64(rangeVal)
	}
	typeBits := elemSize * 8

	// Not worth bitpacking if bit width >= type width (or saves < 25%).
	if bw >= typeBits || bw > typeBits*3/4 {
		return nil, nil, nil
	}

	// Build delta buffer: subtract min from each value (as unsigned).
	deltas := computeDeltas(buf, arr.Length, dtype.PType, minVal)

	// Pack deltas using FastLanes bitpacking.
	packedBuf := packFastLanes(deltas, bw, elemSize)

	// Build FoR reference scalar metadata.
	refScalar := makeRefScalar(minVal, dtype.PType)
	forMeta, err := proto.Marshal(refScalar)
	if err != nil {
		return nil, nil, fmt.Errorf("for: marshalling ref scalar: %w", err)
	}

	// Build BitPacked metadata.
	bpMeta := &encodings.BitPackedMetadata{
		BitWidth: uint32(bw),
		Offset:   0,
	}
	bpMetaBytes, err := proto.Marshal(bpMeta)
	if err != nil {
		return nil, nil, fmt.Errorf("bitpacked: marshalling metadata: %w", err)
	}

	forEncIdx := w.registerArraySpec("fastlanes.for")
	bpEncIdx := w.registerArraySpec("fastlanes.bitpacked")

	// BitPacked child node.
	bpNode := &arrayNodeInfo{
		encodingIdx:   bpEncIdx,
		metadata:      bpMetaBytes,
		bufferLengths: []uint32{uint32(len(packedBuf))},
	}

	// FoR parent node.
	forNode := &arrayNodeInfo{
		encodingIdx: forEncIdx,
		metadata:    forMeta,
		children:    []*arrayNodeInfo{bpNode},
	}

	return forNode, [][]byte{packedBuf}, nil
}

// computeMinMaxUint64 finds min/max values as uint64 for any integer PType.
func computeMinMaxUint64(buf []byte, n int, ptype vortex.PType) (min, max uint64) {
	min = math.MaxUint64
	max = 0

	for i := 0; i < n; i++ {
		v := readAsUint64(buf, i, ptype)
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

// readAsUint64 reads a value from buf at index i, reinterpreted as uint64.
// For signed types, the raw bit pattern is used (wrapping subtraction works correctly).
func readAsUint64(buf []byte, i int, ptype vortex.PType) uint64 {
	switch ptype {
	case vortex.PTypeU8, vortex.PTypeI8:
		return uint64(buf[i])
	case vortex.PTypeU16, vortex.PTypeI16:
		return uint64(binary.LittleEndian.Uint16(buf[i*2:]))
	case vortex.PTypeU32, vortex.PTypeI32:
		return uint64(binary.LittleEndian.Uint32(buf[i*4:]))
	case vortex.PTypeU64, vortex.PTypeI64:
		return binary.LittleEndian.Uint64(buf[i*8:])
	default:
		return 0
	}
}

// computeDeltas subtracts minVal from each element, returning uint64 deltas.
func computeDeltas(buf []byte, n int, ptype vortex.PType, minVal uint64) []uint64 {
	deltas := make([]uint64, n)
	for i := 0; i < n; i++ {
		deltas[i] = readAsUint64(buf, i, ptype) - minVal
	}
	return deltas
}

// makeRefScalar creates a ScalarValue proto for the FoR reference value.
func makeRefScalar(val uint64, ptype vortex.PType) *scalar.ScalarValue {
	switch ptype {
	case vortex.PTypeI8:
		return &scalar.ScalarValue{Kind: &scalar.ScalarValue_Int64Value{Int64Value: int64(int8(val))}}
	case vortex.PTypeI16:
		return &scalar.ScalarValue{Kind: &scalar.ScalarValue_Int64Value{Int64Value: int64(int16(val))}}
	case vortex.PTypeI32:
		return &scalar.ScalarValue{Kind: &scalar.ScalarValue_Int64Value{Int64Value: int64(int32(val))}}
	case vortex.PTypeI64:
		return &scalar.ScalarValue{Kind: &scalar.ScalarValue_Int64Value{Int64Value: int64(val)}}
	case vortex.PTypeU8, vortex.PTypeU16, vortex.PTypeU32, vortex.PTypeU64:
		return &scalar.ScalarValue{Kind: &scalar.ScalarValue_Uint64Value{Uint64Value: val}}
	default:
		return &scalar.ScalarValue{Kind: &scalar.ScalarValue_Uint64Value{Uint64Value: val}}
	}
}

// -----------------------------------------------------------------------
// FastLanes packing (inverse of unpack)
// -----------------------------------------------------------------------

// flOrder is the FastLanes transposition order.
var flOrder = [8]int{0, 4, 2, 6, 1, 5, 3, 7}

const flBlockSize = 1024

// packFastLanes packs uint64 values into a FastLanes bitpacked buffer.
// elemSize is the physical type width in bytes (1, 2, 4, or 8).
// bw is the bit width of each packed value.
func packFastLanes(values []uint64, bw int, elemSize int) []byte {
	if bw == 0 {
		return nil
	}

	tBits := elemSize * 8
	nLanes := flBlockSize / tBits

	// Compute packed buffer size.
	// Each 1024-element block produces nLanes * bw words of T.
	nBlocks := (len(values) + flBlockSize - 1) / flBlockSize
	wordsPerBlock := nLanes * bw
	totalWords := nBlocks * wordsPerBlock
	packedBuf := make([]byte, totalWords*elemSize)

	// Process each 1024-element block.
	for block := 0; block < nBlocks; block++ {
		blockStart := block * flBlockSize
		blockOffset := block * wordsPerBlock * elemSize

		// For each logical index, compute (row, lane) in transposed layout,
		// then pack the value's bw bits starting at bit position row*bw in the lane's column.
		for logIdx := 0; logIdx < flBlockSize; logIdx++ {
			// Get value (pad with 0 beyond input length).
			var val uint64
			srcIdx := blockStart + logIdx
			if srcIdx < len(values) {
				val = values[srcIdx]
			}

			// Transpose: logical index → (row, lane).
			lane := logIdx % nLanes
			s := logIdx / 128
			fl := (logIdx - s*128 - lane) / 16
			row := flOrder[fl]*8 + s

			// Pack: value's bw bits go at bit offset row*bw.
			startBit := row * bw
			for bit := 0; bit < bw; bit++ {
				if val&(1<<uint(bit)) != 0 {
					bitPos := startBit + bit
					word := bitPos / tBits
					shift := bitPos % tBits
					physIdx := word*nLanes + lane
					byteOff := blockOffset + physIdx*elemSize

					// Set bit in the T-sized word.
					switch elemSize {
					case 1:
						packedBuf[byteOff] |= 1 << uint(shift)
					case 2:
						v := binary.LittleEndian.Uint16(packedBuf[byteOff:])
						v |= 1 << uint(shift)
						binary.LittleEndian.PutUint16(packedBuf[byteOff:], v)
					case 4:
						v := binary.LittleEndian.Uint32(packedBuf[byteOff:])
						v |= 1 << uint(shift)
						binary.LittleEndian.PutUint32(packedBuf[byteOff:], v)
					case 8:
						v := binary.LittleEndian.Uint64(packedBuf[byteOff:])
						v |= 1 << uint(shift)
						binary.LittleEndian.PutUint64(packedBuf[byteOff:], v)
					}
				}
			}
		}
	}

	return packedBuf
}

// -----------------------------------------------------------------------
// Pcodec compression for numeric columns
// -----------------------------------------------------------------------

// canonicalizePco attempts pcodec compression for a numeric column.
// Returns nil, nil, nil if pcodec is not applicable or doesn't compress well.
func (w *Writer) canonicalizePco(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
	if arr.Length == 0 || dtype.Kind != vortex.DTypePrimitive {
		return nil, nil, nil
	}

	// Map PType to pcodec NumberType.
	nt, ok := ptypeToNumberType(dtype.PType)
	if !ok {
		return nil, nil, nil // unsupported type (u8/i8)
	}

	if len(arr.Buffers) == 0 {
		return nil, nil, nil
	}

	data := arr.Buffers[0]
	elemSize := vortex.PTypeSize(dtype.PType)
	expectedLen := arr.Length * elemSize
	if len(data) < expectedLen {
		return nil, nil, nil
	}
	data = data[:expectedLen]

	// Compress.
	opts := pcodec.CompressOptions{
		Level: w.opts.PcodecLevel,
	}
	result, err := pcodec.Compress(data, nt, opts)
	if err != nil {
		return nil, nil, nil // compression failed, fall back
	}

	// Check compression ratio — skip if < 10% savings.
	compressedSize := len(result.Header)
	for _, cm := range result.ChunkMetas {
		compressedSize += len(cm)
	}
	for _, p := range result.Pages {
		compressedSize += len(p)
	}
	if compressedSize >= expectedLen*9/10 {
		return nil, nil, nil // not worth it
	}

	// Build PcoMetadata protobuf.
	meta := &encodings.PcoMetadata{
		Header: result.Header,
	}
	pageIdx := 0
	for _, nPages := range result.PageCounts {
		chunk := &encodings.PcoChunkInfo{}
		for p := 0; p < nPages; p++ {
			chunk.Pages = append(chunk.Pages, &encodings.PcoPageInfo{
				NValues: uint32(result.PageNs[pageIdx]),
			})
			pageIdx++
		}
		meta.Chunks = append(meta.Chunks, chunk)
	}

	metaBytes, err := proto.Marshal(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("pco: marshalling metadata: %w", err)
	}

	// Buffers: [chunk_meta_0, ..., chunk_meta_N, page_0, ..., page_M]
	var buffers [][]byte
	var bufLens []uint32
	for _, cm := range result.ChunkMetas {
		buffers = append(buffers, cm)
		bufLens = append(bufLens, uint32(len(cm)))
	}
	for _, p := range result.Pages {
		buffers = append(buffers, p)
		bufLens = append(bufLens, uint32(len(p)))
	}

	encIdx := w.registerArraySpec("vortex.pco")
	node := &arrayNodeInfo{
		encodingIdx:   encIdx,
		metadata:      metaBytes,
		bufferLengths: bufLens,
	}

	return node, buffers, nil
}

// ptypeToNumberType maps a vortex PType to pcodec NumberType.
func ptypeToNumberType(pt vortex.PType) (pcodec.NumberType, bool) {
	switch pt {
	case vortex.PTypeU16:
		return pcodec.NumU16, true
	case vortex.PTypeU32:
		return pcodec.NumU32, true
	case vortex.PTypeU64:
		return pcodec.NumU64, true
	case vortex.PTypeI16:
		return pcodec.NumI16, true
	case vortex.PTypeI32:
		return pcodec.NumI32, true
	case vortex.PTypeI64:
		return pcodec.NumI64, true
	case vortex.PTypeF32:
		return pcodec.NumF32, true
	case vortex.PTypeF64:
		return pcodec.NumF64, true
	default:
		return 0, false // u8/i8 not supported by pcodec
	}
}
