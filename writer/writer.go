// Package writer implements a Vortex file writer.
//
// The writer produces valid Vortex files using canonical (uncompressed) encodings.
// Files are readable by the Go reader, the Rust reference implementation, DuckDB's
// Vortex extension, and the Python vortex library.
//
// Basic usage:
//
//	schema := &vortex.DType{
//	    Kind: vortex.DTypeStruct,
//	    FieldNames: []string{"id", "name"},
//	    FieldTypes: []*vortex.DType{
//	        {Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
//	        {Kind: vortex.DTypeUtf8},
//	    },
//	}
//	w := writer.NewWriter(file, schema, writer.DefaultOptions())
//	w.WriteChunk(columns)
//	w.Close()
package writer

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/bits"

	flatbuffers "github.com/google/flatbuffers/go"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/flatbuf/vortexfb"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"github.com/LaurieRhodes/vortex-go/internal/proto/scalar"
	"google.golang.org/protobuf/proto"
)

// Options controls writer behavior.
type Options struct {
	// Version is the file format version to write. Default: 1.
	Version uint16

	// EnableDict enables dictionary encoding for low-cardinality string columns.
	// When enabled, strings with cardinality ratio < 0.5 are dict-encoded.
	EnableDict bool

	// EnableBitPacking enables FoR + FastLanes BitPacking for integer columns.
	// When enabled, integers with a narrow value range are bitpacked.
	EnableBitPacking bool

	// EnablePcodec enables pcodec compression for numeric columns.
	// Used as fallback for integers when BitPacking doesn't help,
	// and as the primary compressor for floats.
	EnablePcodec bool

	// PcodecLevel controls pcodec compression effort (0=baseline, 6=default multi-bin).
	PcodecLevel int

	// EnableZoneMaps enables per-zone min/max statistics (vortex.stats layout).
	// Each chunk becomes one zone, enabling chunk-level pruning during scans.
	EnableZoneMaps bool
}

// DefaultOptions returns the default writer options with compression enabled.
func DefaultOptions() Options {
	return Options{
		Version:          1,
		EnableDict:       true,
		EnableBitPacking: true,
		EnablePcodec:     true,
		PcodecLevel:      6,
		EnableZoneMaps:   true,
	}
}

// UncompressedOptions returns writer options with no compression.
func UncompressedOptions() Options {
	return Options{Version: 1}
}

// Writer writes a Vortex file.
//
// The writer accumulates data segments as chunks are written, then finalizes
// the file metadata (layout tree, footer, postscript) on Close.
type Writer struct {
	w      io.Writer
	schema *vortex.DType
	opts   Options

	// Accumulated state during writing.
	offset     uint64 // current byte offset in the file
	segSpecs   []vortex.SegmentSpec
	arraySpecs map[string]int // encoding ID → index in arraySpecs list
	arrayIDs   []string       // ordered list of encoding IDs

	// Per-column accumulated data: one entry per column, each entry
	// holds the segment indices and row counts per chunk.
	colChunks [][]colChunk

	// File-level stats per column (updated with each chunk).
	colStats []columnStats

	// Per-column zone map trackers (nil if zone maps disabled).
	zoneTrackers []zoneColTracker

	// Per-column stats table segment indices (filled during Close).
	statsSegIdx []int // -1 means no zone map for that column

	// pendingStringChunks buffers Utf8/Binary column arrays across
	// WriteChunk calls (one entry per call, in order) instead of
	// serializing them immediately. This lets finalizeStringColumns,
	// called once from Close, evaluate dictionary-encoding cardinality
	// across the WHOLE extent rather than one 8192-row zone at a time —
	// see finalizeStringColumns's doc comment for the full rationale.
	// Non-string columns are unaffected and keep streaming immediately,
	// exactly as before.
	pendingStringChunks [][]*vortex.Array

	// dictValuesSegIdx[i] is the segment index of column i's shared
	// dictionary values (if it ended up dictionary-encoded), or -1.
	// Filled by finalizeStringColumns during Close, consulted by
	// buildLayoutFB to decide whether to wrap the column's codes in a
	// Dict layout node. dictValuesCount and dictCodesPType are filled
	// alongside it — buildLayoutFB needs them to construct the Dict
	// layout's DictArrayMetadata bytes (ValuesLen, CodesPtype), which
	// ParseDictLayoutCodesPType reads back on the read side.
	dictValuesSegIdx []int
	dictValuesCount  []int
	dictCodesPType   []vortex.PType

	closed bool
}

// colChunk records the segment index and row count for one chunk of one column.
type colChunk struct {
	segmentIdx uint32
	rowCount   uint64
}

// columnStats tracks running statistics for a column.
type columnStats struct {
	nullCount uint64
	hasMin    bool
	hasMax    bool
	minBytes  []byte // protobuf ScalarValue
	maxBytes  []byte // protobuf ScalarValue
}

// NewWriter creates a new Vortex file writer.
//
// The schema must be a Struct DType with named fields. Each call to WriteChunk
// must provide data for every field in the schema.
func NewWriter(w io.Writer, schema *vortex.DType, opts Options) (*Writer, error) {
	if schema.Kind != vortex.DTypeStruct {
		return nil, fmt.Errorf("writer: schema must be a Struct DType")
	}

	wr := &Writer{
		w:                   w,
		schema:              schema,
		opts:                opts,
		arraySpecs:          make(map[string]int),
		colChunks:           make([][]colChunk, len(schema.FieldNames)),
		colStats:            make([]columnStats, len(schema.FieldNames)),
		pendingStringChunks: make([][]*vortex.Array, len(schema.FieldNames)),
		dictValuesSegIdx:    make([]int, len(schema.FieldNames)),
		dictValuesCount:     make([]int, len(schema.FieldNames)),
		dictCodesPType:      make([]vortex.PType, len(schema.FieldNames)),
	}
	for i := range wr.dictValuesSegIdx {
		wr.dictValuesSegIdx[i] = -1
	}

	// Initialize zone map trackers if enabled.
	if opts.EnableZoneMaps {
		wr.zoneTrackers = make([]zoneColTracker, len(schema.FieldNames))
	}

	// Write leading magic.
	if _, err := w.Write(vortex.Magic[:]); err != nil {
		return nil, fmt.Errorf("writer: writing magic: %w", err)
	}
	wr.offset = 4

	return wr, nil
}

