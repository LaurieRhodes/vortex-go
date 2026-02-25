# vortex-go Project Plan

> **Project:** `github.com/LaurieRhodes/vortex-go`  
> **Goal:** Native Go implementation of the Vortex columnar file format reader/writer  
> **Reference:** Rust `github.com/vortex-data/vortex` (v0.59.4)  
> **Format stability:** Stable since v0.36.0, backwards-compatible  
> **No existing Go implementation exists as of 2026-02-22**

---

## Phase 0: Code Generation from Schemas

### Task 0.1: FlatBuffer Code Generation

**Source:** `schemas/flatbuffers/` (footer.fbs, layout.fbs, dtype.fbs, array.fbs)

**Key types generated:**
- `Postscript` — segment pointers for dtype, layout, statistics, footer
- `PostscriptSegment` — offset, length, alignment, compression
- `Footer` — dictionary tables: array_specs, layout_specs, segment_specs, compression_specs
- `SegmentSpec` — struct: offset(u64), length(u32), alignment_exponent(u8)
- `Layout` — encoding(u16), row_count(u64), metadata([ubyte]), children([Layout]), segments([u32])
- `DType` — union Type { Null, Bool, Primitive, Decimal, Utf8, Binary, Struct_, List, Extension, FixedSizeList }
- `Array` — root ArrayNode + Buffer descriptors
- `ArrayNode` — encoding(u16), metadata([ubyte]), children, buffers([u16]), stats
- `ArrayStats` — min, max, sum, is_sorted, null_count, etc.
- `Buffer` — struct: padding(u16), alignment_exponent(u8), compression(u8), length(u32)

**Action:** `flatc --go -o internal/flatbuf/ schemas/flatbuffers/footer.fbs`

**Note:** Include paths already fixed to same-directory references.

**Deliverable:** Compiled Go FlatBuffer accessors.  
**Verification:** `go build ./internal/flatbuf/...`

---

### Task 0.2: Protobuf Code Generation — Core Types

**Source:** `schemas/proto/` (scalar.proto, dtype.proto)

**Key types generated:**
- `ScalarValue` — oneof: null, bool, int64, uint64, f32, f64, string, bytes, list, f16
- `Scalar` — dtype + value
- `ListValue` — repeated ScalarValue

**Dependency:** `scalar.proto` imports `google/protobuf/struct.proto` (for NullValue).

**Action:** Add `option go_package` to protos, then `protoc --go_out=internal/proto`

**Deliverable:** Generated Go protobuf structs.  
**Verification:** `go build ./internal/proto/...`

---

### Task 0.3: Create Protobuf Definitions for Encoding Metadata

The Rust source defines per-encoding metadata as `#[prost(...)]` structs. No standalone `.proto` files exist — we must create them from the Rust annotations.

Create `schemas/proto/encodings.proto` with these messages (tag numbers verified from source):

**Shared types:**

| Message | Fields (tag) | Source |
|---|---|---|
| PatchesMetadata | len:u64(1), offset:u64(2), indices_ptype:i32(3), chunk_offsets_len:u64?(4), chunk_offsets_ptype:i32?(5) | vortex-array/src/patches.rs |

**FastLanes encodings:**

| Message | Fields (tag) | Array ID |
|---|---|---|
| BitPackedMetadata | bit_width:u32(1), offset:u32(2), patches:PatchesMetadata?(3) | `fastlanes.bitpacked` |
| DeltaMetadata | deltas_len:u64(1), offset:u32(2) | `fastlanes.delta` |
| FastLanesRLEMetadata | values_len:u64(1), indices_len:u64(2), indices_ptype:i32(3), values_idx_offsets_len:u64(4), values_idx_offsets_ptype:i32(5), offset:u64(6) | `fastlanes.rle` |

**Core Vortex encodings:**

| Message | Fields (tag) | Array ID |
|---|---|---|
| RunEndMetadata | ends_ptype:i32(1), num_runs:u64(2), offset:u64(3) | `vortex.runend` |
| SequenceMetadata | base:ScalarValue(1), multiplier:ScalarValue(2) | `vortex.sequence` |
| SparseMetadata | patches:PatchesMetadata(1) required | `vortex.sparse` |

**Temporal and decimal:**

