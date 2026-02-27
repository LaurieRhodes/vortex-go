// Package pcodec implements a decoder for the Pcodec (Pco) lossless numerical
// compression format. This is a Go port of the decoder side of
// https://github.com/pcodec/pcodec, targeting the wrapped API used by Vortex.
//
// Only decompression is supported; compression may be added later.
package pcodec

import (
	"fmt"
	"math"
	"math/bits"
)

// Bitlen is the type used for bit counts and bit widths.
type Bitlen = uint32

// Weight is the ANS symbol weight type.
type Weight = uint32

// Constants matching the Rust pco crate (pco 1.0.1, format version 4.1).
const (
	BitsToEncodeAnsSizeLog             Bitlen = 4
	BitsToEncodeModeVariant            Bitlen = 4
	BitsToEncodeDeltaEncodingVariant   Bitlen = 4
	BitsToEncodeDeltaEncodingOrder     Bitlen = 3
	BitsToEncodeDeltaConvQuantization  Bitlen = 5
	BitsToEncodeDeltaConvNWeights      Bitlen = 5
	BitsToEncodeDeltaLookbackWindowLog Bitlen = 5
	BitsToEncodeDeltaLookbackStateLog  Bitlen = 4
	BitsToEncodeNBins                  Bitlen = 15
	BitsToEncodeQuantizeK              Bitlen = 8
	BitsToEncodeDictLen                Bitlen = 25

	MaxAnsBits      Bitlen = 14
	ANSInterleaving        = 4
	FullBatchN             = 256
)

// LatentType identifies the unsigned integer type used for latent values.
type LatentType int

const (
	LatentU8  LatentType = 8
	LatentU16 LatentType = 16
	LatentU32 LatentType = 32
	LatentU64 LatentType = 64
)

// Bits returns the number of bits for a LatentType.
func (lt LatentType) Bits() Bitlen {
	return Bitlen(lt)
}

// bitsToEncodeOffsetBits returns the number of bits needed to encode
// offset_bits for a bin with the given latent bit width.
// e.g. u8→4, u16→5, u32→6, u64→7
func bitsToEncodeOffsetBits(latentBits Bitlen) Bitlen {
	return Bitlen(bits.Len32(latentBits))
}

// maskForBits returns a bitmask with the lowest n bits set.
func maskForBits(n Bitlen) uint64 {
	if n >= 64 {
		return math.MaxUint64
	}
	return (1 << n) - 1
}

// Bin represents a numerical range in the ANS coding table.
type Bin struct {
	Weight     Weight
	Lower      uint64 // stored as u64 regardless of actual latent width
	OffsetBits Bitlen
}

// --- Number type support ---

// NumberType identifies the number type that was compressed.
type NumberType int

const (
	NumU16 NumberType = iota
	NumU32
	NumU64
	NumI16
	NumI32
	NumI64
	NumF16
	NumF32
	NumF64
)

func (nt NumberType) String() string {
	names := [...]string{"u16", "u32", "u64", "i16", "i32", "i64", "f16", "f32", "f64"}
	if int(nt) < len(names) {
		return names[nt]
	}
	return fmt.Sprintf("NumberType(%d)", int(nt))
}

// LatentTypeFor returns the LatentType used for a given NumberType.
func (nt NumberType) LatentType() LatentType {
	switch nt {
	case NumU16, NumI16, NumF16:
		return LatentU16
	case NumU32, NumI32, NumF32:
		return LatentU32
	case NumU64, NumI64, NumF64:
		return LatentU64
	default:
		panic(fmt.Sprintf("invalid NumberType: %d", nt))
	}
}

// ByteSize returns the byte size of one element.
func (nt NumberType) ByteSize() int {
	switch nt {
	case NumU16, NumI16, NumF16:
		return 2
	case NumU32, NumI32, NumF32:
		return 4
	case NumU64, NumI64, NumF64:
		return 8
	default:
		panic(fmt.Sprintf("invalid NumberType: %d", nt))
	}
}

// --- Latent ↔ Number conversion ---

// fromLatentOrdered converts a latent-ordered value back to the original
// number bits. For unsigned types this is identity; for signed types it
// toggles the sign bit; for floats it uses the canonical mapping.
func fromLatentOrdered(nt NumberType, lat uint64) uint64 {
	switch nt {
	case NumU16, NumU32, NumU64:
		return lat
	case NumI16:
		return lat ^ (1 << 15)
	case NumI32:
		return lat ^ (1 << 31)
	case NumI64:
		return lat ^ (1 << 63)
	case NumF32:
		v := uint32(lat)
		if v&(1<<31) != 0 {
			return uint64(v ^ (1 << 31))
		}
		return uint64(^v)
	case NumF64:
		if lat&(1<<63) != 0 {
			return lat ^ (1 << 63)
		}
		return ^lat
	case NumF16:
		v := uint16(lat)
		if v&(1<<15) != 0 {
			return uint64(v ^ (1 << 15))
		}
		return uint64(^v)
	default:
		return lat
	}
}

// toggleCenter wraps val + MID for the given latent bit width.
// MID is 1 << (bits-1).
func toggleCenter(val uint64, latentBits Bitlen) uint64 {
	mid := uint64(1) << (latentBits - 1)
	return (val + mid) & maskForBits(latentBits)
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxI(a, b int) int {
	if a > b {
		return a
	}
	return b
}
