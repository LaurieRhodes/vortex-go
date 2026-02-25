# vortex-go: A Go Implementation of the Vortex Columnar File Format

> **Status:** Project specification  
> **Created:** 2026-02-22  
> **Format version target:** Vortex file format (stable since v0.36.0)  
> **Reference implementation:** `/media/laurie/Data/Github/vortex` (Rust, v0.59.4)

---

## 1. Project Scope

`vortex-go` is a native Go library for reading and writing files in the Vortex columnar file format. It is a format implementation, not a database engine. It has no opinions about catalogs, query languages, tables, or execution plans.

**In scope:**
- Read Vortex files written by the Rust reference implementation
- Write valid Vortex files readable by Rust, Python, DuckDB, and other consumers
- FlatBuffer and Protobuf code generation from the canonical `.fbs` and `.proto` schemas
- Layout tree navigation (Struct, Chunked, Zoned, Flat, Dictionary)
- Encoding/decoding for all encodings the default writer produces
- Statistics extraction from zone maps and file-level stats
- Column projection (read specific columns without touching others)
- Row filtering via zone map pruning (skip zones by min/max stats)

**Out of scope (belongs in downstream projects like `open-kusto`):**
- Query parsing or execution
- Table/extent catalogs
- Compaction, merge, or defragmentation
- Management commands
- Materialized views
- HTTP APIs

**Analogy:** `vortex-go` is to Vortex what `parquet-go` is to Parquet — a standalone format library.

---

## 2. The Vortex File Format

### 2.1 File Structure

```
Offset 0:
┌──────────────────────────────────────────────────────┐
│ Magic: "VTXF" (4 bytes)                              │
├──────────────────────────────────────────────────────┤
│ Segments: binary data blobs at known offsets          │
│   (column data, compressed arrays, statistics, etc.)  │
│   Optional inter-segment padding for alignment        │
├──────────────────────────────────────────────────────┤
│ Postscript data (FlatBuffer)                         │
├──────────────────────────────────────────────────────┤
│ Version tag (u16)                                    │
│ Postscript length (u16)                              │
│ Magic: "VTXF" (4 bytes)                              │
└──────────────────────────────────────────────────────┘
```

Reading starts from the end:
1. Read last 8 bytes → magic (verify "VTXF"), postscript length, version
2. Read postscript (FlatBuffer) → locations of dtype, layout, statistics, footer
3. Read footer (FlatBuffer) → dictionary tables for array specs, layout specs, segment specs
4. Read dtype (FlatBuffer) → logical schema
5. Read layout (FlatBuffer) → physical layout tree
6. Navigate layout tree → read segments on demand

### 2.2 Serialization Layers

The format uses three serialization technologies, all with mature Go support:

| Layer | Technology | Go Library | Purpose |
|---|---|---|---|
| File structure | FlatBuffers | `github.com/google/flatbuffers/go` | Postscript, footer, layout tree, dtype, array nodes |
| Encoding metadata | Protobuf (prost) | `google.golang.org/protobuf` | Per-encoding parameters (bit width, dictionary size, etc.) |
| Scalar values | Protobuf | Same | Statistics (min/max/sum in zone maps) |
| Segment compression | LZ4, ZStd | `github.com/pierrec/lz4/v4`, `github.com/klauspost/compress/zstd` | Optional segment-level compression |

### 2.3 The Indirection Model

The file uses dictionary-encoded registries in the Footer to keep the Layout tree compact:

```
Footer contains:
  array_specs:   [ArraySpec]    — e.g. [{id:"fastlanes.bitpacked"}, {id:"vortex.dict"}, ...]
  layout_specs:  [LayoutSpec]   — e.g. [{id:"vortex.flat"}, {id:"vortex.chunked"}, ...]
  segment_specs: [SegmentSpec]  — e.g. [{offset:0, length:4096, alignment:4}, ...]
  compression_specs: [CompressionSpec] — e.g. [{scheme:None}, {scheme:LZ4}, ...]

Layout tree nodes reference these by index:
  Layout {
    encoding: uint16    → index into layout_specs  → "vortex.chunked"
    children: [Layout]  → child layout nodes
    segments: [uint32]  → indices into segment_specs → byte ranges in file
    row_count: uint64
    metadata: [ubyte]   → layout-specific metadata
  }

Array nodes (inside Flat layouts) reference by index:
  ArrayNode {
    encoding: uint16    → index into array_specs   → "fastlanes.bitpacked"
    children: [ArrayNode]
    buffers: [uint16]   → indices into Buffer table → padding, alignment, compression, length
    metadata: [ubyte]   → protobuf-encoded encoding metadata
    stats: ArrayStats   → min, max, null_count, is_sorted, etc.
  }
```

