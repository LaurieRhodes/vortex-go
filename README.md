# vortex-go

[![CI](https://github.com/LaurieRhodes/vortex-go/actions/workflows/ci.yml/badge.svg)](https://github.com/LaurieRhodes/vortex-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/LaurieRhodes/vortex-go.svg)](https://pkg.go.dev/github.com/LaurieRhodes/vortex-go)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A native Go implementation of the [Vortex columnar file format](https://spiraldb.com/vortex).

Vortex is a next-generation columnar file format (Linux Foundation project) designed for analytical workloads. It achieves significantly faster scans and better compression than Apache Parquet through adaptive encoding selection and lazy materialisation.

`vortex-go` provides full read and write support from pure Go — no CGo, no Rust FFI.

## Key Features

- **25 encoding decoders** — ALP, BitPacking, Dict, FoR, FSST, Pcodec, RLE, Zstd, and more
- **Compressed writer** — automatic encoding selection (dict, FoR+BitPacking, Pcodec, zone maps)
- **Column projection** — read only the columns you need, skip the rest
- **Zone map pruning** — skip entire chunks based on min/max filter predicates
- **Full cross-compatibility** — files readable by the Rust reference, Python vortex, and DuckDB
- **CLI tools** — `vtxinfo` for file inspection, `vtxconvert` for Parquet-to-Vortex conversion
- **Zero external dependencies at runtime** — only Go standard library plus FlatBuffers/Protobuf

## Performance

On a 7-column schema at 1M rows (Intel 14th Gen, Go 1.24):

| Metric                    | Parquet     | Vortex (Go) | Improvement     |
| ------------------------- | ----------- | ----------- | --------------- |
| File size                 | 45.6 MB     | 19.3 MB     | **58% smaller** |
| Full scan                 | 188 ms      | 40 ms       | **5× faster**   |
| Projected scan (2/7 cols) | 188 ms      | 16 ms       | **12× faster**  |
| Throughput                | 5.3M rows/s | 25M rows/s  | **5× faster**   |

Zone map pruning on 100K rows (10 chunks, filter selects 10%): **8.5× faster** than full scan.

See [docs/BENCHMARKS.md](docs/BENCHMARKS.md) for full results including Go vs Rust comparison.

## Installation

**As a library:**

```bash
go get github.com/LaurieRhodes/vortex-go
```

**CLI tools:**

```bash
go install github.com/LaurieRhodes/vortex-go/cmd/vtxinfo@latest
go install github.com/LaurieRhodes/vortex-go/cmd/vtxconvert@latest
```

Or download pre-built binaries from [Releases](https://github.com/LaurieRhodes/vortex-go/releases).

## Quick Start

### Reading a Vortex File

```go
package main

import (
    "fmt"
    "os"

    vortex "github.com/LaurieRhodes/vortex-go"
    _ "github.com/LaurieRhodes/vortex-go/encoding" // register all decoders
)

func main() {
    f, _ := os.Open("data.vtx")
    defer f.Close()
    fi, _ := f.Stat()

    file, _ := vortex.Open(f, fi.Size())

    // Scan all columns
    iter, _ := file.Scan(vortex.ScanOptions{})
    for iter.Next() {
        result := iter.Result()
        fmt.Printf("Chunk: %d rows\n", result.RowCount)
        for name, arr := range result.Columns {
            fmt.Printf("  %s: %d values\n", name, arr.Length)
        }
    }
}
```

### Column Projection

```go
// Read only "id" and "name" columns — skips decoding all others
iter, _ := file.Scan(vortex.ScanOptions{
    Columns: []string{"id", "name"},
})
```

### Filtered Scan with Zone Pruning

```go
// Skip chunks where "amount" cannot be in range [100, 500]
iter, _ := file.Scan(vortex.ScanOptions{
    RowFilter: vortex.NewRowFilter(
        vortex.GTE("amount", float64(100)),
        vortex.LTE("amount", float64(500)),
    ),
})
// Chunks outside the range are never read or decoded
```

### Writing a Vortex File

```go
package main

import (
    "encoding/binary"
    "math"
    "os"

    vortex "github.com/LaurieRhodes/vortex-go"
    "github.com/LaurieRhodes/vortex-go/writer"
)

func main() {
    schema := &vortex.DType{
        Kind:       vortex.DTypeStruct,
        FieldNames: []string{"id", "value", "name"},
        FieldTypes: []*vortex.DType{
            {Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64},
            {Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64},
            {Kind: vortex.DTypeUtf8},
        },
    }

    out, _ := os.Create("output.vtx")
    defer out.Close()

    w, _ := writer.NewWriter(out, schema, writer.DefaultOptions())

    // Write a chunk of 1000 rows
    n := 1000
    ids := make([]byte, n*8)
    vals := make([]byte, n*8)
    views := make([]byte, n*16)
    names := []string{"alice", "bob", "charlie"}

    for i := 0; i < n; i++ {
        binary.LittleEndian.PutUint64(ids[i*8:], uint64(int64(i)))
        binary.LittleEndian.PutUint64(vals[i*8:], math.Float64bits(float64(i)*0.5))
        s := names[i%3]
        binary.LittleEndian.PutUint32(views[i*16:], uint32(len(s)))
        copy(views[i*16+4:], s)
    }

    w.WriteChunk(map[string]*vortex.Array{
        "id":    {DType: schema.FieldTypes[0], Length: n, Buffers: [][]byte{ids}},
        "value": {DType: schema.FieldTypes[1], Length: n, Buffers: [][]byte{vals}},
        "name":  {DType: schema.FieldTypes[2], Length: n, Buffers: [][]byte{views}},
    })
    w.Close()
}
```

## CLI Tools

### vtxinfo

Inspect Vortex file structure:

```
$ vtxinfo data.vtx
File:    data.vtx
Size:    19.3 MB
Version: 1

Schema:
  struct {
    id: i64
    value: f64
    category: utf8
  }

Rows:    1000000
Columns: 3
Segments: 42
Encodings used: fastlanes.bitpacked, fastlanes.for, vortex.bool,
                vortex.dict, vortex.primitive, vortex.struct, vortex.varbinview

Layout types: vortex.flat, vortex.struct, vortex.stats
```

### vtxconvert

Convert Parquet to Vortex:

```
$ vtxconvert input.parquet output.vtx
Converted: input.parquet → output.vtx
  Rows:    1000000
  Parquet: 45.6 MB → Vortex: 19.3 MB (42% of original)
  Time:    490ms
```

## Cross-Compatibility

Files written by vortex-go are readable by every consumer in the Vortex ecosystem:

| Writer → Reader | Go  | Rust | Python | DuckDB |
| --------------- | --- | ---- | ------ | ------ |
| **Go writer**   | ✅   | ✅    | ✅      | ✅      |
| **Rust writer** | ✅   | ✅    | ✅      | ✅      |

Verified with 91 Go tests, Rust cross-compatibility tests, and 48 Python/DuckDB checks.

## Project Structure

```
vortex.go           Core types: File, Open(), DType, Array
scan.go             Scan API: ScanOptions, ScanIterator, column projection
filter.go           Filter predicates: GTE, LTE, EQ, etc.
zonemap.go          Zone map statistics and pruning logic
reader.go           File reader: footer parsing, segment loading
layout.go           Layout tree navigation

encoding/           25 array encoding decoders
  registry.go       Decoder registry and dispatch
  primitive.go      Primitive (flat numeric) arrays
  dict.go           Dictionary encoding
  fastlanes_*.go    FastLanes BitPacking, FoR, Delta, RLE
  alp.go            ALP float encoding
  pco.go            Pcodec integration
  fsst.go           FSST string compression
  ...

pcodec/             Pcodec numerical compression (full Go port)
writer/             Vortex file writer with compression
cmd/vtxinfo/        File inspection CLI
cmd/vtxconvert/     Parquet → Vortex converter CLI
integration/        Integration tests
benchmark/          Performance benchmarks

internal/flatbuf/   Generated FlatBuffer code (file structure)
internal/proto/     Generated Protobuf code (encoding metadata)
schemas/            Source .fbs and .proto schemas
testdata/           Reference test files (Rust-generated + Go-generated)
docs/               Architecture, benchmarks, encoding reference
```

## Documentation

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — File format, decode pipeline, scan internals
- **[docs/BENCHMARKS.md](docs/BENCHMARKS.md)** — Performance data: Vortex vs Parquet, Go vs Rust
- **[docs/ENCODINGS.md](docs/ENCODINGS.md)** — Supported encodings reference and writer strategy
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — Development setup, testing, adding encodings
- **[CHANGELOG.md](CHANGELOG.md)** — Release history

## Building from Source

```bash
git clone https://github.com/LaurieRhodes/vortex-go.git
cd vortex-go

# Run all checks (format, vet, build, test)
make ci

# Run benchmarks
make bench

# Build release binaries
make release-build
```

## Dependencies

| Dependency                           | Purpose                                        |
| ------------------------------------ | ---------------------------------------------- |
| `github.com/google/flatbuffers/go`   | FlatBuffer parsing (file structure)            |
| `google.golang.org/protobuf`         | Protobuf decoding (encoding metadata)          |
| `github.com/pierrec/lz4/v4`          | LZ4 decompression                              |
| `github.com/klauspost/compress/zstd` | Zstd decompression                             |
| `github.com/parquet-go/parquet-go`   | Parquet reading (vtxconvert + benchmarks only) |

## Related Projects

- [Vortex](https://github.com/spiraldb/vortex) — Rust reference implementation (Spiral/Linux Foundation)
- [open-kusto](https://github.com/LaurieRhodes/open-kusto) — KQL query engine using vortex-go as storage backend

## Licence

MIT — see [LICENSE](LICENSE)