| Message | Fields (tag) | Array ID |
|---|---|---|
| DateTimePartsMetadata | days_ptype:i32(1), seconds_ptype:i32(2), subseconds_ptype:i32(3) | `vortex.datetimeparts` |
| DecimalBytesPartsMetadata | zeroth_child_ptype:i32(1), lower_part_count:u32(2) | `vortex.decimal_byte_parts` |

**Float encodings:**

| Message | Fields (tag) | Array ID |
|---|---|---|
| ALPMetadata | exp_e:u32(1), exp_f:u32(2), patches:PatchesMetadata?(3) | `vortex.alp` |
| ALPRDMetadata | right_bit_width:u32(1), dict_len:u32(2), dict:repeated u32(3), left_parts_ptype:i32(4), patches:PatchesMetadata?(5) | `vortex.alprd` |

**String/compression encodings:**

| Message | Fields (tag) | Array ID |
|---|---|---|
| FSSTMetadata | uncompressed_lengths_ptype:i32(1), codes_offsets_ptype:i32(2) | `vortex.fsst` |
| ZstdFrameMetadata | uncompressed_size:u64(1), n_values:u64(2) | (sub-message) |
| ZstdMetadata | dictionary_size:u32(1), frames:repeated ZstdFrameMetadata(2) | `vortex.zstd` |
| ZstdBuffersMetadata | inner_encoding_id:string(1), inner_metadata:bytes(2), uncompressed_sizes:repeated u64(3), buffer_alignments:repeated u32(4) | `vortex.zstd_buffers` |
| PcoChunkInfo | pages:repeated PcoPageInfo(1) | (sub-message) |
| PcoMetadata | header:bytes(1), chunks:repeated PcoChunkInfo(2) | `vortex.pco` |

**Action:** Write `.proto` file from this table, generate Go code.  
**Deliverable:** Go structs for every encoding's metadata.  
**Verification:** Round-trip: encode/decode with known values.

---

### Task 0.4: Create Makefile for Code Generation

Create `Makefile` or `generate.go` with `//go:generate` directives to regenerate all code from schemas in one step. Include schema source verification (checksum against reference repo versions).

**Deliverable:** `make generate` or `go generate ./...` regenerates all code.

---

## Phase 1: File Reader — Trailer, Postscript, Footer

Open a Vortex file and parse all structural metadata.

### Task 1.1: Read Trailer (8 bytes)

**File structure (end of file):**
```
[version: u16 LE] [postscript_length: u16 LE] [magic: "VTXF" 4 bytes]
```

**Action:** Read last 8 bytes, validate magic = "VTXF", extract version and postscript_length.

**Note:** Initial read should be 64KB (u16::MAX) to guarantee the postscript is captured in one read. The postscript is guaranteed to be <= 65528 bytes.

**Deliverable:** `vortex.Open(r io.ReaderAt, size int64) (*File, error)` returns a File handle.  
**Verification:** Open a file from `vx convert`, verify version and postscript length match `vx browse` output.

**Already started in:** `vortex.go`

---

### Task 1.2: Parse Postscript (FlatBuffer)

**Postscript contains 4 PostscriptSegment entries:**
- `dtype` — location of root DType segment
- `layout` — location of root Layout segment (required)
- `statistics` — location of FileStatistics segment
- `footer` — location of Footer segment (required)

Each PostscriptSegment has: offset(u64), length(u32), alignment_exponent(u8), optional compression, optional encryption.

**Action:** Parse postscript FlatBuffer using generated code. Extract the 4 segment pointers.

**Deliverable:** Postscript struct with 4 segment locations.  
**Verification:** Segment offsets are within file bounds, lengths are reasonable.

---

### Task 1.3: Parse Footer (FlatBuffer)

**Footer contains dictionary tables:**
- `array_specs: [ArraySpec]` — each has `id: string` (e.g. "fastlanes.bitpacked")
- `layout_specs: [LayoutSpec]` — each has `id: string` (e.g. "vortex.chunked")
- `segment_specs: [SegmentSpec]` — each is a packed struct: offset(u64), length(u32), alignment(u8), compression(u8), encryption(u16)
- `compression_specs: [CompressionSpec]` — scheme enum: None(0), LZ4(1), ZLib(2), ZStd(3)