### 2.4 Schema Files

All schema definitions live in the reference repository and should be used for code generation:

| File | Format | Generates |
|---|---|---|
| `vortex-flatbuffers/flatbuffers/vortex-file/footer.fbs` | FlatBuffer | Postscript, Footer, SegmentSpec, CompressionSpec |
| `vortex-flatbuffers/flatbuffers/vortex-layout/layout.fbs` | FlatBuffer | Layout tree nodes |
| `vortex-flatbuffers/flatbuffers/vortex-dtype/dtype.fbs` | FlatBuffer | DType, PType, Struct_, List, Extension, etc. |
| `vortex-flatbuffers/flatbuffers/vortex-array/array.fbs` | FlatBuffer | Array, ArrayNode, Buffer, ArrayStats |
| `vortex-proto/proto/scalar.proto` | Protobuf | ScalarValue (used in statistics) |
| `vortex-proto/proto/dtype.proto` | Protobuf | DType (protobuf variant, used in encoding metadata) |

Per-encoding metadata is defined as `prost::Message` structs in the Rust source. These are protobuf-compatible and can be decoded with standard protobuf libraries using the tag numbers from the `#[prost(...)]` annotations.

---

## 3. Layout Types

The layout tree describes how data is physically arranged in the file. Five layout types exist:

| Layout ID | String ID | Purpose | Children | Segments |
|---|---|---|---|---|
| Flat | `vortex.flat` | Leaf node: holds a serialised array (ArrayNode + buffers) | None | 1 (the serialised array) |
| Chunked | `vortex.chunked` | Row-wise partitioning into chunks | 1+ child layouts (each is a chunk) | None |
| Struct | `vortex.struct` | Column-wise partitioning | 1 child per column | None |
| Zoned | `vortex.stats` | Zone map statistics for pruning | 2 children: [stats_table, data_layout] | None |
| Dictionary | `vortex.dict` | Shared dictionary across chunks | 2 children: [values_layout, codes_layout] | None |

### 3.1 Default Layout Tree (produced by the Rust writer)

```
TableStrategy (Struct — splits columns)
  └─ per column:
     RepartitionStrategy (8K row blocks)
       └─ ZonedStrategy ("vortex.stats" — zone map per 8K rows)
            ├─ stats table (compressed Flat)
            └─ DictStrategy (optional — if dictionary encoding helps)
                 └─ RepartitionStrategy (coalesce to ~1MB)
                      └─ CompressingStrategy (BtrBlocks)
                           └─ BufferedStrategy (~2MB buffer)
                                └─ ChunkedLayout ("vortex.chunked")
                                     └─ FlatLayout ("vortex.flat" — serialised arrays)
```

For the reader, this means a typical file has:
- Top-level: Struct layout (one child per column)
- Per column: Zoned layout (first child = stats, second child = data)
- Data: Chunked layout containing Flat layouts
- Each Flat layout: one segment containing an ArrayNode tree with compressed data

### 3.2 The Read Path

To read a specific column with a row filter:

1. Parse footer → resolve layout specs and segment specs
2. Navigate Struct layout → find the child for the target column
3. Read Zoned layout → fetch stats table from first child
4. Check zone statistics against filter predicate → identify which zones match
5. Navigate to Chunked layout → skip chunks for non-matching zones
6. For each matching chunk's Flat layout → read the segment
7. Parse ArrayNode tree → decode buffers through the encoding chain
8. Return decoded arrays

---

## 4. Array Encodings

The ArrayNode tree inside each Flat layout segment describes how data is encoded. Each node has an encoding ID (referencing `array_specs`), child nodes, buffer references, and protobuf metadata.

### 4.1 Canonical Encodings (Always Present)

These are the "leaf" forms that every other encoding eventually decompresses to:

| Encoding ID | String ID | Purpose | Metadata |
|---|---|---|---|
| Null | `vortex.null` | All-null array | None |
| Bool | `vortex.bool` | Bit-packed booleans | 1 buffer: bit vector |
| Primitive | `vortex.primitive` | Fixed-width numbers (i8–i64, u8–u64, f16–f64) | 1 buffer: raw values |
| VarBinView | `vortex.varbinview` | Variable-length strings/binary (canonical for Utf8/Binary) | Views buffer + 0..N data buffers |
| Struct | `vortex.struct` | Named fields (canonical for struct types) | 1 child per field |
| Dict | `vortex.dict` | Dictionary encoding (values + codes) | 2 children: [codes, values] |
| Constant | `vortex.constant` | Single repeated value | Scalar in metadata |
| Masked | `vortex.masked` | Validity mask (nullability) | 2 children: [values, mask] |
| Chunked | `vortex.chunked` | Concatenated sub-arrays | N children |

### 4.2 Compressed Encodings (BtrBlocks Default Output)

These are the encodings the default writer actually selects. Implementation priority should follow this list:

**Integer encodings** (selected by BtrBlocks for integer/temporal columns):

| Encoding | String ID | Algorithm | Metadata (protobuf) | Priority |
|---|---|---|---|---|
| Frame-of-Reference | `fastlanes.for` | Subtract minimum, then bitpack child | `min_value` (reference scalar) | P0 |
| BitPacking | `fastlanes.bitpacked` | Pack N-bit integers into dense buffer | `bit_width: u32`, `offset: u32`, optional `patches` | P0 |
| Dictionary | `vortex.dict` | Values array + codes array | (uses canonical dict, no extra metadata) | P0 |
| RunEnd | `vortex.runend` | Run-length with end positions | `ends_ptype: PType`, `num_runs: u64`, `offset: u64` | P1 |
| ZigZag | `vortex.zigzag` | Map signed → unsigned for bitpacking | (no metadata, just wraps child) | P1 |
| Constant | `vortex.constant` | Single value repeated | Scalar value in metadata | P0 |
| Sparse | `vortex.sparse` | Fill value + patches at known indices | `fill_value`, `indices_ptype`, `len` | P1 |
| Delta | `fastlanes.delta` | Delta encoding + bitpack deltas | `validity_len: u64` | P2 |
| Sequence | `vortex.sequence` | Fixed-interval runs (0,1,2,3...) | `start`, `stop`, `step` scalars | P2 |
| RLE | `fastlanes.rle` | Generic run-length | `run_ends_ptype: PType`, `length: u64` | P1 |
| Pco | `vortex.pco` | PCodec compression | `header_len`, `num_latent_batches`, etc. | P3 |

**Float encodings:**

| Encoding | String ID | Algorithm | Priority |
|---|---|---|---|
| ALP | `vortex.alp` | Adaptive Lossless FP: encode floats as scaled integers | P2 |
| ALPrd | `vortex.alprd` | ALP with right digit truncation | P3 |
| Dictionary / Constant / Sparse / RunEnd / RLE | (same as integer) | Same algorithms | (same) |

**String encodings:**

| Encoding | String ID | Algorithm | Priority |
|---|---|---|---|
| Dictionary | `vortex.dict` | Unique values + integer codes | P0 |
| FSST | `vortex.fsst` | Fast Static Symbol Table compression | P2 |
| Constant / Sparse | (same) | Same | (same) |
| Zstd | `vortex.zstd` | Zstandard block compression | P1 |

**Other:**

| Encoding | String ID | Purpose | Priority |
|---|---|---|---|
| ByteBool | `vortex.bytebool` | Byte-per-boolean (compute-friendly) | P1 |
| DateTimeParts | `vortex.datetimeparts` | Decomposed timestamp encoding | P1 |
| DecimalByteParts | `vortex.decimal_byte_parts` | Decomposed decimal encoding | P3 |

### 4.3 Cascading Encodings

BtrBlocks applies encodings in cascades. A typical integer column might be encoded as:

```
vortex.dict                        (dictionary encode → codes + values)
  codes: fastlanes.for             (subtract minimum from codes)
    encoded: fastlanes.bitpacked   (bitpack the offset codes)
      buffer: raw packed bits
  values: vortex.varbinview        (canonical string values)
    buffer: string data
```

The reader must recursively walk the ArrayNode tree, decoding each layer until reaching a canonical form with raw buffers.

### 4.4 Encoding Metadata

