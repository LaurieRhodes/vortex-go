package pcodec

import "encoding/binary"

// BitWriter writes bits in little-endian order, mirroring the Rust pco BitWriter.
// It buffers bits and flushes to the destination byte slice.
type BitWriter struct {
	dst          []byte
	bitBuf       uint64
	bitsPending  Bitlen // number of valid bits in bitBuf
}

// NewBitWriter creates a new BitWriter that appends to dst.
func NewBitWriter() *BitWriter {
	return &BitWriter{}
}

// WriteUint writes n bits of val in little-endian order.
func (w *BitWriter) WriteUint(val uint64, n Bitlen) {
	if n == 0 {
		return
	}
	if w.bitsPending+n <= 64 {
		// Fast path: everything fits in the 64-bit buffer.
		w.bitBuf |= val << w.bitsPending
		w.bitsPending += n
	} else {
		// Slow path: split across buffer boundary.
		// Fill the current buffer with the low bits.
		lowBits := 64 - w.bitsPending
		w.bitBuf |= val << w.bitsPending
		w.bitsPending = 64
		w.flushBytes()
		// Write the remaining high bits.
		w.bitBuf = val >> lowBits
		w.bitsPending = n - lowBits
	}
	w.flushBytes()
}

// WriteBitlen writes a Bitlen value using n bits.
func (w *BitWriter) WriteBitlen(val Bitlen, n Bitlen) {
	w.WriteUint(uint64(val), n)
}

// WriteBool writes a single bit.
func (w *BitWriter) WriteBool(val bool) {
	if val {
		w.WriteUint(1, 1)
	} else {
		w.WriteUint(0, 1)
	}
}

// WriteAlignedBytes writes raw bytes, flushing any pending bits first.
func (w *BitWriter) WriteAlignedBytes(data []byte) {
	w.FinishByte()
	w.dst = append(w.dst, data...)
}

// FinishByte pads the current byte with zeros and flushes it.
func (w *BitWriter) FinishByte() {
	if w.bitsPending%8 != 0 {
		w.bitsPending = ((w.bitsPending + 7) / 8) * 8
	}
	w.flushBytes()
}

// Flush writes all complete bytes from the bit buffer to dst.
func (w *BitWriter) Flush() {
	w.flushBytes()
}

// Bytes returns the accumulated output bytes.
func (w *BitWriter) Bytes() []byte {
	return w.dst
}

// Reset clears the writer for reuse.
func (w *BitWriter) Reset() {
	w.dst = w.dst[:0]
	w.bitBuf = 0
	w.bitsPending = 0
}

// flushBytes writes complete bytes from bitBuf to dst.
func (w *BitWriter) flushBytes() {
	for w.bitsPending >= 8 {
		w.dst = append(w.dst, byte(w.bitBuf))
		w.bitBuf >>= 8
		w.bitsPending -= 8
	}
}

// WriteUint32LE writes a uint32 in little-endian.
func (w *BitWriter) WriteUint32LE(val uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], val)
	w.WriteAlignedBytes(buf[:])
}
