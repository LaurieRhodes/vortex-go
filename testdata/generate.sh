#!/usr/bin/env bash
# generate.sh — Regenerate all test data files for vortex-go.
#
# Prerequisites:
#   - Rust toolchain (cargo) with access to vortex crate dependencies
#   - The vortex Rust workspace should be available (../vortex/) for crate resolution
#
# Usage:
#   cd testdata && ./generate.sh
#
# This script regenerates:
#   testdata/rust/*.vortex  — Rust reference files (13 files)
#   testdata/*.vtx          — Go integration test files (5 files)
#   testdata/go_written*.vtx — Go-written cross-compatibility files (2 files)
#
# It also runs:
#   - Full Go test suite (89 tests)
#   - Rust cross-compatibility test (reads Go-written files)
#
# All files are deterministic: running this script twice produces identical output.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TESTGEN_DIR="$SCRIPT_DIR/testgen"

echo "=== Regenerating vortex-go test data ==="
echo "  testgen dir: $TESTGEN_DIR"
echo ""

# Build and run the Rust test generator.
echo "Building Rust testgen..."
(cd "$TESTGEN_DIR" && cargo build --release 2>&1 | grep -v '^\s*Compiling\|^\s*Downloading\|^\s*Downloaded')

echo "Running Rust testgen..."
(cd "$TESTGEN_DIR" && cargo run --release 2>&1 | grep -v '^\s*Compiling\|^\s*Finished\|^warning:')

echo ""
echo "=== Verifying generated files ==="

# Check that all expected files exist.
EXPECTED_RUST=(
    "rust/timestamps.vortex"
    "rust/repetitive_ints.vortex"
    "rust/zstd_strings.vortex"
    "rust/alp_floats.vortex"
    "rust/delta_ints.vortex"
    "rust/pco_ints.vortex"
    "rust/dict_strings.vortex"
    "rust/large_ints.vortex"
    "rust/mixed.vortex"
    "rust/simple_ints.vortex"
    "rust/const_sorted.vortex"
    "rust/nulls.vortex"
    "rust/zone_pruning.vortex"
)

EXPECTED_VTX=(
    "small.vtx"
    "integers.vtx"
    "mixed.vtx"
    "nulls.vtx"
    "strings.vtx"
    "go_written.vtx"
    "go_written_large.vtx"
)

MISSING=0
for f in "${EXPECTED_RUST[@]}" "${EXPECTED_VTX[@]}"; do
    if [ ! -f "$SCRIPT_DIR/$f" ]; then
        echo "  MISSING: $f"
        MISSING=$((MISSING + 1))
    fi
done

TOTAL=$((${#EXPECTED_RUST[@]} + ${#EXPECTED_VTX[@]}))

if [ "$MISSING" -gt 0 ]; then
    echo "  ERROR: $MISSING of $TOTAL files missing!"
    exit 1
fi

echo "  All $TOTAL test files present."
echo ""

# Generate Go-written cross-compatibility test files.
echo "=== Generating Go-written cross-compatibility files ==="
cat > /tmp/_gen_go_vtx.go << 'GOEOF'
package main

import (
	"encoding/binary"
	"math"
	"os"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/writer"
)

func main() {
	genSimple()
	genLarge()
}

func genSimple() {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"id", "value", "name"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
			{Kind: vortex.DTypeUtf8},
		},
	}
	n := 100
	idBuf := make([]byte, n*4)
	valBuf := make([]byte, n*8)
	names := []string{"alice", "bob", "charlie", "diana", "eve"}
	viewsBuf := make([]byte, n*16)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(idBuf[i*4:], uint32(int32(i*10)))
		binary.LittleEndian.PutUint64(valBuf[i*8:], math.Float64bits(float64(i)*1.5))
		s := names[i%5]
		off := i * 16
		binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
		copy(viewsBuf[off+4:], s)
	}
	out, _ := os.Create(os.Args[1] + "/go_written.vtx")
	defer out.Close()
	w, _ := writer.NewWriter(out, schema, writer.DefaultOptions())
	w.WriteChunk(map[string]*vortex.Array{
		"id":    {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{idBuf}},
		"value": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{valBuf}},
		"name":  {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{viewsBuf}},
	})
	w.Close()
}

func genLarge() {
	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: []string{"counter", "temperature", "label"},
		FieldTypes: []*vortex.DType{
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
			{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
			{Kind: vortex.DTypeUtf8},
		},
	}
	opts := writer.DefaultOptions()
	out, _ := os.Create(os.Args[1] + "/go_written_large.vtx")
	defer out.Close()
	w, _ := writer.NewWriter(out, schema, opts)
	names := []string{"sensor_a", "sensor_b", "sensor_c"}
	for chunk := 0; chunk < 3; chunk++ {
		n := 500
		base := chunk * n
		counterBuf := make([]byte, n*8)
		tempBuf := make([]byte, n*8)
		viewsBuf := make([]byte, n*16)
		for i := 0; i < n; i++ {
			row := base + i
			binary.LittleEndian.PutUint64(counterBuf[i*8:], uint64(int64(row)))
			binary.LittleEndian.PutUint64(tempBuf[i*8:], math.Float64bits(20.0+float64(row)*0.01))
			s := names[row%3]
			off := i * 16
			binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
			copy(viewsBuf[off+4:], s)
		}
		w.WriteChunk(map[string]*vortex.Array{
			"counter":     {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{counterBuf}},
			"temperature": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{tempBuf}},
			"label":       {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{viewsBuf}},
		})
	}
	w.Close()
}
GOEOF
(cd "$SCRIPT_DIR/.." && go run /tmp/_gen_go_vtx.go "$SCRIPT_DIR")
echo "  go_written.vtx and go_written_large.vtx generated"
echo ""

# Run Go tests to verify files are valid.
echo "=== Running Go tests to verify ==="
(cd "$SCRIPT_DIR/.." && go test ./... -count=1)

# Run Rust cross-compatibility test (reads Go-written files).
echo ""
echo "=== Running Rust cross-compatibility test ==="
(cd "$TESTGEN_DIR" && cargo run --bin read_go_file --release 2>&1 | grep -v '^\s*Compiling\|^\s*Finished\|^warning:')

echo ""
echo "=== Done. All test data regenerated and verified. ==="
