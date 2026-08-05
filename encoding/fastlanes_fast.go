package encoding

// fastlanes_fast.go — table-driven FastLanes unpacking.
//
// The naive kernel recomputes the FL_ORDER logical→physical mapping
// per element (two divisions, a modulo, a table lookup) and reads
// values through a switch-dispatched bounds-checked byte accessor.
// But the mapping depends only on (elemSize, bw): for each of the
// 1024 logical positions in a block, the source word index, shift,
// and straddle partner are fixed. So we precompute a plan per
// (elemSize, bw) — at most 4 element sizes × 64 bit widths, built
// lazily and cached forever — and unpack with a tight generic loop
// over zero-copy typed views of the packed and output buffers.
//
// The naive unpackFastLanesTyped remains as the fallback for
// unaligned buffers and as the differential oracle in tests.

import (
	"sync"
	"unsafe"
)

// flPlan maps each logical position in a 1024-element FastLanes block
// to its packed location.
type flPlan struct {
	srcIdx  [fastlanesBlockSize]int32 // primary word index, in T elements from block start
	shift   [fastlanesBlockSize]uint8
	srcIdx2 [fastlanesBlockSize]int32 // straddle partner word index; -1 = none
}

var flPlanCache sync.Map // key uint16(elemSize)<<8 | bw → *flPlan

func getFLPlan(elemSize, bw int) *flPlan {
	key := uint16(elemSize)<<8 | uint16(bw)
	if p, ok := flPlanCache.Load(key); ok {
		return p.(*flPlan)
	}
	tBits := elemSize * 8
	nLanes := fastlanesBlockSize / tBits
	p := &flPlan{}
	for logIdx := 0; logIdx < fastlanesBlockSize; logIdx++ {
		lane := logIdx % nLanes
		s := logIdx / 128
		flVal := (logIdx - s*128 - lane) / 16
		row := flOrder[flVal]*8 + s

		startBit := row * bw
		word := startBit / tBits
		shift := startBit % tBits

		p.srcIdx[logIdx] = int32(word*nLanes + lane)
		p.shift[logIdx] = uint8(shift)
		if shift+bw > tBits {
			p.srcIdx2[logIdx] = int32((word+1)*nLanes + lane)
		} else {
			p.srcIdx2[logIdx] = -1
		}
	}
	flPlanCache.Store(key, p)
	return p
}

// typedView reinterprets buf as []T when the host is little-endian and
// the buffer is aligned; nil otherwise.
func typedView[T uint8 | uint16 | uint32 | uint64](buf []byte, n int) []T {
	var zero T
	size := int(unsafe.Sizeof(zero))
	if !littleEndianHost || n == 0 || len(buf) < n*size {
		return nil
	}
	p := unsafe.Pointer(unsafe.SliceData(buf))
	if uintptr(p)%uintptr(size) != 0 {
		return nil
	}
	return unsafe.Slice((*T)(p), n)
}

// unpackFLBlocks is the generic table-driven kernel. packedT holds all
// blocks contiguously (wordsPerBlock T-elements per block); out
// receives count values for logical positions [offset, offset+count).
func unpackFLBlocks[T uint8 | uint16 | uint32 | uint64](packedT []T, out []T, plan *flPlan,
	wordsPerBlock, offset, count, bw int) {

	tBits := int(unsafe.Sizeof(*new(T))) * 8
	var mask T
	if bw == tBits {
		mask = ^T(0)
	} else {
		mask = T(1)<<bw - 1
	}

	totalElems := offset + count
	numBlocks := (totalElems + fastlanesBlockSize - 1) / fastlanesBlockSize

	for block := 0; block < numBlocks; block++ {
		base := block * wordsPerBlock
		blockT := packedT[base : base+wordsPerBlock]
		blockLogStart := block * fastlanesBlockSize

		lo := 0
		if offset > blockLogStart {
			lo = offset - blockLogStart
		}
		hi := fastlanesBlockSize
		if totalElems-blockLogStart < hi {
			hi = totalElems - blockLogStart
		}

		outBase := blockLogStart - offset
		for i := lo; i < hi; i++ {
			raw := blockT[plan.srcIdx[i]]
			sh := plan.shift[i]
			v := raw >> sh
			if j := plan.srcIdx2[i]; j >= 0 {
				v |= blockT[j] << (uint8(tBits) - sh)
			}
			out[outBase+i] = v & mask
		}
	}
}

// unpackFastLanesFast unpacks using the table-driven kernel when the
// packed buffer admits a zero-copy typed view; falls back to the
// naive path otherwise. Output layout matches unpackFastLanesTyped
// exactly.
func unpackFastLanesFast(packed []byte, bw, offset, count, elemSize int) ([]byte, error) {
	tBits := elemSize * 8
	nLanes := fastlanesBlockSize / tBits
	wordsPerBlock := nLanes * bw

	totalElems := offset + count
	numBlocks := (totalElems + fastlanesBlockSize - 1) / fastlanesBlockSize
	needWords := numBlocks * wordsPerBlock

	outBuf := make([]byte, count*elemSize)
	plan := getFLPlan(elemSize, bw)

	switch elemSize {
	case 1:
		packedT := typedView[uint8](packed, needWords)
		outT := typedView[uint8](outBuf, count)
		if packedT == nil || outT == nil {
			return unpackFastLanesTyped(packed, bw, offset, count, elemSize)
		}
		unpackFLBlocks(packedT, outT, plan, wordsPerBlock, offset, count, bw)
	case 2:
		packedT := typedView[uint16](packed, needWords)
		outT := typedView[uint16](outBuf, count)
		if packedT == nil || outT == nil {
			return unpackFastLanesTyped(packed, bw, offset, count, elemSize)
		}
		unpackFLBlocks(packedT, outT, plan, wordsPerBlock, offset, count, bw)
	case 4:
		packedT := typedView[uint32](packed, needWords)
		outT := typedView[uint32](outBuf, count)
		if packedT == nil || outT == nil {
			return unpackFastLanesTyped(packed, bw, offset, count, elemSize)
		}
		unpackFLBlocks(packedT, outT, plan, wordsPerBlock, offset, count, bw)
	case 8:
		packedT := typedView[uint64](packed, needWords)
		outT := typedView[uint64](outBuf, count)
		if packedT == nil || outT == nil {
			return unpackFastLanesTyped(packed, bw, offset, count, elemSize)
		}
		unpackFLBlocks(packedT, outT, plan, wordsPerBlock, offset, count, bw)
	default:
		return unpackFastLanesTyped(packed, bw, offset, count, elemSize)
	}

	return outBuf, nil
}
