package encoding

import (
	"encoding/binary"
	"math"
	"os"
	"testing"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// openRustTestFile opens a Rust-generated Vortex reference file from testdata/rust/.
func openRustTestFile(t *testing.T, name string) *vortex.File {
	t.Helper()
	path := "../testdata/rust/" + name
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		t.Fatalf("vortex.Open %s: %v", path, err)
	}
	return vf
}

// TestRustDictStrings validates reading a Rust-generated file with
// fastlanes.bitpacked dictionary codes (FL_ORDER transposition).
func TestRustDictStrings(t *testing.T) {
	vf := openRustTestFile(t, "dict_strings.vortex")

	arr, err := vf.ReadColumn("name")
	if err != nil {
		t.Fatal(err)
	}
	if arr.Length != 1000 {
		t.Fatalf("expected 1000 rows, got %d", arr.Length)
	}

	strs, err := DictLookupStrings(arr)
	if err != nil {
		t.Fatalf("DictLookupStrings: %v", err)
	}

	expected := []string{"alice", "bob", "charlie", "diana", "eve"}
	for i, s := range strs {
		want := expected[i%len(expected)]
		if s != want {
			t.Errorf("name[%d] = %q, want %q", i, s, want)
			break
		}
	}
	t.Logf("dict_strings.vortex: 1000 strings cycling %v ✓", expected)
}

// TestRustMixedName validates the name column from the Rust-generated mixed file.
func TestRustMixedName(t *testing.T) {
	vf := openRustTestFile(t, "mixed.vortex")

	arr, err := vf.ReadColumn("name")
	if err != nil {
		t.Fatal(err)
	}
	if arr.Length != 500 {
		t.Fatalf("expected 500 rows, got %d", arr.Length)
	}

	strs, err := DictLookupStrings(arr)
	if err != nil {
		t.Fatalf("DictLookupStrings: %v", err)
	}

	expected := []string{"alice", "bob", "charlie", "diana", "eve"}
	for i, s := range strs {
		want := expected[i%len(expected)]
		if s != want {
			t.Errorf("name[%d] = %q, want %q", i, s, want)
			break
		}
	}
	t.Logf("mixed.vortex name: 500 strings cycling %v ✓", expected)
}

// TestRustMixedId validates the id column (sequential i32, no bitpacking).
func TestRustMixedId(t *testing.T) {
	vf := openRustTestFile(t, "mixed.vortex")

	arr, err := vf.ReadColumn("id")
	if err != nil {
		t.Fatal(err)
	}
	if arr.Length != 500 {
		t.Fatalf("expected 500 rows, got %d", arr.Length)
	}

	buf := arr.Buffers[0]
	for i := 0; i < 500; i++ {
		got := int32(binary.LittleEndian.Uint32(buf[i*4:]))
		want := int32(i + 1)
		if got != want {
			t.Errorf("id[%d] = %d, want %d", i, got, want)
			break
		}
	}
	t.Log("mixed.vortex id: 500 sequential ints ✓")
}

// TestRustMixedScore validates the score column (dict-encoded f64 with bitpacked codes).
func TestRustMixedScore(t *testing.T) {
	vf := openRustTestFile(t, "mixed.vortex")

	arr, err := vf.ReadColumn("score")
	if err != nil {
		t.Fatal(err)
	}
	if arr.Length != 500 {
		t.Fatalf("expected 500 rows, got %d", arr.Length)
	}
	if len(arr.Children) < 2 {
		t.Fatalf("expected dict array with 2 children, got %d", len(arr.Children))
	}

	codes := arr.Children[0]
	vals := arr.Children[1]

	// Read dictionary f64 values.
	dictBuf := vals.Buffers[0]
	dictVals := make([]float64, vals.Length)
	for i := 0; i < vals.Length; i++ {
		dictVals[i] = math.Float64frombits(binary.LittleEndian.Uint64(dictBuf[i*8:]))
	}

	// Resolve codes to floats.
	codeBuf := codes.Buffers[0]
	elemSize := vortex.PTypeSize(codes.DType.PType)
	for i := 0; i < 500; i++ {
		var code int
		switch elemSize {
		case 1:
			code = int(codeBuf[i])
		case 2:
			code = int(binary.LittleEndian.Uint16(codeBuf[i*2:]))
		case 4:
			code = int(binary.LittleEndian.Uint32(codeBuf[i*4:]))
		default:
			t.Fatalf("unsupported codes element size: %d", elemSize)
		}
		got := dictVals[code]
		// Scores cycle through 10 values: 1.5, 1.6, ..., 2.4
		want := 1.5 + float64(i%10)*0.1
		if math.Abs(got-want) > 0.001 {
			t.Errorf("score[%d] = %f, want %f (code=%d)", i, got, want, code)
			break
		}
	}
	t.Log("mixed.vortex score: 500 cycling f64 values ✓")
}
