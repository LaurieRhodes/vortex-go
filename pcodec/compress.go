package pcodec

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"sort"
)

// CompressOptions controls pcodec compression behavior.
type CompressOptions struct {
	// Level controls compression effort: 0 = baseline (fast), 1+ = multi-bin.
	Level int
	// ValuesPerPage is the target page size. 0 means use default (same as chunk).
	ValuesPerPage int
	// ValuesPerChunk is the max values per chunk. 0 means DEFAULT_VALUES_PER_CHUNK.
	ValuesPerChunk int
}

const (
	defaultValuesPerChunk = 1 << 18 // 262144, matching Rust pco DEFAULT_MAX_PAGE_N
	formatVersionMajor    = 4
	formatVersionMinor    = 1
)

// DefaultCompressOptions returns sensible defaults.
func DefaultCompressOptions() CompressOptions {
	return CompressOptions{
		Level:          6,
		ValuesPerChunk: defaultValuesPerChunk,
	}
}

// CompressResult holds the output of compression.
type CompressResult struct {
	Header     []byte   // format version bytes
	ChunkMetas [][]byte // one per chunk: serialized ChunkMeta
	Pages      [][]byte // all pages across all chunks
	PageCounts []int    // number of pages per chunk
	PageNs     []int    // number of values in each page
}

// Compress compresses a slice of values into pcodec format.
// The numberType identifies the element type.
// The data must be a flat little-endian byte buffer of the elements.
func Compress(data []byte, nt NumberType, opts CompressOptions) (*CompressResult, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("pcodec: empty data")
	}

	lt := nt.LatentType()
	bs := nt.ByteSize()
	n := len(data) / bs
	if n*bs != len(data) {
		return nil, fmt.Errorf("pcodec: data length %d not aligned to %d-byte elements", len(data), bs)
	}

	// Convert to latent-ordered uint64 values.
	latents := make([]uint64, n)
	for i := 0; i < n; i++ {
		latents[i] = toLatentOrdered(nt, readRawValue(data, i, bs))
	}

	valuesPerChunk := opts.ValuesPerChunk
	if valuesPerChunk <= 0 {
		valuesPerChunk = defaultValuesPerChunk
	}
	valuesPerPage := opts.ValuesPerPage
	if valuesPerPage <= 0 {
		valuesPerPage = valuesPerChunk
	}

	// Write header.
	header := []byte{formatVersionMajor, formatVersionMinor}

	var chunkMetas [][]byte
	var pages [][]byte
	var pageCounts []int
	var pageNs []int

	for chunkStart := 0; chunkStart < n; chunkStart += valuesPerChunk {
		chunkEnd := chunkStart + valuesPerChunk
		if chunkEnd > n {
			chunkEnd = n
		}
		chunkLatents := latents[chunkStart:chunkEnd]
		chunkN := len(chunkLatents)

		// Compute page sizes.
		var pSizes []int
		for pos := 0; pos < chunkN; pos += valuesPerPage {
			end := pos + valuesPerPage
			if end > chunkN {
				end = chunkN
			}
			pSizes = append(pSizes, end-pos)
		}

		var cmBytes []byte
		var chunkPages [][]byte

		if opts.Level >= 1 {
			// Multi-bin compression.
			cmBytes, chunkPages = compressChunkMultiBin(chunkLatents, pSizes, lt)
		} else {
			// Baseline: single bin, no ANS.
			cmBytes, chunkPages = compressChunkBaseline(chunkLatents, pSizes, lt)
		}

		chunkMetas = append(chunkMetas, cmBytes)
		pages = append(pages, chunkPages...)
		pageCounts = append(pageCounts, len(pSizes))
		pageNs = append(pageNs, pSizes...)
	}

	return &CompressResult{
		Header:     header,
		ChunkMetas: chunkMetas,
		Pages:      pages,
		PageCounts: pageCounts,
		PageNs:     pageNs,
	}, nil
}

// readRawValue reads an element from a LE byte buffer.
func readRawValue(data []byte, idx, byteSize int) uint64 {
	off := idx * byteSize
	switch byteSize {
	case 2:
		return uint64(binary.LittleEndian.Uint16(data[off:]))
	case 4:
		return uint64(binary.LittleEndian.Uint32(data[off:]))
	case 8:
		return binary.LittleEndian.Uint64(data[off:])
	}
	return 0
}

