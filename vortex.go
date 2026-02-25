// Package vortex provides a native Go implementation of the Vortex columnar file format.
//
// Vortex is a next-generation columnar file format (Linux Foundation project) designed
// for high-performance analytical data processing. This package reads and writes Vortex
// files from pure Go.
//
// Basic usage:
//
//	// Open a Vortex file
//	f, err := vortex.Open(readerAt, fileSize)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Inspect schema
//	fmt.Println(f.DType())
//
//	// Scan with column projection
//	iter, err := f.Scan(vortex.ScanOptions{
//	    Columns: []string{"id", "name"},
//	})
package vortex

import (
	"encoding/binary"
	"fmt"
	"io"

	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/LaurieRhodes/vortex-go/internal/flatbuf/vortexfb"
)

// Magic bytes at the start and end of every Vortex file.
var Magic = [4]byte{'V', 'T', 'X', 'F'}

// EndOfFileSize is the 8-byte trailer: version(u16) + postscript_length(u16) + magic(4).
const EndOfFileSize = 8

// maxInitialRead is the maximum initial read from the end of the file.
// The postscript is guaranteed to fit within u16::MAX bytes.
const maxInitialRead = 65536

// PostscriptSegment describes the location and properties of a metadata segment
// as stored in the Postscript.
type PostscriptSegment struct {
	Offset            uint64
	Length            uint32
	AlignmentExponent uint8
}

// File represents an opened Vortex file.
type File struct {
	r    io.ReaderAt
	size int64

	version          uint16
	postscriptLength uint16

	// Segment locations extracted from the Postscript.
	dtypeSeg  *PostscriptSegment
	layoutSeg *PostscriptSegment
	statsSeg  *PostscriptSegment
	footerSeg *PostscriptSegment

	// Parsed structures.
	footer *Footer
	dtype  *DType
	layout *Layout
	stats  *FileStatistics
}

// Open reads the footer of a Vortex file and returns a File handle for further reads.
func Open(r io.ReaderAt, size int64) (*File, error) {
	if size < EndOfFileSize {
		return nil, fmt.Errorf("vortex: file too small (%d bytes)", size)
	}

	// Read up to 64KB from the end of the file — guaranteed to capture
	// the trailer + the entire postscript.
	readSize := int64(maxInitialRead)
	if readSize > size {
		readSize = size
	}

	tailBuf := make([]byte, readSize)
	if _, err := r.ReadAt(tailBuf, size-readSize); err != nil {
		return nil, fmt.Errorf("vortex: reading tail: %w", err)
	}

	// Parse the 8-byte trailer at the very end.
	trailer := tailBuf[len(tailBuf)-EndOfFileSize:]
	var magic [4]byte
	copy(magic[:], trailer[4:8])
	if magic != Magic {
		return nil, fmt.Errorf("vortex: invalid trailing magic: got %q, want %q", magic, Magic)
	}

	version := binary.LittleEndian.Uint16(trailer[0:2])
	postscriptLen := binary.LittleEndian.Uint16(trailer[2:4])

	f := &File{
		r:                r,
		size:             size,
		version:          version,
		postscriptLength: postscriptLen,
	}

	// Validate leading magic.
	leadMagic := make([]byte, 4)
	if _, err := r.ReadAt(leadMagic, 0); err != nil {
		return nil, fmt.Errorf("vortex: reading leading magic: %w", err)
	}
	var lm [4]byte
	copy(lm[:], leadMagic)
	if lm != Magic {
		return nil, fmt.Errorf("vortex: invalid leading magic: got %q, want %q", lm, Magic)
	}

	// Extract the postscript bytes from the tail buffer.
	psEnd := len(tailBuf) - EndOfFileSize
	psStart := psEnd - int(postscriptLen)
	if psStart < 0 {
		return nil, fmt.Errorf("vortex: postscript length %d exceeds read buffer", postscriptLen)
	}
	psBytes := tailBuf[psStart:psEnd]

	// Parse the FlatBuffer Postscript.
	if err := f.parsePostscript(psBytes); err != nil {
		return nil, fmt.Errorf("vortex: parsing postscript: %w", err)
	}

	// Parse the Footer (required).
	if f.footerSeg == nil {
		return nil, fmt.Errorf("vortex: postscript missing footer segment")
	}
	if err := f.parseFooter(); err != nil {
		return nil, fmt.Errorf("vortex: parsing footer: %w", err)
	}

	// Parse the DType (optional but expected).
	if f.dtypeSeg != nil {
		if err := f.parseDType(); err != nil {
			return nil, fmt.Errorf("vortex: parsing dtype: %w", err)
		}
	}

	// Parse the Layout (required).
	if f.layoutSeg == nil {
		return nil, fmt.Errorf("vortex: postscript missing layout segment")
	}
	if err := f.parseLayout(); err != nil {
		return nil, fmt.Errorf("vortex: parsing layout: %w", err)
	}

	// Parse Statistics (optional).
	if f.statsSeg != nil {
		if err := f.parseStatistics(); err != nil {
			return nil, fmt.Errorf("vortex: parsing statistics: %w", err)
		}
	}

	return f, nil
}

