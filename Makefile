# vortex-go Makefile
#
# Prerequisites:
#   sudo apt install -y flatbuffers-compiler protobuf-compiler
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

.PHONY: all generate generate-flatbuf generate-proto build test vet fmt lint ci clean

# Default target
all: fmt build vet test

# --- Code Generation ---

generate: generate-flatbuf generate-proto

generate-flatbuf:
	@echo "==> Generating FlatBuffer Go code..."
	rm -rf internal/flatbuf/vortexfb/
	flatc --go --gen-all \
		--go-namespace vortexfb \
		-o internal/flatbuf/ \
		-I schemas/flatbuffers/ \
		schemas/flatbuffers/footer.fbs
	flatc --go \
		--go-namespace vortexfb \
		-o internal/flatbuf/ \
		schemas/flatbuffers/dtype.fbs
	@echo "    Generated $$(ls internal/flatbuf/vortexfb/*.go | wc -l) FlatBuffer files"

generate-proto:
	@echo "==> Generating Protobuf Go code..."
	rm -rf internal/proto/scalar/ internal/proto/dtype/ internal/proto/encodings/
	mkdir -p internal/proto/scalar internal/proto/dtype internal/proto/encodings
	protoc --go_out=internal/proto/scalar --go_opt=paths=source_relative \
		-I schemas/proto/ \
		schemas/proto/scalar.proto
	protoc --go_out=internal/proto/dtype --go_opt=paths=source_relative \
		-I schemas/proto/ \
		schemas/proto/dtype.proto
	protoc --go_out=internal/proto/encodings --go_opt=paths=source_relative \
		-I schemas/proto/ \
		schemas/proto/encodings.proto
	@echo "    Generated Protobuf files"

# --- Quality ---

fmt:
	@echo "==> Formatting..."
	@gofmt -w $$(find . -name '*.go' -not -path './.venv/*' -not -path '*/target/*')

fmt-check:
	@echo "==> Checking format..."
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './.venv/*' -not -path '*/target/*'))" \
		|| (echo "Files need gofmt:"; gofmt -l $$(find . -name '*.go' -not -path './.venv/*' -not -path '*/target/*'); exit 1)

vet:
	@echo "==> Running go vet..."
	@go vet ./...

lint: fmt-check vet
	@echo "==> Running golangci-lint..."
	@golangci-lint run ./... || echo "(install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)"

# --- Build & Test ---

build:
	@echo "==> Building..."
	@go build ./...

test:
	@echo "==> Testing..."
	@go test ./... -count=1 -timeout 300s

test-verbose:
	@echo "==> Testing (verbose)..."
	@go test ./... -v -count=1 -timeout 300s

bench:
	@echo "==> Running performance benchmarks..."
	@go test ./benchmark/ -run TestPerformanceReport -v -timeout 300s

bench-crosslang:
	@echo "==> Running cross-language benchmarks..."
	@go test ./benchmark/ -run TestCrossLanguageBenchmark -v -timeout 300s
	@echo ""
	@echo "==> Running Rust benchmark on same files..."
	@cd testdata/testgen && cargo run --bin bench --release -- /tmp/vortex-bench

# --- CI (mirrors GitHub Actions) ---

ci: fmt-check vet build test
	@echo ""
	@echo "==> CI checks passed ✓"

# --- Release ---

BINARY_NAME_INFO = vtxinfo
BINARY_NAME_CONV = vtxconvert
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS = -ldflags "-s -w -X main.version=$(VERSION)"

release-build:
	@echo "==> Building release binaries ($(VERSION))..."
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_INFO)-linux-amd64   ./cmd/vtxinfo
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_CONV)-linux-amd64   ./cmd/vtxconvert
	GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_INFO)-linux-arm64   ./cmd/vtxinfo
	GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_CONV)-linux-arm64   ./cmd/vtxconvert
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_INFO)-darwin-amd64  ./cmd/vtxinfo
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_CONV)-darwin-amd64  ./cmd/vtxconvert
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_INFO)-darwin-arm64  ./cmd/vtxinfo
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_CONV)-darwin-arm64  ./cmd/vtxconvert
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_INFO)-windows-amd64.exe  ./cmd/vtxinfo
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/$(BINARY_NAME_CONV)-windows-amd64.exe  ./cmd/vtxconvert
	@cd dist && sha256sum * > checksums.txt
	@echo "    Built $$(ls dist/ | wc -l) files in dist/"

clean:
	rm -rf dist/
	rm -rf internal/flatbuf/vortexfb/ internal/proto/scalar/ internal/proto/dtype/ internal/proto/encodings/