// toLatentOrdered converts a raw number to its latent-ordered representation.
// This is the inverse of fromLatentOrdered.
func toLatentOrdered(nt NumberType, raw uint64) uint64 {
	switch nt {
	case NumU16, NumU32, NumU64:
		return raw
	case NumI16:
		return raw ^ (1 << 15)
	case NumI32:
		return raw ^ (1 << 31)
	case NumI64:
		return raw ^ (1 << 63)
	case NumF32:
		v := uint32(raw)
		if v&(1<<31) != 0 {
			return uint64(^v)
		}
		return uint64(v ^ (1 << 31))
	case NumF64:
		if raw&(1<<63) != 0 {
			return ^raw
		}
		return raw ^ (1 << 63)
	case NumF16:
		v := uint16(raw)
		if v&(1<<15) != 0 {
			return uint64(^v)
		}
		return uint64(v ^ (1 << 15))
	default:
		return raw
	}
}

// ============================================================
// Baseline compressor: 1 bin, no ANS, no delta
// ============================================================

func compressChunkBaseline(latents []uint64, pageSizes []int, lt LatentType) ([]byte, [][]byte) {
	latentBits := lt.Bits()

	// Compute actual range for the single bin.
	minVal := latents[0]
	maxVal := latents[0]
	for _, v := range latents[1:] {
		if v < minVal {
			minVal = v
		}
		if v > maxVal {
			maxVal = v
		}
	}
	span := maxVal - minVal
	offsetBits := Bitlen(0)
	if span > 0 {
		offsetBits = Bitlen(bits.Len64(span))
	}

	// Build chunk metadata: Classic mode + NoOp delta + 1 bin covering [minVal, maxVal].
	w := NewBitWriter()

	// Mode: Classic = 0 (4 bits)
	w.WriteBitlen(0, BitsToEncodeModeVariant)

	// Delta: NoOp = 0 (4 bits)
	w.WriteBitlen(0, BitsToEncodeDeltaEncodingVariant)

	// Flush the mode+delta byte.
	w.Flush()

	// Primary ChunkLatentVarMeta:
	// ans_size_log = 0 (4 bits)
	w.WriteBitlen(0, BitsToEncodeAnsSizeLog)
	// n_bins = 1 (15 bits)
	w.WriteUint(1, BitsToEncodeNBins)
	// Bin: weight-1 = 0 (0 bits since ans_size_log=0), lower = minVal, offset_bits
	w.WriteUint(minVal, latentBits) // lower
	offsetBitsBits := Bitlen(bits.Len32(latentBits))
	w.WriteBitlen(offsetBits, offsetBitsBits) // offset_bits

	// Drain to byte boundary.
	w.FinishByte()
	w.Flush()
	chunkMeta := make([]byte, len(w.Bytes()))
	copy(chunkMeta, w.Bytes())

	// Pages: for baseline, page meta is empty (0 bits for ANS states + 0 delta state),
	// body is latent offsets relative to lower.
	var pages [][]byte
	offset := 0
	for _, pageN := range pageSizes {
		pw := NewBitWriter()
		// Page meta: drain to byte (already at 0 bits) — no bits written.
		// Body: write each value as (latent - lower) in offsetBits bits.
		for i := 0; i < pageN; i++ {
			pw.WriteUint(latents[offset+i]-minVal, offsetBits)
		}
		pw.FinishByte()
		pw.Flush()
		page := make([]byte, len(pw.Bytes()))
		copy(page, pw.Bytes())
		pages = append(pages, page)
		offset += pageN
	}

	return chunkMeta, pages
}

// ============================================================
// Multi-bin compressor: histogram → bins → ANS encoding
// ============================================================

// binInfo holds compression info for one bin during optimization.
type binInfo struct {
	lower      uint64
	upperIncl  uint64 // inclusive upper bound
	offsetBits Bitlen
	count      int    // number of values falling in this bin
	weight     Weight // quantized ANS weight
}