// Version returns the file format version.
func (f *File) Version() uint16 {
	return f.version
}

// DType returns the logical schema of the file.
func (f *File) DType() *DType {
	return f.dtype
}

// Layout returns the physical layout tree of the file.
func (f *File) Layout() *Layout {
	return f.layout
}

// Footer returns the parsed footer with lookup tables.
func (f *File) Footer() *Footer {
	return f.footer
}

// Statistics returns file-level per-column statistics, or nil if not present.
func (f *File) Statistics() *FileStatistics {
	return f.stats
}

// parsePostscript parses the FlatBuffer Postscript and extracts segment locations.
func (f *File) parsePostscript(data []byte) error {
	ps := vortexfb.GetRootAsPostscript(data, 0)

	// Extract each segment location.
	f.dtypeSeg = extractPostscriptSegment(ps, func(ps *vortexfb.Postscript, obj *vortexfb.PostscriptSegment) *vortexfb.PostscriptSegment {
		return ps.Dtype(obj)
	})
	f.layoutSeg = extractPostscriptSegment(ps, func(ps *vortexfb.Postscript, obj *vortexfb.PostscriptSegment) *vortexfb.PostscriptSegment {
		return ps.Layout(obj)
	})
	f.statsSeg = extractPostscriptSegment(ps, func(ps *vortexfb.Postscript, obj *vortexfb.PostscriptSegment) *vortexfb.PostscriptSegment {
		return ps.Statistics(obj)
	})
	f.footerSeg = extractPostscriptSegment(ps, func(ps *vortexfb.Postscript, obj *vortexfb.PostscriptSegment) *vortexfb.PostscriptSegment {
		return ps.Footer(obj)
	})

	return nil
}

// extractPostscriptSegment reads a PostscriptSegment from the FlatBuffer if present.
func extractPostscriptSegment(
	ps *vortexfb.Postscript,
	getter func(*vortexfb.Postscript, *vortexfb.PostscriptSegment) *vortexfb.PostscriptSegment,
) *PostscriptSegment {
	obj := new(vortexfb.PostscriptSegment)
	result := getter(ps, obj)
	if result == nil {
		return nil
	}
	return &PostscriptSegment{
		Offset:            result.Offset(),
		Length:            result.Length(),
		AlignmentExponent: result.AlignmentExponent(),
	}
}

// readSegment reads a raw segment from the file.
func (f *File) readSegment(seg *PostscriptSegment) ([]byte, error) {
	buf := make([]byte, seg.Length)
	if _, err := f.r.ReadAt(buf, int64(seg.Offset)); err != nil {
		return nil, fmt.Errorf("reading segment at offset %d, length %d: %w", seg.Offset, seg.Length, err)
	}
	return buf, nil
}

// readSegmentSpec reads a raw segment described by a SegmentSpec from the footer.
func (f *File) readSegmentSpec(spec SegmentSpec) ([]byte, error) {
	buf := make([]byte, spec.Length)
	if _, err := f.r.ReadAt(buf, int64(spec.Offset)); err != nil {
		return nil, fmt.Errorf("reading segment at offset %d, length %d: %w", spec.Offset, spec.Length, err)
	}
	return buf, nil
}

// parseFooter reads and parses the Footer FlatBuffer from its segment.
func (f *File) parseFooter() error {
	data, err := f.readSegment(f.footerSeg)
	if err != nil {
		return err
	}

	fb := vortexfb.GetRootAsFooter(data, 0)
	footer := &Footer{}

	// Array specs: encoding string IDs.
	footer.ArraySpecs = make([]string, fb.ArraySpecsLength())
	var aspec vortexfb.ArraySpec
	for i := 0; i < fb.ArraySpecsLength(); i++ {
		if fb.ArraySpecs(&aspec, i) {
			footer.ArraySpecs[i] = string(aspec.Id())
		}
	}

	// Layout specs: layout string IDs.
	footer.LayoutSpecs = make([]string, fb.LayoutSpecsLength())
	var lspec vortexfb.LayoutSpec
	for i := 0; i < fb.LayoutSpecsLength(); i++ {
		if fb.LayoutSpecs(&lspec, i) {
			footer.LayoutSpecs[i] = string(lspec.Id())
		}
	}

	// Segment specs: byte ranges in the file.
	footer.SegmentSpecs = make([]SegmentSpec, fb.SegmentSpecsLength())
	var sspec vortexfb.SegmentSpec
	for i := 0; i < fb.SegmentSpecsLength(); i++ {
		if fb.SegmentSpecs(&sspec, i) {
			footer.SegmentSpecs[i] = SegmentSpec{
				Offset:            sspec.Offset(),
				Length:            sspec.Length(),
				AlignmentExponent: sspec.AlignmentExponent(),
			}
		}
	}

	// Compression specs.
	footer.CompressionSpecs = make([]CompressionScheme, fb.CompressionSpecsLength())
	var cspec vortexfb.CompressionSpec
	for i := 0; i < fb.CompressionSpecsLength(); i++ {
		if fb.CompressionSpecs(&cspec, i) {
			footer.CompressionSpecs[i] = CompressionScheme(cspec.Scheme())
		}
	}

	f.footer = footer
	return nil
}