// WriteChunk writes one batch of column data to the file.
//
// The columns map must contain an entry for every field in the schema.
// Each Array must match its schema DType. All arrays must have the same length.
func (w *Writer) WriteChunk(columns map[string]*vortex.Array) error {
	if w.closed {
		return fmt.Errorf("writer: already closed")
	}

	// Validate all columns present and same length.
	var rowCount int
	for i, name := range w.schema.FieldNames {
		arr, ok := columns[name]
		if !ok {
			return fmt.Errorf("writer: missing column %q", name)
		}
		if i == 0 {
			rowCount = arr.Length
		} else if arr.Length != rowCount {
			return fmt.Errorf("writer: column %q has %d rows, expected %d", name, arr.Length, rowCount)
		}
	}

	// Write each column as a Flat layout segment.
	for i, name := range w.schema.FieldNames {
		arr := columns[name]
		// Extension dtypes serialize their storage representation; the
		// logical extension identity lives only in the file schema.
		colDType := vortex.ResolveStorageDType(w.schema.FieldTypes[i])

		// File-level and per-zone statistics are computed straight from
		// the array's own values, independent of which encoding it
		// eventually gets serialized with — so these run eagerly here
		// regardless of whether this column's segment writing is
		// deferred below.
		w.updateStats(i, arr, colDType)
		if w.zoneTrackers != nil {
			w.zoneTrackers[i].addZone(arr, colDType)
		}

		// Extension-typed columns are excluded even when their storage
		// resolves to Utf8/Binary: canonicalizeArray/serializeArray's
		// normal immediate path has its own extension-specific
		// handling (see "Extension DType write support" in vortex-go's
		// history) that finalizeStringColumns doesn't replicate — the
		// deferred dict/flat decision here is only for genuinely plain
		// string columns.
		isExtension := w.schema.FieldTypes[i].Kind == vortex.DTypeExtension
		if w.opts.EnableDict && !isExtension && (colDType.Kind == vortex.DTypeUtf8 || colDType.Kind == vortex.DTypeBinary) {
			// Defer: buffer this chunk's array. The dictionary-encoding
			// decision and all segment writing for this column happen
			// once, extent-wide, in finalizeStringColumns (called from
			// Close) — see its doc comment for why per-chunk decisions
			// silently miss cross-chunk-but-still-low cardinality.
			w.pendingStringChunks[i] = append(w.pendingStringChunks[i], arr)
			continue
		}

		segData, alignExp, err := w.serializeArray(arr, colDType)
		if err != nil {
			return fmt.Errorf("writer: serializing column %q: %w", name, err)
		}

		segIdx, err := w.writeAlignedSegment(segData, alignExp)
		if err != nil {
			return fmt.Errorf("writer: writing segment for %q: %w", name, err)
		}

		w.colChunks[i] = append(w.colChunks[i], colChunk{
			segmentIdx: segIdx,
			rowCount:   uint64(arr.Length),
		})
	}

	return nil
}

// writeAlignedSegment pads the current write offset up to segData's
// required alignment, writes segData, records its SegmentSpec, and
// returns the new segment's index. Factored out of WriteChunk's inline
// version (behavior identical) so finalizeStringColumns can reuse the
// exact same offset/alignment bookkeeping when writing dict values and
// per-chunk codes segments outside the normal WriteChunk flow.
func (w *Writer) writeAlignedSegment(segData []byte, alignExp uint8) (uint32, error) {
	segAlign := 1 << int(alignExp)
	if pad := (segAlign - int(w.offset%uint64(segAlign))) % segAlign; pad > 0 {
		zeros := make([]byte, pad)
		if _, err := w.w.Write(zeros); err != nil {
			return 0, fmt.Errorf("aligning segment: %w", err)
		}
		w.offset += uint64(pad)
	}

	segIdx := uint32(len(w.segSpecs))
	w.segSpecs = append(w.segSpecs, vortex.SegmentSpec{
		Offset:            w.offset,
		Length:            uint32(len(segData)),
		AlignmentExponent: alignExp,
	})

	if _, err := w.w.Write(segData); err != nil {
		return 0, fmt.Errorf("writing segment: %w", err)
	}
	w.offset += uint64(len(segData))

	return segIdx, nil
}

// Close finalizes the file by writing the metadata footer.
//
// The file structure after Close:
//
//	[magic "VTXF"] [data segments...] [dtype FB] [layout FB] [stats FB] [footer FB] [postscript FB] [version u16] [ps_len u16] [magic "VTXF"]
func (w *Writer) Close() error {
	if w.closed {
		return fmt.Errorf("writer: already closed")
	}
	w.closed = true

	// Decide encoding and write segments for any string columns whose
	// serialization was deferred by WriteChunk — must happen before
	// layout-spec registration below, since that needs to know whether
	// any column ended up dictionary-encoded.
	if err := w.finalizeStringColumns(); err != nil {
		return err
	}

	// Build the layout specs list.
	layoutSpecs := []string{vortex.LayoutFlat, vortex.LayoutStruct}
	layoutSpecIdx := map[string]uint16{
		vortex.LayoutFlat:   0,
		vortex.LayoutStruct: 1,
	}

	// Check if we need Chunked layout (multiple chunks per column).
	needChunked := false
	for _, chunks := range w.colChunks {
		if len(chunks) > 1 {
			needChunked = true
			break
		}
	}
	if needChunked {
		layoutSpecIdx[vortex.LayoutChunked] = uint16(len(layoutSpecs))
		layoutSpecs = append(layoutSpecs, vortex.LayoutChunked)
	}

	// Check if we need Dict layout (any column ended up dict-encoded).
	needDict := false
	for _, idx := range w.dictValuesSegIdx {
		if idx >= 0 {
			needDict = true
			break
		}
	}
	if needDict {
		layoutSpecIdx[vortex.LayoutDict] = uint16(len(layoutSpecs))
		layoutSpecs = append(layoutSpecs, vortex.LayoutDict)
	}

	// Write zone map stats table segments (if enabled).
	w.statsSegIdx = make([]int, len(w.schema.FieldNames))
	for i := range w.statsSegIdx {
		w.statsSegIdx[i] = -1
	}
	if w.zoneTrackers != nil {
		needZoned := false
		for i, tracker := range w.zoneTrackers {
			if tracker.hasZones() {
				needZoned = true

				segData, alignExp, err := w.serializeStatsTable(&w.zoneTrackers[i])
				if err != nil {
					return fmt.Errorf("writer: serializing zone stats for column %d: %w", i, err)
				}

				// Align segment.
				segAlign := 1 << int(alignExp)
				if pad := (segAlign - int(w.offset%uint64(segAlign))) % segAlign; pad > 0 {
					zeros := make([]byte, pad)
					if _, err := w.w.Write(zeros); err != nil {
						return fmt.Errorf("writer: aligning stats segment: %w", err)
					}
					w.offset += uint64(pad)
				}

				segIdx := len(w.segSpecs)
				w.segSpecs = append(w.segSpecs, vortex.SegmentSpec{
					Offset:            w.offset,
					Length:            uint32(len(segData)),
					AlignmentExponent: alignExp,
				})

				if _, err := w.w.Write(segData); err != nil {
					return fmt.Errorf("writer: writing stats segment: %w", err)
				}
				w.offset += uint64(len(segData))

				w.statsSegIdx[i] = segIdx
			}
		}
		if needZoned {
			layoutSpecIdx[vortex.LayoutZoned] = uint16(len(layoutSpecs))
			layoutSpecs = append(layoutSpecs, vortex.LayoutZoned)
		}
	}

	// 1. Write DType FlatBuffer.
	dtypeBytes := buildDTypeFB(w.schema)
	dtypeOffset := w.offset
	if _, err := w.w.Write(dtypeBytes); err != nil {
		return fmt.Errorf("writer: writing dtype: %w", err)
	}
	w.offset += uint64(len(dtypeBytes))

	// 2. Build and write Layout FlatBuffer.
	totalRows := uint64(0)
	if len(w.colChunks) > 0 {
		for _, c := range w.colChunks[0] {
			totalRows += c.rowCount
		}
	}

	layoutBytes := w.buildLayoutFB(layoutSpecIdx, totalRows)
	layoutOffset := w.offset
	if _, err := w.w.Write(layoutBytes); err != nil {
		return fmt.Errorf("writer: writing layout: %w", err)
	}
	w.offset += uint64(len(layoutBytes))

	// 3. Write Statistics FlatBuffer.
	statsBytes := w.buildStatisticsFB()
	var statsOffset uint64
	hasStats := len(statsBytes) > 0
	if hasStats {
		statsOffset = w.offset
		if _, err := w.w.Write(statsBytes); err != nil {
			return fmt.Errorf("writer: writing statistics: %w", err)
		}
		w.offset += uint64(len(statsBytes))
	}

	// 4. Write Footer FlatBuffer.
	footerBytes := buildFooterFB(w.arrayIDs, layoutSpecs, w.segSpecs)
	footerOffset := w.offset
	if _, err := w.w.Write(footerBytes); err != nil {
		return fmt.Errorf("writer: writing footer: %w", err)
	}
	w.offset += uint64(len(footerBytes))

	// 5. Write Postscript FlatBuffer.
	dtypeSeg := &psSegInfo{dtypeOffset, uint32(len(dtypeBytes))}
	layoutSeg := &psSegInfo{layoutOffset, uint32(len(layoutBytes))}
	footerSegI := &psSegInfo{footerOffset, uint32(len(footerBytes))}

	var statsSeg *psSegInfo
	if hasStats {
		statsSeg = &psSegInfo{statsOffset, uint32(len(statsBytes))}
	}

	psBytes := buildPostscriptFB(dtypeSeg, layoutSeg, statsSeg, footerSegI)
	if _, err := w.w.Write(psBytes); err != nil {
		return fmt.Errorf("writer: writing postscript: %w", err)
	}

	// 6. Write the 8-byte EOF trailer.
	var trailer [vortex.EndOfFileSize]byte
	binary.LittleEndian.PutUint16(trailer[0:2], w.opts.Version)
	binary.LittleEndian.PutUint16(trailer[2:4], uint16(len(psBytes)))
	copy(trailer[4:8], vortex.Magic[:])
	if _, err := w.w.Write(trailer[:]); err != nil {
		return fmt.Errorf("writer: writing trailer: %w", err)
	}

	return nil
}

