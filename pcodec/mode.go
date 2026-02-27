package pcodec

import (
	"fmt"
	"math"
)

// JoinLatents applies the mode's inverse transformation to reconstruct
// number values from primary (and optional secondary) latent slices.
// The result is written to dst as latent-ordered uint64 values.
func JoinLatents(mode *Mode, primary []uint64, secondary []uint64, dst []uint64, latentBits Bitlen) error {
	switch mode.Kind {
	case ModeClassic:
		return joinClassic(primary, dst)
	case ModeIntMult:
		return joinIntMult(mode.Base, primary, secondary, dst, latentBits)
	case ModeFloatMult:
		return joinFloatMult(mode.Base, primary, secondary, dst, latentBits)
	case ModeFloatQuant:
		return joinFloatQuant(mode.QuantK, primary, secondary, dst, latentBits)
	case ModeDict:
		return joinDict(mode.Dict, primary, dst)
	default:
		return fmt.Errorf("pcodec: unknown mode kind %d", mode.Kind)
	}
}

// Classic: num = latent (identity).
func joinClassic(primary []uint64, dst []uint64) error {
	copy(dst, primary)
	return nil
}

// IntMult: num = base * mult + adj.
func joinIntMult(base uint64, primary []uint64, secondary []uint64, dst []uint64, latentBits Bitlen) error {
	if secondary == nil {
		return fmt.Errorf("pcodec: IntMult requires secondary latents")
	}
	mask := maskForBits(latentBits)
	for i := range dst {
		// wrapping multiply and add, matching Rust semantics.
		dst[i] = ((primary[i] * base) + secondary[i]) & mask
	}
	return nil
}

// FloatMult: num = from_latent_ordered(to_latent_ordered(int_float_from_latent(mult) * base) + toggle_center(adj))
// For float mult, the base is stored as latent-ordered. We convert mult to an
// integer-valued float, multiply by the float base, then adjust.
func joinFloatMult(baseLat uint64, primary []uint64, secondary []uint64, dst []uint64, latentBits Bitlen) error {
	if secondary == nil {
		return fmt.Errorf("pcodec: FloatMult requires secondary latents")
	}

	switch latentBits {
	case 32:
		baseF := intFloatFromLatent32(uint32(baseLat))
		for i := range dst {
			unadjustedF := intFloatFromLatent32(uint32(primary[i])) * baseF
			unadjustedLat := uint64(math.Float32bits(unadjustedF))
			adj := toggleCenter(secondary[i], 32)
			dst[i] = (unadjustedLat + adj) & maskForBits(32)
		}
	case 64:
		baseF := intFloatFromLatent64(baseLat)
		for i := range dst {
			unadjustedF := intFloatFromLatent64(primary[i]) * baseF
			unadjustedLat := math.Float64bits(unadjustedF)
			adj := toggleCenter(secondary[i], 64)
			dst[i] = unadjustedLat + adj
		}
	default:
		return fmt.Errorf("pcodec: FloatMult not supported for %d-bit latents", latentBits)
	}
	return nil
}

// intFloatFromLatent32 converts a latent-ordered u32 to the "integer float"
// representation: maps latent to a float that represents the integer magnitude
// with sign.
func intFloatFromLatent32(l uint32) float32 {
	mid := uint32(1 << 31)
	if l >= mid {
		return float32(l - mid)
	}
	return -float32(mid - l)
}

// intFloatFromLatent64 converts a latent-ordered u64 to the integer float.
func intFloatFromLatent64(l uint64) float64 {
	mid := uint64(1 << 63)
	if l >= mid {
		return float64(l - mid)
	}
	return -float64(mid - l)
}

// FloatQuant: num = from_bits((quantums << k) + lowest_k_bits)
// where lowest_k_bits depends on sign.
func joinFloatQuant(k Bitlen, primary []uint64, secondary []uint64, dst []uint64, latentBits Bitlen) error {
	if secondary == nil {
		return fmt.Errorf("pcodec: FloatQuant requires secondary latents")
	}

	signCutoff := (uint64(1) << (latentBits - 1)) >> k
	lowestKBitsMax := (uint64(1) << k) - 1

	for i := range dst {
		y := primary[i]
		m := secondary[i]
		isPosAsFloat := y >= signCutoff
		var lowestKBits uint64
		if isPosAsFloat {
			lowestKBits = m
		} else {
			lowestKBits = lowestKBitsMax - m
		}
		dst[i] = (y << k) + lowestKBits
	}
	return nil
}

// Dict: num = dictionary[index].
func joinDict(dict []uint64, primary []uint64, dst []uint64) error {
	dictLen := uint64(len(dict))
	for i := range dst {
		idx := primary[i]
		if idx >= dictLen {
			return fmt.Errorf("pcodec: dict index %d exceeds dict length %d", idx, dictLen)
		}
		dst[i] = dict[idx]
	}
	return nil
}
