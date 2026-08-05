package encoding

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestUnpackFastLanesFastDifferential requires the table-driven kernel
// to produce byte-identical output to the naive oracle across element
// sizes, bit widths, offsets, and counts.
func TestUnpackFastLanesFastDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, elemSize := range []int{1, 2, 4, 8} {
		tBits := elemSize * 8
		for bw := 1; bw <= tBits; bw++ {
			nLanes := fastlanesBlockSize / tBits
			wordsPerBlock := nLanes * bw
			for _, blocks := range []int{1, 3} {
				packed := make([]byte, blocks*wordsPerBlock*elemSize)
				rng.Read(packed)
				total := blocks * fastlanesBlockSize
				for _, tc := range []struct{ offset, count int }{
					{0, total},
					{0, total - 17},
					{5, total - 5},
					{fastlanesBlockSize + 3, total - fastlanesBlockSize - 40},
					{0, 1},
				} {
					if tc.count <= 0 || tc.offset+tc.count > total {
						continue
					}
					want, err := unpackFastLanesTyped(packed, bw, tc.offset, tc.count, elemSize)
					if err != nil {
						t.Fatalf("oracle: %v", err)
					}
					got, err := unpackFastLanesFast(packed, bw, tc.offset, tc.count, elemSize)
					if err != nil {
						t.Fatalf("fast: %v", err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("mismatch elemSize=%d bw=%d offset=%d count=%d", elemSize, bw, tc.offset, tc.count)
					}
				}
			}
		}
	}
}

func BenchmarkUnpackFastLanes(b *testing.B) {
	const elemSize, bw = 8, 3 // Sev-like: i64 column, 5 distinct values
	nLanes := fastlanesBlockSize / (elemSize * 8)
	blocks := 8 // 8192 rows = one scan chunk
	packed := make([]byte, blocks*nLanes*bw*elemSize)
	rand.New(rand.NewSource(1)).Read(packed)
	count := blocks * fastlanesBlockSize

	b.Run("naive", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			unpackFastLanesTyped(packed, bw, 0, count, elemSize)
		}
	})
	b.Run("fast", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			unpackFastLanesFast(packed, bw, 0, count, elemSize)
		}
	})
}
