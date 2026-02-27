package pcodec

import "fmt"

// FormatVersion is the pco format version.
type FormatVersion struct {
	Major uint8
	Minor uint8
}

// ReadFormatVersion parses the format version from header bytes.
func ReadFormatVersion(data []byte) (FormatVersion, error) {
	if len(data) < 1 {
		return FormatVersion{}, fmt.Errorf("pcodec: empty header")
	}
	fv := FormatVersion{Major: data[0]}
	if fv.Major >= 4 && len(data) >= 2 {
		fv.Minor = data[1]
	}
	if fv.Major > 4 {
		return fv, fmt.Errorf("pcodec: unsupported format version %d.%d", fv.Major, fv.Minor)
	}
	return fv, nil
}

func (fv FormatVersion) supportsDeltaVariants() bool {
	return fv.Major >= 3
}

// --- Mode ---

// ModeKind identifies the compression mode for a chunk.
type ModeKind int

const (
	ModeClassic    ModeKind = iota // num = latent
	ModeIntMult                    // num = base * mult + adj
	ModeFloatMult                  // num = base * mult + adj_ulps
	ModeFloatQuant                 // num = from_bits(quantums << k + adj)
	ModeDict                       // num = dictionary[index]
)

// Mode describes how numbers are split into latent variables.
type Mode struct {
	Kind   ModeKind
	Base   uint64   // IntMult/FloatMult: base as latent-ordered u64
	QuantK Bitlen   // FloatQuant: quantization bits
	Dict   []uint64 // Dict: dictionary values (latent-ordered)
}

func readMode(r *BitReader, fv FormatVersion, latentType LatentType) (Mode, error) {
	latentBits := latentType.Bits()
	variant := r.ReadBitlen(BitsToEncodeModeVariant)
	switch variant {
	case 0:
		return Mode{Kind: ModeClassic}, nil
	case 1:
		if fv.Major == 0 {
			return Mode{}, fmt.Errorf("pcodec: cannot decompress v0 GCD encoding")
		}
		return Mode{Kind: ModeIntMult, Base: r.ReadUint(latentBits)}, nil
	case 2:
		return Mode{Kind: ModeFloatMult, Base: r.ReadUint(latentBits)}, nil
	case 3:
		return Mode{Kind: ModeFloatQuant, QuantK: r.ReadBitlen(BitsToEncodeQuantizeK)}, nil
	case 4:
		nUnique := int(r.ReadUint(BitsToEncodeDictLen))
		if err := r.DrainEmptyByte(); err != nil {
			return Mode{}, err
		}
		dict := make([]uint64, nUnique)
		for i := range dict {
			dict[i] = r.ReadUint(latentBits)
		}
		return Mode{Kind: ModeDict, Dict: dict}, nil
	default:
		return Mode{}, fmt.Errorf("pcodec: unknown mode variant %d", variant)
	}
}

// primaryLatentType returns the latent type for the primary latent variable.
func (m *Mode) primaryLatentType(numberLatentType LatentType) LatentType {
	if m.Kind == ModeDict {
		return LatentU32
	}
	return numberLatentType
}

// hasSecondary returns true if the mode produces a secondary latent variable.
func (m *Mode) hasSecondary() bool {
	return m.Kind == ModeIntMult || m.Kind == ModeFloatMult || m.Kind == ModeFloatQuant
}

// --- DeltaEncoding ---

// DeltaKind identifies the delta encoding variant.
type DeltaKind int

const (
	DeltaNoOp        DeltaKind = iota
	DeltaConsecutive           // order-N consecutive differences
	DeltaLookback              // LZ77-style lookback
	DeltaConv1                 // convolutional prediction
)

// LookbackConfig for lookback delta encoding.
type LookbackConfig struct {
	WindowNLog Bitlen
	StateNLog  Bitlen
}

func (c LookbackConfig) WindowN() int { return 1 << c.WindowNLog }
func (c LookbackConfig) StateN() int  { return 1 << c.StateNLog }

// Conv1Config for convolutional delta encoding.
type Conv1Config struct {
	Quantization Bitlen
	Bias         int64
	Weights      []int64
}

// DeltaEncoding describes how delta encoding was applied.
type DeltaEncoding struct {
	Kind               DeltaKind
	Order              int  // Consecutive: order
	SecondaryUsesDelta bool // Consecutive/Lookback
	Lookback           LookbackConfig
	Conv1              Conv1Config
}