// registerArraySpec returns the index for an encoding ID, registering it if new.
func (w *Writer) registerArraySpec(id string) uint16 {
	if idx, ok := w.arraySpecs[id]; ok {
		return uint16(idx)
	}
	idx := len(w.arrayIDs)
	w.arraySpecs[id] = idx
	w.arrayIDs = append(w.arrayIDs, id)
	return uint16(idx)
}

// serializeArray converts an Array into a Vortex Flat layout segment.
//
// Segment format: [buffer data...] [Array FlatBuffer] [fb_length: u32 LE]
func (w *Writer) serializeArray(arr *vortex.Array, dtype *vortex.DType) ([]byte, uint8, error) {
	// Build the array tree: determine encoding, extract buffers.
	node, buffers, err := w.canonicalizeArray(arr, dtype)
	if err != nil {
		return nil, 0, err
	}
	return w.serializeNode(node, buffers)
}

// serializeNode assembles an already-canonicalized array node tree (plus
// its buffers) into final segment bytes. Factored out of serializeArray
// so callers that build a node tree directly — without going through
// canonicalizeArray's per-array encoding-choice logic — can still reuse
// the exact same, tested byte-layout assembly. finalizeStringColumns
// uses this to write a values segment and per-chunk codes segments
// independently, since canonicalizeDictString itself only ever builds
// one combined codes+values segment for a single array.
//
// Segment format: [buffer data...] [Array FlatBuffer] [fb_length: u32 LE]
func (w *Writer) serializeNode(node *arrayNodeInfo, buffers [][]byte) ([]byte, uint8, error) {
	// Assemble the segment: [padded buffer data] [FB padding] [Array FlatBuffer] [fb_length: u32 LE]
	// Each buffer is padded to its required alignment for zero-copy reads.
	// VarBinView views require 16-byte alignment; everything else uses 8-byte.
	bufAligns := collectBufferAligns(node)

	// Calculate padded sizes using per-buffer alignment.
	var pos int
	paddings := make([]uint16, len(buffers))
	maxAlign := 8 // minimum segment alignment
	for i, bd := range buffers {
		a := 8 // default alignment
		if i < len(bufAligns) {
			a = 1 << int(bufAligns[i])
		}
		if a > maxAlign {
			maxAlign = a
		}
		pad := (a - (pos % a)) % a
		paddings[i] = uint16(pad)
		pos += pad + len(bd)
	}
	// Pad before the FlatBuffer too (use max alignment).
	fbPad := (maxAlign - (pos % maxAlign)) % maxAlign
	pos += fbPad

	// Build the Array FlatBuffer with per-buffer padding and alignment info.
	arrayFB := w.buildArrayFB(node, buffers, paddings, bufAligns)

	segSize := pos + len(arrayFB) + 4
	seg := make([]byte, 0, segSize)

	// Write padded buffer data.
	zeros := make([]byte, maxAlign)
	for i, bd := range buffers {
		if paddings[i] > 0 {
			seg = append(seg, zeros[:paddings[i]]...)
		}
		seg = append(seg, bd...)
	}
	// FlatBuffer alignment padding.
	if fbPad > 0 {
		seg = append(seg, zeros[:fbPad]...)
	}

	// Write Array FlatBuffer.
	seg = append(seg, arrayFB...)

	// Write FlatBuffer length.
	fbLen := make([]byte, 4)
	binary.LittleEndian.PutUint32(fbLen, uint32(len(arrayFB)))
	seg = append(seg, fbLen...)

	// Compute alignment exponent (e.g., 8→3, 16→4).
	alignExp := uint8(bits.TrailingZeros(uint(maxAlign)))

	return seg, alignExp, nil
}

// arrayNodeInfo captures the encoding tree for building the Array FlatBuffer.
type arrayNodeInfo struct {
	encodingIdx   uint16
	metadata      []byte
	children      []*arrayNodeInfo
	bufferLengths []uint32 // length of each buffer this node owns
	bufferAligns  []uint8  // alignment exponent per buffer (0=1, 1=2, 2=4, 3=8)
}

// canonicalizeArray determines the canonical encoding for an array and
// extracts its buffers. Returns the node tree and ordered buffer slices.
func (w *Writer) canonicalizeArray(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
	switch dtype.Kind {
	case vortex.DTypePrimitive:
		// Try FoR + BitPacking first (integers only, narrow range).
		if w.opts.EnableBitPacking {
			if node, bufs, err := w.canonicalizeFoRBitPacked(arr, dtype); err != nil {
				return nil, nil, err
			} else if node != nil {
				return node, bufs, nil
			}
		}
		// Try Pcodec (integers and floats).
		if w.opts.EnablePcodec {
			if node, bufs, err := w.canonicalizePco(arr, dtype); err != nil {
				return nil, nil, err
			} else if node != nil {
				return node, bufs, nil
			}
		}
		return w.canonicalizePrimitive(arr, dtype)
	case vortex.DTypeBool:
		return w.canonicalizeBool(arr)
	case vortex.DTypeUtf8, vortex.DTypeBinary:
		// Try dictionary encoding first.
		if w.opts.EnableDict {
			if node, bufs, err := w.canonicalizeDictString(arr, dtype); err != nil {
				return nil, nil, err
			} else if node != nil {
				return node, bufs, nil
			}
		}
		return w.canonicalizeVarBinView(arr, dtype)
	case vortex.DTypeNull:
		return w.canonicalizeNull(arr)
	default:
		return nil, nil, fmt.Errorf("unsupported dtype for writing: %v", dtype.Kind)
	}
}

