package encoding

import (
	"fmt"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"github.com/LaurieRhodes/vortex-go/pcodec"
	"google.golang.org/protobuf/proto"
)

// PcoDecoder decodes vortex.pco arrays using the pure-Go pcodec package.
//
// Vortex PcoArray layout:
//
//	Metadata (protobuf): PcoMetadata { header, chunks[] { pages[] { n_values } } }
//	Buffers: [chunk_meta_0, ..., chunk_meta_N, page_0, page_1, ..., page_M]
//	    First len(chunks) buffers are chunk metadata (pco ChunkMeta bytes).
//	    Remaining buffers are page data (ordered by chunk, then page within chunk).
//	Children: 0 or 1 (optional validity bitmap).
type PcoDecoder struct{}

func (d *PcoDecoder) Decode(node *vortex.ArrayNode, buffers [][]byte, dtype *vortex.DType, rowCount uint64, ctx *DecodeContext) (*vortex.Array, error) {
	// Parse protobuf metadata.
	var meta encodings.PcoMetadata
	if len(node.Metadata) > 0 {
		if err := proto.Unmarshal(node.Metadata, &meta); err != nil {
			return nil, fmt.Errorf("pco: parsing metadata: %w", err)
		}
	}

	// Map the vortex DType to pcodec types.
	latentType, numberType, err := pcoTypesFromDType(dtype)
	if err != nil {
		return nil, err
	}

	// Split buffers: first N = chunk_metas, rest = pages.
	nChunks := len(meta.Chunks)
	if len(buffers) < nChunks {
		return nil, fmt.Errorf("pco: expected at least %d buffers for chunk metas, got %d",
			nChunks, len(buffers))
	}
	chunkMetaBufs := buffers[:nChunks]
	pageBufs := buffers[nChunks:]

	// Build the page layout from metadata.
	var (
		pagesPerChunk []int
		pageNValues   []int
		totalPages    int
	)
	for _, chunk := range meta.Chunks {
		nPages := len(chunk.Pages)
		pagesPerChunk = append(pagesPerChunk, nPages)
		for _, page := range chunk.Pages {
			pageNValues = append(pageNValues, int(page.NValues))
		}
		totalPages += nPages
	}

	if len(pageBufs) < totalPages {
		return nil, fmt.Errorf("pco: expected %d page buffers, got %d", totalPages, len(pageBufs))
	}

	// Decompress using the pure-Go pcodec package.
	latents, err := pcodec.Decompress(
		meta.Header,
		chunkMetaBufs,
		pageBufs[:totalPages],
		pagesPerChunk,
		pageNValues,
		latentType,
	)
	if err != nil {
		return nil, fmt.Errorf("pco: decompression: %w", err)
	}

	// Convert latent-ordered uint64 values to final byte representation.
	data := pcodec.LatentsToBytes(numberType, latents)

	// Handle validity child.
	arr := &vortex.Array{
		DType:   dtype,
		Length:  int(rowCount),
		Buffers: [][]byte{data},
		Stats:   node.Stats,
	}

	// Decode validity child if present.
	if len(node.Children) > 0 {
		validityNode := node.Children[0]
		validityDType := &vortex.DType{Kind: vortex.DTypeBool}
		validityArr, err := decodeWithContext(validityNode, validityDType, rowCount, ctx)
		if err != nil {
			return nil, fmt.Errorf("pco: decoding validity: %w", err)
		}
		arr.Children = []*vortex.Array{validityArr}
	}

	return arr, nil
}

// pcoTypesFromDType maps a vortex DType to pcodec LatentType and NumberType.
func pcoTypesFromDType(dtype *vortex.DType) (pcodec.LatentType, pcodec.NumberType, error) {
	if dtype.Kind != vortex.DTypePrimitive {
		return 0, 0, fmt.Errorf("pco: unsupported dtype kind %v (only primitive types)", dtype.Kind)
	}
	switch dtype.PType {
	case vortex.PTypeU16:
		return pcodec.LatentU16, pcodec.NumU16, nil
	case vortex.PTypeU32:
		return pcodec.LatentU32, pcodec.NumU32, nil
	case vortex.PTypeU64:
		return pcodec.LatentU64, pcodec.NumU64, nil
	case vortex.PTypeI16:
		return pcodec.LatentU16, pcodec.NumI16, nil
	case vortex.PTypeI32:
		return pcodec.LatentU32, pcodec.NumI32, nil
	case vortex.PTypeI64:
		return pcodec.LatentU64, pcodec.NumI64, nil
	case vortex.PTypeF16:
		return pcodec.LatentU16, pcodec.NumF16, nil
	case vortex.PTypeF32:
		return pcodec.LatentU32, pcodec.NumF32, nil
	case vortex.PTypeF64:
		return pcodec.LatentU64, pcodec.NumF64, nil
	default:
		return 0, 0, fmt.Errorf("pco: unsupported ptype %v", dtype.PType)
	}
}

func init() {
	Register("vortex.pco", &PcoDecoder{})
}
