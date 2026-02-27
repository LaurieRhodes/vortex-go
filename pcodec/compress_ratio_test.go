package pcodec_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
	
	"github.com/LaurieRhodes/vortex-go/pcodec"
)

func TestPcoCompressionRatio(t *testing.T) {
	tests := []struct {
		name string
		nt   pcodec.NumberType
		data func() []byte
	}{
		{
			name: "sequential_i32_1000",
			nt:   pcodec.NumI32,
			data: func() []byte {
				buf := make([]byte, 1000*4)
				for i := 0; i < 1000; i++ {
					binary.LittleEndian.PutUint32(buf[i*4:], uint32(int32(i*10+5000)))
				}
				return buf
			},
		},
		{
			name: "clustered_f64_1000",
			nt:   pcodec.NumF64,
			data: func() []byte {
				buf := make([]byte, 1000*8)
				for i := 0; i < 1000; i++ {
					v := 100.0 + float64(i)*0.01 // 100.00..109.99
					binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
				}
				return buf
			},
		},
		{
			name: "random_f64_1000",
			nt:   pcodec.NumF64,
			data: func() []byte {
				buf := make([]byte, 1000*8)
				for i := 0; i < 1000; i++ {
					v := float64(i*i) * 1.23456 // wide range
					binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
				}
				return buf
			},
		},
		{
			name: "constant_i32_1000",
			nt:   pcodec.NumI32,
			data: func() []byte {
				buf := make([]byte, 1000*4)
				for i := 0; i < 1000; i++ {
					binary.LittleEndian.PutUint32(buf[i*4:], 42)
				}
				return buf
			},
		},
		{
			name: "narrow_i64_1000",
			nt:   pcodec.NumI64,
			data: func() []byte {
				buf := make([]byte, 1000*8)
				for i := 0; i < 1000; i++ {
					binary.LittleEndian.PutUint64(buf[i*8:], uint64(int64(1000000+i%100)))
				}
				return buf
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.data()
			rawSize := len(data)
			
			for _, level := range []int{0, 6} {
				result, err := pcodec.Compress(data, tt.nt, pcodec.CompressOptions{Level: level})
				if err != nil {
					t.Fatalf("level %d: %v", level, err)
				}
				
				compSize := len(result.Header)
				for _, cm := range result.ChunkMetas {
					compSize += len(cm)
				}
				for _, p := range result.Pages {
					compSize += len(p)
				}
				ratio := float64(compSize) / float64(rawSize) * 100
				t.Logf("level=%d: raw=%d compressed=%d ratio=%.1f%%", level, rawSize, compSize, ratio)
				
				// Verify round-trip.
				lt := tt.nt.LatentType()
				pagesPerChunk := result.PageCounts
				latents, err := pcodec.Decompress(result.Header, result.ChunkMetas, result.Pages, pagesPerChunk, result.PageNs, lt)
				if err != nil {
					t.Fatalf("level=%d decompress: %v", level, err)
				}
				decoded := pcodec.LatentsToBytes(tt.nt, latents)
				if len(decoded) != len(data) {
					t.Fatalf("level=%d: decoded %d bytes, want %d", level, len(decoded), len(data))
				}
				for i := range data {
					if decoded[i] != data[i] {
						t.Fatalf("level=%d: mismatch at byte %d", level, i)
					}
				}
			}
			fmt.Println()
		})
	}
}

func TestPcoMultiBinBenefit(t *testing.T) {
	// Bimodal data: values clustered in two distant groups.
	// Baseline needs bits for full range, multi-bin can use 2 narrow bins.
	n := 2000
	buf := make([]byte, n*4)
	for i := 0; i < n; i++ {
		var v int32
		if i%2 == 0 {
			v = int32(100 + i%20)   // cluster 1: [100, 119]
		} else {
			v = int32(50000 + i%20) // cluster 2: [50000, 50019]
		}
		binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
	}

	for _, level := range []int{0, 6} {
		result, err := pcodec.Compress(buf, pcodec.NumI32, pcodec.CompressOptions{Level: level})
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		compSize := len(result.Header)
		for _, cm := range result.ChunkMetas {
			compSize += len(cm)
		}
		for _, p := range result.Pages {
			compSize += len(p)
		}
		ratio := float64(compSize) / float64(len(buf)) * 100
		t.Logf("level=%d: raw=%d compressed=%d ratio=%.1f%%", level, len(buf), compSize, ratio)

		// Verify round-trip.
		latents, err := pcodec.Decompress(result.Header, result.ChunkMetas, result.Pages, result.PageCounts, result.PageNs, pcodec.LatentU32)
		if err != nil {
			t.Fatalf("level=%d decompress: %v", level, err)
		}
		decoded := pcodec.LatentsToBytes(pcodec.NumI32, latents)
		for i := range buf {
			if decoded[i] != buf[i] {
				t.Fatalf("level=%d: mismatch at byte %d", level, i)
			}
		}
	}
}
