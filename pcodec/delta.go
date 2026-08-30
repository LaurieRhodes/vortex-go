package pcodec

// delta.go implements the three delta decoding strategies: consecutive,
// lookback, and conv1. All operate in-place on latent []uint64 slices.

// toggleCenterSlice applies wrapping add of MID to each element.
func toggleCenterSlice(latents []uint64, latentBits Bitlen) {
	mid := uint64(1) << (latentBits - 1)
	mask := maskForBits(latentBits)
	for i := range latents {
		latents[i] = (latents[i] + mid) & mask
	}
}

// --- Consecutive delta decoding ---

// DecodeConsecutive applies order-N consecutive delta decoding in-place.
// deltaMoments is the running state (modified in-place across batches).
func DecodeConsecutive(deltaMoments []uint64, latents []uint64, latentBits Bitlen) {
	toggleCenterSlice(latents, latentBits)
	mask := maskForBits(latentBits)
	// Apply first-order decode for each order level, from highest to lowest.
	for o := len(deltaMoments) - 1; o >= 0; o-- {
		moment := deltaMoments[o]
		for i := range latents {
			tmp := latents[i]
			latents[i] = moment & mask
			moment = (moment + tmp) & mask
		}
		deltaMoments[o] = moment
	}
}

// --- Lookback delta decoding ---

// LookbackState holds the window buffer and position for lookback decoding.
type LookbackState struct {
	WindowBuffer []uint64
	Pos          int
	Config       LookbackConfig
	LatentBits   Bitlen
}

// NewLookbackState creates a new lookback state from the page delta state.
func NewLookbackState(config LookbackConfig, deltaState []uint64, latentBits Bitlen) *LookbackState {
	windowN := config.WindowN()
	bufferN := maxI(windowN, FullBatchN) * 2
	buf := make([]uint64, bufferN)
	// State fills the end of the window.
	stateN := len(deltaState)
	copy(buf[windowN-stateN:windowN], deltaState)
	return &LookbackState{
		WindowBuffer: buf,
		Pos:          windowN,
		Config:       config,
		LatentBits:   latentBits,
	}
}

// DecodeLookback applies lookback delta decoding in-place.
// lookbacks is the decoded delta latent slice (lookback indices).
func DecodeLookback(state *LookbackState, lookbacks []uint64, latents []uint64) error {
	toggleCenterSlice(latents, state.LatentBits)

	windowN := state.Config.WindowN()
	mask := maskForBits(state.LatentBits)
	batchN := len(latents)

	// Cycle buffer if needed.
	if state.Pos+batchN > len(state.WindowBuffer) {
		copy(state.WindowBuffer, state.WindowBuffer[state.Pos-windowN:state.Pos])
		state.Pos = windowN
	}

	for i := 0; i < batchN && i < len(lookbacks); i++ {
		lookback := int(lookbacks[i])
		if lookback > windowN || lookback == 0 {
			lookback = 1 // corrupt data — don't panic, just clamp
		}
		pos := state.Pos + i
		prev := state.WindowBuffer[pos-lookback]
		state.WindowBuffer[pos] = (latents[i] + prev) & mask
	}

	// Handle any remaining latents beyond lookbacks (end of page).
	for i := len(lookbacks); i < batchN; i++ {
		pos := state.Pos + i
		state.WindowBuffer[pos] = latents[i]
	}

	// Copy decoded values back. The state_n offset accounts for the fact
	// that the first state_n values in the window are pre-populated state.
	stateN := state.Config.StateN()
	endPos := state.Pos + batchN
	copy(latents, state.WindowBuffer[state.Pos-stateN:endPos-stateN])
	state.Pos = endPos

	return nil
}

// --- Conv1 delta decoding ---

// Conv1State holds the running state for conv1 delta decoding.
type Conv1State struct {
	State      []uint64 // running state (len = order)
	Config     Conv1Config
	LatentBits Bitlen
}

// NewConv1State creates a new conv1 state from page delta state.
func NewConv1State(config Conv1Config, deltaState []uint64, latentBits Bitlen) *Conv1State {
	state := make([]uint64, len(config.Weights))
	copy(state, deltaState)
	return &Conv1State{State: state, Config: config, LatentBits: latentBits}
}

// DecodeConv1 applies convolutional delta decoding in-place.
func DecodeConv1(cs *Conv1State, latents []uint64) {
	toggleCenterSlice(latents, cs.LatentBits)
	mask := maskForBits(cs.LatentBits)

	order := len(cs.Config.Weights)
	// Build a working buffer: [state | latents]
	buf := make([]uint64, order+len(latents))
	copy(buf[:order], cs.State)
	copy(buf[order:], latents)

	// Decode residuals: each value += prediction of preceding elements.
	for i := order; i < len(buf); i++ {
		pred := predictOne(buf[i-order:i], cs.Config.Weights, cs.Config.Bias, cs.Config.Quantization, cs.LatentBits)
		buf[i] = (buf[i] + pred) & mask
	}

	// Copy results back.
	copy(latents, buf[order:])
	// Update state to the last `order` values.
	copy(cs.State, buf[len(buf)-order:])
}

// predictOne computes a single conv1 prediction.
// weights and bias operate in signed space (Conv type).
func predictOne(preceding []uint64, weights []int64, bias int64, quantization Bitlen, latentBits Bitlen) uint64 {
	// Conv type mapping: u8→i16, u16→i32, u32→i64, u64→i64
	// We use int64 for all since it's the largest accumulator.
	var s = bias
	for j, w := range weights {
		// Convert latent to its Conv representation.
		// For u8/u16/u32 this is a zero-extending cast; for u64 it's reinterpret.
		l := toConv(preceding[j], latentBits)
		s += w * l
	}
	if s < 0 {
		s = 0
	}
	s >>= quantization
	return fromConv(s, latentBits)
}

// toConv converts a latent uint64 to its Conv (signed) representation.
func toConv(val uint64, latentBits Bitlen) int64 {
	switch latentBits {
	case 8:
		return int64(int16(uint8(val)))
	case 16:
		return int64(int32(uint16(val)))
	case 32:
		return int64(uint32(val)) // zero-extend u32 to i64
	case 64:
		return int64(val) // reinterpret
	default:
		return int64(val)
	}
}

// fromConv converts a Conv (signed) value back to a latent uint64.
func fromConv(val int64, latentBits Bitlen) uint64 {
	mask := maskForBits(latentBits)
	return uint64(val) & mask
}