Every encoding's metadata is a protobuf message. The tag numbers are defined in the Rust source as `#[prost(..., tag = "N")]` annotations. For Go code generation, extract these into `.proto` files:

```protobuf
// fastlanes_bitpacked.proto (derived from Rust source)
message BitPackedMetadata {
  uint32 bit_width = 1;
  uint32 offset = 2;
  PatchesMetadata patches = 3;
}

// fastlanes_for.proto
message FoRMetadata {
  // Reference value is stored as a child scalar, not in metadata
}

// vortex_runend.proto
message RunEndMetadata {
  uint32 ends_ptype = 1;  // PType enum value
  uint64 num_runs = 2;
  uint64 offset = 3;
}
```

---

## 5. Implementation Roadmap

### Phase 0: Project Skeleton + Code Generation

- Initialise Go module: `github.com/your-org/vortex-go`
- Copy `.fbs` and `.proto` files from reference repo
- Run `flatc --go` on all `.fbs` files → generated Go FlatBuffer accessors
- Run `protoc --go_out` on scalar.proto, dtype.proto → generated Go protobuf structs
- Create `.proto` files for each encoding's metadata (extracted from Rust `#[prost]` annotations)
- Run `protoc --go_out` on encoding metadata protos

**Deliverable:** Generated Go types for all file format structures.

### Phase 1: File Reader — Footer + Schema (P0)

- Read last 8 bytes → validate magic, extract postscript length and version
- Read and parse Postscript FlatBuffer → segment locations for dtype, layout, footer, statistics
- Read and parse Footer FlatBuffer → build lookup tables for array_specs, layout_specs, segment_specs
- Read and parse DType FlatBuffer → reconstruct logical schema
- Read and parse file-level Statistics → extract per-column min/max

```go
type File struct {
    r          io.ReaderAt
    size       int64
    version    uint16
    postscript *fb.Postscript
    footer     *Footer
    dtype      DType
    layout     *Layout
    stats      *FileStatistics
}

type Footer struct {
    ArraySpecs      []string      // encoding string IDs, indexed by ArrayNode.encoding
    LayoutSpecs     []string      // layout string IDs, indexed by Layout.encoding
    SegmentSpecs    []SegmentSpec // byte ranges, indexed by Layout.segments
    CompressionSpecs []CompressionScheme
}

type SegmentSpec struct {
    Offset    uint64
    Length    uint32
    Alignment uint8
    Compression CompressionScheme
}

func Open(r io.ReaderAt, size int64) (*File, error)
```

**Test:** Open a file written by `vx` CLI, print schema and stats. Validate against `vx browse` output.

### Phase 2: Layout Tree Navigation (P0)

- Parse Layout FlatBuffer into a navigable tree
- Implement layout type dispatch: Struct, Chunked, Zoned, Flat, Dict
- Struct layout → column projection (find child by index)
- Chunked layout → iterate chunks
- Zoned layout → access stats table and data layout children
- Flat layout → read segment bytes, parse ArrayNode tree

```go
type Layout struct {
    EncodingID string   // resolved from layout_specs
    RowCount   uint64
    Metadata   []byte
    Children   []*Layout
    Segments   []uint32 // indices into segment_specs
}

// Navigate to a specific column
func (f *File) Column(name string) (*Layout, error)

// Iterate chunks within a column
func (l *Layout) Chunks() []*Layout
```

**Test:** Navigate layout tree, verify structure matches `vx browse` output.

### Phase 3: Flat Layout Decoding — Canonical Encodings (P0)

- Read segment bytes (with optional LZ4/ZStd decompression)
- Parse Array FlatBuffer → ArrayNode tree + Buffer descriptors
- Decode canonical encodings:
  - `vortex.primitive` → read raw bytes as typed slice
  - `vortex.bool` → unpack bit vector
  - `vortex.varbinview` → reconstruct strings from views + data buffers
  - `vortex.null` → generate null array
  - `vortex.constant` → expand scalar to array
  - `vortex.dict` → reconstruct values + codes
  - `vortex.struct` → recurse per field
  - `vortex.masked` → apply validity mask
  - `vortex.chunked` → concatenate children

```go
type Array struct {
    DType    DType
    Length   int
    Buffers  [][]byte
    Children []*Array
    Stats    *ArrayStats
}

// Decode an ArrayNode tree into a usable Array
func DecodeArray(node *ArrayNode, buffers [][]byte, specs []string) (*Array, error)
```

