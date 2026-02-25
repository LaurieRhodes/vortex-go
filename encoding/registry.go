package encoding

import (
	"fmt"
	"sync"

	vortex "github.com/LaurieRhodes/vortex-go"
)

// DecodeContext carries all state needed for recursive array decoding.
type DecodeContext struct {
	// AllBuffers is the complete buffer set extracted from the segment.
	AllBuffers [][]byte
}

// Decoder decodes an ArrayNode with its buffer data into a decoded Array.
type Decoder interface {
	// Decode decodes the given node. nodeBuffers are the buffers selected by the node's
	// BufferIndices. ctx provides access to allBuffers for child decoding.
	Decode(node *vortex.ArrayNode, nodeBuffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error)
}

// registry is the global encoding decoder registry.
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Decoder)
)

// Register adds a decoder for the given encoding ID.
func Register(id string, dec Decoder) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[id] = dec
}

// Get returns the decoder for the given encoding ID.
func Get(id string) (Decoder, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	dec, ok := registry[id]
	if !ok {
		return nil, fmt.Errorf("no decoder for encoding %q", id)
	}
	return dec, nil
}

// DecodeArray recursively decodes an ArrayNode tree into an Array.
// It resolves the encoding for each node and delegates to the appropriate decoder.
// allBuffers contains all buffers extracted from the segment; each node's
// BufferIndices select which buffers it uses.
func DecodeArray(node *vortex.ArrayNode, allBuffers [][]byte, dtype *vortex.DType, rowCount uint64) (*vortex.Array, error) {
	ctx := &DecodeContext{AllBuffers: allBuffers}
	return decodeWithContext(node, dtype, rowCount, ctx)
}

// decodeWithContext is the internal recursive decoder.
func decodeWithContext(node *vortex.ArrayNode, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	dec, err := Get(node.EncodingID)
	if err != nil {
		return nil, err
	}

	// Select the buffers this node references.
	nodeBuffers := make([][]byte, len(node.BufferIndices))
	for i, idx := range node.BufferIndices {
		if int(idx) >= len(ctx.AllBuffers) {
			return nil, fmt.Errorf("buffer index %d out of range for encoding %q", idx, node.EncodingID)
		}
		nodeBuffers[i] = ctx.AllBuffers[idx]
	}

	return dec.Decode(node, nodeBuffers, dtype, rowCount, ctx)
}

// DecodeChild is a convenience for decoders that need to recursively decode a child node.
func DecodeChild(node *vortex.ArrayNode, childIdx int, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	if childIdx >= len(node.Children) {
		return nil, fmt.Errorf("child index %d out of range (have %d children)", childIdx, len(node.Children))
	}
	return decodeWithContext(node.Children[childIdx], dtype, rowCount, ctx)
}
