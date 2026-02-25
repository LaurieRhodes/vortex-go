# vortex-go Implementation Plan

> **Created:** 2026-02-22  
> **Reference spec:** `/media/laurie/Data/Github/open-kusto/docs/engineering/VORTEX_GO_PROJECT.md`  
> **Reference implementation:** `/media/laurie/Data/Github/vortex` (Rust, v0.59.4)

Status: `[ ]` not started · `[~]` in progress · `[x]` complete

---

## Phase 0: Code Generation and Build Pipeline

### 0.1 FlatBuffer Code Generation
- [x] Copy `.fbs` schemas from reference repo to `schemas/flatbuffers/`
- [x] Fix include paths (flatten to single directory)
- [ ] Install/verify `flatc` compiler
- [ ] Add Go namespace declarations to each `.fbs` file
- [ ] Generate Go code: `flatc --go -o internal/flatbuf schemas/flatbuffers/footer.fbs`
- [ ] Generate Go code for `layout.fbs`, `dtype.fbs`, `array.fbs`
- [ ] Verify generated code compiles
- [ ] Add `go:generate` directives to `internal/flatbuf/generate.go`

### 0.2 Protobuf Code Generation
- [x] Copy `.proto` schemas from reference repo to `schemas/proto/`
- [ ] Install/verify `protoc` and `protoc-gen-go`
- [ ] Add `go_package` options to each `.proto` file
- [ ] Generate Go code: `protoc --go_out=internal/proto --go_opt=paths=source_relative schemas/proto/*.proto`
- [ ] Verify generated code compiles
- [ ] Add `go:generate` directives

### 0.3 Encoding Metadata Proto Extraction
Extract protobuf message definitions from Rust `#[prost(...)]` annotations into `.proto` files:
- [ ] `fastlanes_bitpacked.proto` — `BitPackedMetadata { bit_width, offset, patches }`
- [ ] `fastlanes_for.proto` — `FoRMetadata` (check if metadata-free, reference may be child scalar)
- [ ] `fastlanes_delta.proto` — `DeltaMetadata { validity_len }`
- [ ] `fastlanes_rle.proto` — `RLEMetadata { run_ends_ptype, length }`
- [ ] `vortex_runend.proto` — `RunEndMetadata { ends_ptype, num_runs, offset }`
- [ ] `vortex_sequence.proto` — `SequenceMetadata { start, stop, step }`
- [ ] `vortex_sparse.proto` — `SparseMetadata { fill_value, indices_ptype, len }`
- [ ] `vortex_fsst.proto` — `FSSTMetadata { ... }`
- [ ] `vortex_alp.proto` — `ALPMetadata { ... }`
- [ ] `vortex_alprd.proto` — `ALPRDMetadata { ... }`
- [ ] `vortex_datetimeparts.proto` — `DateTimePartsMetadata { ... }`
- [ ] `vortex_zstd.proto` — `ZstdMetadata { ... }`
- [ ] `vortex_pco.proto` — `PcoMetadata { ... }`
- [ ] Generate Go code from all encoding metadata protos
- [ ] Verify all generated code compiles

### 0.4 Dependencies and Build Verification
- [ ] `go get github.com/google/flatbuffers/go`
- [ ] `go get google.golang.org/protobuf`
- [ ] `go get github.com/pierrec/lz4/v4`
- [ ] `go get github.com/klauspost/compress/zstd`
- [ ] `go mod tidy`
- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes

### 0.5 Test Fixture Generation
- [ ] Install Rust `vx` CLI: `cargo install vortex-tui --locked`
- [ ] Write `testdata/generate.sh` to produce reference files
- [ ] Generate `testdata/integers.vtx` — i32 and i64 columns, no nulls
- [ ] Generate `testdata/strings.vtx` — low-cardinality strings (triggers dictionary encoding)
- [ ] Generate `testdata/mixed.vtx` — int + string + float + bool columns
- [ ] Generate `testdata/nulls.vtx` — nullable columns with null values
- [ ] Generate `testdata/timestamps.vtx` — datetime columns
- [ ] Generate `testdata/large.vtx` — >8192 rows (triggers multiple zones)
- [ ] Run `vx browse` on each and capture expected layout/schema for validation
- [ ] Commit fixtures to repo

---

## Phase 1: File Reader — Metadata Layer

### 1.1 End-of-File Parsing (`vortex.go`)
- [x] Define `Magic` constant and `EndOfFileSize`
- [x] Read last 8 bytes: version(u16) + postscript_length(u16) + magic(4)
- [x] Validate trailing magic
- [x] Extract version and postscript length
- [x] Validate leading magic (first 4 bytes)
- [ ] Test: valid reference file opens without error
- [ ] Test: file too small returns error
- [ ] Test: wrong magic returns error
- [ ] Test: verify version matches expected

