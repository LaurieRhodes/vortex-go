package pcodec

import (
	"encoding/binary"
	"fmt"
)

// BitReader reads bits from a byte slice in little-endian order.
// It mirrors the Rust pco BitReader.
type BitReader struct {
	src          []byte
	byteIdx      int
	bitsPastByte Bitlen
}

// NewBitReader creates a new BitReader over src.
// Callers should ensure src has at least 8 bytes of padding beyond valid data
// for safe unaligned reads.
func NewBitReader(src []byte) *BitReader {
	return &BitReader{src: src}
}

// BitIdx returns the current bit position.
func (r *BitReader) BitIdx() int {
	return r.byteIdx*8 + int(r.bitsPastByte)
}

func (r *BitReader) refill() {
	r.byteIdx += int(r.bitsPastByte) / 8
	r.bitsPastByte %= 8
}

func (r *BitReader) consume(n Bitlen) {
	r.bitsPastByte += n
}

// u64At reads a little-endian u64 from src at byteIdx.
// Safe near EOF: reads available bytes and zero-pads the rest.
func (r *BitReader) u64At(byteIdx int) uint64 {
	if byteIdx+8 <= len(r.src) {
		return binary.LittleEndian.Uint64(r.src[byteIdx:])
	}
	var buf [8]byte
	copy(buf[:], r.src[byteIdx:])
	return binary.LittleEndian.Uint64(buf[:])
}

// u32At reads a little-endian u32 from src at byteIdx.
func (r *BitReader) u32At(byteIdx int) uint32 {
	if byteIdx+4 <= len(r.src) {
		return binary.LittleEndian.Uint32(r.src[byteIdx:])
	}
	var buf [4]byte
	copy(buf[:], r.src[byteIdx:])
	return binary.LittleEndian.Uint32(buf[:])
}

// ReadUint reads n bits as a uint64 value.
func (r *BitReader) ReadUint(n Bitlen) uint64 {
	if n == 0 {
		return 0
	}
	r.refill()
	var val uint64
	if n <= 25 {
		raw := uint64(r.u32At(r.byteIdx))
		val = (raw >> r.bitsPastByte) & maskForBits(n)
	} else if n <= 57 {
		raw := r.u64At(r.byteIdx)
		val = (raw >> r.bitsPastByte) & maskForBits(n)
	} else {
		// Two u64 reads for wide values (up to 64 bits).
		first := r.u64At(r.byteIdx) >> r.bitsPastByte
		processed := 56 - r.bitsPastByte
		second := r.u64At(r.byteIdx+7) << processed
		val = (first | second) & maskForBits(n)
	}
	r.consume(n)
	return val
}

// ReadBitlen reads n bits as a Bitlen.
func (r *BitReader) ReadBitlen(n Bitlen) Bitlen {
	return Bitlen(r.ReadUint(n))
}

// ReadBool reads a single bit.
func (r *BitReader) ReadBool() bool {
	r.refill()
	if r.byteIdx >= len(r.src) {
		r.consume(1)
		return false
	}
	val := r.src[r.byteIdx]&(1<<r.bitsPastByte) != 0
	r.consume(1)
	return val
}

// DrainEmptyByte advances to the next byte boundary, verifying trailing
// bits are zero.
func (r *BitReader) DrainEmptyByte() error {
	r.refill()
	if r.bitsPastByte != 0 {
		if r.byteIdx < len(r.src) {
			if (r.src[r.byteIdx] >> r.bitsPastByte) != 0 {
				return fmt.Errorf("pcodec: non-zero trailing bits at byte %d, bits_past %d",
					r.byteIdx, r.bitsPastByte)
			}
		}
		r.consume(8 - r.bitsPastByte)
	}
	return nil
}

// ByteOffset returns the current byte-aligned offset (after refill).
func (r *BitReader) ByteOffset() int {
	r.refill()
	return r.byteIdx
}