These tables are referenced by index from Layout and ArrayNode trees.

**Action:** Parse footer FlatBuffer. Build lookup slices: `arraySpecs []string`, `layoutSpecs []string`, `segmentSpecs []SegmentSpec`.

**Deliverable:** Footer struct with all lookup tables populated.  
**Verification:** Print array_specs and layout_specs, compare to `vx browse` output.

---

### Task 1.4: Parse DType (FlatBuffer)

**DType is a recursive union type:**
```
DType { type: Type }
Type = Null | Bool | Primitive(ptype, nullable) | Decimal(precision, scale, nullable) |
       Utf8(nullable) | Binary(nullable) | Struct_(names, dtypes, nullable) |
       List(element_type, nullable) | FixedSizeList(element_type, size, nullable) |
       Extension(id, storage_dtype, metadata)
```

**PType enum:** U8(0), U16(1), U32(2), U64(3), I8(4), I16(5), I32(6), I64(7), F16(8), F32(9), F64(10)

**Action:** Parse DType FlatBuffer recursively. Map to the `DType` Go struct already defined in `dtype.go`.

**Deliverable:** `file.DType()` returns the full schema tree.  
**Verification:** For a 2-column file (i32 
+ utf8), verify DType shows `struct { A: i32, B: utf8 }`.

---

### Task 1.5: Parse File-Level Statistics

**FileStatistics contains:** `field_stats: [ArrayStats]` — one per field in root schema.

**ArrayStats fields:** min([ubyte] protobuf ScalarValue), max, sum, is_sorted(bool?), is_strict_sorted(bool?), is_constant(bool?), null_count(u64?), uncompressed_size_in_bytes(u64?), nan_count(u64?).

**Action:** Parse statistics segment, decode ScalarValue protobuf for min/max.

**Deliverable:** `file.Statistics()` returns per-column min/max/null_count.  
**Verification:** Compare against known data values.

---

### Task 1.6: Parse Layout Tree (FlatBuffer)

**Layout node:** encoding(u16 → index into layout_specs), row_count(u64), metadata([ubyte]), children([Layout]), segments([u32] → indices into segment_specs).

**Known layout IDs:**
- `vortex.struct` — one child per column
- `vortex.chunked` — one child per row chunk
- `vortex.stats` — two children: [stats_table, data_layout] (zone maps)
- `vortex.flat` — leaf node, has segments pointing to serialised array data
- `vortex.dict` — two children: [values_layout, codes_layout]

**Zoned layout metadata:** first byte is a boolean indicating whether the first child is a stats table.

**Action:** Parse Layout FlatBuffer recursively, resolve encoding IDs from layout_specs.

**Deliverable:** `file.Layout()` returns navigable tree. Can print tree structure matching `vx browse`.  
**Verification:** Tree depth and shape matches `vx browse` output for test files.

---

## Phase 2: Segment Reading and Decompression

### Task 2.1: Read Raw Segments

Given a SegmentSpec (offset, length), read bytes from file using `io.ReaderAt`.

**Deliverable:** `readSegment(spec SegmentSpec) ([]byte, error)`  
**Verification:** Read a known segment, verify length matches spec.

---

### Task 2.2: Segment Decompression

If a segment's CompressionSpec is not None, decompress after reading.

**Compression schemes:** None(0), LZ4(1), ZLib(2), ZStd(3).

**Go libraries:**
- LZ4: `github.com/pierrec/lz4/v4`
- ZStd: `github.com/klauspost/compress/zstd`
- ZLib: `compress/zlib` (stdlib)

**Deliverable:** `decompressSegment(data []byte, scheme CompressionScheme) ([]byte, error)`  
**Verification:** Decompress a segment from a compressed file, verify expected byte count.

---

## Phase 3: Flat Layout Decoding — Array Parsing

A Flat layout segment contains a serialised Array FlatBuffer followed by data buffers.

### Task 3.1: Parse Array FlatBuffer

**Array contains:** root ArrayNode + Buffer descriptor table.

**ArrayNode:** encoding(u16 → array_specs), metadata([ubyte]), children([ArrayNode]), buffers([u16] → index into Buffer table), stats(ArrayStats).