func (w *Writer) canonicalizePrimitive(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
	encIdx := w.registerArraySpec("vortex.primitive")

	if len(arr.Buffers) == 0 {
		// Generate a zero buffer.
		elemSize := vortex.PTypeSize(dtype.PType)
		buf := make([]byte, arr.Length*elemSize)
		return &arrayNodeInfo{
			encodingIdx:   encIdx,
			bufferLengths: []uint32{uint32(len(buf))},
		}, [][]byte{buf}, nil
	}

	return &arrayNodeInfo{
		encodingIdx:   encIdx,
		bufferLengths: []uint32{uint32(len(arr.Buffers[0]))},
	}, arr.Buffers[:1], nil
}

func (w *Writer) canonicalizeBool(arr *vortex.Array) (*arrayNodeInfo, [][]byte, error) {
	encIdx := w.registerArraySpec("vortex.bool")

	if len(arr.Buffers) == 0 {
		// Generate a zero bit-vector.
		bufLen := (arr.Length + 7) / 8
		buf := make([]byte, bufLen)
		return &arrayNodeInfo{
			encodingIdx:   encIdx,
			bufferLengths: []uint32{uint32(bufLen)},
		}, [][]byte{buf}, nil
	}

	return &arrayNodeInfo{
		encodingIdx:   encIdx,
		bufferLengths: []uint32{uint32(len(arr.Buffers[0]))},
	}, arr.Buffers[:1], nil
}

func (w *Writer) canonicalizeVarBinView(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
	encIdx := w.registerArraySpec("vortex.varbinview")

	if len(arr.Buffers) == 0 {
		// Empty string array: N views all pointing to zero-length strings.
		viewsBuf := make([]byte, arr.Length*16)
		return &arrayNodeInfo{
			encodingIdx:   encIdx,
			bufferLengths: []uint32{uint32(len(viewsBuf))},
			bufferAligns:  []uint8{4}, // BinaryView requires 16-byte (2^4) alignment
		}, [][]byte{viewsBuf}, nil
	}

	// VarBinView: buffer[0] = views, buffer[1..N] = data buffers.
	lengths := make([]uint32, len(arr.Buffers))
	aligns := make([]uint8, len(arr.Buffers))
	for i, buf := range arr.Buffers {
		lengths[i] = uint32(len(buf))
		if i == 0 {
			aligns[i] = 4 // views buffer: BinaryView requires 16-byte (2^4) alignment
		} else {
			aligns[i] = 3 // data buffers: 8-byte alignment
		}
	}

	return &arrayNodeInfo{
		encodingIdx:   encIdx,
		bufferLengths: lengths,
		bufferAligns:  aligns,
	}, arr.Buffers, nil
}

func (w *Writer) canonicalizeNull(arr *vortex.Array) (*arrayNodeInfo, [][]byte, error) {
	encIdx := w.registerArraySpec("vortex.null")
	return &arrayNodeInfo{
		encodingIdx: encIdx,
	}, nil, nil
}

// buildArrayFB builds the Array FlatBuffer from a node tree and buffers.
func (w *Writer) buildArrayFB(root *arrayNodeInfo, buffers [][]byte, paddings []uint16, bufAligns []uint8) []byte {
	b := flatbuffers.NewBuilder(256)

	// Build Buffer descriptors. One per buffer, sequentially.
	bufIdx := 0
	totalBuffers := countBuffers(root)
	vortexfb.ArrayStartBuffersVector(b, totalBuffers)
	// FlatBuffers are built back-to-front, so build in reverse.
	lengths := collectBufferLengths(root)
	for i := len(lengths) - 1; i >= 0; i-- {
		pad := uint16(0)
		if i < len(paddings) {
			pad = paddings[i]
		}
		ae := uint8(3) // default: 2^3 = 8 bytes
		if i < len(bufAligns) {
			ae = bufAligns[i]
		}
		vortexfb.CreateBuffer(b, pad, ae, vortexfb.CompressionNone, lengths[i])
	}
	buffersVec := b.EndVector(totalBuffers)

	// Build the ArrayNode tree.
	rootOff := buildArrayNodeFB(b, root, &bufIdx)

	// Build the Array table.
	vortexfb.ArrayStart(b)
	vortexfb.ArrayAddRoot(b, rootOff)
	vortexfb.ArrayAddBuffers(b, buffersVec)
	arrayOff := vortexfb.ArrayEnd(b)

	b.Finish(arrayOff)
	return b.FinishedBytes()
}

func buildArrayNodeFB(b *flatbuffers.Builder, node *arrayNodeInfo, bufIdx *int) flatbuffers.UOffsetT {
	// Build children first (FlatBuffers are back-to-front).
	var childOffsets []flatbuffers.UOffsetT
	for _, child := range node.children {
		childOffsets = append(childOffsets, buildArrayNodeFB(b, child, bufIdx))
	}

	// Build children vector.
	var childrenVec flatbuffers.UOffsetT
	if len(childOffsets) > 0 {
		vortexfb.ArrayNodeStartChildrenVector(b, len(childOffsets))
		for i := len(childOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(childOffsets[i])
		}
		childrenVec = b.EndVector(len(childOffsets))
	}

	// Build buffer indices vector.
	var bufsVec flatbuffers.UOffsetT
	numBufs := len(node.bufferLengths)
	if numBufs > 0 {
		vortexfb.ArrayNodeStartBuffersVector(b, numBufs)
		for i := numBufs - 1; i >= 0; i-- {
			b.PrependUint16(uint16(*bufIdx + i))
		}
		bufsVec = b.EndVector(numBufs)
		*bufIdx += numBufs
	}

	// Build metadata vector.
	var metaVec flatbuffers.UOffsetT
	if len(node.metadata) > 0 {
		metaVec = b.CreateByteVector(node.metadata)
	}

	// Build the node.
	vortexfb.ArrayNodeStart(b)
	vortexfb.ArrayNodeAddEncoding(b, node.encodingIdx)
	if len(node.metadata) > 0 {
		vortexfb.ArrayNodeAddMetadata(b, metaVec)
	}
	if len(childOffsets) > 0 {
		vortexfb.ArrayNodeAddChildren(b, childrenVec)
	}
	if numBufs > 0 {
		vortexfb.ArrayNodeAddBuffers(b, bufsVec)
	}
	return vortexfb.ArrayNodeEnd(b)
}

func countBuffers(node *arrayNodeInfo) int {
	n := len(node.bufferLengths)
	for _, child := range node.children {
		n += countBuffers(child)
	}
	return n
}

func collectBufferLengths(node *arrayNodeInfo) []uint32 {
	var lengths []uint32
	lengths = append(lengths, node.bufferLengths...)
	for _, child := range node.children {
		lengths = append(lengths, collectBufferLengths(child)...)
	}
	return lengths
}

// collectBufferAligns collects alignment exponents from the node tree.
// Defaults to 3 (8-byte alignment) for buffers without explicit alignment.
func collectBufferAligns(node *arrayNodeInfo) []uint8 {
	var aligns []uint8
	for i := range node.bufferLengths {
		if i < len(node.bufferAligns) {
			aligns = append(aligns, node.bufferAligns[i])
		} else {
			aligns = append(aligns, 3) // default: 8-byte
		}
	}
	for _, child := range node.children {
		aligns = append(aligns, collectBufferAligns(child)...)
	}
	return aligns
}

