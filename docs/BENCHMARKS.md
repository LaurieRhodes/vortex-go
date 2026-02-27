# vortex-go Performance Benchmarks

Performance measurements for the vortex-go reader and writer, including comparisons
against Apache Parquet and the Rust reference implementation.

**Test environment:** Intel 14th Gen (Raptor Lake), 64 GB RAM, NVMe SSD, Linux Mint,
Go 1.24, Rust 1.93 (release mode). NVIDIA RTX 4090 present but not used by either reader
(verified: no CUDA libraries linked).

## Vortex vs Parquet

Schema: `id:i64, user_id:i32, amount:f64, category:utf8, description:utf8, score:f64, active:bool` (7 columns)

All Vortex files written by the Go writer with default compression (dict encoding for strings,
FoR+BitPacking for integers, Pcodec for clustered floats, zone map statistics).

### File Size Comparison

| Rows | Parquet Size | Vortex Size | Compression |
|------|-------------|-------------|-------------|
| 10K | 468 KB | 208 KB | **56% smaller** |
| 100K | 4.6 MB | 2.0 MB | **56% smaller** |
| 500K | 22.8 MB | 9.7 MB | **58% smaller** |
| 1M | 45.6 MB | 19.3 MB | **58% smaller** |

### Scan Throughput

| Rows | Parquet Scan | Vortex Full Scan | Speedup |
|------|-------------|------------------|---------|
| 10K | 4.1 ms | 0.5 ms | **8× faster** |
| 100K | 22 ms | 3.5 ms | **6× faster** |
| 500K | 96 ms | 20 ms | **5× faster** |
| 1M | 188 ms | 40 ms | **5× faster** |

**Throughput at 1M rows:**

| Metric | Parquet | Vortex |
|--------|---------|--------|
| Rows/s | 5.3M | 25M |
| MB/s | 255 | 510 |

### Column Projection

Vortex's columnar layout enables reading only the requested columns, skipping I/O
and decode work for the rest.

| Scan Type (1M rows) | Time | vs Parquet |
|---------------------|------|------------|
| Parquet full scan | 188 ms | — |
| Vortex full scan (7/7 cols) | 40 ms | 5× faster |
| Vortex projected (2/7 cols) | 16 ms | **12× faster** |

Projecting 2 of 7 columns yields 2.5× speedup over full Vortex scan and 12× over Parquet.

### Zone Map Pruning

The Go writer produces zone map statistics (`vortex.stats` layout) for integer and
float columns. The scan API evaluates filter predicates against per-chunk min/max
to skip chunks that cannot contain matching rows.

| Metric | Value |
|--------|-------|
| Dataset | 100K rows, 10 chunks × 10K |
| Filter | `id >= 50000 AND id <= 150000` (selects ~10%) |
| Full scan | 820 µs |
| Filtered scan | 96 µs (9/10 chunks pruned) |
| **Speedup** | **8.5× faster** |

## Go vs Rust Reader

Both readers tested on identical Go-written Vortex files to eliminate format differences.

Schema: `id:i64, value:f64, category:utf8` (3 columns).

### Methodology

To eliminate CPU cache effects, each run follows this protocol:

1. Generate benchmark files with the Go writer
2. Evict page cache (128 MB random data read to displace cached file pages)
3. Run **Rust benchmark first** (cold cache)
4. Evict page cache again
5. Run **Go benchmark second** (cold cache)

Both readers forced to fully decode all columns: Go via `DecodeArray()` encoding tree
traversal, Rust via `to_struct()` + `to_primitive()`/`to_varbinview()` per chunk.

### Results

| Rows | File Size | Go Full Decode | Rust Full Decode | Ratio |
|------|-----------|---------------|-----------------|-------|
| 100K | 1.0 MB | 1.6 ms (61M rows/s) | 0.6 ms (171M rows/s) | Rust **2.8×** faster |
| 500K | 5.3 MB | 3.8 ms (130M rows/s) | 0.6 ms (805M rows/s) | Rust **6.2×** faster |
| 1M | 10.5 MB | 7.7 ms (129M rows/s) | 1.2 ms (846M rows/s) | Rust **6.5×** faster |

Rust lazy scan (no decode, just chunk length): 1.8 GB/s — confirms most time is in decode.

### Analysis

**At 100K (1.0 MB, fits in L2/L3 cache):** The 2.8× gap is the purest CPU comparison.
It reflects the cost of Go's garbage collector, bounds checking, and memory copying
versus Rust's zero-copy slices and compile-time safety guarantees.

**At 500K–1M:** The gap widens to ~6× because Rust's `to_primitive()` returns a
reference into the existing buffer (zero allocation), while Go's `DecodeArray()`
allocates and copies each decoded column. With 3 columns × 20 chunks at 1M rows,
Go makes 60 heap allocations that Rust avoids entirely.

**CUDA note:** The Vortex Rust crate includes an optional `vortex-cuda` module for
GPU-accelerated scans, but it is not enabled in these benchmarks (verified: the
benchmark binary links only libc, libm, and libgcc). These are pure CPU-vs-CPU results.

### Perspective

The Go reader at 130M rows/s is 5× faster than Parquet and highly competitive for
a pure-Go implementation. The Rust reference implementation benefits from years of
optimisation, zero-copy buffer management, and LLVM auto-vectorisation.

For Go applications that need Vortex support — such as Go services, CLI tools, or
query engines like open-kusto — this performance eliminates any need for CGo or FFI
bridges to the Rust library.

## Cross-Compatibility

All test files are readable by every consumer in the Vortex ecosystem:

| Writer → Reader | Go | Rust | Python vortex | DuckDB |
|---|---|---|---|---|
| **Go writer** | ✅ 91 tests | ✅ verified | ✅ 48 checks | ✅ full SQL |
| **Rust writer** | ✅ 52 encoding tests | ✅ native | ✅ 13 files | ✅ 13 files |

## Reproducing the Benchmarks

### Vortex vs Parquet

```bash
go test ./benchmark/ -run TestPerformanceReport -v -timeout 300s
```

### Go vs Rust Cross-Language

```bash
# Option 1: combined script (runs Rust first for cache fairness)
./benchmark/run_crosslang.sh

# Option 2: manual steps
go test ./benchmark/ -run TestCrossLanguageBenchmark -v -timeout 300s
cd testdata/testgen && cargo run --bin bench --release -- /tmp/vortex-bench
```

### Python/DuckDB Interop

```bash
python3 -m venv .venv && . .venv/bin/activate
pip install -r testdata/requirements.txt
python3 testdata/test_python_interop.py
```

## Encoding Effectiveness

The writer's encoding selection produces these compression ratios on the 7-column benchmark
schema (1M rows):

| Column | Type | Encoding | Raw Size | Encoded | Ratio |
|--------|------|----------|----------|---------|-------|
| id | i64 | FoR + BitPacking | 8 MB | ~2.5 MB | 3.2× |
| user_id | i32 | FoR + BitPacking | 4 MB | ~0.5 MB | 8× |
| amount | f64 | Pcodec | 8 MB | ~4 MB | 2× |
| category | utf8 | Dict | ~12 MB | ~0.2 MB | 60× |
| description | utf8 | Dict | ~20 MB | ~8 MB | 2.5× |
| score | f64 | Pcodec | 8 MB | ~3 MB | 2.7× |
| active | bool | Bool (bit-packed) | 1 MB | ~0.1 MB | 8× |

Dict encoding is most effective on low-cardinality string columns, while FoR+BitPacking
excels on narrow integer ranges.