### 1.2 Postscript Parsing
- [x] Calculate postscript offset from file end
- [x] Read postscript bytes
- [ ] Parse FlatBuffer `Postscript` table
- [ ] Extract `PostscriptSegment` for dtype (offset, length, compression)
- [ ] Extract `PostscriptSegment` for layout (required)
- [ ] Extract `PostscriptSegment` for statistics (optional)
- [ ] Extract `PostscriptSegment` for footer (required)
- [ ] Handle compressed postscript segments (CompressionSpec inline)
- [ ] Test: verify segment offsets are within file bounds
- [ ] Test: verify against `vx browse` output

### 1.3 Footer Parsing (`footer.go`)
- [x] Define `Footer` struct with spec lookup tables
- [x] Define `SegmentSpec` and `CompressionScheme` types
- [ ] Read footer segment (using offset/length from postscript)
- [ ] Decompress if needed (check postscript compression spec)
- [ ] Parse FlatBuffer `Footer` table
- [ ] Build `ArraySpecs` slice: index → string encoding ID (e.g. "fastlanes.bitpacked")
- [ ] Build `LayoutSpecs` slice: index → string layout ID (e.g. "vortex.chunked")
- [ ] Build `SegmentSpecs` slice: index → `SegmentSpec{Offset, Length, Alignment, Compression}`
- [ ] Build `CompressionSpecs` slice
- [ ] Test: verify array spec count matches reference
- [ ] Test: verify layout spec strings match known values
- [ ] Test: verify segment count matches reference

### 1.4 DType Parsing (`dtype.go`)
- [x] Define `DType` struct with all variant fields
- [x] Define `DTypeKind` and `PType` enums
- [x] Implement `String()` methods
- [ ] Parse FlatBuffer `DType` table recursively
- [ ] Handle `Null` variant
- [ ] Handle `Bool` variant (with nullable)
- [ ] Handle `Primitive` variant (with PType + nullable)
- [ ] Handle `Decimal` variant (with precision + scale + nullable)
- [ ] Handle `Utf8` variant (with nullable)
- [ ] Handle `Binary` variant (with nullable)
- [ ] Handle `Struct_` variant (with names + recursive dtypes + nullable)
- [ ] Handle `List` variant (with element_type + nullable)
- [ ] Handle `FixedSizeList` variant (with element_type + size + nullable)
- [ ] Handle `Extension` variant (with id + storage_dtype + metadata)
- [ ] Test: parse schema from integers.vtx → verify column names and types
- [ ] Test: parse schema from mixed.vtx → verify all type variants
- [ ] Test: parse nested struct schema if present

### 1.5 Layout Tree Parsing (`layout.go`)
- [x] Define `Layout` struct
- [x] Define layout ID constants
- [ ] Parse FlatBuffer `Layout` table recursively
- [ ] Resolve `encoding` (uint16) → string ID via `footer.LayoutSpecs`
- [ ] Extract `row_count`
- [ ] Extract `metadata` bytes
- [ ] Recursively parse `children`
- [ ] Extract `segments` (uint32 indices into SegmentSpecs)
- [ ] Test: verify top-level is Struct layout
- [ ] Test: verify child count matches column count from DType
- [ ] Test: verify Zoned layouts appear under Struct children
- [ ] Test: verify Flat layouts are leaf nodes with segment references

### 1.6 File-Level Statistics
- [ ] Read statistics segment (from postscript, optional)
- [ ] Parse FlatBuffer `FileStatistics` → per-field `ArrayStats`
- [ ] Decode protobuf `ScalarValue` for min/max/sum
- [ ] Map field stats to column names (using DType field order)
- [ ] Test: verify min/max for integer column matches expected data range
- [ ] Test: verify null_count matches known null count

### 1.7 Segment Reader
- [ ] Implement `readSegment(spec SegmentSpec) → []byte`
- [ ] Handle `CompressionNone` → raw read
- [ ] Handle `CompressionLZ4` → LZ4 decompress after read
- [ ] Handle `CompressionZStd` → ZStd decompress after read
- [ ] Respect alignment from `AlignmentExponent`
- [ ] Test: read known segment, verify byte count matches Length

### 1.8 Integration: `vtxinfo` CLI
- [ ] Create `cmd/vtxinfo/main.go`
- [ ] Accept filename argument
- [ ] Open file with `vortex.Open()`
- [ ] Print version, schema, layout tree summary, statistics
- [ ] Compare output against `vx browse` for same file
- [ ] This is the first end-to-end validation tool