// buildLayoutFB builds the root Layout FlatBuffer.
func (w *Writer) buildLayoutFB(layoutSpecIdx map[string]uint16, totalRows uint64) []byte {
	b := flatbuffers.NewBuilder(512)

	// Build per-column child layouts.
	colOffsets := make([]flatbuffers.UOffsetT, len(w.schema.FieldNames))
	for i := len(w.schema.FieldNames) - 1; i >= 0; i-- {
		chunks := w.colChunks[i]

		var dataLayout flatbuffers.UOffsetT
		var colRows uint64

		if len(chunks) == 1 {
			// Single chunk → single Flat layout.
			dataLayout = buildFlatLayoutFB(b, layoutSpecIdx[vortex.LayoutFlat], chunks[0])
			colRows = chunks[0].rowCount
		} else {
			// Multiple chunks → Chunked layout wrapping Flat children.
			childOffsets := make([]flatbuffers.UOffsetT, len(chunks))
			for j := len(chunks) - 1; j >= 0; j-- {
				childOffsets[j] = buildFlatLayoutFB(b, layoutSpecIdx[vortex.LayoutFlat], chunks[j])
			}

			colRows = 0
			for _, c := range chunks {
				colRows += c.rowCount
			}

			vortexfb.LayoutStartChildrenVector(b, len(childOffsets))
			for j := len(childOffsets) - 1; j >= 0; j-- {
				b.PrependUOffsetT(childOffsets[j])
			}
			chunkedChildrenVec := b.EndVector(len(childOffsets))

			vortexfb.LayoutStart(b)
			vortexfb.LayoutAddEncoding(b, layoutSpecIdx[vortex.LayoutChunked])
			vortexfb.LayoutAddRowCount(b, colRows)
			vortexfb.LayoutAddChildren(b, chunkedChildrenVec)
			dataLayout = vortexfb.LayoutEnd(b)
		}

		// Wrap with Dict layout if this column was dictionary-encoded
		// (see finalizeStringColumns). dataLayout above is currently
		// the CODES layout (Flat or Chunked, built identically to any
		// other column's data layout — colChunks[i] holds the codes
		// segments here, not raw string segments). We wrap it with a
		// sibling Flat layout for the shared dictionary values, under
		// a new Dict-encoded layout node — exactly the shape
		// NavigateToData expects: children[0]=values, children[1]=codes.
		if w.dictValuesSegIdx[i] >= 0 {
			valuesLayout := buildFlatLayoutFB(b, layoutSpecIdx[vortex.LayoutFlat], colChunk{
				segmentIdx: uint32(w.dictValuesSegIdx[i]),
				rowCount:   uint64(w.dictValuesCount[i]),
			})

			// Layout-level Dict metadata is DictLayoutMetadata (just
			// codes_ptype, field 1) — a DIFFERENT message than
			// DictArrayMetadata (values_len + codes_ptype, used at the
			// array-node level by canonicalizeDictString). values_len
			// isn't needed here: the reader already derives dictionary
			// size from the values layout's own RowCount
			// (dictValuesLayout.RowCount, see prepareScanColumn).
			dictMeta := &encodings.DictLayoutMetadata{
				CodesPtype: int32(w.dictCodesPType[i]),
			}
			dictMetaBytes, err := proto.Marshal(dictMeta)
			if err != nil {
				// Metadata is small and always well-formed here; a
				// marshal failure would indicate a real bug, not a
				// recoverable runtime condition.
				panic(fmt.Sprintf("writer: marshaling dict layout metadata: %v", err))
			}
			dictMetaVec := b.CreateByteVector(dictMetaBytes)

			vortexfb.LayoutStartChildrenVector(b, 2)
			b.PrependUOffsetT(dataLayout)   // children[1] = codes
			b.PrependUOffsetT(valuesLayout) // children[0] = values
			dictChildrenVec := b.EndVector(2)

			vortexfb.LayoutStart(b)
			vortexfb.LayoutAddEncoding(b, layoutSpecIdx[vortex.LayoutDict])
			vortexfb.LayoutAddRowCount(b, colRows)
			vortexfb.LayoutAddChildren(b, dictChildrenVec)
			vortexfb.LayoutAddMetadata(b, dictMetaVec)
			dataLayout = vortexfb.LayoutEnd(b)
		}

		// Wrap with Zoned (vortex.stats) layout if zone map stats available.
		if w.statsSegIdx != nil && w.statsSegIdx[i] >= 0 {
			// Determine zone_len from first chunk size.
			zoneLen := uint32(chunks[0].rowCount)

			// Build stats table Flat layout referencing the stats segment.
			numZones := uint64(len(chunks))
			if w.zoneTrackers != nil {
				numZones = uint64(len(w.zoneTrackers[i].zones))
			}
			statsSegIdx := uint32(w.statsSegIdx[i])

			vortexfb.LayoutStartSegmentsVector(b, 1)
			b.PrependUint32(statsSegIdx)
			statsSegsVec := b.EndVector(1)

			vortexfb.LayoutStart(b)
			vortexfb.LayoutAddEncoding(b, layoutSpecIdx[vortex.LayoutFlat])
			vortexfb.LayoutAddRowCount(b, numZones)
			vortexfb.LayoutAddSegments(b, statsSegsVec)
			statsLayout := vortexfb.LayoutEnd(b)

			// Build Zoned metadata: zone_len + stat bitset.
			zonedMeta := buildZonedMeta(zoneLen)
			metaVec := b.CreateByteVector(zonedMeta)

			// Zoned layout: child[0] = data, child[1] = stats table.
			vortexfb.LayoutStartChildrenVector(b, 2)
			b.PrependUOffsetT(statsLayout)
			b.PrependUOffsetT(dataLayout)
			zonedChildrenVec := b.EndVector(2)

			vortexfb.LayoutStart(b)
			vortexfb.LayoutAddEncoding(b, layoutSpecIdx[vortex.LayoutZoned])
			vortexfb.LayoutAddRowCount(b, colRows)
			vortexfb.LayoutAddMetadata(b, metaVec)
			vortexfb.LayoutAddChildren(b, zonedChildrenVec)
			colOffsets[i] = vortexfb.LayoutEnd(b)
		} else {
			colOffsets[i] = dataLayout
		}
	}

	// Build the Struct layout wrapping all columns.
	vortexfb.LayoutStartChildrenVector(b, len(colOffsets))
	for i := len(colOffsets) - 1; i >= 0; i-- {
		b.PrependUOffsetT(colOffsets[i])
	}
	childrenVec := b.EndVector(len(colOffsets))

	vortexfb.LayoutStart(b)
	vortexfb.LayoutAddEncoding(b, layoutSpecIdx[vortex.LayoutStruct])
	vortexfb.LayoutAddRowCount(b, totalRows)
	vortexfb.LayoutAddChildren(b, childrenVec)
	rootOff := vortexfb.LayoutEnd(b)

	b.Finish(rootOff)
	return b.FinishedBytes()
}

func buildFlatLayoutFB(b *flatbuffers.Builder, encIdx uint16, chunk colChunk) flatbuffers.UOffsetT {
	vortexfb.LayoutStartSegmentsVector(b, 1)
	b.PrependUint32(chunk.segmentIdx)
	segsVec := b.EndVector(1)

	vortexfb.LayoutStart(b)
	vortexfb.LayoutAddEncoding(b, encIdx)
	vortexfb.LayoutAddRowCount(b, chunk.rowCount)
	vortexfb.LayoutAddSegments(b, segsVec)
	return vortexfb.LayoutEnd(b)
}