func compressChunkMultiBin(latents []uint64, pageSizes []int, lt LatentType) ([]byte, [][]byte) {
	latentBits := lt.Bits()

	// 1. Build histogram and choose bins.
	infos := chooseBins(latents, latentBits)
	if len(infos) <= 1 {
		// Fall back to baseline if only 1 bin.
		return compressChunkBaseline(latents, pageSizes, lt)
	}

	// 2. Quantize weights and determine ANS table size.
	ansSizeLog := quantizeWeights(infos, len(latents))

	// 3. Build ANS encoding table.
	encoder := buildANSEncoder(infos, ansSizeLog)

	// 4. Build lookup table: latent → (binIdx, offset).
	lookup := buildBinLookup(infos)

	// 5. Write chunk metadata.
	chunkMeta := writeChunkMeta(infos, ansSizeLog, latentBits)

	// 6. Compress pages.
	var pages [][]byte
	offset := 0
	for _, pageN := range pageSizes {
		page := compressPage(latents[offset:offset+pageN], infos, encoder, lookup, ansSizeLog, latentBits)
		pages = append(pages, page)
		offset += pageN
	}

	return chunkMeta, pages
}

// chooseBins analyzes latent values and returns bins for compression.
func chooseBins(latents []uint64, latentBits Bitlen) []binInfo {
	if len(latents) == 0 {
		return nil
	}

	// Sort a copy to find value distribution.
	sorted := make([]uint64, len(latents))
	copy(sorted, latents)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Determine number of bins based on data characteristics.
	// Use log2(n) bins, capped at 256 for practical ANS table sizes.
	nLogCeil := Bitlen(0)
	if len(latents) > 1 {
		nLogCeil = Bitlen(bits.Len(uint(len(latents) - 1)))
	}
	maxBins := 1 << minBitlen(nLogCeil, 8)
	if maxBins > len(latents) {
		maxBins = len(latents)
	}

	// Build equi-depth histogram bins.
	binSize := len(sorted) / maxBins
	if binSize < 1 {
		binSize = 1
	}

	var infos []binInfo
	i := 0
	for i < len(sorted) {
		lower := sorted[i]
		end := i + binSize
		if end > len(sorted) {
			end = len(sorted)
		}
		// Extend to include all equal values.
		for end < len(sorted) && sorted[end] == sorted[end-1] {
			end++
		}
		upperIncl := sorted[end-1]

		// Compute offset bits needed.
		span := upperIncl - lower
		ob := Bitlen(0)
		if span > 0 {
			ob = Bitlen(bits.Len64(span))
		}

		infos = append(infos, binInfo{
			lower:      lower,
			upperIncl:  upperIncl,
			offsetBits: ob,
			count:      end - i,
		})
		i = end
	}

	// Merge adjacent bins if they would produce the same offset_bits
	// (no compression benefit to keeping them separate).
	merged := []binInfo{infos[0]}
	for _, b := range infos[1:] {
		prev := &merged[len(merged)-1]
		combinedSpan := b.upperIncl - prev.lower
		combinedOB := Bitlen(bits.Len64(combinedSpan))
		// Merge if the combined offset_bits is the same as both individual bins.
		if combinedOB <= maxBitlen(prev.offsetBits, b.offsetBits) {
			prev.upperIncl = b.upperIncl
			prev.offsetBits = combinedOB
			prev.count += b.count
		} else {
			merged = append(merged, b)
		}
	}

	// If we ended up with too few bins or no benefit, return single bin.
	if len(merged) <= 1 {
		return merged
	}

	// Estimate compression: would multi-bin save space vs range-aware single-bin baseline?
	fullSpan := sorted[len(sorted)-1] - sorted[0]
	baselineOB := Bitlen(0)
	if fullSpan > 0 {
		baselineOB = Bitlen(bits.Len64(fullSpan))
	}
	baselineBits := len(latents) * int(baselineOB)
	multiBinBits := 0
	// ANS overhead per symbol is approximately log2(tableSize) bits ≈ log2(nBins)+2.
	ansOverhead := int(bits.Len(uint(len(merged)))) + 2
	for _, b := range merged {
		multiBinBits += b.count * (int(b.offsetBits) + ansOverhead)
	}
	// Add metadata overhead: ~4 bytes per bin.
	multiBinBits += len(merged) * 32

	if multiBinBits >= baselineBits {
		// No benefit, use single bin.
		fullBin := binInfo{
			lower:      sorted[0],
			upperIncl:  sorted[len(sorted)-1],
			offsetBits: Bitlen(bits.Len64(sorted[len(sorted)-1] - sorted[0])),
			count:      len(latents),
		}
		if fullBin.offsetBits == 0 && sorted[0] == sorted[len(sorted)-1] {
			fullBin.offsetBits = 0
		}
		return []binInfo{fullBin}
	}

	return merged
}