---

## Phase 2: Flat Layout Decoding — Canonical Encodings

### 2.1 Array Segment Parsing
- [ ] Read Flat layout segment bytes
- [ ] Parse FlatBuffer `Array` table → `ArrayNode` tree + `Buffer` descriptors
- [ ] Resolve buffer byte ranges within the segment (cumulative offset from Buffer padding + length)
- [ ] Resolve encoding IDs via `footer.ArraySpecs`
- [ ] Handle buffer-level LZ4 decompression (Buffer.compression field)

### 2.2 Encoding Registry
- [ ] Create `encoding/registry.go` with decoder registration
- [ ] Define `Decoder` interface: `Decode(node *ArrayNode, buffers [][]byte, dtype *DType) (*Array, error)`
- [ ] Implement `Get(encodingID string) (Decoder, error)`
- [ ] Register all implemented decoders at init time

### 2.3 Primitive Decoder (`encoding/primitive.go`)
- [ ] Read raw buffer bytes
- [ ] Interpret as typed slice based on PType (i8, i16, i32, i64, u8, ..., f32, f64)
- [ ] Handle endianness (Vortex is little-endian)
- [ ] Test: decode i32 array, verify values match
- [ ] Test: decode f64 array, verify values match
- [ ] Test: verify length matches expected row count

### 2.4 Bool Decoder (`encoding/bool.go`)
- [ ] Read bit-vector buffer
- [ ] Unpack bits to `[]bool`
- [ ] Handle trailing bits in last byte
- [ ] Test: decode bool array, verify values

### 2.5 VarBinView Decoder (`encoding/varbinview.go`)
- [ ] Parse views buffer (each view: length + inline-or-offset data)
- [ ] For short strings (≤12 bytes): read inline from view
- [ ] For long strings: read from referenced data buffer at offset
- [ ] Handle multiple data buffers
- [ ] Test: decode string array with short strings
- [ ] Test: decode string array with long strings
- [ ] Test: verify string values match expected

### 2.6 Null Decoder (`encoding/null.go`)
- [ ] Return array of N nulls (no buffers needed)
- [ ] Test: verify length

### 2.7 Constant Decoder (`encoding/constant.go`)
- [ ] Read scalar value from metadata
- [ ] Expand to array of N copies
- [ ] Handle all scalar types
- [ ] Test: verify all values are identical

### 2.8 Dict Decoder (`encoding/dict.go`)
- [ ] Decode codes child array (integers)
- [ ] Decode values child array (the dictionary)
- [ ] Reconstruct: for each code, look up value in dictionary
- [ ] Test: round-trip low-cardinality strings through dictionary

### 2.9 Masked Decoder (`encoding/masked.go`)
- [ ] Decode values child array
- [ ] Decode mask child array (boolean validity)
- [ ] Apply mask: where mask is false, value is null
- [ ] Test: verify null positions match expected

### 2.10 Chunked Decoder (`encoding/chunked.go`)
- [ ] Decode each child array
- [ ] Concatenate into single array
- [ ] Test: verify total length equals sum of chunk lengths

### 2.11 Integration Test
- [ ] Open `testdata/integers.vtx` → navigate to column → decode → verify values
- [ ] Open `testdata/strings.vtx` → navigate to column → decode → verify values
- [ ] Open `testdata/mixed.vtx` → decode all columns → verify all types work together

---

## Phase 3: Compressed Encoding Decoders — P0 (Required)

### 3.1 FastLanes Frame-of-Reference (`encoding/fastlanes_for.go`)
- [ ] Parse FoR metadata (reference/minimum value)
- [ ] Decode child array (the offset values, typically bitpacked)
- [ ] Add reference value to each decoded offset
- [ ] Test: known values → FoR encode with Rust → decode with Go → verify

### 3.2 FastLanes BitPacking (`encoding/fastlanes_bitpacked.go`)
- [ ] Parse `BitPackedMetadata`: bit_width, offset, patches
- [ ] Implement scalar unpacking: extract N-bit integers from packed buffer
- [ ] Handle the FastLanes 1024-element block interleaving pattern
- [ ] Handle `offset` (sub-block start position)
- [ ] Handle patches (outlier values stored separately)
- [ ] Test: unpack known bitpacked buffer, verify values
- [ ] Test: verify patch values are correctly applied
- [ ] Test: multiple block sizes (< 1024, exact 1024, > 1024 rows)

