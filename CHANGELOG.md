# Changelog

All notable changes to vortex-go are documented here.

## [Unreleased]

### Core Library
- Native Go reader for the Vortex columnar file format
- 25 encoding decoders: Primitive, Bool, VarBinView, Dict, Chunked, Constant,
  Masked, Null, Sparse, Struct, Sequence, RunEnd, ZigZag, ByteBool,
  ALP, ALP-RD, DateTimeParts, DecimalByteParts, FSST, Zstd,
  FastLanes BitPacking, FastLanes FoR, FastLanes Delta, FastLanes RLE, Pcodec
- Scan API with column projection and row limits
- Zone map pruning with declarative filter predicates (GTE, LTE, EQ, NEQ, GT, LT)
- Compressed writer with automatic encoding selection:
  - Integers: FoR + BitPacking, Pcodec, or Primitive
  - Floats: Pcodec or Primitive
  - Strings: Dict encoding (with FoR+BitPacked codes) or VarBinView
  - Booleans: bit-packed Bool
  - Zone map statistics for all numeric columns
- Pcodec compression: full Go implementation (baseline + multi-bin ANS entropy coding)
- LZ4 and Zstd segment decompression

### CLI Tools
- `vtxinfo` — inspect Vortex file structure, schema, encodings, statistics
- `vtxconvert` — convert Parquet files to Vortex format

### Cross-Compatibility
- Go ↔ Rust: verified bidirectional read (Go-written files readable by Rust and vice versa)
- Go ↔ Python: verified via vortex-data library (48 checks)
- Go ↔ DuckDB: verified via PyArrow bridge (full SQL queries on Vortex data)

### Testing
- 91 Go unit tests across 6 packages
- 13 Rust-generated reference files covering all major encoding patterns
- Integration tests: Parquet round-trip, multi-chunk, cross-compat
- Performance benchmarks: Vortex vs Parquet, Go vs Rust reader

### Performance (1M rows, 7 columns)
- 58% smaller than Parquet
- 5× faster full scan than Parquet
- 12× faster projected scan (2/7 columns) than Parquet
- 25M rows/s scan throughput
- 8.5× faster filtered scan via zone map pruning (100K rows, 90% pruned)
- Go reader achieves 130M rows/s on 3-column workload (vs Rust at 846M rows/s)
