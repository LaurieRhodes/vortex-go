package pcodec

import (
	"encoding/binary"
	"fmt"
)

// pageLatentState holds the per-page mutable state for a single latent variable
// during decompression. It tracks ANS state indices and delta decoding state.
type pageLatentState struct {
	ansStateIdxs [ANSInterleaving]uint32
	ld           *latentDecompressor
	deltaState   interface{} // []uint64 (consecutive), *LookbackState, or *Conv1State
}

// ChunkDecompressor holds precomputed state for decompressing pages within a chunk.
type ChunkDecompressor struct {
	ChunkMeta  *ChunkMeta
	LatentType LatentType

	// Per-latent-variable decompressor state.
	DeltaLD   *latentDecompressor
	PrimaryLD *latentDecompressor
	SecondLD  *latentDecompressor
}

// latentDecompressor holds the ANS decoder and per-chunk precomputed state
// for a single latent variable.
type latentDecompressor struct {
	Decoder     *AnsDecoder
	StateLowers []uint64 // table_size entries: lower bound for each ANS state
	NBins       int
	AnsSizeLog  Bitlen
	LatentBits  Bitlen
	DeltaEnc    LatentVarDelta

	// Scratch space for a single batch (reused across batches).
	offsetBitsCsum [FullBatchN]uint32
	offsetBits     [FullBatchN]uint32
	latents        [FullBatchN]uint64
}

// NewChunkDecompressor builds a ChunkDecompressor from parsed ChunkMeta.
func NewChunkDecompressor(cm *ChunkMeta, latentType LatentType) *ChunkDecompressor {
	cd := &ChunkDecompressor{ChunkMeta: cm, LatentType: latentType}

	if cm.Delta != nil {
		cd.DeltaLD = newLatentDecompressor(cm.Delta,
			cm.DeltaEncoding.latentVarDeltaEncoding(KeyDelta))
	}

	cd.PrimaryLD = newLatentDecompressor(&cm.Primary,
		cm.DeltaEncoding.latentVarDeltaEncoding(KeyPrimary))

	if cm.Secondary != nil {
		cd.SecondLD = newLatentDecompressor(cm.Secondary,
			cm.DeltaEncoding.latentVarDeltaEncoding(KeySecondary))
	}

	return cd
}

func newLatentDecompressor(meta *ChunkLatentVarMeta, de LatentVarDelta) *latentDecompressor {
	decoder, stateLowers := BuildAnsDecoder(meta.AnsSizeLog, meta.Bins)

	ld := &latentDecompressor{
		Decoder:     decoder,
		StateLowers: stateLowers,
		NBins:       len(meta.Bins),
		AnsSizeLog:  meta.AnsSizeLog,
		LatentBits:  meta.LatentType.Bits(),
		DeltaEnc:    de,
	}

	// For trivial (0 or 1 bin) cases, pre-fill scratch.
	if len(meta.Bins) == 1 {
		bin := meta.Bins[0]
		var csum uint32
		for i := 0; i < FullBatchN; i++ {
			ld.offsetBits[i] = bin.OffsetBits
			ld.offsetBitsCsum[i] = csum
			ld.latents[i] = bin.Lower
			csum += bin.OffsetBits
		}
	}

	return ld
}

