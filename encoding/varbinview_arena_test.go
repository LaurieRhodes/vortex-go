package encoding

import (
	"encoding/binary"
	"strings"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// viewArray builds a flat VarBinView array: strings of up to 12 bytes
// inline, longer ones out of line across two data buffers.
func viewArray(vals []string) *vortex.Array {
	views := make([]byte, 16*len(vals))
	data := [][]byte{nil, nil}
	for i, s := range vals {
		v := views[i*16:]
		binary.LittleEndian.PutUint32(v[0:4], uint32(len(s)))
		if len(s) <= 12 {
			copy(v[4:16], s)
			continue
		}
		b := i % 2
		copy(v[4:8], s[:4])
		binary.LittleEndian.PutUint32(v[8:12], uint32(b))
		binary.LittleEndian.PutUint32(v[12:16], uint32(len(data[b])))
		data[b] = append(data[b], s...)
	}
	return &vortex.Array{
		DType:   &vortex.DType{Kind: vortex.DTypeUtf8},
		Length:  len(vals),
		Buffers: [][]byte{views, data[0], data[1]},
	}
}

func TestStringValuesArenaMatchesStringValues(t *testing.T) {
	vals := []string{"", "a", "twelve bytes", "thirteen byte", "", strings.Repeat("x", 300),
		"Ärger über Öl", "short", strings.Repeat("yz", 40), ""}
	arr := viewArray(vals)
	want, err := StringValues(arr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := StringValuesArena(arr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(vals) {
		t.Fatalf("len %d, want %d", len(got), len(vals))
	}
	for i := range vals {
		if got[i] != want[i] || got[i] != vals[i] {
			t.Errorf("row %d: arena %q, StringValues %q, input %q", i, got[i], want[i], vals[i])
		}
	}
	// The arena is a copy: overwriting the source buffers leaves the
	// strings intact.
	for _, b := range arr.Buffers {
		for i := range b {
			b[i] = '!'
		}
	}
	for i := range vals {
		if got[i] != vals[i] {
			t.Errorf("row %d changed with its source buffer: %q", i, got[i])
		}
	}

	empty, err := StringValuesArena(viewArray([]string{"", ""}))
	if err != nil || len(empty) != 2 || empty[0] != "" || empty[1] != "" {
		t.Errorf("all-empty array: %q, %v", empty, err)
	}
}

func TestStringValuesArenaRejectsBadViews(t *testing.T) {
	arr := viewArray([]string{strings.Repeat("q", 20)})
	binary.LittleEndian.PutUint32(arr.Buffers[0][12:16], 1000) // offset past the buffer
	if _, err := StringValuesArena(arr); err == nil {
		t.Error("expected overflow error")
	}
	arr = viewArray([]string{strings.Repeat("q", 20)})
	binary.LittleEndian.PutUint32(arr.Buffers[0][8:12], 9) // no such buffer
	if _, err := StringValuesArena(arr); err == nil {
		t.Error("expected buffer index error")
	}
}
