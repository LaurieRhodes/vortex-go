#!/usr/bin/env bash
# run_crosslang.sh — Run Rust FIRST then Go on identical files to eliminate
# cache-warming bias. Also drops page cache between runs.
#
# Usage:
#   ./benchmark/run_crosslang.sh

set -euo pipefail
PROJ_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="/tmp/vortex-bench"

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  Step 1: Generate benchmark files (Go writer)"
echo "═══════════════════════════════════════════════════════════════════════════"
echo ""

cd "$PROJ_DIR"
rm -rf "$BENCH_DIR"
go test ./benchmark/ -run TestCrossLanguageBenchmark -v -timeout 300s -count=1 2>&1 | \
    grep -E '(file:|PASS)' || true

echo ""
echo "  Files generated:"
ls -lh "$BENCH_DIR"/*.vtx

# Drop page cache if we have permission
echo ""
if [ -w /proc/sys/vm/drop_caches ]; then
    echo "  Dropping page cache..."
    sync
    echo 3 > /proc/sys/vm/drop_caches
    echo "  Cache dropped."
else
    echo "  NOTE: Cannot drop page cache (no root). Run with sudo for cleanest results."
    echo "  Flushing with sync..."
    sync
fi

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  Step 2: Rust reader FIRST (cold cache)"
echo "═══════════════════════════════════════════════════════════════════════════"
echo ""

cd "$PROJ_DIR/testdata/testgen"
cargo run --bin bench --release -- "$BENCH_DIR" 2>&1 | grep -v '^\s*Finished\|^\s*Running'

# Drop cache again before Go
if [ -w /proc/sys/vm/drop_caches ]; then
    echo "  Dropping page cache again..."
    sync
    echo 3 > /proc/sys/vm/drop_caches
fi

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  Step 3: Go reader SECOND (cold cache)"
echo "═══════════════════════════════════════════════════════════════════════════"
echo ""

cd "$PROJ_DIR"
go test ./benchmark/ -run TestCrossLanguageBenchmark -v -timeout 300s -count=1 2>&1 | \
    grep -E '(│|┌|└)' || true

echo ""
echo "═══════════════════════════════════════════════════════════════════════════"
echo "  Summary"
echo "═══════════════════════════════════════════════════════════════════════════"
echo ""
echo "  Order: Rust ran first (cold), then Go (cold if cache dropped)"
echo "  Schema: id:i64, value:f64, category:utf8 (3 columns)"
echo "  Both readers: full decode, all columns materialised"
echo ""