// quantizeWeights assigns ANS weights to bins and returns the ANS size log.
func quantizeWeights(infos []binInfo, totalCount int) Bitlen {
	if len(infos) <= 1 {
		infos[0].weight = 1
		return 0
	}

	// Minimum size_log to fit all bins.
	minLog := Bitlen(bits.Len(uint(len(infos) - 1)))
	// Cap at the data's log2(n).
	nLogCeil := Bitlen(0)
	if totalCount > 1 {
		nLogCeil = Bitlen(bits.Len(uint(totalCount - 1)))
	}
	sizeLog := minBitlen(minLog+2, minBitlen(nLogCeil, MaxAnsBits))
	if sizeLog < minLog {
		sizeLog = minLog
	}

	tableSize := Weight(1) << sizeLog
	multiplier := float64(tableSize) / float64(totalCount)

	// Assign weights proportional to count, minimum 1.
	surplusDesired := make([]float64, len(infos))
	for i, info := range infos {
		surplusDesired[i] = math.Max(float64(info.count)*multiplier-1.0, 0.0)
	}
	totalSurplus := 0.0
	for _, s := range surplusDesired {
		totalSurplus += s
	}

	requiredSurplus := tableSize - Weight(len(infos))
	surplusMult := 0.0
	if totalSurplus > 0 {
		surplusMult = float64(requiredSurplus) / totalSurplus
	}

	weightSum := Weight(0)
	for i := range infos {
		w := Weight(math.Round(1.0 + surplusDesired[i]*surplusMult))
		if w < 1 {
			w = 1
		}
		infos[i].weight = w
		weightSum += w
	}

	// Adjust to hit exact table size.
	for weightSum > tableSize {
		// Remove from bins that were rounded up.
		for i := range infos {
			if weightSum <= tableSize {
				break
			}
			if infos[i].weight > 1 {
				infos[i].weight--
				weightSum--
			}
		}
	}
	for weightSum < tableSize {
		// Add to bins with highest count.
		bestIdx := 0
		for i := 1; i < len(infos); i++ {
			if infos[i].count > infos[bestIdx].count {
				bestIdx = i
			}
		}
		infos[bestIdx].weight++
		weightSum++
	}

	// Reduce size_log if all weights are divisible by a power of 2.
	minTrailingZeros := Bitlen(32)
	for _, info := range infos {
		tz := Bitlen(bits.TrailingZeros32(uint32(info.weight)))
		if tz < minTrailingZeros {
			minTrailingZeros = tz
		}
	}
	if minTrailingZeros > 0 {
		sizeLog -= minTrailingZeros
		for i := range infos {
			infos[i].weight >>= minTrailingZeros
		}
	}

	return sizeLog
}

// ansEncoderState holds per-symbol encoding info.
type ansEncoderState struct {
	renormBitCutoff uint32
	minRenormBits   Bitlen
	nextStates      []uint32 // x_s → next state
}

// ansEncoder holds the ANS encoding table.
type ansEncoder struct {
	symbols   []ansEncoderState
	sizeLog   Bitlen
	tableSize uint32
}