**Buffer struct (packed 8 bytes):** padding(u16), alignment_exponent(u8), compression(u8: 0=None, 1=LZ4), length(u32).

**Buffer data layout:** After the FlatBuffer bytes, buffers are laid out sequentially with padding between them. Buffer[i] starts at: sum of (padding[j] + length[j]) for j < i, offset from end of FlatBuffer.

**Action:** Parse Array FlatBuffer, extract ArrayNode tree and buffer byte slices.

**Deliverable:** `parseArraySegment(data []byte, specs []string) (*ArrayNode, [][]byte, error)`  
**Verification:** For a Primitive array, verify one buffer of expected size.

---

### Task 3.2: Decode Canonical Encodings

These are the base forms every other encoding decompresses to.

**`vortex.primitive`** — No metadata. One buffer containing raw typed values. Buffer length = row_count * PTypeSize(ptype). PType is inferred from the DType context.

**`vortex.bool`** — No metadata. One buffer containing bit-packed booleans. Buffer length = ceil(row_count / 8).

**`vortex.varbinview`** — Canonical string/binary encoding. Uses "views" (16-byte descriptors per string: if len <= 12, inline; otherwise offset into data buffers). First buffer = views, remaining buffers = string data.

**`vortex.null`** — No buffers, no metadata. Represents row_count nulls.

**`vortex.constant`** — Metadata contains a protobuf ScalarValue. Represents row_count copies of that value.

**`vortex.struct`** — No buffers. One child ArrayNode per field. DType context provides field names.

**`vortex.dict`** — No metadata. Two children: [codes, values]. Codes is an integer array of indices into values.

**`vortex.masked`** — Two children: [values, validity_mask]. Mask is a boolean array (true = valid).

**`vortex.chunked`** — N children, each a sub-array. Concatenate to get the full array.

**Deliverable:** `DecodeArray(node *ArrayNode, buffers [][]byte, dtype *DType) (*Array, error)` for all canonical encodings.  
**Verification:** Read a file written with no compression, verify all values match input.

---

## Phase 4: Compressed Encoding Decoders

Implement in priority order. Each encoding is independent — implement, test, move to next.

### Task 4.1: Frame-of-Reference (P0) — `fastlanes.for`

**Array ID:** `fastlanes.for`  
**Metadata:** The reference (minimum) value is stored as a child scalar — specifically, the first child is a Primitive array containing the reference values (one per 1024-element block, or one for the whole array).  
**Children:** 1 child (the encoded values with reference subtracted, typically BitPacked).  
**Algorithm:** decoded[i] = child[i] + reference_value  
**Deliverable:** Decoder for `fastlanes.for` → delegates to child decoder, then adds reference.  
**Verification:** Create test data [100, 101, 102, ...], FoR-encode, decode, verify.

---

### Task 4.2: BitPacking (P0) — `fastlanes.bitpacked`

**Array ID:** `fastlanes.bitpacked`  
**Metadata:** `BitPackedMetadata { bit_width, offset, patches? }`  
**Buffers:** 1 buffer containing packed bits.  
**Children:** 0 (unless patches present, then 1 child for patch values).

**Algorithm:** The FastLanes paper (Afroozeh & Boncz, TU Eindhoven, 2023) defines an interleaved layout for SIMD-friendly access. Values within 1024-element blocks are transposed across lanes.

**Critical detail:** The bit layout is NOT simple sequential bit-packing. It uses the FastLanes transposition for SIMD parallelism. The Go implementation must replicate this exact interleaving to decode data written by the Rust SIMD encoder. The paper provides pseudocode.

**Offset field:** If offset > 0, the first `offset` values are stored separately (before the main packed buffer).

**Patches:** Outlier values exceeding bit_width are stored in a patches child array (PatchesMetadata gives indices and values).

**Deliverable:** Decoder for `fastlanes.bitpacked` — scalar Go implementation (no SIMD needed).  
**Verification:** Create known integer arrays, compress with Rust, decode with Go, compare.

**Reference:** FastLanes paper, plus `vortex/encodings/fastlanes/src/bitpacking/` for bit layout.

---

### Task 4.3: Dictionary (P0) — `vortex.dict`