// buildStatisticsFB builds the FileStatistics FlatBuffer.
func (w *Writer) buildStatisticsFB() []byte {
	hasAnyStats := false
	for _, cs := range w.colStats {
		if cs.hasMin || cs.hasMax || cs.nullCount > 0 {
			hasAnyStats = true
			break
		}
	}
	if !hasAnyStats {
		return nil
	}

	b := flatbuffers.NewBuilder(256)

	// Build ArrayStats for each column.
	statOffsets := make([]flatbuffers.UOffsetT, len(w.colStats))
	for i := len(w.colStats) - 1; i >= 0; i-- {
		cs := w.colStats[i]

		var minVec, maxVec flatbuffers.UOffsetT
		if cs.hasMin {
			minVec = b.CreateByteVector(cs.minBytes)
		}
		if cs.hasMax {
			maxVec = b.CreateByteVector(cs.maxBytes)
		}

		vortexfb.ArrayStatsStart(b)
		if cs.hasMin {
			vortexfb.ArrayStatsAddMin(b, minVec)
		}
		if cs.hasMax {
			vortexfb.ArrayStatsAddMax(b, maxVec)
		}
		if cs.nullCount > 0 {
			vortexfb.ArrayStatsAddNullCount(b, cs.nullCount)
		}
		statOffsets[i] = vortexfb.ArrayStatsEnd(b)
	}

	vortexfb.FileStatisticsStartFieldStatsVector(b, len(statOffsets))
	for i := len(statOffsets) - 1; i >= 0; i-- {
		b.PrependUOffsetT(statOffsets[i])
	}
	statsVec := b.EndVector(len(statOffsets))

	vortexfb.FileStatisticsStart(b)
	vortexfb.FileStatisticsAddFieldStats(b, statsVec)
	rootOff := vortexfb.FileStatisticsEnd(b)

	b.Finish(rootOff)
	return b.FinishedBytes()
}

// updateStats updates the running statistics for a column.
func (w *Writer) updateStats(colIdx int, arr *vortex.Array, dtype *vortex.DType) {
	cs := &w.colStats[colIdx]

	switch dtype.Kind {
	case vortex.DTypePrimitive:
		w.updatePrimitiveStats(cs, arr, dtype)
	case vortex.DTypeUtf8:
		w.updateUtf8Stats(cs, arr)
	case vortex.DTypeBool:
		// Could track true/false counts, but min/max not meaningful.
	}
}

func (w *Writer) updatePrimitiveStats(cs *columnStats, arr *vortex.Array, dtype *vortex.DType) {
	if len(arr.Buffers) == 0 || arr.Length == 0 {
		return
	}
	buf := arr.Buffers[0]

	switch dtype.PType {
	case vortex.PTypeI32:
		var minV, maxV int32
		minV = math.MaxInt32
		maxV = math.MinInt32
		for i := 0; i < arr.Length; i++ {
			v := int32(binary.LittleEndian.Uint32(buf[i*4:]))
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		cs.minBytes = encodeScalarInt64(int64(minV))
		cs.maxBytes = encodeScalarInt64(int64(maxV))
		cs.hasMin = true
		cs.hasMax = true

	case vortex.PTypeI64:
		var minV, maxV int64
		minV = math.MaxInt64
		maxV = math.MinInt64
		for i := 0; i < arr.Length; i++ {
			v := int64(binary.LittleEndian.Uint64(buf[i*8:]))
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		cs.minBytes = encodeScalarInt64(minV)
		cs.maxBytes = encodeScalarInt64(maxV)
		cs.hasMin = true
		cs.hasMax = true

	case vortex.PTypeF64:
		var minV, maxV float64
		minV = math.MaxFloat64
		maxV = -math.MaxFloat64
		for i := 0; i < arr.Length; i++ {
			v := math.Float64frombits(binary.LittleEndian.Uint64(buf[i*8:]))
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		cs.minBytes = encodeScalarFloat64(minV)
		cs.maxBytes = encodeScalarFloat64(maxV)
		cs.hasMin = true
		cs.hasMax = true
	}
}

func (w *Writer) updateUtf8Stats(cs *columnStats, arr *vortex.Array) {
	if len(arr.Buffers) == 0 || arr.Length == 0 {
		return
	}
	// Extract strings from VarBinView for min/max.
	viewsBuf := arr.Buffers[0]
	dataBufs := arr.Buffers[1:]

	var minS, maxS string
	first := true
	for i := 0; i < arr.Length; i++ {
		off := i * 16
		strLen := binary.LittleEndian.Uint32(viewsBuf[off:])
		var s string
		if strLen <= 12 {
			s = string(viewsBuf[off+4 : off+4+int(strLen)])
		} else {
			bufIdx := binary.LittleEndian.Uint32(viewsBuf[off+8:])
			bufOff := binary.LittleEndian.Uint32(viewsBuf[off+12:])
			if int(bufIdx) < len(dataBufs) {
				s = string(dataBufs[bufIdx][bufOff : bufOff+strLen])
			}
		}
		if first || s < minS {
			minS = s
		}
		if first || s > maxS {
			maxS = s
		}
		first = false
	}

	if !first {
		cs.minBytes = encodeScalarString(minS)
		cs.maxBytes = encodeScalarString(maxS)
		cs.hasMin = true
		cs.hasMax = true
	}
}

// --- Scalar encoding helpers ---

func encodeScalarInt64(v int64) []byte {
	sv := &scalar.ScalarValue{Kind: &scalar.ScalarValue_Int64Value{Int64Value: v}}
	data, _ := proto.Marshal(sv)
	return data
}

func encodeScalarFloat64(v float64) []byte {
	sv := &scalar.ScalarValue{Kind: &scalar.ScalarValue_F64Value{F64Value: v}}
	data, _ := proto.Marshal(sv)
	return data
}

func encodeScalarString(v string) []byte {
	sv := &scalar.ScalarValue{Kind: &scalar.ScalarValue_StringValue{StringValue: v}}
	data, _ := proto.Marshal(sv)
	return data
}

// --- FlatBuffer helpers ---

func buildDTypeFB(dt *vortex.DType) []byte {
	b := flatbuffers.NewBuilder(256)
	root := buildDTypeNode(b, dt)
	b.Finish(root)
	return b.FinishedBytes()
}

func buildDTypeNode(b *flatbuffers.Builder, dt *vortex.DType) flatbuffers.UOffsetT {
	switch dt.Kind {
	case vortex.DTypeNull:
		vortexfb.NullStart(b)
		nullOff := vortexfb.NullEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeNull)
		vortexfb.DTypeAddType(b, nullOff)
		return vortexfb.DTypeEnd(b)

	case vortex.DTypeBool:
		vortexfb.BoolStart(b)
		vortexfb.BoolAddNullable(b, dt.Nullable)
		boolOff := vortexfb.BoolEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeBool)
		vortexfb.DTypeAddType(b, boolOff)
		return vortexfb.DTypeEnd(b)

	case vortex.DTypePrimitive:
		vortexfb.PrimitiveStart(b)
		vortexfb.PrimitiveAddPtype(b, vortexfb.PType(dt.PType))
		vortexfb.PrimitiveAddNullable(b, dt.Nullable)
		primOff := vortexfb.PrimitiveEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypePrimitive)
		vortexfb.DTypeAddType(b, primOff)
		return vortexfb.DTypeEnd(b)

	case vortex.DTypeUtf8:
		vortexfb.Utf8Start(b)
		vortexfb.Utf8AddNullable(b, dt.Nullable)
		utf8Off := vortexfb.Utf8End(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeUtf8)
		vortexfb.DTypeAddType(b, utf8Off)
		return vortexfb.DTypeEnd(b)

	case vortex.DTypeBinary:
		vortexfb.BinaryStart(b)
		vortexfb.BinaryAddNullable(b, dt.Nullable)
		binOff := vortexfb.BinaryEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeBinary)
		vortexfb.DTypeAddType(b, binOff)
		return vortexfb.DTypeEnd(b)

	case vortex.DTypeExtension:
		// Build storage dtype and id first (tables cannot nest builds).
		storageOff := buildDTypeNode(b, dt.StorageDType)
		idOff := b.CreateString(dt.ExtensionID)
		var metaOff flatbuffers.UOffsetT
		if len(dt.ExtMetadata) > 0 {
			vortexfb.ExtensionStartMetadataVector(b, len(dt.ExtMetadata))
			for i := len(dt.ExtMetadata) - 1; i >= 0; i-- {
				b.PrependByte(dt.ExtMetadata[i])
			}
			metaOff = b.EndVector(len(dt.ExtMetadata))
		}
		vortexfb.ExtensionStart(b)
		vortexfb.ExtensionAddId(b, idOff)
		vortexfb.ExtensionAddStorageDtype(b, storageOff)
		if len(dt.ExtMetadata) > 0 {
			vortexfb.ExtensionAddMetadata(b, metaOff)
		}
		extOff := vortexfb.ExtensionEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeExtension)
		vortexfb.DTypeAddType(b, extOff)
		return vortexfb.DTypeEnd(b)

	case vortex.DTypeStruct:
		// Build child DTypes first.
		childOffsets := make([]flatbuffers.UOffsetT, len(dt.FieldTypes))
		for i := len(dt.FieldTypes) - 1; i >= 0; i-- {
			childOffsets[i] = buildDTypeNode(b, dt.FieldTypes[i])
		}

		// Build name strings.
		nameOffsets := make([]flatbuffers.UOffsetT, len(dt.FieldNames))
		for i := len(dt.FieldNames) - 1; i >= 0; i-- {
			nameOffsets[i] = b.CreateString(dt.FieldNames[i])
		}

		// Names vector.
		vortexfb.Struct_StartNamesVector(b, len(nameOffsets))
		for i := len(nameOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(nameOffsets[i])
		}
		namesVec := b.EndVector(len(nameOffsets))

		// DTypes vector.
		vortexfb.Struct_StartDtypesVector(b, len(childOffsets))
		for i := len(childOffsets) - 1; i >= 0; i-- {
			b.PrependUOffsetT(childOffsets[i])
		}
		dtypesVec := b.EndVector(len(childOffsets))

		vortexfb.Struct_Start(b)
		vortexfb.Struct_AddNames(b, namesVec)
		vortexfb.Struct_AddDtypes(b, dtypesVec)
		vortexfb.Struct_AddNullable(b, dt.Nullable)
		structOff := vortexfb.Struct_End(b)

		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeStruct_)
		vortexfb.DTypeAddType(b, structOff)
		return vortexfb.DTypeEnd(b)

	default:
		// Fallback: Null.
		vortexfb.NullStart(b)
		nullOff := vortexfb.NullEnd(b)
		vortexfb.DTypeStart(b)
		vortexfb.DTypeAddTypeType(b, vortexfb.TypeNull)
		vortexfb.DTypeAddType(b, nullOff)
		return vortexfb.DTypeEnd(b)
	}
}