**Test:** Read a Vortex file containing uncompressed data (written with compression disabled), verify values match.

### Phase 4: Compressed Encoding Decoders (P0–P1)

Implement in priority order based on what BtrBlocks actually produces:

**P0 — Required for reading any typical file:**
- `fastlanes.for` — Frame of reference: subtract stored minimum, decode child
- `fastlanes.bitpacked` — Unpack N-bit integers from packed buffer (scalar implementation, no SIMD)
- `vortex.dict` — Already done in Phase 3

**P1 — Common in real data:**
- `vortex.runend` — Expand run-ends into values
- `vortex.zigzag` — Map unsigned back to signed: `(n >> 1) ^ -(n & 1)`
- `vortex.sparse` — Fill array with fill_value, then apply patches at indices
- `fastlanes.rle` — Expand run-length encoded data
- `vortex.bytebool` — Trivial: one byte per boolean
- `vortex.datetimeparts` — Reconstruct timestamps from days/seconds/subseconds
- `vortex.zstd` — Decompress with standard zstd library

**P2 — For completeness:**
- `vortex.fsst` — FSST symbol table decompression (published algorithm, moderate complexity)
- `vortex.alp` — Reverse the float→integer scaling
- `fastlanes.delta` — Prefix-sum to reverse delta encoding
- `vortex.sequence` — Generate values from start/stop/step

**P3 — Rare or advanced:**
- `vortex.alprd` — ALP right-digit variant
- `vortex.pco` — PCodec decompression
- `vortex.decimal_byte_parts` — Decimal reconstruction

**Test strategy:** For each encoding, write a test file using the Rust library with known data, then verify the Go reader produces identical output. The `vx` CLI can inspect files to verify structure.

### Phase 5: Zone Map Pruning (P1)

- Read Zoned layout's stats table (first child)
- Decode stats per zone (min/max per column per 8K-row block)
- Given a predicate, determine which zones can be skipped
- Return only the chunks from matching zones

```go
type ScanOptions struct {
    Columns   []string           // column projection
    Filter    func(stats ZoneStats) bool  // zone pruning predicate
}

func (f *File) Scan(opts ScanOptions) (*ArrayIterator, error)
```

**Test:** Write a file with known time ranges, verify that a time filter skips the correct zones.

### Phase 6: File Writer — Canonical Encodings (P1)

- Write valid Vortex files with canonical (uncompressed) encodings
- Layout: Struct → Zoned → Chunked → Flat
- Compute zone statistics (min/max per 8K rows)
- Emit correct FlatBuffer footer, postscript, and magic bytes

```go
type Writer struct {
    w      io.Writer
    schema DType
    opts   WriteOptions
}

type WriteOptions struct {
    RowBlockSize int  // zone size, default 8192
}

func NewWriter(w io.Writer, schema DType, opts WriteOptions) *Writer
func (w *Writer) WriteChunk(arrays map[string]*Array) error
func (w *Writer) Close() error  // writes footer, postscript, magic
```

Files written with canonical encodings will be larger than Rust-compressed files but are valid Vortex files readable by all consumers. This is equivalent to writing Parquet with `compression=NONE` — correct but unoptimised.

**Test:** Write a file with Go, read it back with Go, read it with `vx browse`, read it with the Rust library's test harness.

### Phase 7: Compressed Writers (P2+)

Add compression to the writer, encoding by encoding:
1. Dictionary encoding for strings (biggest win for typical data)
2. FastLanes FoR + BitPacking for integers
3. ZStd for fallback string compression
4. RunEnd / RLE for repetitive columns
5. FSST for string-heavy datasets
6. ALP for float columns

Each encoding is independent — add them one at a time, test with round-trip validation.

---

## 6. FastLanes Bitpacking — The Key Algorithm

FastLanes bitpacking is the most common encoding in typical Vortex files (integers, dictionary codes, timestamps). The Go implementation does not need SIMD — a scalar implementation reads the same bytes correctly.

### 6.1 The Algorithm

Given an array of N unsigned integers, each requiring at most B bits:

**Pack:** For each integer, write its B least-significant bits into a contiguous bit stream. The Rust SIMD implementation interleaves values across 1024-element blocks for SIMD lane parallelism, but the bit layout is deterministic.