Already handled as a canonical encoding (Task 3.2). Two children: codes (integer) + values.  
When BtrBlocks selects dictionary, the codes child will typically be FoR+BitPacked.

**Deliverable:** Verify dictionary works with compressed code arrays (not just canonical).

---

### Task 4.4: ZigZag (P1) — `vortex.zigzag`

**Array ID:** `vortex.zigzag`  
**Metadata:** None.  
**Children:** 1 child (unsigned integer array).  
**Algorithm:** `signed = (unsigned >> 1) ^ -(unsigned & 1)` (standard protobuf zigzag).

**Deliverable:** Trivial decoder — apply formula to child values.

---

### Task 4.5: RunEnd (P1) — `vortex.runend`

**Array ID:** `vortex.runend`  
**Metadata:** `RunEndMetadata { ends_ptype, num_runs, offset }`  
**Children:** 2 children: [ends, values]. Ends is an integer array of run-end positions. Values has one entry per run.

**Algorithm:** For position i, find the run where ends[run] > i, return values[run].

**Deliverable:** Decoder that expands run-end to full array.

---

### Task 4.6: Sparse (P1) — `vortex.sparse`

**Array ID:** `vortex.sparse`  
**Metadata:** `SparseMetadata { patches: PatchesMetadata }`  
**Children:** Contains indices and values for non-fill positions.

**Algorithm:** Fill array with fill_value, then overwrite at patch indices with patch values.

**Deliverable:** Decoder for sparse arrays.

---

### Task 4.7: FastLanes RLE (P1) — `fastlanes.rle`

**Array ID:** `fastlanes.rle`  
**Metadata:** `FastLanesRLEMetadata { values_len, indices_len, indices_ptype, ... }`  
**Children:** Values and indices children.

**Algorithm:** Standard run-length encoding expansion.

**Deliverable:** Decoder for FastLanes RLE.

---

### Task 4.8: ByteBool (P1) — `vortex.bytebool`

**Array ID:** `vortex.bytebool`  
**Metadata:** None.  
**Buffers:** 1 buffer, one byte per boolean (0x00=false, 0x01=true).

**Deliverable:** Trivial decoder — read bytes as booleans.

---

### Task 4.9: DateTimeParts (P1) — `vortex.datetimeparts`

**Array ID:** `vortex.datetimeparts`  
**Metadata:** `DateTimePartsMetadata { days_ptype, seconds_ptype, subseconds_ptype }`  
**Children:** 3 children: [days, seconds, subseconds]. Each is an integer array.

**Algorithm:** `timestamp = days * 86400_000_000_000 + seconds * 1_000_000_000 + subseconds` (nanoseconds).

**Deliverable:** Decoder that reconstructs timestamps from parts.

---

### Task 4.10: Zstd Encoding (P1) — `vortex.zstd`

**Array ID:** `vortex.zstd`  
**Metadata:** `ZstdMetadata { dictionary_size, frames[] }`  
**Buffers:** Compressed data (optional Zstd dictionary + compressed frames).

**Algorithm:** Decompress with standard Zstd, then interpret as the underlying array type.

**Deliverable:** Decoder using `github.com/klauspost/compress/zstd`.

---

### Task 4.11: FSST (P2) — `vortex.fsst`

**Array ID:** `vortex.fsst`  
**Metadata:** `FSSTMetadata { uncompressed_lengths_ptype, codes_offsets_ptype }`  
**Children:** Symbol table, compressed codes, offsets, uncompressed lengths.

**Algorithm:** FSST paper (Boncz et al., VLDB 2020). Build symbol table of up to 255 substrings (1-8 bytes), replace occurrences with single-byte codes. Decompressor looks up each code byte in symbol table.

**Reference:** C++ reference at `github.com/cwida/fsst`

**Deliverable:** FSST decompressor in Go.

---

### Task 4.12: ALP (P2) — `vortex.alp`

**Array ID:** `vortex.alp`  
**Metadata:** `ALPMetadata { exp_e, exp_f, patches? }`  
**Children:** 1 child (integer-encoded float values), optional patches child.

**Algorithm:** ALP paper (Afroozeh & Boncz, SIGMOD 2023). Floats encoded as: `int_value = round(float_value * 10^exp_e) / 10^exp_f`. Decoder reverses: `float_value = int_value * 10^exp_f / 10^exp_e`.

