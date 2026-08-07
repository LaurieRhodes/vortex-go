package vortex

import (
	"fmt"
	"io"
)

// MemReader wraps an already fully-resident byte slice as an
// io.ReaderAt while also implementing byteSlicer, so File.Open callers
// that have already read a whole (typically small) file into memory
// can get zero-copy segment reads instead of paying an extra
// allocate+copy for every segment readSegmentSpec touches. Use
// bytes.NewReader (or a real *os.File) instead when the data is not
// already resident, or is large enough that true partial/lazy reads
// matter more than avoiding this indirection.
type MemReader struct {
	data []byte
}

// NewMemReader wraps data for zero-copy segment reads. data must not
// be modified for the lifetime of any File opened against it.
func NewMemReader(data []byte) *MemReader {
	return &MemReader{data: data}
}

// ReadAt implements io.ReaderAt (copies, per the interface contract —
// use Slice directly, or rely on readSegmentSpec's fast path, to avoid
// the copy).
func (m *MemReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(m.data)) {
		return 0, fmt.Errorf("MemReader.ReadAt: offset %d out of range [0,%d]", off, len(m.data))
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Slice returns a zero-copy view into the underlying buffer. The
// returned slice aliases data — callers must treat it as read-only.
func (m *MemReader) Slice(off, length int64) ([]byte, error) {
	if off < 0 || length < 0 || off+length > int64(len(m.data)) {
		return nil, fmt.Errorf("MemReader.Slice: range [%d,%d) out of bounds for length %d", off, off+length, len(m.data))
	}
	return m.data[off : off+length], nil
}