func buildANSEncoder(infos []binInfo, sizeLog Bitlen) *ansEncoder {
	tableSize := uint32(1) << sizeLog
	weights := make([]Weight, len(infos))
	for i, info := range infos {
		weights[i] = info.weight
	}

	// Spread symbols (same as decoder).
	stateSymbols := spreadSymbolsForEncoder(int(tableSize), weights)

	// Build per-symbol encoding info.
	symbols := make([]ansEncoderState, len(infos))
	for i, w := range weights {
		maxXS := 2*w - 1
		minRB := sizeLog - Bitlen(bits.Len32(maxXS)) + 1
		symbols[i] = ansEncoderState{
			renormBitCutoff: uint32(2 * w * (1 << minRB)),
			minRenormBits:   minRB,
			nextStates:      make([]uint32, 0, w),
		}
	}

	for stateIdx, sym := range stateSymbols {
		symbols[sym].nextStates = append(symbols[sym].nextStates,
			uint32(int(tableSize)+stateIdx))
	}

	return &ansEncoder{symbols: symbols, sizeLog: sizeLog, tableSize: tableSize}
}

func spreadSymbolsForEncoder(tableSize int, weights []Weight) []uint32 {
	stride := uint32(3*tableSize) / 5
	if stride%2 == 0 {
		stride++
	}
	sizeMask := uint32(tableSize - 1)
	result := make([]uint32, tableSize)
	var step uint32
	for symbol, weight := range weights {
		for j := Weight(0); j < weight; j++ {
			stateIdx := (stride * step) & sizeMask
			result[stateIdx] = uint32(symbol)
			step++
		}
	}
	return result
}

// encode returns (newState, renormBits).
// The caller must write the low renormBits bits of the old state.
func (e *ansEncoder) encode(state uint32, symbol int) (uint32, Bitlen) {
	si := &e.symbols[symbol]
	var renormBits Bitlen
	if state >= si.renormBitCutoff {
		renormBits = si.minRenormBits + 1
	} else {
		renormBits = si.minRenormBits
	}
	xS := state >> renormBits
	return si.nextStates[xS-uint32(len(si.nextStates))], renormBits
}

// binLookup provides O(log n) latent→bin lookup.
type binLookup struct {
	lowers []uint64 // sorted bin lower bounds
	nBins  int
}

func buildBinLookup(infos []binInfo) *binLookup {
	lowers := make([]uint64, len(infos))
	for i, b := range infos {
		lowers[i] = b.lower
	}
	return &binLookup{lowers: lowers, nBins: len(infos)}
}

// find returns the bin index for a latent value.
func (bl *binLookup) find(val uint64) int {
	// Binary search: find the last bin whose lower <= val.
	idx := sort.Search(bl.nBins, func(i int) bool {
		return bl.lowers[i] > val
	}) - 1
	if idx < 0 {
		idx = 0
	}
	return idx
}

// writeChunkMeta serializes chunk metadata in pco format.
func writeChunkMeta(infos []binInfo, ansSizeLog Bitlen, latentBits Bitlen) []byte {
	w := NewBitWriter()

	// Mode: Classic = 0
	w.WriteBitlen(0, BitsToEncodeModeVariant)
	// Delta: NoOp = 0
	w.WriteBitlen(0, BitsToEncodeDeltaEncodingVariant)
	w.Flush()

	// Primary ChunkLatentVarMeta:
	w.WriteBitlen(ansSizeLog, BitsToEncodeAnsSizeLog)
	w.WriteUint(uint64(len(infos)), BitsToEncodeNBins)

	offsetBitsBits := Bitlen(bits.Len32(latentBits))
	for _, bin := range infos {
		// weight-1 in ans_size_log bits
		w.WriteUint(uint64(bin.weight-1), ansSizeLog)
		// lower in latent_bits bits
		w.WriteUint(bin.lower, latentBits)
		// offset_bits
		w.WriteBitlen(bin.offsetBits, offsetBitsBits)
	}
	w.Flush()

	w.FinishByte()
	w.Flush()
	out := make([]byte, len(w.Bytes()))
	copy(out, w.Bytes())
	return out
}