**Deliverable:** ALP decoder.

---

### Task 4.13: Delta (P2) — `fastlanes.delta`

**Array ID:** `fastlanes.delta`  
**Metadata:** `DeltaMetadata { deltas_len, offset }`  
**Children:** 1 child containing base values per block, plus delta buffer.

**Algorithm:** Prefix-sum (cumulative sum) to reconstruct values from deltas.

**Deliverable:** Delta decoder.

---

### Task 4.14: Sequence (P2) — `vortex.sequence`

**Array ID:** `vortex.sequence`  
**Metadata:** `SequenceMetadata { base: ScalarValue, multiplier: ScalarValue }`  
**No children, no buffers.

**Algorithm:** `value[i] = base + (i * multiplier)`. Pure computation, no I/O.

**Deliverable:** Sequence generator.

---

### Task 4.15: ALPRD, Pco, DecimalByteParts (P3)

Lower priority. Implement when needed.
- `vortex.alprd` — ALP Right-Digit variant for doubles with right-digit patterns
- `vortex.pco` — PCodec, complex but published at pcodec.org
- `vortex.decimal_byte_parts` — Decimal reconstruction from byte parts

---

## Phase 5: Zone Map Pruning (Scan API)

### Task 5.1: Zoned Layout Reader

Parse the Zoned layout ("vortex.stats"). First child is the stats table (a Flat layout containing per-zone statistics). Second child is the data layout.

**Stats table structure:** A struct array with one row per zone. Each column contains min/max/null_count for the corresponding data column. Zone size = 8192 rows (configurable, stored in layout metadata).

**Action:** Decode stats table. For each zone, extract per-column statistics.

**Deliverable:** `ZonedReader` that returns zone-level stats and can skip zones.

---

### Task 5.2: Scan API with Filter Pushdown

Combine column projection (Struct layout navigation) with zone pruning (Zoned layout stats).

**Deliverable:** `file.Scan(ScanOptions{Columns, Filter, Limit}) → ArrayIterator`  
**Verification:** Write file with timestamps spanning multiple zones, filter by time range, verify only matching zones are read (count ReadAt calls).

---

## Phase 6: File Writer

### Task 6.1: Write Canonical (Uncompressed) Vortex Files

Write valid Vortex files using only canonical encodings (Primitive, Bool, VarBinView). Layout: Struct → Zoned → Chunked → Flat.

**File structure to emit:**
1. Magic "VTXF" (4 bytes)
2. Data segments (column chunks with padding)
3. Statistics segments
4. Footer FlatBuffer (array_specs, layout_specs, segment_specs)
5. DType FlatBuffer
6. Layout FlatBuffer
7. FileStatistics FlatBuffer
8. Postscript FlatBuffer (segment pointers)
9. Version(u16) + PostscriptLength(u16) + Magic "VTXF"

**Zone statistics:** Compute min/max per 8192-row block for each column.

**Deliverable:** `NewWriter(w, schema, opts)` → `WriteChunk()` → `Close()`  
**Verification:** Write with Go, read with Go. Write with Go, read with Rust `vx browse`. Write with Go, read with DuckDB Vortex extension.

---

### Task 6.2: Add Dictionary Encoding to Writer

For string columns with cardinality < threshold (e.g. < 65536 unique values), dictionary-encode automatically.

**Deliverable:** Writer optionally applies dictionary encoding.  
**Verification:** Round-trip dictionary-encoded strings through Go writer → Go reader.

---

### Task 6.3: Add FastLanes BitPacking to Writer

For integer columns, compute optimal bit width, apply FoR + BitPacking.

**Deliverable:** Writer applies integer compression.  
**Verification:** Write compressed integers, read back with both Go and Rust.

---

## Phase 7: CLI Tool and Cross-Validation

### Task 7.1: vtxinfo CLI

Build `cmd/vtxinfo/` — a Go equivalent of `vx browse` for inspecting Vortex files.

**Output:** File version, schema, row count, layout tree structure, per-column statistics, encoding breakdown.

**Deliverable:** `go build ./cmd/vtxinfo && vtxinfo testfile.vtx`  
**Verification:** Output matches `vx browse` for the same file.