### 3.3 Cascaded Decoding Integration
- [ ] Verify recursive decoding works: FoR → BitPacking → raw buffer
- [ ] Verify Dict → FoR → BitPacking cascade
- [ ] Test: open reference file with typical BtrBlocks output, decode full column

---

## Phase 4: Compressed Encoding Decoders — P1 (Common)

### 4.1 RunEnd Decoder (`encoding/runend.go`)
- [ ] Parse `RunEndMetadata`: ends_ptype, num_runs, offset
- [ ] Decode run-ends child array and values child array
- [ ] Expand: for each run, repeat value from start to end position
- [ ] Test: verify expanded values match expected

### 4.2 ZigZag Decoder (`encoding/zigzag.go`)
- [ ] Decode child array (unsigned integers)
- [ ] Apply inverse zigzag: `(n >> 1) ^ -(n & 1)`
- [ ] Test: verify negative values decode correctly

### 4.3 Sparse Decoder (`encoding/sparse.go`)
- [ ] Parse `SparseMetadata`: fill_value, indices_ptype, len
- [ ] Create array filled with fill_value
- [ ] Decode indices child and values child
- [ ] Place values at specified indices
- [ ] Test: verify sparse array with known fill + patches

### 4.4 FastLanes RLE Decoder (`encoding/fastlanes_rle.go`)
- [ ] Parse `RLEMetadata`: run_ends_ptype, length
- [ ] Decode run-ends and values
- [ ] Expand runs
- [ ] Test: verify expanded array

### 4.5 ByteBool Decoder (`encoding/bytebool.go`)
- [ ] Read buffer: one byte per boolean (0x00 = false, 0x01 = true)
- [ ] Convert to `[]bool`
- [ ] Test: verify values

### 4.6 DateTimeParts Decoder (`encoding/datetimeparts.go`)
- [ ] Parse `DateTimePartsMetadata`
- [ ] Decode three child arrays: days, seconds, subseconds
- [ ] Reconstruct timestamps: combine parts into nanosecond-precision values
- [ ] Test: verify known timestamps decode correctly

### 4.7 ZStd Encoding Decoder (`encoding/zstd.go`)
- [ ] Parse `ZstdMetadata`
- [ ] Decompress buffer using `klauspost/compress/zstd`
- [ ] Decode decompressed child array
- [ ] Test: verify string data decompresses correctly

---

## Phase 5: Layout-Level Features

### 5.1 Struct Layout Navigation (`layout/struct.go`)
- [ ] Given column name + DType schema → find child index
- [ ] Return child Layout for the target column
- [ ] Handle column projection: only navigate to requested columns
- [ ] Test: project 2 of 5 columns, verify only those are accessed

### 5.2 Chunked Layout Iteration (`layout/chunked.go`)
- [ ] Iterate children as row-group chunks
- [ ] Return chunk layout + row range for each
- [ ] Test: verify chunk count and row counts

### 5.3 Zoned Layout Pruning (`layout/zoned.go`)
- [ ] Read stats table from first child (compressed Flat layout)
- [ ] Decode zone statistics: per-zone min/max for each column
- [ ] Given a predicate, determine which zones match
- [ ] Return only matching data chunks from second child
- [ ] Test: write file with distinct value ranges per zone, verify pruning skips correct zones

### 5.4 Dictionary Layout (`layout/dict.go`)
- [ ] Read shared dictionary values from first child
- [ ] Read codes from second child
- [ ] Reconstruct values per chunk
- [ ] Test: verify dictionary layout decode

### 5.5 Scan API Integration (`scan.go`)
- [ ] Implement `File.Scan()` with `ScanOptions`
- [ ] Column projection → Struct layout navigation
- [ ] Zone pruning → Zoned layout stats check
- [ ] Row limit → stop after N rows
- [ ] Stream results via `ArrayIterator`
- [ ] Test: full scan of reference file, verify all rows returned
- [ ] Test: projected scan, verify only requested columns
- [ ] Test: filtered scan, verify zone pruning reduces I/O
- [ ] Test: limited scan, verify row count capped

---

## Phase 6: File Writer

