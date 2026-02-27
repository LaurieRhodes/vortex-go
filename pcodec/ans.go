package pcodec

import "math/bits"

// AnsNode is a single entry in the tANS decoding table.
type AnsNode struct {
	NextStateIdxBase uint16
	OffsetBits       uint8
	BitsToRead       uint8
}

// AnsDecoder holds the precomputed tANS decoding table.
type AnsDecoder struct {
	Nodes []AnsNode
}

// spreadSymbols implements the Rust pco spread function.
// It assigns symbol indices to table positions using a stride of ~3/5 the
// table size (made odd).
func spreadSymbols(tableSize int, weights []Weight) []uint32 {
	stride := uint32(3*tableSize) / 5
	if stride%2 == 0 {
		stride++
	}
	sizeMask := uint32(tableSize - 1) // tableSize is always a power of 2

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

// BuildAnsDecoder constructs the ANS decoding table from chunk metadata.
// The state_lowers array (mapping table index → bin lower bound) is also
// returned since it's needed during page decompression.
func BuildAnsDecoder(ansSizeLog Bitlen, binSlice []Bin) (*AnsDecoder, []uint64) {
	if len(binSlice) == 0 {
		// Degenerate: no bins.
		return &AnsDecoder{Nodes: []AnsNode{{}}}, []uint64{0}
	}

	tableSize := 1 << ansSizeLog
	if tableSize == 0 {
		return &AnsDecoder{}, nil
	}

	// Build weights and offset_bits arrays.
	symbolWeights := make([]Weight, len(binSlice))
	binOffsetBits := make([]Bitlen, len(binSlice))
	for i, bin := range binSlice {
		symbolWeights[i] = bin.Weight
		binOffsetBits[i] = bin.OffsetBits
	}

	stateSymbols := spreadSymbols(tableSize, symbolWeights)

	// Build state_lowers: for each table entry, the lower bound of the
	// corresponding bin.
	stateLowers := make([]uint64, tableSize)
	for i, sym := range stateSymbols {
		stateLowers[i] = binSlice[sym].Lower
	}

	// Build decoding nodes.
	// xs tracks per-symbol running count (starts at the weight).
	xs := make([]uint32, len(binSlice))
	copy(xs, symbolWeights)

	tableSizeU32 := uint32(tableSize)
	tableSizeLZ := Bitlen(bits.LeadingZeros32(tableSizeU32))

	nodes := make([]AnsNode, tableSize)
	for i, symbol := range stateSymbols {
		nextStateBase := xs[symbol]
		bitsToRead := Bitlen(bits.LeadingZeros32(nextStateBase)) - tableSizeLZ
		nextStateBase <<= bitsToRead

		offsetBits := Bitlen(0)
		if int(symbol) < len(binOffsetBits) {
			offsetBits = binOffsetBits[symbol]
		}

		nodes[i] = AnsNode{
			NextStateIdxBase: uint16(nextStateBase - tableSizeU32),
			OffsetBits:       uint8(offsetBits),
			BitsToRead:       uint8(bitsToRead),
		}
		xs[symbol]++
	}

	return &AnsDecoder{Nodes: nodes}, stateLowers
}