// parseDType reads and parses the DType FlatBuffer from its segment.
func (f *File) parseDType() error {
	data, err := f.readSegment(f.dtypeSeg)
	if err != nil {
		return err
	}

	fbDtype := vortexfb.GetRootAsDType(data, 0)
	dt, err := parseFBDType(fbDtype)
	if err != nil {
		return err
	}
	f.dtype = dt
	return nil
}

// parseFBDType converts a FlatBuffer DType into our Go DType.
func parseFBDType(fb *vortexfb.DType) (*DType, error) {
	dt := &DType{}

	unionTable := new(flatbuffers.Table)
	if !fb.Type(unionTable) {
		return nil, fmt.Errorf("DType has no type union")
	}

	switch fb.TypeType() {
	case vortexfb.TypeNull:
		dt.Kind = DTypeNull

	case vortexfb.TypeBool:
		dt.Kind = DTypeBool
		var b vortexfb.Bool
		b.Init(unionTable.Bytes, unionTable.Pos)
		dt.Nullable = b.Nullable()

	case vortexfb.TypePrimitive:
		dt.Kind = DTypePrimitive
		var p vortexfb.Primitive
		p.Init(unionTable.Bytes, unionTable.Pos)
		dt.PType = PType(p.Ptype())
		dt.Nullable = p.Nullable()

	case vortexfb.TypeDecimal:
		dt.Kind = DTypeDecimal
		var d vortexfb.Decimal
		d.Init(unionTable.Bytes, unionTable.Pos)
		dt.Precision = d.Precision()
		dt.Scale = d.Scale()
		dt.Nullable = d.Nullable()

	case vortexfb.TypeUtf8:
		dt.Kind = DTypeUtf8
		var u vortexfb.Utf8
		u.Init(unionTable.Bytes, unionTable.Pos)
		dt.Nullable = u.Nullable()

	case vortexfb.TypeBinary:
		dt.Kind = DTypeBinary
		var b vortexfb.Binary
		b.Init(unionTable.Bytes, unionTable.Pos)
		dt.Nullable = b.Nullable()

	case vortexfb.TypeStruct_:
		dt.Kind = DTypeStruct
		var s vortexfb.Struct_
		s.Init(unionTable.Bytes, unionTable.Pos)
		dt.Nullable = s.Nullable()

		nFields := s.NamesLength()
		dt.FieldNames = make([]string, nFields)
		for i := 0; i < nFields; i++ {
			dt.FieldNames[i] = string(s.Names(i))
		}

		dt.FieldTypes = make([]*DType, s.DtypesLength())
		for i := 0; i < s.DtypesLength(); i++ {
			var childFB vortexfb.DType
			if s.Dtypes(&childFB, i) {
				child, err := parseFBDType(&childFB)
				if err != nil {
					return nil, fmt.Errorf("parsing struct field %d (%s): %w", i, dt.FieldNames[i], err)
				}
				dt.FieldTypes[i] = child
			}
		}

	case vortexfb.TypeList:
		dt.Kind = DTypeList
		var l vortexfb.List
		l.Init(unionTable.Bytes, unionTable.Pos)
		dt.Nullable = l.Nullable()

		var elemFB vortexfb.DType
		if l.ElementType(&elemFB) != nil {
			elem, err := parseFBDType(&elemFB)
			if err != nil {
				return nil, fmt.Errorf("parsing list element type: %w", err)
			}
			dt.ElementType = elem
		}

	case vortexfb.TypeFixedSizeList:
		dt.Kind = DTypeFixedSizeList
		var fl vortexfb.FixedSizeList
		fl.Init(unionTable.Bytes, unionTable.Pos)
		dt.Nullable = fl.Nullable()
		dt.FixedSize = fl.Size()

		var elemFB vortexfb.DType
		if fl.ElementType(&elemFB) != nil {
			elem, err := parseFBDType(&elemFB)
			if err != nil {
				return nil, fmt.Errorf("parsing fixed_size_list element type: %w", err)
			}
			dt.ElementType = elem
		}

	case vortexfb.TypeExtension:
		dt.Kind = DTypeExtension
		var e vortexfb.Extension
		e.Init(unionTable.Bytes, unionTable.Pos)
		dt.ExtensionID = string(e.Id())

		metaLen := e.MetadataLength()
		if metaLen > 0 {
			dt.ExtMetadata = make([]byte, metaLen)
			for i := 0; i < metaLen; i++ {
				dt.ExtMetadata[i] = e.Metadata(i)
			}
		}

		var storageFB vortexfb.DType
		if e.StorageDtype(&storageFB) != nil {
			storage, err := parseFBDType(&storageFB)
			if err != nil {
				return nil, fmt.Errorf("parsing extension storage dtype: %w", err)
			}
			dt.StorageDType = storage
		}

	default:
		return nil, fmt.Errorf("unknown DType type: %d", fb.TypeType())
	}

	return dt, nil
}