### 6.1 Canonical Writer (`writer/writer.go`)
- [ ] Accept schema (DType) and write options
- [ ] Write leading magic "VTXF"
- [ ] Accept column arrays as chunks
- [ ] Serialise each chunk as ArrayNode + buffers (canonical encodings only)
- [ ] Build Flat layouts for each chunk
- [ ] Build Chunked layouts wrapping chunk Flats
- [ ] Build Zoned layouts with computed zone statistics
- [ ] Build Struct layout wrapping all columns
- [ ] Compute zone stats: min/max per 8192-row block per column
- [ ] Write footer FlatBuffer (array_specs, layout_specs, segment_specs)
- [ ] Write DType FlatBuffer
- [ ] Write Layout FlatBuffer
- [ ] Write Statistics FlatBuffer
- [ ] Write Postscript FlatBuffer
- [ ] Write trailing version(u16) + postscript_length(u16) + magic "VTXF"
- [ ] Test: write file → read back with Go reader → verify values round-trip
- [ ] Test: write file → read with Rust `vx browse` → verify valid
- [ ] Test: write file → read with DuckDB Vortex extension → verify valid

### 6.2 Segment Management
- [ ] Track segment offsets as data is written
- [ ] Apply alignment padding between segments
- [ ] Build segment_specs table from recorded positions
- [ ] Optional: LZ4 or ZStd compression for segments

### 6.3 Statistics Computation
- [ ] Compute min/max for Primitive columns (per zone)
- [ ] Compute min/max for Utf8 columns (per zone)
- [ ] Compute null_count per column per zone
- [ ] Compute is_sorted per column
- [ ] Encode stats as protobuf ScalarValue
- [ ] Embed in Zoned layout stats table

---

## Phase 7: Compressed Writers (Incremental)

### 7.1 Dictionary Encoding Writer
- [ ] Detect low-cardinality string columns
- [ ] Build dictionary from unique values
- [ ] Emit Dict array: codes + values
- [ ] Test: round-trip through Go writer → Go reader

### 7.2 FastLanes FoR + BitPacking Writer
- [ ] Compute minimum value for integer column
- [ ] Subtract minimum (Frame-of-Reference)
- [ ] Compute required bit width
- [ ] Pack integers using FastLanes 1024-block interleaving
- [ ] Emit BitPacked array with metadata
- [ ] Test: round-trip through Go writer → Go reader
- [ ] Test: read Go-written file with Rust reader

### 7.3 ZStd Compression Writer
- [ ] Compress string buffers with ZStd
- [ ] Emit Zstd-wrapped array
- [ ] Test: round-trip verification

### 7.4 RunEnd / RLE Writer
- [ ] Detect runs in sorted or repetitive columns
- [ ] Emit run-end encoded arrays
- [ ] Test: round-trip verification

---

## Phase 8: CLI Tooling

### 8.1 `vtxinfo` — File Inspector (`cmd/vtxinfo/`)
- [ ] Open file, print format version
- [ ] Print schema (column names and types)
- [ ] Print layout tree (indented, with row counts and encoding IDs)
- [ ] Print file-level statistics (min/max per column)
- [ ] Print segment summary (count, total size, compression breakdown)
- [ ] Optional `--json` output for programmatic use

### 8.2 `vtxcat` — Print Contents (future)
- [ ] Read and print first N rows as table
- [ ] Column projection via `--columns` flag
- [ ] Row filter via `--where` flag (simple predicate)

---

## Cross-Cutting Concerns

### Error Handling
- [ ] All public APIs return descriptive errors with context
- [ ] Unknown encoding IDs return error (not panic)
- [ ] Corrupt/truncated segments return error
- [ ] Fuzz test FlatBuffer parsing
- [ ] Fuzz test Protobuf parsing

### Performance (Deferred)
- [ ] Benchmark column read (projected vs full)
- [ ] Benchmark zone pruning (filtered vs unfiltered)
- [ ] Profile memory allocation during decode
- [ ] Consider buffer pooling for segment reads
- [ ] Consider SIMD-optimised bitpacking (Go assembly) as stretch goal

### Documentation
- [x] README.md with project overview
- [x] `docs/IMPLEMENTATION_PLAN.md` (this file)
- [ ] GoDoc comments on all public types and functions
- [ ] Architecture overview in `docs/ARCHITECTURE.md`
- [ ] Contributing guide

---

## Milestone Definitions

**M1 — Can read metadata:** Open any Vortex file, print schema + layout tree + statistics. (`vtxinfo` works.)
> Phases: 0 + 1

**M2 — Can read data:** Decode all canonical + P0 compressed encodings. Full column reads work.
> Phases: 2 + 3

**M3 — Can scan efficiently:** Column projection, zone pruning, row limits. Scan API works.
> Phases: 4 + 5

**M4 — Can write files:** Produce valid Vortex files readable by Rust/DuckDB/Python.
> Phase: 6

**M5 — Can write compressed:** Dictionary, BitPacking, ZStd compression in writer.
> Phase: 7

**M6 — Production ready:** CLI tools, documentation, fuzz testing, benchmarks.
> Phase: 8 + cross-cutting