---

### Task 7.2: Cross-Validation Test Suite

Generate reference test files using Rust:
- `integers.vtx` — Various integer types with known values
- `strings.vtx` — String columns (low and high cardinality)
- `mixed.vtx` — Multiple column types including nullable
- `timestamps.vtx` — DateTime columns spanning multiple zones
- `large.vtx` — Enough rows to trigger multiple chunks and zones
- `all_encodings.vtx` — Force specific encodings for coverage

Store in `testdata/`. Include `testdata/generate.sh` that regenerates from Rust.

**Deliverable:** `go test ./...` validates every encoding against reference files.

---

## Phase 8: Integration Testing with Downstream

### Task 8.1: DuckDB Round-Trip

Write a Vortex file with Go, load it with DuckDB's Vortex extension, verify query results.

### Task 8.2: Python Round-Trip

Write a Vortex file with Go, load with `import vortex` (Python), verify values.

### Task 8.3: Parquet Conversion Utility

Build a `vtxconvert` CLI that converts Parquet files to Vortex using `parquet-go` for reading and `vortex-go` for writing.

---

## Appendix A: Complete Array Encoding ID Registry

All encoding string IDs that may appear in the footer's `array_specs`:

**Canonical (vortex-array):**
`vortex.null`, `vortex.bool`, `vortex.primitive`, `vortex.decimal`, `vortex.varbin`, `vortex.varbinview`, `vortex.list`, `vortex.listview`, `vortex.fixed_size_list`, `vortex.struct`, `vortex.chunked`, `vortex.constant`, `vortex.masked`, `vortex.dict`, `vortex.slice`, `vortex.shared`, `vortex.ext`, `vortex.arrow`

**Compressed (encoding crates):**
`vortex.alp`, `vortex.alprd`, `vortex.bytebool`, `vortex.datetimeparts`, `vortex.decimal_byte_parts`, `vortex.fsst`, `vortex.pco`, `vortex.runend`, `vortex.sequence`, `vortex.sparse`, `vortex.zigzag`, `vortex.zstd`, `vortex.zstd_buffers`

**FastLanes:**
`fastlanes.bitpacked`, `fastlanes.delta`, `fastlanes.for`, `fastlanes.rle`

## Appendix B: Layout Encoding ID Registry

All layout string IDs that may appear in the footer's `layout_specs`:

`vortex.flat`, `vortex.chunked`, `vortex.struct`, `vortex.stats` (zoned), `vortex.dict`

## Appendix C: Default Write Strategy (Rust Reference)

The default Rust writer produces this layout tree (from `vortex-file/src/strategy.rs`):

```
Step 0: TableStrategy (Struct — splits columns, extracts validity)
Step 1: RepartitionStrategy (8K row blocks, no minimum byte size)
Step 2: ZonedStrategy (zone map stats per 8K rows)
Step 3: DictStrategy (dictionary encode if beneficial, fallback to Step 4)
Step 4: RepartitionStrategy (coalesce to ~1MB blocks, canonicalize)
Step 5: CompressingStrategy (BtrBlocks cascading compression)
Step 6: BufferedStrategy (2MB buffer)
Step 7: ChunkedLayoutStrategy → FlatLayoutStrategy (leaf)
```

BtrBlocks selects per-column from:
- **Integers:** Uncompressed, Constant, FoR, BitPacking, ZigZag, Sparse, Dict, RunEnd, Sequence, RLE, Pco
- **Floats:** Uncompressed, Constant, ALP, ALPrd, Dict, RunEnd, RLE, Sparse, Pco
- **Strings:** Uncompressed, Dict, FSST, Constant, Sparse, Zstd, ZstdBuffers

## Appendix D: Go Dependencies

| Package | Version | Purpose |
|---|---|---|
| `github.com/google/flatbuffers/go` | latest | FlatBuffer parsing (zero-copy reads) |
| `google.golang.org/protobuf` | latest | Protobuf decoding (encoding metadata + scalars) |
| `github.com/pierrec/lz4/v4` | latest | LZ4 segment decompression |
| `github.com/klauspost/compress/zstd` | latest | ZStd segment + encoding decompression |

No CGo. No Rust FFI. Pure Go.