type psSegInfo struct {
	offset uint64
	length uint32
}

func buildPostscriptFB(dtype, layout *psSegInfo, stats *psSegInfo, footer *psSegInfo) []byte {
	b := flatbuffers.NewBuilder(128)

	// Build PostscriptSegment tables. Must build before the Postscript.
	var dtypeOff, layoutOff, statsOff, footerOff flatbuffers.UOffsetT

	if dtype != nil {
		vortexfb.PostscriptSegmentStart(b)
		vortexfb.PostscriptSegmentAddOffset(b, dtype.offset)
		vortexfb.PostscriptSegmentAddLength(b, dtype.length)
		dtypeOff = vortexfb.PostscriptSegmentEnd(b)
	}

	vortexfb.PostscriptSegmentStart(b)
	vortexfb.PostscriptSegmentAddOffset(b, layout.offset)
	vortexfb.PostscriptSegmentAddLength(b, layout.length)
	layoutOff = vortexfb.PostscriptSegmentEnd(b)

	if stats != nil {
		vortexfb.PostscriptSegmentStart(b)
		vortexfb.PostscriptSegmentAddOffset(b, stats.offset)
		vortexfb.PostscriptSegmentAddLength(b, stats.length)
		statsOff = vortexfb.PostscriptSegmentEnd(b)
	}

	vortexfb.PostscriptSegmentStart(b)
	vortexfb.PostscriptSegmentAddOffset(b, footer.offset)
	vortexfb.PostscriptSegmentAddLength(b, footer.length)
	footerOff = vortexfb.PostscriptSegmentEnd(b)

	vortexfb.PostscriptStart(b)
	if dtype != nil {
		vortexfb.PostscriptAddDtype(b, dtypeOff)
	}
	vortexfb.PostscriptAddLayout(b, layoutOff)
	if stats != nil {
		vortexfb.PostscriptAddStatistics(b, statsOff)
	}
	vortexfb.PostscriptAddFooter(b, footerOff)
	psOff := vortexfb.PostscriptEnd(b)

	b.Finish(psOff)
	return b.FinishedBytes()
}

func buildFooterFB(arraySpecs, layoutSpecs []string, segSpecs []vortex.SegmentSpec) []byte {
	b := flatbuffers.NewBuilder(256)

	// ArraySpec entries.
	aspecOffs := make([]flatbuffers.UOffsetT, len(arraySpecs))
	for i := len(arraySpecs) - 1; i >= 0; i-- {
		idOff := b.CreateString(arraySpecs[i])
		vortexfb.ArraySpecStart(b)
		vortexfb.ArraySpecAddId(b, idOff)
		aspecOffs[i] = vortexfb.ArraySpecEnd(b)
	}
	vortexfb.FooterStartArraySpecsVector(b, len(aspecOffs))
	for i := len(aspecOffs) - 1; i >= 0; i-- {
		b.PrependUOffsetT(aspecOffs[i])
	}
	aVec := b.EndVector(len(aspecOffs))

	// LayoutSpec entries.
	lspecOffs := make([]flatbuffers.UOffsetT, len(layoutSpecs))
	for i := len(layoutSpecs) - 1; i >= 0; i-- {
		idOff := b.CreateString(layoutSpecs[i])
		vortexfb.LayoutSpecStart(b)
		vortexfb.LayoutSpecAddId(b, idOff)
		lspecOffs[i] = vortexfb.LayoutSpecEnd(b)
	}
	vortexfb.FooterStartLayoutSpecsVector(b, len(lspecOffs))
	for i := len(lspecOffs) - 1; i >= 0; i-- {
		b.PrependUOffsetT(lspecOffs[i])
	}
	lVec := b.EndVector(len(lspecOffs))

	// SegmentSpec structs (packed, inline).
	vortexfb.FooterStartSegmentSpecsVector(b, len(segSpecs))
	for i := len(segSpecs) - 1; i >= 0; i-- {
		s := segSpecs[i]
		vortexfb.CreateSegmentSpec(b, s.Offset, s.Length, s.AlignmentExponent, 0, 0)
	}
	sVec := b.EndVector(len(segSpecs))

	vortexfb.FooterStart(b)
	vortexfb.FooterAddArraySpecs(b, aVec)
	vortexfb.FooterAddLayoutSpecs(b, lVec)
	vortexfb.FooterAddSegmentSpecs(b, sVec)
	footerOff := vortexfb.FooterEnd(b)

	b.Finish(footerOff)
	return b.FinishedBytes()
}