**Unpack:** Read B bits at a time from the packed buffer, reconstruct each integer. The `offset` field in metadata handles cases where the array doesn't start at a 1024-element boundary.

**Patches:** If some values exceed the bit width (outliers), they're stored separately in a patches array referenced by the metadata. The patches contain (index, value) pairs.

### 6.2 The FastLanes Interleaved Layout

The published FastLanes paper (Afroozeh & Boncz, 2023, TU Eindhoven) defines a specific interleaving pattern for SIMD-friendly memory access. Values within each 1024-element block are transposed across T "lanes" where T is the SIMD register width (e.g., 128/256/512 bits). The Go implementation must replicate this interleaving to correctly decode data written by the Rust SIMD encoder.

The paper is publicly available and provides the complete algorithm with pseudocode.

---

## 7. FSST — String Compression

FSST (Fast Static Symbol Table) is used for string columns when dictionary encoding isn't effective (high cardinality but with repeating substrings). The algorithm:

1. Build a symbol table of up to 255 common substrings (1–8 bytes each)
2. Replace occurrences with single-byte codes
3. Store the symbol table + compressed bytes

The decompressor is straightforward: for each byte in the compressed stream, look up the symbol table and emit the corresponding substring. The symbol table is stored in the FSST encoding's metadata.

Reference: Boncz et al., "FSST: Fast Random Access String Compression", VLDB 2020.

---

## 8. Test Strategy

### 8.1 Reference File Generation

Use the Rust `vx` CLI and Vortex Python library to generate test fixtures:

```bash
# Install vx CLI
cargo install vortex-tui --locked

# Convert Parquet test files to Vortex
vx convert testdata/integers.parquet -o testdata/integers.vtx
vx convert testdata/strings.parquet -o testdata/strings.vtx
vx convert testdata/mixed.parquet -o testdata/mixed.vtx

# Inspect structure for validation
vx browse testdata/integers.vtx
```

Alternatively, use the Python library:

```python
import vortex

# Create test data with known values
arr = vortex.encoding.struct_({
    "id": vortex.array([1, 2, 3, 4, 5]),
    "name": vortex.array(["alice", "bob", "charlie", "alice", "bob"]),
    "score": vortex.array([1.5, 2.7, 3.14, 4.0, 5.5]),
    "active": vortex.array([True, False, True, True, False]),
})

vortex.io.write(arr, "testdata/known_values.vtx")
```

### 8.2 Round-Trip Testing

For each encoding:
1. Generate a file with known data using Rust/Python
2. Read with `vortex-go` → verify values match exactly
3. Once the writer is implemented: write with Go → read with Rust/Python → verify

### 8.3 Fuzz Testing

For the reader, fuzz the FlatBuffer and protobuf parsing with Go's built-in fuzzing:

```go
func FuzzReadFooter(f *testing.F) {
    f.Add(validFooterBytes)
    f.Fuzz(func(t *testing.T, data []byte) {
        // Should not panic, just return error
        _, err := parseFooter(data)
        if err != nil { return }
    })
}
```

---

## 9. Package Structure