// parseLayout reads and parses the Layout FlatBuffer from its segment.
func (f *File) parseLayout() error {
	data, err := f.readSegment(f.layoutSeg)
	if err != nil {
		return err
	}

	fbLayout := vortexfb.GetRootAsLayout(data, 0)
	layout, err := parseFBLayout(fbLayout, f.footer)
	if err != nil {
		return err
	}
	f.layout = layout
	return nil
}

// parseFBLayout converts a FlatBuffer Layout into our Go Layout recursively.
func parseFBLayout(fb *vortexfb.Layout, footer *Footer) (*Layout, error) {
	l := &Layout{
		RowCount: fb.RowCount(),
		Metadata: fb.MetadataBytes(),
	}

	// Resolve encoding index → string ID via footer lookup.
	encIdx := fb.Encoding()
	if int(encIdx) < len(footer.LayoutSpecs) {
		l.EncodingID = footer.LayoutSpecs[encIdx]
	} else {
		return nil, fmt.Errorf("layout encoding index %d out of range (have %d specs)", encIdx, len(footer.LayoutSpecs))
	}

	// Parse segments (indices into footer.SegmentSpecs).
	l.Segments = make([]uint32, fb.SegmentsLength())
	for i := 0; i < fb.SegmentsLength(); i++ {
		l.Segments[i] = fb.Segments(i)
	}

	// Recursively parse children.
	l.Children = make([]*Layout, fb.ChildrenLength())
	for i := 0; i < fb.ChildrenLength(); i++ {
		var childFB vortexfb.Layout
		if fb.Children(&childFB, i) {
			child, err := parseFBLayout(&childFB, footer)
			if err != nil {
				return nil, fmt.Errorf("parsing layout child %d: %w", i, err)
			}
			l.Children[i] = child
		}
	}

	return l, nil
}

// parseStatistics reads and parses the FileStatistics FlatBuffer from its segment.
func (f *File) parseStatistics() error {
	data, err := f.readSegment(f.statsSeg)
	if err != nil {
		return err
	}

	fbStats := vortexfb.GetRootAsFileStatistics(data, 0)
	stats := &FileStatistics{}

	stats.FieldStats = make([]ArrayStats, fbStats.FieldStatsLength())
	var fbAS vortexfb.ArrayStats
	for i := 0; i < fbStats.FieldStatsLength(); i++ {
		if fbStats.FieldStats(&fbAS, i) {
			stats.FieldStats[i] = parseArrayStats(&fbAS)
		}
	}

	f.stats = stats
	return nil
}

// parseArrayStats converts a FlatBuffer ArrayStats into our Go ArrayStats.
func parseArrayStats(fb *vortexfb.ArrayStats) ArrayStats {
	s := ArrayStats{
		Min:              fb.MinBytes(),
		Max:              fb.MaxBytes(),
		Sum:              fb.SumBytes(),
		NullCount:        fb.NullCount(),
		IsSorted:         fb.IsSorted(),
		IsStrictSorted:   fb.IsStrictSorted(),
		IsConstant:       fb.IsConstant(),
		UncompressedSize: fb.UncompressedSizeInBytes(),
		NanCount:         fb.NanCount(),
	}
	return s
}

// GetRootAsLayout reads the root Layout from FlatBuffer bytes (for use in sub-parsers).
func GetRootAsLayout(data []byte) *vortexfb.Layout {
	return vortexfb.GetRootAsLayout(data, 0)
}