// DecompressPage decompresses a single page of pco data.
//
// Parameters:
//   - pageData: raw page bytes (page metadata + compressed body)
//   - cm: the parsed ChunkMeta for this chunk
//   - cd: the ChunkDecompressor for this chunk
//   - nValues: number of values in this page
//   - latentBits: the latent bit width for the number type
//
// Returns the decompressed values as latent-ordered uint64 slice.
func DecompressPage(pageData []byte, cm *ChunkMeta, cd *ChunkDecompressor, nValues int, latentBits Bitlen) ([]uint64, error) {
	// Parse page metadata.
	pm, metaBytes, err := ReadPageMeta(pageData, cm)
	if err != nil {
		return nil, fmt.Errorf("pcodec: page meta: %w", err)
	}

	// Create the bit reader over the page body (after metadata).
	body := pageData[metaBytes:]
	padded := make([]byte, len(body)+16)
	copy(padded, body)
	reader := NewBitReader(padded)

	// Initialize per-page delta state for each latent variable.
	initPageState := func(plm *PageLatentVarMeta, ld *latentDecompressor) *pageLatentState {
		pls := &pageLatentState{
			ansStateIdxs: plm.AnsFinalStateIdxs,
			ld:           ld,
		}
		switch ld.DeltaEnc.Kind {
		case DeltaConsecutive:
			state := make([]uint64, len(plm.DeltaState))
			copy(state, plm.DeltaState)
			pls.deltaState = state
		case DeltaLookback:
			pls.deltaState = NewLookbackState(ld.DeltaEnc.Lookback, plm.DeltaState, ld.LatentBits)
		case DeltaConv1:
			pls.deltaState = NewConv1State(ld.DeltaEnc.Conv1, plm.DeltaState, ld.LatentBits)
		}
		return pls
	}

	var deltaPS *pageLatentState
	if cd.DeltaLD != nil && pm.Delta != nil {
		deltaPS = initPageState(pm.Delta, cd.DeltaLD)
	}

	primaryPS := initPageState(&pm.Primary, cd.PrimaryLD)

	var secondPS *pageLatentState
	if cd.SecondLD != nil && pm.Secondary != nil {
		secondPS = initPageState(pm.Secondary, cd.SecondLD)
	}

	// Allocate output.
	result := make([]uint64, nValues)

	// Process in batches of FullBatchN.
	nRemaining := nValues
	nDeltaLatentsPerState := cd.PrimaryLD.DeltaEnc.NLatentsPerState()
	offset := 0

	for nRemaining > 0 {
		batchN := minI(FullBatchN, nRemaining)

		// 1. Decompress delta latents (if lookback mode).
		var deltaLatents []uint64
		if deltaPS != nil {
			limit := minI(maxI(nRemaining-nDeltaLatentsPerState, 0), batchN)
			readBatchANS(reader, deltaPS, cd.DeltaLD, limit)
			readBatchOffsets(reader, cd.DeltaLD, limit)
			// Delta latents don't have delta encoding applied to them
			// (DeltaEnc is always NoOp for the delta variable).
			deltaLatents = cd.DeltaLD.latents[:batchN]
		}

		// 2. Decompress primary latents.
		primaryPreDeltaLen := minI(maxI(nRemaining-nDeltaLatentsPerState, 0), batchN)
		readBatchANS(reader, primaryPS, cd.PrimaryLD, primaryPreDeltaLen)
		readBatchOffsets(reader, cd.PrimaryLD, primaryPreDeltaLen)
		applyDelta(primaryPS, cd.PrimaryLD, deltaLatents, batchN, nRemaining)
		primaryBatch := make([]uint64, batchN)
		copy(primaryBatch, cd.PrimaryLD.latents[:batchN])

		// 3. Decompress secondary latents (if present).
		var secondaryBatch []uint64
		if secondPS != nil {
			secPreDeltaLen := minI(maxI(nRemaining-nDeltaLatentsPerState, 0), batchN)
			readBatchANS(reader, secondPS, cd.SecondLD, secPreDeltaLen)
			readBatchOffsets(reader, cd.SecondLD, secPreDeltaLen)
			applyDelta(secondPS, cd.SecondLD, deltaLatents, batchN, nRemaining)
			secondaryBatch = make([]uint64, batchN)
			copy(secondaryBatch, cd.SecondLD.latents[:batchN])
		}

		// 4. Join latents via mode.
		if err := JoinLatents(&cm.Mode, primaryBatch, secondaryBatch,
			result[offset:offset+batchN], latentBits); err != nil {
			return nil, err
		}

		offset += batchN
		nRemaining -= batchN
	}

	// Drain trailing bits.
	if err := reader.DrainEmptyByte(); err != nil {
		// Not fatal — some pages may have exact byte alignment.
		_ = err
	}

	return result, nil
}