```
vortex-go/
├── go.mod
├── go.sum
├── vortex.go                    # Top-level: Open(), File, Scan()
├── dtype.go                     # DType system (Null, Bool, Primitive, Utf8, Struct, etc.)
├── array.go                     # Array representation (decoded data)
├── stats.go                     # ArrayStats, ZoneStats, FileStatistics
│
├── internal/
│   ├── flatbuf/                 # Generated FlatBuffer code (flatc --go)
│   │   ├── footer/              # Postscript, Footer, SegmentSpec, etc.
│   │   ├── layout/              # Layout tree nodes
│   │   ├── dtype/               # DType definitions
│   │   └── array/               # Array, ArrayNode, Buffer
│   │
│   └── proto/                   # Generated protobuf code (protoc --go_out)
│       ├── scalar/              # ScalarValue (for statistics)
│       └── encoding/            # Per-encoding metadata messages
│
├── layout/
│   ├── layout.go                # Layout tree types and navigation
│   ├── struct.go                # Struct layout reader
│   ├── chunked.go               # Chunked layout reader
│   ├── zoned.go                 # Zoned layout reader (zone map pruning)
│   ├── flat.go                  # Flat layout reader (array decoding)
│   └── dict.go                  # Dictionary layout reader
│
├── encoding/
│   ├── registry.go              # Encoding registry (string ID → decoder)
│   ├── primitive.go             # vortex.primitive
│   ├── bool.go                  # vortex.bool
│   ├── varbinview.go            # vortex.varbinview (strings)
│   ├── dict.go                  # vortex.dict
│   ├── constant.go              # vortex.constant
│   ├── masked.go                # vortex.masked (validity)
│   ├── null.go                  # vortex.null
│   ├── sparse.go                # vortex.sparse
│   ├── runend.go                # vortex.runend
│   ├── zigzag.go                # vortex.zigzag
│   ├── bytebool.go              # vortex.bytebool
│   ├── datetimeparts.go         # vortex.datetimeparts
│   ├── zstd.go                  # vortex.zstd
│   ├── fastlanes_bitpacked.go   # fastlanes.bitpacked
│   ├── fastlanes_for.go         # fastlanes.for
│   ├── fastlanes_delta.go       # fastlanes.delta
│   ├── fastlanes_rle.go         # fastlanes.rle
│   ├── fsst.go                  # vortex.fsst
│   ├── alp.go                   # vortex.alp
│   └── pco.go                   # vortex.pco
│
├── writer/
│   ├── writer.go                # File writer
│   ├── strategy.go              # Layout strategy (default: Struct → Zoned → Chunked → Flat)
│   └── compress.go              # Optional compression (dictionary, bitpacking, etc.)
│
├── cmd/
│   └── vtxinfo/                 # CLI tool: inspect Vortex files (Go equivalent of vx browse)
│       └── main.go
│
└── testdata/                    # Reference files generated by Rust/Python
    ├── integers.vtx
    ├── strings.vtx
    ├── mixed_types.vtx
    ├── with_nulls.vtx
    ├── large_chunked.vtx
    └── generate.sh              # Script to regenerate from Rust
```

---

## 10. Dependencies

Minimal dependency tree:

| Dependency | Purpose | Notes |
|---|---|---|
| `github.com/google/flatbuffers/go` | FlatBuffer parsing | Zero-copy, no allocations for reads |
| `google.golang.org/protobuf` | Protobuf decoding | Encoding metadata + scalar values |
| `github.com/pierrec/lz4/v4` | LZ4 segment decompression | Used in CompressionSpec |
| `github.com/klauspost/compress/zstd` | ZStd segment + encoding decompression | Used in CompressionSpec and vortex.zstd encoding |

No CGo. No Rust FFI. Pure Go.

---

## 11. Encoding Algorithm References

Published papers and reference implementations for the non-trivial encodings:

| Encoding | Paper | Reference Code |
|---|---|---|
| FastLanes (BitPack, Delta, FoR, RLE) | Afroozeh & Boncz, "FastLanes: An SIMD-friendly Layout for Columnar Data", VLDB 2023 | `vortex/encodings/fastlanes/` |
| FSST | Boncz et al., "FSST: Fast Random Access String Compression", VLDB 2020 | `vortex/encodings/fsst/` + [cwida/fsst](https://github.com/cwida/fsst) (C++) |
| ALP | Afroozeh & Boncz, "ALP: Adaptive Lossless Floating-Point Compression", SIGMOD 2023 | `vortex/encodings/alp/` |
| BtrBlocks | Kuschewski et al., "BtrBlocks: Efficient Columnar Compression for Data Lakes", SIGMOD 2023 | `vortex/vortex-btrblocks/` |
| PCodec | [pcodec.org](https://pcodec.org) | `vortex/encodings/pco/` |

---

## 12. Relationship to Downstream Projects

```
vortex-go (this project)
  │
  │  import "github.com/your-org/vortex-go"
  │
  ├─── open-kusto
  │      KQL parser, extent catalog, query execution,
  │      materialized views, management commands.
  │      Uses vortex-go as its storage format.
  │
  ├─── Any Go data pipeline
  │      Read/write Vortex files from Go ETL tools,
  │      data ingestion services, etc.
  │
  └─── CLI tools
         vtxinfo (inspect), vtxconvert (Parquet ↔ Vortex), etc.
```

`vortex-go` should never import or know about KQL, Kusto, extents, or databases. It reads and writes files. Everything else is the consumer's concern.