// finalizeStringColumns decides dictionary encoding for each buffered
// string column using cardinality computed across the WHOLE extent
// (every WriteChunk call combined), then writes the resulting
// segments. Called once from Close, before the layout tree is built.
//
// This exists because canonicalizeDictString's cardinality check, when
// run once per WriteChunk call (as every other array type's encoding
// decision is), only ever sees one 8192-row zone at a time. A column
// can have low cardinality across a whole extent while still showing
// >50% distinct values within any single 8192-row window — e.g. 5000
// distinct values cycling through 49152 rows: each individual zone
// sees close to its own 5000 distinct values out of 8192 rows (~61%,
// failing the <0.5 threshold), even though the true, whole-extent
// ratio is ~10%. The column silently falls back to flat encoding
// forever, paying for both worse compression AND — since every row
// then needs its own independent string materialized at read time,
// instead of decoding a small shared dictionary once — a much larger
// read-time cost.
//
// If cardinality qualifies, this builds ONE shared dictionary for the
// whole column and writes it as a single values segment, then writes
// one codes segment per original chunk (preserving the existing
// 8192-row zone boundaries for zone-map purposes) referencing that
// shared dictionary — exactly the layout shape NavigateToData already
// expects (Dict layout: children[0]=values, children[1]=codes), so no
// reader changes are needed; this is a write-time-only fix.
func (w *Writer) finalizeStringColumns() error {
	for i, chunks := range w.pendingStringChunks {
		if len(chunks) == 0 {
			continue
		}
		colDType := vortex.ResolveStorageDType(w.schema.FieldTypes[i])

		chunkStrings := make([][]string, len(chunks))
		total := 0
		for ci, arr := range chunks {
			strs, err := extractStringsFromArray(arr)
			if err != nil {
				return fmt.Errorf("writer: extracting strings for column %d chunk %d: %w", i, ci, err)
			}
			chunkStrings[ci] = strs
			total += len(strs)
		}

		unique := make(map[string]struct{})
		for _, strs := range chunkStrings {
			for _, s := range strs {
				unique[s] = struct{}{}
			}
		}
		ratio := 1.0
		if total > 0 {
			ratio = float64(len(unique)) / float64(total)
		}

		if w.opts.EnableDict && total > 0 && ratio < 0.5 {
			if err := w.writeSharedDictColumn(i, chunkStrings); err != nil {
				return fmt.Errorf("writer: dict-encoding column %d: %w", i, err)
			}
			continue
		}

		// Fall back to flat encoding, one segment per original chunk —
		// exactly what WriteChunk would have done immediately before
		// this deferral existed.
		for ci, arr := range chunks {
			segData, alignExp, err := w.serializeArray(arr, colDType)
			if err != nil {
				return fmt.Errorf("writer: serializing column %d chunk %d: %w", i, ci, err)
			}
			segIdx, err := w.writeAlignedSegment(segData, alignExp)
			if err != nil {
				return fmt.Errorf("writer: writing column %d chunk %d: %w", i, ci, err)
			}
			w.colChunks[i] = append(w.colChunks[i], colChunk{
				segmentIdx: segIdx,
				rowCount:   uint64(arr.Length),
			})
		}
	}
	return nil
}

// writeSharedDictColumn builds one shared, sorted dictionary across all
// of a column's chunks, writes it as a single values segment, and
// writes one codes segment per original chunk referencing it. Reuses
// buildDictionary/encodeCodesBuf/encodeVarBinView — the exact same
// helpers canonicalizeDictString uses for its single-array, combined
// codes+values segment — just applied across the concatenation of all
// chunks and split back into independent segments so the codes stay
// aligned with the existing per-chunk zone boundaries.
func (w *Writer) writeSharedDictColumn(i int, chunkStrings [][]string) error {
	var allStrings []string
	for _, strs := range chunkStrings {
		allStrings = append(allStrings, strs...)
	}
	dict, allCodes := buildDictionary(allStrings)

	var codesPType vortex.PType
	switch {
	case len(dict) <= 256:
		codesPType = vortex.PTypeU8
	case len(dict) <= 65536:
		codesPType = vortex.PTypeU16
	default:
		codesPType = vortex.PTypeU32
	}

	// --- Write the shared values segment, once. ---
	viewsBuf, dataBufs := encodeVarBinView(dict)
	vbvEncIdx := w.registerArraySpec("vortex.varbinview")

	vbvLengths := make([]uint32, 1+len(dataBufs))
	vbvLengths[0] = uint32(len(viewsBuf))
	for j, db := range dataBufs {
		vbvLengths[j+1] = uint32(len(db))
	}
	vbvAligns := make([]uint8, len(vbvLengths))
	vbvAligns[0] = 4 // views buffer: 16-byte alignment
	for j := 1; j < len(vbvAligns); j++ {
		vbvAligns[j] = 3 // data buffers: 8-byte alignment
	}
	valuesNode := &arrayNodeInfo{
		encodingIdx:   vbvEncIdx,
		bufferLengths: vbvLengths,
		bufferAligns:  vbvAligns,
	}
	valuesBuffers := append([][]byte{viewsBuf}, dataBufs...)

	valuesSegData, valuesAlignExp, err := w.serializeNode(valuesNode, valuesBuffers)
	if err != nil {
		return fmt.Errorf("serializing shared dict values: %w", err)
	}
	valuesSegIdx, err := w.writeAlignedSegment(valuesSegData, valuesAlignExp)
	if err != nil {
		return fmt.Errorf("writing shared dict values: %w", err)
	}
	w.dictValuesSegIdx[i] = int(valuesSegIdx)
	w.dictValuesCount[i] = len(dict)
	w.dictCodesPType[i] = codesPType

	primEncIdx := w.registerArraySpec("vortex.primitive")

	// --- Write one codes segment per original chunk. ---
	offset := 0
	for _, strs := range chunkStrings {
		n := len(strs)
		chunkCodes := allCodes[offset : offset+n]
		offset += n

		codesBuf := encodeCodesBuf(chunkCodes, codesPType)
		codesNode := &arrayNodeInfo{
			encodingIdx:   primEncIdx,
			bufferLengths: []uint32{uint32(len(codesBuf))},
		}
		codesSegData, codesAlignExp, err := w.serializeNode(codesNode, [][]byte{codesBuf})
		if err != nil {
			return fmt.Errorf("serializing dict codes chunk: %w", err)
		}
		codesSegIdx, err := w.writeAlignedSegment(codesSegData, codesAlignExp)
		if err != nil {
			return fmt.Errorf("writing dict codes chunk: %w", err)
		}
		w.colChunks[i] = append(w.colChunks[i], colChunk{
			segmentIdx: codesSegIdx,
			rowCount:   uint64(n),
		})
	}
	return nil
}
