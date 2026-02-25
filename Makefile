# vortex-go Makefile
#
# Prerequisites:
#   sudo apt install -y flatbuffers-compiler protobuf-compiler
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

.PHONY: generate generate-flatbuf generate-proto build test vet clean

# Default target
all: generate build vet

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

# --- Build & Test ---

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf internal/flatbuf/vortexfb/ internal/proto/scalar/ internal/proto/dtype/ internal/proto/encodings/
