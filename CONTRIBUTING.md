# Contributing to vortex-go

## Development Setup

### Prerequisites

- Go 1.24 or later
- (Optional) Rust toolchain — for cross-compatibility tests and benchmarks
- (Optional) Python 3.12+ — for Python/DuckDB interop tests
- (Optional) `flatc` and `protoc` — only if regenerating code from schemas

### Clone and Verify

```bash
git clone https://github.com/LaurieRhodes/vortex-go.git
cd vortex-go

# Run all CI checks locally
make ci
```

This runs formatting check, `go vet`, build, and the full test suite.

## Project Layout

| Path | Purpose |
|------|---------|
| `vortex.go` | Core types: `File`, `Open()`, `DType`, `Array` |
| `scan.go` | Scan API: `ScanOptions`, `ScanIterator`, column projection |
| `filter.go` | Filter predicates and zone map pruning logic |
| `zonemap.go` | Zone map statistics types |
| `reader.go` | File reader: footer parsing, segment loading |
| `layout.go` | Layout tree types and navigation |
| `encoding/` | 25 array encoding decoders |
| `pcodec/` | Pcodec compression (full Go port of the decoder + encoder) |
| `writer/` | Vortex file writer with compression selection |
| `cmd/vtxinfo/` | CLI: file inspection |
| `cmd/vtxconvert/` | CLI: Parquet → Vortex conversion |
| `integration/` | Integration tests (Parquet round-trip, cross-compat) |
| `benchmark/` | Performance benchmarks (vs Parquet, vs Rust) |
| `internal/flatbuf/` | Generated FlatBuffer code (do not edit) |
| `internal/proto/` | Generated Protobuf code (do not edit) |
| `schemas/` | Source `.fbs` and `.proto` schemas |
| `testdata/` | Reference test files and generators |

## Running Tests

### Go Tests (primary)

```bash
# All tests
go test ./... -count=1 -timeout 300s

# Verbose
go test ./... -v -count=1 -timeout 300s

# Single package
go test ./encoding/ -v -run TestDict
```

### Rust Cross-Compatibility

Requires the Rust vortex workspace at `../vortex/`:

```bash
cd testdata/testgen

# Verify Rust can read Go-written files
cargo run --bin read_go_file --release
```

### Python/DuckDB Interop

```bash
# Setup (once)
python3 -m venv .venv
. .venv/bin/activate
pip install -r testdata/requirements.txt

# Run
python3 testdata/test_python_interop.py
```

### Benchmarks

```bash
# Vortex vs Parquet performance
make bench

# Go vs Rust cross-language comparison
make bench-crosslang
```

## Regenerating Test Data

If you modify the writer or need fresh reference files:

```bash
cd testdata
./generate.sh
```

This regenerates all Rust reference files, Go cross-compat files, runs the Go test suite,
and (if available) the Rust cross-compat and Python interop tests.

## Regenerating Code from Schemas

Only needed if the `.fbs` or `.proto` schemas change:

```bash
# Install prerequisites
sudo apt install -y flatbuffers-compiler protobuf-compiler
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

# Regenerate
make generate
```

## Code Style

- **Format:** All code must pass `gofmt`. Run `make fmt` before committing.
- **Vet:** Must pass `go vet ./...` with no warnings.
- **Lint:** Must pass `golangci-lint run ./...` (see `.golangci.yml` for config).
- **Tests:** All new functionality should have tests. Encoding decoders should be
  tested against Rust-generated reference files where possible.
- **Comments:** All exported types and functions must have doc comments.
- **Error handling:** Return errors rather than panicking. Use `fmt.Errorf` with
  `%w` for wrapping.

## Adding a New Encoding

See [docs/ENCODINGS.md](docs/ENCODINGS.md) for the step-by-step guide to implementing
a decoder and optionally a writer for a new encoding.

## Making a Release

1. Ensure `make ci` passes
2. Update `CHANGELOG.md`
3. Tag: `git tag v0.x.0 && git push --tags`
4. The GitHub Actions release workflow will automatically build binaries and create
   a GitHub Release
