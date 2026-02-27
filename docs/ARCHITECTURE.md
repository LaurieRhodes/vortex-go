# vortex-go Architecture

This document describes the internal design of vortex-go: how Vortex files are
structured, how the reader decodes data, and how the writer produces compressed output.

## File Format Overview

A Vortex file is a binary format with the following structure:

```
┌──────────────────────────────┐
│  Segment 0 (data buffers)    │
│  Segment 1 (data buffers)    │
│  ...                         │
│  Segment N (data buffers)    │
├──────────────────────────────┤
│  Footer (FlatBuffer)         │
│    - Schema (DType tree)     │
│    - Layout tree             │
│    - Array specs (encodings) │
│    - Segment table           │
├──────────────────────────────┤
│  Postscript (16 bytes)       │
│    - Footer offset + length  │
│    - Magic bytes: VRTX       │
└──────────────────────────────┘
```

**Segments** are contiguous byte ranges in the file, each optionally compressed with
LZ4 or Zstd. A segment contains one or more buffers for a single array encoding.

**The footer** is a FlatBuffer message containing the entire file metadata: the schema
as a DType tree, a layout tree describing how columns are organised, array specifications
mapping encoding IDs to their names and metadata, and a segment table mapping segment
indices to file offsets and lengths.

**The postscript** is a fixed 16-byte trailer at the end of the file that points to
the footer, allowing the reader to locate all metadata with a single read from the end.

## Schema (DType)

Vortex schemas are trees of DType nodes, serialised as Protobuf in the footer:

```
Struct
├── "id": Primitive(I64)
├── "value": Primitive(F64)
└── "name": Utf8
```

The Go representation is:

```go
type DType struct {
    Kind       DTypeKind     // Struct, Primitive, Utf8, Bool, Null, Binary, Extension
    PType      PType         // For Primitive: I8..I64, U8..U64, F16..F64
    Nullable   bool
    FieldNames []string      // For Struct
    FieldTypes []*DType      // For Struct
}
```

## Layout Tree

The layout tree describes how data is physically organised in the file. Each column
has a layout that may be nested:

```
Struct (root)
├── Column "id": Zoned(Stats) → Chunked → [Flat, Flat, ...]
├── Column "value": Zoned(Stats) → Chunked → [Flat, Flat, ...]
└── Column "name": Zoned(Stats) → Chunked → [Flat, Flat, ...]
```

Layout types:

| Type | Purpose |
|------|---------|
| `Struct` | Groups columns together; children are per-column layouts |
| `Chunked` | Splits a column into multiple chunks (row groups) |
| `Zoned` / `Stats` | Wraps a Chunked layout with per-zone min/max statistics |
| `Flat` | Leaf node: references segments containing actual encoded data |
| `InlineDType` | Embeds the column's DType alongside data |

**Navigation:** To read column `"id"`, the reader traverses: root Struct → child 0 →
unwrap Stats → unwrap Chunked → iterate Flat children (one per chunk).

## Encoding Tree

Within each Flat leaf, data is stored as an **encoding tree** — a tree of ArrayNode
structures where each node is a specific encoding. For example, a dict-encoded string
column looks like:

```
Dict
├── codes: FoR
│   └── child: BitPacked
│       └── child: Primitive(U16)
└── values: VarBinView
    └── buffers: [views, heap0, heap1, ...]
```

Each ArrayNode contains:
- An encoding ID (`"vortex.dict"`, `"fastlanes.for"`, etc.)
- Buffers (raw data segments)
- Children (sub-arrays for composite encodings)
- Metadata (Protobuf-encoded, encoding-specific parameters)

The reader recursively decodes by looking up the encoding ID in the decoder registry
and calling the appropriate `Decoder.Decode()` method, which may recursively decode
children before producing the final flat arrays.

## Decoder Registry

All 25 decoders register themselves in `encoding/registry.go` at import time:

```go
import _ "github.com/LaurieRhodes/vortex-go/encoding" // registers all decoders
```

Each decoder implements the `Decoder` interface:

```go
type Decoder interface {
    Decode(ctx *DecodeContext) (*vortex.Array, error)
}
```

The `DecodeContext` provides access to the node's metadata, buffers, children, DType,
and row count. Decoders call `ctx.DecodeChild(i)` to recursively decode sub-arrays.

Adding a new encoding requires implementing `Decoder` and calling `Register("encoding.name", decoder)`.

## Scan Pipeline

The scan API (`file.Scan(opts)`) returns a `ScanIterator` that yields chunks:

```
1. Parse ScanOptions
   ├── Column list (or all columns if empty)
   ├── RowFilter predicates
   └── Limit

2. Pre-compute layout navigation
   └── For each requested column: locate layout path through Struct → Stats → Chunked

3. Zone map evaluation (if RowFilter set)
   ├── Load zone statistics from Stats layout
   ├── For each zone: evaluate predicates against min/max
   └── Mark zones as pruned or active

4. Chunk iteration (ScanIterator.Next())
   ├── Check if current chunk's zones are all pruned → skip
   ├── For each requested column:
   │   ├── Navigate to the Flat layout for this chunk
   │   ├── Load segments (decompress LZ4/Zstd if needed)
   │   ├── Build ArrayNode tree from array specs
   │   └── Call DecodeArray() → recursive decode through encoding tree
   └── Return ScanResult{RowCount, Columns}
```

**Column projection** works at step 2: only requested columns have their layouts
navigated and segments loaded. Unrequested columns are never read from disk.

**Zone pruning** works at step 3-4: entire chunks are skipped without loading any
data when the filter predicates prove no rows can match based on zone statistics.

## Writer Pipeline

The writer (`writer.NewWriter()`) produces valid Vortex files:

```
1. Receive schema (DType) and options

2. For each WriteChunk(columns):
   ├── For each column, select encoding:
   │   ├── Integers: try FoR+BitPacking → try Pcodec → fall back to Primitive
   │   ├── Floats: try Pcodec → fall back to Primitive
   │   ├── Strings: try Dict(VarBinView) → fall back to VarBinView
   │   └── Bools: Bool (bit-packed)
   ├── Compute zone statistics (min/max for numeric columns)
   ├── Serialise encoded arrays to segment buffers
   └── Track segment offsets

3. On Close():
   ├── Write Stats segments (zone map metadata)
   ├── Build layout tree:
   │   └── Struct → [Stats → Chunked → [Flat...] per column]
   ├── Build footer FlatBuffer (schema, layouts, array specs, segments)
   ├── Write footer
   └── Write 16-byte postscript
```

**Encoding selection** is greedy: the writer tries the most compressed encoding first
and falls back to less compressed alternatives. The decision is per-chunk, so different
chunks of the same column may use different encodings depending on data characteristics.

## Buffer Alignment

VarBinView arrays require 16-byte alignment for the views buffer (which contains
128-bit view structures). All other buffers use 8-byte alignment. The writer handles
this by specifying `bufferAligns` when constructing array nodes, and padding segment
data to the required boundary.

## Segment Compression

Each segment can be independently compressed:

| Scheme | Code | Usage |
|--------|------|-------|
| None | 0 | Uncompressed |
| LZ4 | 1 | Fast decompression |
| Zstd | 2 | Better compression ratio |

The reader detects the compression scheme from the segment metadata and decompresses
on the fly during scan. The writer currently produces uncompressed segments (the
encoding-level compression from dict/FoR/BitPacking/Pcodec provides the primary
compression).

## File Statistics

The reader extracts statistics from the footer for `vtxinfo` and diagnostic purposes:

- Total rows (from root Struct metadata)
- Per-column zone statistics (min/max scalars per zone)
- Encoding usage (unique encoding IDs across all array specs)
- Layout tree structure (types and nesting)
- Segment count and total data size
