# vortex-go Encoding Reference

vortex-go supports 25 array encodings for reading and 8 for writing. This document
describes each encoding, when the writer selects it, and how to add new ones.

## Encoding Support Matrix

| Encoding | Read | Write | Data Types | Description |
|----------|------|-------|-----------|-------------|
| `vortex.primitive` | ✅ | ✅ | All numeric, bool | Flat uncompressed values |
| `vortex.bool` | ✅ | ✅ | Bool | Bit-packed booleans (1 bit per value) |
| `vortex.varbinview` | ✅ | ✅ | Utf8, Binary | Variable-length strings with 16-byte views |
| `vortex.dict` | ✅ | ✅ | Any | Dictionary encoding (codes + values) |
| `vortex.null` | ✅ | ✅ | Any | All-null array |
| `vortex.pco` | ✅ | ✅ | Numeric | Pcodec lossless numerical compression |
| `fastlanes.for` | ✅ | ✅ | Integer | Frame-of-Reference (subtract minimum) |
| `fastlanes.bitpacked` | ✅ | ✅ | Integer | Bit-width reduction via FastLanes SIMD layout |
| `vortex.alp` | ✅ | | Float | Adaptive Lossless float-to-int encoding |
| `vortex.alprd` | ✅ | | Float | ALP Real-Doubles variant |
| `vortex.chunked` | ✅ | | Any | Multi-chunk container (structural) |
| `vortex.constant` | ✅ | | Any | All values identical |
| `vortex.datetimeparts` | ✅ | | Timestamp | Decomposed date/time fields |
| `vortex.decimal_byte_parts` | ✅ | | Decimal | Decomposed decimal encoding |
| `vortex.fsst` | ✅ | | Utf8 | Fast Static Symbol Table string compression |
| `vortex.masked` | ✅ | | Any | Nullable wrapper (validity bitmap + values) |
| `vortex.runend` | ✅ | | Any | Run-length encoding (value + end positions) |
| `vortex.sequence` | ✅ | | Integer | Sequential integers with step |
| `vortex.sparse` | ✅ | | Any | Sparse representation (indices + values) |
| `vortex.struct` | ✅ | | Struct | Column grouping (structural) |
| `vortex.bytebool` | ✅ | | Bool | One-byte-per-boolean representation |
| `vortex.zigzag` | ✅ | | Signed int | ZigZag encoding for signed integers |
| `vortex.zstd` | ✅ | | Any | Zstandard buffer compression |
| `fastlanes.delta` | ✅ | | Integer | Delta encoding via FastLanes layout |
| `fastlanes.rle` | ✅ | | Any | Run-length encoding via FastLanes layout |

**Read:** Decoder registered; files using this encoding can be read.
**Write:** Writer can produce this encoding; used during compression selection.

## Writer Encoding Selection

The writer examines each column's data type and characteristics to select the best encoding
per chunk. Different chunks of the same column may use different encodings.

### Integer Columns

```
Try FoR + BitPacking:
  ├── Compute min value in chunk
  ├── Subtract min (Frame-of-Reference) → all values ≥ 0
  ├── Compute max shifted value → determine bit width
  ├── If bit width < type width: BitPack at reduced width ✓
  └── Otherwise: fall through

Try Pcodec:
  ├── Compress chunk with Pcodec
  ├── If compressed size < 90% of raw size: use Pcodec ✓
  └── Otherwise: fall through

Fall back to Primitive (uncompressed)
```

FoR+BitPacking is highly effective for columns with narrow value ranges (e.g., status codes,
small IDs). Pcodec excels on monotonic sequences and clustered distributions.

### Float Columns

```
Try Pcodec:
  ├── Compress chunk with Pcodec
  ├── If compressed size < 90% of raw size: use Pcodec ✓
  └── Otherwise: fall through

Fall back to Primitive (uncompressed)
```

Pcodec is particularly effective on sensor data, time series, and other
clustered float distributions.

### String Columns (Utf8)

```
Try Dict encoding:
  ├── Count unique values in chunk
  ├── If unique count < 65536 (fits in uint16 codes):
  │   ├── Build dictionary (unique values as VarBinView)
  │   ├── Encode codes via FoR+BitPacking (if beneficial)
  │   └── Use Dict(codes, values) ✓
  └── Otherwise: fall through

Fall back to VarBinView (uncompressed views + heap buffers)
```

Dict encoding is most effective for categorical columns (e.g., country codes,
product categories, status strings) where the same values repeat many times.

### Boolean Columns

Always encoded as `vortex.bool` — bit-packed at 1 bit per value, an 8× reduction
from byte-per-boolean representation.

### Zone Map Statistics

All numeric columns (integer and float) automatically get min/max statistics
recorded per chunk. These are written as `vortex.stats` layout wrappers,
enabling the scan API to prune entire chunks based on filter predicates.

## Encoding Trees

Encodings compose into trees. The writer produces these patterns:

**Integer with FoR+BitPacking:**
```
FoR (reference = min_value)
└── BitPacked (bit_width = ceil(log2(max_shifted + 1)))
    └── Primitive (physical storage)
```

**Dict-encoded string:**
```
Dict
├── codes: FoR → BitPacked → Primitive(U16)
└── values: VarBinView (unique strings)
```

**Pcodec integer:**
```
Pco (compressed Pcodec stream)
```

## Adding a New Encoding

To add support for a new encoding:

1. **Create the decoder** in `encoding/`:

```go
// encoding/myencoding.go
package encoding

import vortex "github.com/LaurieRhodes/vortex-go"

type MyEncodingDecoder struct{}

func init() {
    Register("vortex.myencoding", &MyEncodingDecoder{})
}

func (d *MyEncodingDecoder) Decode(ctx *DecodeContext) (*vortex.Array, error) {
    // Parse encoding-specific metadata from ctx.Metadata()
    // Access buffers via ctx.Buffer(i)
    // Decode children via ctx.DecodeChild(i)
    // Return flat Array with decoded data
}
```

2. **Add tests** in `encoding/` using the Rust-generated reference files:

```go
func TestMyEncoding(t *testing.T) {
    data := loadTestFile(t, "testdata/rust/my_test.vortex")
    // Verify decode produces expected values
}
```

3. **(Optional) Add write support** in `writer/compress.go`:

```go
func (w *Writer) canonicalizeMyEncoding(arr *vortex.Array, dtype *vortex.DType) (*arrayNodeInfo, [][]byte, error) {
    // Transform array data into the encoding's wire format
    // Return array node info and serialised buffers
}
```

4. **Add the encoding to the writer's selection logic** in `writer/writer.go`.