func readDeltaEncoding(r *BitReader, fv FormatVersion) (DeltaEncoding, error) {
	if !fv.supportsDeltaVariants() {
		order := int(r.ReadUint(BitsToEncodeDeltaEncodingOrder))
		if order == 0 {
			return DeltaEncoding{Kind: DeltaNoOp}, nil
		}
		return DeltaEncoding{Kind: DeltaConsecutive, Order: order}, nil
	}

	variant := r.ReadBitlen(BitsToEncodeDeltaEncodingVariant)
	switch variant {
	case 0:
		return DeltaEncoding{Kind: DeltaNoOp}, nil
	case 1:
		order := int(r.ReadUint(BitsToEncodeDeltaEncodingOrder))
		if order == 0 {
			return DeltaEncoding{}, fmt.Errorf("pcodec: consecutive delta order must not be 0")
		}
		return DeltaEncoding{Kind: DeltaConsecutive, Order: order, SecondaryUsesDelta: r.ReadBool()}, nil
	case 2:
		windowNLog := 1 + r.ReadBitlen(BitsToEncodeDeltaLookbackWindowLog)
		stateNLog := r.ReadBitlen(BitsToEncodeDeltaLookbackStateLog)
		return DeltaEncoding{
			Kind:               DeltaLookback,
			SecondaryUsesDelta: r.ReadBool(),
			Lookback:           LookbackConfig{WindowNLog: windowNLog, StateNLog: stateNLog},
		}, nil
	case 3:
		quant := r.ReadBitlen(BitsToEncodeDeltaConvQuantization)
		biasRaw := r.ReadUint(64)
		bias := int64(biasRaw ^ (1 << 63)) // fromLatentOrdered for i64
		nWeights := 1 + int(r.ReadUint(BitsToEncodeDeltaConvNWeights))
		weights := make([]int64, nWeights)
		for i := range weights {
			wRaw := r.ReadUint(32)
			weights[i] = int64(int32(uint32(wRaw) ^ (1 << 31)))
		}
		return DeltaEncoding{
			Kind:  DeltaConv1,
			Conv1: Conv1Config{Quantization: quant, Bias: bias, Weights: weights},
		}, nil
	default:
		return DeltaEncoding{}, fmt.Errorf("pcodec: unknown delta encoding variant %d", variant)
	}
}

// LatentVarKey identifies which latent variable stream.
type LatentVarKey int

const (
	KeyDelta     LatentVarKey = iota
	KeyPrimary
	KeySecondary
)

// LatentVarDelta is the delta encoding for a specific latent variable.
type LatentVarDelta struct {
	Kind     DeltaKind
	Order    int
	Lookback LookbackConfig
	Conv1    Conv1Config
}

// NLatentsPerState returns how many delta state values are stored in page meta.
func (lvd *LatentVarDelta) NLatentsPerState() int {
	switch lvd.Kind {
	case DeltaNoOp:
		return 0
	case DeltaConsecutive:
		return lvd.Order
	case DeltaLookback:
		return lvd.Lookback.StateN()
	case DeltaConv1:
		return len(lvd.Conv1.Weights)
	default:
		return 0
	}
}

// latentVarDeltaEncoding returns the delta encoding for a specific latent var.
func (de *DeltaEncoding) latentVarDeltaEncoding(key LatentVarKey) LatentVarDelta {
	switch {
	case de.Kind == DeltaNoOp:
		return LatentVarDelta{Kind: DeltaNoOp}
	case key == KeyDelta:
		return LatentVarDelta{Kind: DeltaNoOp} // never recursive
	case key == KeyPrimary:
		return de.asLatentVarDelta()
	case key == KeySecondary && de.SecondaryUsesDelta:
		return de.asLatentVarDelta()
	default:
		return LatentVarDelta{Kind: DeltaNoOp}
	}
}

func (de *DeltaEncoding) asLatentVarDelta() LatentVarDelta {
	switch de.Kind {
	case DeltaConsecutive:
		return LatentVarDelta{Kind: DeltaConsecutive, Order: de.Order}
	case DeltaLookback:
		return LatentVarDelta{Kind: DeltaLookback, Lookback: de.Lookback}
	case DeltaConv1:
		return LatentVarDelta{Kind: DeltaConv1, Conv1: de.Conv1}
	default:
		return LatentVarDelta{Kind: DeltaNoOp}
	}
}

// hasExtraDeltaLatent returns true if a delta latent variable stream exists.
func (de *DeltaEncoding) hasExtraDeltaLatent() bool {
	return de.Kind == DeltaLookback
}

// --- ChunkLatentVarMeta ---

// ChunkLatentVarMeta is metadata for a single latent variable within a chunk.
type ChunkLatentVarMeta struct {
	AnsSizeLog Bitlen
	LatentType LatentType
	Bins       []Bin
}

func readChunkLatentVarMeta(r *BitReader, lt LatentType) (ChunkLatentVarMeta, error) {
	ansSizeLog := r.ReadBitlen(BitsToEncodeAnsSizeLog)
	nBins := int(r.ReadUint(BitsToEncodeNBins))

	if nBins > 0 && (1<<ansSizeLog) < nBins {
		return ChunkLatentVarMeta{}, fmt.Errorf("pcodec: ANS size log %d too small for %d bins", ansSizeLog, nBins)
	}
	if nBins == 1 && ansSizeLog > 0 {
		return ChunkLatentVarMeta{}, fmt.Errorf("pcodec: single bin but ANS size log is %d", ansSizeLog)
	}
	if ansSizeLog > MaxAnsBits {
		return ChunkLatentVarMeta{}, fmt.Errorf("pcodec: ANS size log %d exceeds max %d", ansSizeLog, MaxAnsBits)
	}

	latentBits := lt.Bits()
	offsetBitsBits := bitsToEncodeOffsetBits(latentBits)

	bins := make([]Bin, nBins)
	for i := range bins {
		weight := Weight(r.ReadUint(ansSizeLog)) + 1
		lower := r.ReadUint(latentBits)
		offsetBits := r.ReadBitlen(offsetBitsBits)
		if offsetBits > latentBits {
			return ChunkLatentVarMeta{}, fmt.Errorf("pcodec: offset bits %d exceeds latent bits %d", offsetBits, latentBits)
		}
		bins[i] = Bin{Weight: weight, Lower: lower, OffsetBits: offsetBits}
	}

	return ChunkLatentVarMeta{AnsSizeLog: ansSizeLog, LatentType: lt, Bins: bins}, nil
}