// readBatchANS reads ANS symbols for a batch, populating ld.latents and
// ld.offsetBits/offsetBitsCsum.
func readBatchANS(reader *BitReader, ps *pageLatentState, ld *latentDecompressor, batchN int) {
	if batchN == 0 {
		return
	}
	if ld.NBins <= 1 {
		// Trivial case: reset latents to the pre-filled lower for this batch.
		// The offsetBits/offsetBitsCsum arrays are already correct from the
		// constructor since they're the same for every element.
		if ld.NBins == 1 {
			lower := ld.StateLowers[0]
			for i := 0; i < batchN; i++ {
				ld.latents[i] = lower
			}
		}
		return
	}

	nodes := ld.Decoder.Nodes
	stateLowers := ld.StateLowers
	stateIdxs := &ps.ansStateIdxs
	var offsetBitIdx uint32

	for i := 0; i < batchN; i++ {
		j := i % ANSInterleaving
		stateIdx := stateIdxs[j]

		node := &nodes[stateIdx]
		bitsToRead := Bitlen(node.BitsToRead)
		ansVal := uint32(reader.ReadUint(bitsToRead))

		ld.latents[i] = stateLowers[stateIdx]
		ob := Bitlen(node.OffsetBits)
		ld.offsetBitsCsum[i] = offsetBitIdx
		ld.offsetBits[i] = ob
		offsetBitIdx += ob

		stateIdxs[j] = uint32(node.NextStateIdxBase) + ansVal
	}
}

// readBatchOffsets reads the per-symbol offset values and adds them to the
// base latent values.
func readBatchOffsets(reader *BitReader, ld *latentDecompressor, batchN int) {
	if batchN == 0 {
		return
	}

	mask := maskForBits(ld.LatentBits)

	for i := 0; i < batchN; i++ {
		ob := ld.offsetBits[i]
		if ob == 0 {
			continue
		}
		offset := reader.ReadUint(ob)
		ld.latents[i] = (ld.latents[i] + offset) & mask
	}
}

// applyDelta applies the appropriate delta decoding to a batch of latents.
func applyDelta(ps *pageLatentState, ld *latentDecompressor, deltaLatents []uint64, batchN int, nRemaining int) {
	dst := ld.latents[:batchN]

	switch ld.DeltaEnc.Kind {
	case DeltaNoOp:
		// Nothing to do.
	case DeltaConsecutive:
		moments := ps.deltaState.([]uint64)
		DecodeConsecutive(moments, dst, ld.LatentBits)
	case DeltaLookback:
		state := ps.deltaState.(*LookbackState)
		_ = DecodeLookback(state, deltaLatents, dst)
	case DeltaConv1:
		state := ps.deltaState.(*Conv1State)
		DecodeConv1(state, dst)
	}
}

// --- High-level API ---

// Decompress decompresses a complete pco-encoded array.
//
// Parameters:
//   - header: the pco file header bytes (format version)
//   - chunkMetas: raw chunk metadata buffers (one per chunk)
//   - pages: raw page data buffers (ordered: all pages of chunk 0, then chunk 1, etc.)
//   - pagesPerChunk: number of pages in each chunk
//   - pageNValues: number of values in each page (same ordering as pages)
//   - latentType: the LatentType of the number type being decompressed
//
// Returns the decompressed values as latent-ordered uint64 values.
func Decompress(
	header []byte,
	chunkMetas [][]byte,
	pages [][]byte,
	pagesPerChunk []int,
	pageNValues []int,
	latentType LatentType,
) ([]uint64, error) {
	fv, err := ReadFormatVersion(header)
	if err != nil {
		return nil, err
	}

	latentBits := latentType.Bits()
	var result []uint64
	pageIdx := 0

	for chunkIdx, cmData := range chunkMetas {
		cm, err := ReadChunkMeta(cmData, fv, latentType)
		if err != nil {
			return nil, fmt.Errorf("pcodec: chunk %d: %w", chunkIdx, err)
		}

		cd := NewChunkDecompressor(cm, latentType)

		nPages := pagesPerChunk[chunkIdx]
		for p := 0; p < nPages; p++ {
			nv := pageNValues[pageIdx]
			vals, err := DecompressPage(pages[pageIdx], cm, cd, nv, latentBits)
			if err != nil {
				return nil, fmt.Errorf("pcodec: chunk %d page %d: %w", chunkIdx, p, err)
			}
			result = append(result, vals...)
			pageIdx++
		}
	}

	return result, nil
}

// LatentsToBytes converts latent-ordered uint64 values to their final byte
// representation for the given NumberType, applying fromLatentOrdered.
func LatentsToBytes(nt NumberType, latents []uint64) []byte {
	bs := nt.ByteSize()
	out := make([]byte, len(latents)*bs)
	for i, lat := range latents {
		v := fromLatentOrdered(nt, lat)
		switch bs {
		case 2:
			binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
		case 4:
			binary.LittleEndian.PutUint32(out[i*4:], uint32(v))
		case 8:
			binary.LittleEndian.PutUint64(out[i*8:], v)
		}
	}
	return out
}
