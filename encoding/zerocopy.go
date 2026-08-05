package encoding

// zerocopy.go — zero-copy typed views over canonical primitive
// buffers.
//
// Canonical primitive arrays store values as little-endian bytes. On a
// little-endian machine with an aligned buffer, those bytes ARE the
// typed values, so the accessor copy loop (make + per-element
// LittleEndian.UintN) is pure waste: for a 1M-row scan it was ~8MB of
// allocation, zeroing, and memmove per column per query.
//
// primView returns an unsafe slice view when (a) the platform is
// little-endian and (b) the buffer start is aligned for the element
// type; otherwise callers fall back to the copying path. The view
// aliases the array's buffer: it is valid for the lifetime of the
// backing buffer and must be treated as read-only. Engine callers box
// (copy) individual elements before retaining them beyond a chunk,
// which preserves both constraints.

import (
	"unsafe"
)

// littleEndianHost reports whether the host stores integers
// little-endian. Computed once; constant-folds on amd64/arm64 in
// practice.
var littleEndianHost = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// primView returns a []T view over buf when zero-copy is safe, or nil
// when the caller must copy. n is the element count.
func primView[T int32 | uint32 | int64 | uint64 | float64 | float32](buf []byte, n int) []T {
	var zero T
	size := int(unsafe.Sizeof(zero))
	if !littleEndianHost || n == 0 || len(buf) < n*size {
		return nil
	}
	p := unsafe.Pointer(unsafe.SliceData(buf))
	if uintptr(p)%uintptr(size) != 0 {
		return nil // unaligned: copying path
	}
	return unsafe.Slice((*T)(p), n)
}