// compressPage compresses a single page using ANS encoding.
func compressPage(latents []uint64, infos []binInfo, encoder *ansEncoder,
	lookup *binLookup, ansSizeLog Bitlen, latentBits Bitlen) []byte {

	n := len(latents)

	// Process in batches of FullBatchN.
	// ANS is LIFO, so we need to process each batch in reverse.
	type ansRecord struct {
		renormBits Bitlen
		stateBits  uint32 // low bits of old state to write
	}

	// For each batch, we accumulate:
	// 1. ANS records (written in reverse)
	// 2. Offsets (written forward)
	pw := NewBitWriter()

	// Page meta: NoOp delta → 0 delta state bits.
	// ANS final states: 4 × ansSizeLog bits.
	// We need to know the final ANS states, so we must process all batches first,
	// then write the page meta + body.

	// Pre-compute all bin indices and offsets.
	binIdxs := make([]int, n)
	offsets := make([]uint64, n)
	for i, lat := range latents {
		bi := lookup.find(lat)
		binIdxs[i] = bi
		offsets[i] = lat - infos[bi].lower
	}

	// Process ANS encoding in batches.
	type batchData struct {
		ansRecords []ansRecord
		offsetBits []Bitlen
		offsets    []uint64
		batchLen   int
	}

	var batches []batchData
	for batchStart := 0; batchStart < n; batchStart += FullBatchN {
		batchEnd := batchStart + FullBatchN
		if batchEnd > n {
			batchEnd = n
		}
		batchLen := batchEnd - batchStart

		bd := batchData{
			ansRecords: make([]ansRecord, batchLen),
			offsetBits: make([]Bitlen, batchLen),
			offsets:    make([]uint64, batchLen),
			batchLen:   batchLen,
		}

		for i := 0; i < batchLen; i++ {
			idx := batchStart + i
			bd.offsetBits[i] = infos[binIdxs[idx]].offsetBits
			bd.offsets[i] = offsets[idx]
		}

		batches = append(batches, bd)
	}

	// ANS encoding: process in the interleaved pattern matching the decoder.
	// The decoder reads ANS symbols forward, interleaved across 4 streams.
	// The encoder must produce symbols such that the decoder reconstructs them.
	//
	// For each batch, the decoder does:
	//   for i := 0; i < batchN; i++ {
	//     j := i % ANSInterleaving
	//     read from stream j
	//   }
	//
	// ANS is stack-based: the encoder processes in reverse order.
	// Within each interleaved stream, we process symbols in reverse.

	ansStates := [ANSInterleaving]uint32{}
	for i := range ansStates {
		ansStates[i] = encoder.tableSize // default state
	}

	// For each batch, encode ANS in reverse to get the records.
	for bi := len(batches) - 1; bi >= 0; bi-- {
		bd := &batches[bi]
		for i := bd.batchLen - 1; i >= 0; i-- {
			j := i % ANSInterleaving
			symbol := binIdxs[bi*FullBatchN+i]
			newState, renormBits := encoder.encode(ansStates[j], symbol)
			bd.ansRecords[i] = ansRecord{
				renormBits: renormBits,
				stateBits:  ansStates[j],
			}
			ansStates[j] = newState
		}
	}

	// Now write: page meta, then body.
	// Page meta: 0 delta state + 4 ANS final state indices.
	for i := 0; i < ANSInterleaving; i++ {
		pw.WriteUint(uint64(ansStates[i]-encoder.tableSize), ansSizeLog)
	}
	pw.FinishByte()

	// Body: for each batch, write ANS symbols + offsets.
	for _, bd := range batches {
		// Write ANS renormalization bits (forward order — matching decoder read order).
		if len(infos) > 1 {
			for i := 0; i < bd.batchLen; i++ {
				rec := bd.ansRecords[i]
				if rec.renormBits > 0 {
					pw.WriteUint(uint64(rec.stateBits)&maskForBits(rec.renormBits), rec.renormBits)
				}
			}
		}

		// Write offsets (forward order).
		for i := 0; i < bd.batchLen; i++ {
			ob := bd.offsetBits[i]
			if ob > 0 {
				pw.WriteUint(bd.offsets[i], ob)
			}
		}
	}

	pw.FinishByte()
	pw.Flush()
	out := make([]byte, len(pw.Bytes()))
	copy(out, pw.Bytes())
	return out
}

func minBitlen(a, b Bitlen) Bitlen {
	if a < b {
		return a
	}
	return b
}

func maxBitlen(a, b Bitlen) Bitlen {
	if a > b {
		return a
	}
	return b
}