// --- ChunkMeta ---

// ChunkMeta is the metadata for a pco chunk.
type ChunkMeta struct {
	Mode          Mode
	DeltaEncoding DeltaEncoding
	Delta         *ChunkLatentVarMeta
	Primary       ChunkLatentVarMeta
	Secondary     *ChunkLatentVarMeta
}

// ReadChunkMeta parses a ChunkMeta from raw chunk_meta bytes.
func ReadChunkMeta(data []byte, fv FormatVersion, latentType LatentType) (*ChunkMeta, error) {
	// Pad for safe unaligned reads.
	padded := make([]byte, len(data)+16)
	copy(padded, data)
	r := NewBitReader(padded)

	mode, err := readMode(r, fv, latentType)
	if err != nil {
		return nil, err
	}

	de, err := readDeltaEncoding(r, fv)
	if err != nil {
		return nil, err
	}

	cm := &ChunkMeta{Mode: mode, DeltaEncoding: de}

	if de.hasExtraDeltaLatent() {
		meta, err := readChunkLatentVarMeta(r, LatentU32)
		if err != nil {
			return nil, fmt.Errorf("pcodec: delta latent var: %w", err)
		}
		cm.Delta = &meta
	}

	primaryLT := mode.primaryLatentType(latentType)
	primary, err := readChunkLatentVarMeta(r, primaryLT)
	if err != nil {
		return nil, fmt.Errorf("pcodec: primary latent var: %w", err)
	}
	cm.Primary = primary

	if mode.hasSecondary() {
		meta, err := readChunkLatentVarMeta(r, latentType)
		if err != nil {
			return nil, fmt.Errorf("pcodec: secondary latent var: %w", err)
		}
		cm.Secondary = &meta
	}

	if err := r.DrainEmptyByte(); err != nil {
		return nil, fmt.Errorf("pcodec: trailing bits in chunk meta: %w", err)
	}

	return cm, nil
}

// --- PageMeta ---

// PageLatentVarMeta stores per-page state for a latent variable.
type PageLatentVarMeta struct {
	DeltaState        []uint64
	AnsFinalStateIdxs [ANSInterleaving]uint32
}

// PageMeta is metadata for a single data page within a chunk.
type PageMeta struct {
	Delta     *PageLatentVarMeta
	Primary   PageLatentVarMeta
	Secondary *PageLatentVarMeta
}

func readPageLatentVarMeta(r *BitReader, lt LatentType, nDeltaState int, ansSizeLog Bitlen) PageLatentVarMeta {
	latentBits := lt.Bits()
	state := make([]uint64, nDeltaState)
	for i := range state {
		state[i] = r.ReadUint(latentBits)
	}
	var ansStates [ANSInterleaving]uint32
	for i := range ansStates {
		ansStates[i] = uint32(r.ReadUint(ansSizeLog))
	}
	return PageLatentVarMeta{DeltaState: state, AnsFinalStateIdxs: ansStates}
}

// ReadPageMeta parses page metadata from the beginning of a page buffer.
// Returns the PageMeta and the number of bytes consumed.
func ReadPageMeta(data []byte, cm *ChunkMeta) (*PageMeta, int, error) {
	padded := make([]byte, len(data)+16)
	copy(padded, data)
	r := NewBitReader(padded)

	pm := &PageMeta{}

	if cm.Delta != nil {
		lvd := cm.DeltaEncoding.latentVarDeltaEncoding(KeyDelta)
		meta := readPageLatentVarMeta(r, cm.Delta.LatentType, lvd.NLatentsPerState(), cm.Delta.AnsSizeLog)
		pm.Delta = &meta
	}

	{
		lvd := cm.DeltaEncoding.latentVarDeltaEncoding(KeyPrimary)
		pm.Primary = readPageLatentVarMeta(r, cm.Primary.LatentType, lvd.NLatentsPerState(), cm.Primary.AnsSizeLog)
	}

	if cm.Secondary != nil {
		lvd := cm.DeltaEncoding.latentVarDeltaEncoding(KeySecondary)
		meta := readPageLatentVarMeta(r, cm.Secondary.LatentType, lvd.NLatentsPerState(), cm.Secondary.AnsSizeLog)
		pm.Secondary = &meta
	}

	if err := r.DrainEmptyByte(); err != nil {
		return nil, 0, fmt.Errorf("pcodec: trailing bits in page meta: %w", err)
	}

	return pm, r.ByteOffset(), nil
}
