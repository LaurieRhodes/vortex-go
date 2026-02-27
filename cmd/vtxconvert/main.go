// vtxconvert converts Parquet files to Vortex (.vtx) format.
//
// Usage:
//
//	vtxconvert input.parquet [output.vtx]
//
// If output is omitted, it defaults to input with .vtx extension.
// Supports: bool, int32, int64, float, double, byte_array (→ utf8) columns.
// Nullable columns are written as non-nullable (nulls become zero values).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	vortex "github.com/LaurieRhodes/vortex-go"
	"github.com/LaurieRhodes/vortex-go/writer"
)

var (
	flagChunkSize    = flag.Int("chunk-size", 65536, "rows per vortex chunk")
	flagUncompressed = flag.Bool("uncompressed", false, "disable compression")
	flagVerbose      = flag.Bool("v", false, "verbose output")
)

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: vtxconvert [flags] input.parquet [output.vtx]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	inputPath := flag.Arg(0)
	outputPath := flag.Arg(1)
	if outputPath == "" {
		ext := filepath.Ext(inputPath)
		outputPath = strings.TrimSuffix(inputPath, ext) + ".vtx"
	}

	if err := convert(inputPath, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func convert(inputPath, outputPath string) error {
	start := time.Now()

	// Open parquet file.
	pf, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("opening input: %w", err)
	}
	defer pf.Close()

	fi, err := pf.Stat()
	if err != nil {
		return fmt.Errorf("stat input: %w", err)
	}

	pqFile, err := parquet.OpenFile(pf, fi.Size())
	if err != nil {
		return fmt.Errorf("opening parquet file: %w", err)
	}

	// Convert schema.
	schema, colInfos, err := convertSchema(pqFile.Root())
	if err != nil {
		return fmt.Errorf("converting schema: %w", err)
	}

	if *flagVerbose {
		fmt.Printf("Input:   %s (%d bytes)\n", inputPath, fi.Size())
		fmt.Printf("Rows:    %d\n", pqFile.NumRows())
		fmt.Printf("Columns: %d\n", len(colInfos))
		for _, ci := range colInfos {
			fmt.Printf("  %s: %s\n", ci.name, ci.vortexDType)
		}
	}

	// Create output file.
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output: %w", err)
	}
	defer outFile.Close()

	opts := writer.DefaultOptions()
	if *flagUncompressed {
		opts = writer.UncompressedOptions()
	}

	w, err := writer.NewWriter(outFile, schema, opts)
	if err != nil {
		return fmt.Errorf("creating writer: %w", err)
	}

	// Process each row group.
	totalRows := int64(0)
	chunkSize := *flagChunkSize

	for _, rg := range pqFile.RowGroups() {
		rgRows := rg.NumRows()
		if rgRows == 0 {
			continue
		}

		// Read all column data for this row group.
		colData, err := readRowGroup(rg, colInfos)
		if err != nil {
			return fmt.Errorf("reading row group: %w", err)
		}

		// Write in chunks.
		for offset := 0; offset < int(rgRows); offset += chunkSize {
			end := offset + chunkSize
			if end > int(rgRows) {
				end = int(rgRows)
			}
			n := end - offset

			chunk := make(map[string]*vortex.Array, len(colInfos))
			for i, ci := range colInfos {
				arr, err := sliceColumnData(colData[i], offset, end, ci)
				if err != nil {
					return fmt.Errorf("slicing column %q: %w", ci.name, err)
				}
				chunk[ci.name] = arr
			}

			if err := w.WriteChunk(chunk); err != nil {
				return fmt.Errorf("writing chunk at offset %d: %w", offset, err)
			}
			totalRows += int64(n)
		}
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("closing writer: %w", err)
	}

	outFi, _ := outFile.Stat()
	outSize := int64(0)
	if outFi != nil {
		outSize = outFi.Size()
	} else {
		// Stat after close
		if info, err := os.Stat(outputPath); err == nil {
			outSize = info.Size()
		}
	}

	elapsed := time.Since(start)

	if *flagVerbose {
		ratio := float64(outSize) / float64(fi.Size()) * 100
		fmt.Printf("Output:  %s (%d bytes, %.1f%% of input)\n", outputPath, outSize, ratio)
		fmt.Printf("Rows:    %d written in %v\n", totalRows, elapsed.Round(time.Millisecond))
	} else {
		fmt.Printf("%s → %s (%d rows, %d → %d bytes)\n",
			inputPath, outputPath, totalRows, fi.Size(), outSize)
	}

	return nil
}

// --- Schema conversion ---

type colInfo struct {
	name        string
	colIdx      int
	kind        parquet.Kind
	nullable    bool
	vortexDType *vortex.DType
}

func convertSchema(root *parquet.Column) (*vortex.DType, []colInfo, error) {
	columns := root.Columns()
	if len(columns) == 0 {
		return nil, nil, fmt.Errorf("no columns in parquet file")
	}

	var fieldNames []string
	var fieldTypes []*vortex.DType
	var infos []colInfo

	for i, col := range columns {
		if !col.Leaf() {
			// Skip nested columns for now.
			continue
		}

		dt, err := parquetKindToVortex(col)
		if err != nil {
			if *flagVerbose {
				fmt.Fprintf(os.Stderr, "warning: skipping column %q: %v\n", col.Name(), err)
			}
			continue
		}

		fieldNames = append(fieldNames, col.Name())
		fieldTypes = append(fieldTypes, dt)
		infos = append(infos, colInfo{
			name:        col.Name(),
			colIdx:      i,
			kind:        col.Type().Kind(),
			nullable:    col.Optional(),
			vortexDType: dt,
		})
	}

	if len(fieldNames) == 0 {
		return nil, nil, fmt.Errorf("no supported columns found")
	}

	schema := &vortex.DType{
		Kind:       vortex.DTypeStruct,
		FieldNames: fieldNames,
		FieldTypes: fieldTypes,
	}

	return schema, infos, nil
}

func parquetKindToVortex(col *parquet.Column) (*vortex.DType, error) {
	kind := col.Type().Kind()
	switch kind {
	case parquet.Boolean:
		return &vortex.DType{Kind: vortex.DTypeBool}, nil
	case parquet.Int32:
		return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI32}, nil
	case parquet.Int64:
		return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeI64}, nil
	case parquet.Float:
		return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF32}, nil
	case parquet.Double:
		return &vortex.DType{Kind: vortex.DTypePrimitive, PType: vortex.PTypeF64}, nil
	case parquet.ByteArray:
		return &vortex.DType{Kind: vortex.DTypeUtf8}, nil
	case parquet.FixedLenByteArray:
		return &vortex.DType{Kind: vortex.DTypeBinary}, nil
	default:
		return nil, fmt.Errorf("unsupported parquet kind: %s", kind)
	}
}

// --- Row group reading ---

// columnData holds raw column values for one row group.
type columnData struct {
	bools   []bool
	int32s  []int32
	int64s  []int64
	floats  []float32
	doubles []float64
	strings []string // for ByteArray/FixedLenByteArray
	nulls   []bool   // true = null
}

func readRowGroup(rg parquet.RowGroup, colInfos []colInfo) ([]*columnData, error) {
	nRows := int(rg.NumRows())
	chunks := rg.ColumnChunks()

	result := make([]*columnData, len(colInfos))

	for i, ci := range colInfos {
		if ci.colIdx >= len(chunks) {
			return nil, fmt.Errorf("column index %d out of range", ci.colIdx)
		}

		cd := &columnData{
			nulls: make([]bool, 0, nRows),
		}

		pages := chunks[ci.colIdx].Pages()
		defer pages.Close()

		for {
			page, err := pages.ReadPage()
			if err != nil {
				break // EOF or error
			}

			values := page.Values()
			buf := make([]parquet.Value, 1024)

			for {
				n, err := values.ReadValues(buf)
				for j := 0; j < n; j++ {
					v := buf[j]
					isNull := v.IsNull()
					cd.nulls = append(cd.nulls, isNull)

					switch ci.kind {
					case parquet.Boolean:
						if isNull {
							cd.bools = append(cd.bools, false)
						} else {
							cd.bools = append(cd.bools, v.Boolean())
						}
					case parquet.Int32:
						if isNull {
							cd.int32s = append(cd.int32s, 0)
						} else {
							cd.int32s = append(cd.int32s, v.Int32())
						}
					case parquet.Int64:
						if isNull {
							cd.int64s = append(cd.int64s, 0)
						} else {
							cd.int64s = append(cd.int64s, v.Int64())
						}
					case parquet.Float:
						if isNull {
							cd.floats = append(cd.floats, 0)
						} else {
							cd.floats = append(cd.floats, v.Float())
						}
					case parquet.Double:
						if isNull {
							cd.doubles = append(cd.doubles, 0)
						} else {
							cd.doubles = append(cd.doubles, v.Double())
						}
					case parquet.ByteArray, parquet.FixedLenByteArray:
						if isNull {
							cd.strings = append(cd.strings, "")
						} else {
							cd.strings = append(cd.strings, string(v.ByteArray()))
						}
					}
				}
				if err != nil {
					break
				}
			}
		}

		result[i] = cd
	}

	return result, nil
}

// --- Column slicing and array building ---

func sliceColumnData(cd *columnData, start, end int, ci colInfo) (*vortex.Array, error) {
	n := end - start

	switch ci.kind {
	case parquet.Boolean:
		return buildBoolArray(cd.bools[start:end], n), nil
	case parquet.Int32:
		return buildPrimitiveArray(cd.int32s[start:end], n, ci.vortexDType, 4, func(buf []byte, i int, v int32) {
			binary.LittleEndian.PutUint32(buf[i*4:], uint32(v))
		}), nil
	case parquet.Int64:
		return buildPrimitiveArray(cd.int64s[start:end], n, ci.vortexDType, 8, func(buf []byte, i int, v int64) {
			binary.LittleEndian.PutUint64(buf[i*8:], uint64(v))
		}), nil
	case parquet.Float:
		return buildPrimitiveArray(cd.floats[start:end], n, ci.vortexDType, 4, func(buf []byte, i int, v float32) {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
		}), nil
	case parquet.Double:
		return buildPrimitiveArray(cd.doubles[start:end], n, ci.vortexDType, 8, func(buf []byte, i int, v float64) {
			binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
		}), nil
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return buildStringArray(cd.strings[start:end], n, ci.vortexDType), nil
	default:
		return nil, fmt.Errorf("unsupported kind: %s", ci.kind)
	}
}

func buildBoolArray(vals []bool, n int) *vortex.Array {
	bufSize := (n + 7) / 8
	buf := make([]byte, bufSize)
	for i, v := range vals {
		if v {
			buf[i/8] |= 1 << uint(i%8)
		}
	}
	return &vortex.Array{
		DType:   &vortex.DType{Kind: vortex.DTypeBool},
		Length:  n,
		Buffers: [][]byte{buf},
	}
}

func buildPrimitiveArray[T any](vals []T, n int, dt *vortex.DType, elemSize int, encode func([]byte, int, T)) *vortex.Array {
	buf := make([]byte, n*elemSize)
	for i, v := range vals {
		encode(buf, i, v)
	}
	return &vortex.Array{
		DType:   dt,
		Length:  n,
		Buffers: [][]byte{buf},
	}
}

func buildStringArray(vals []string, n int, dt *vortex.DType) *vortex.Array {
	// Build VarBinView encoding: 16 bytes per view + data buffer.
	viewsBuf := make([]byte, n*16)
	var dataBuf []byte

	for i, s := range vals {
		off := i * 16
		binary.LittleEndian.PutUint32(viewsBuf[off:], uint32(len(s)))
		if len(s) <= 12 {
			copy(viewsBuf[off+4:], s)
		} else {
			copy(viewsBuf[off+4:off+8], s[:4])
			binary.LittleEndian.PutUint32(viewsBuf[off+8:], 0) // buffer index
			binary.LittleEndian.PutUint32(viewsBuf[off+12:], uint32(len(dataBuf)))
			dataBuf = append(dataBuf, s...)
		}
	}

	bufs := [][]byte{viewsBuf}
	if len(dataBuf) > 0 {
		bufs = append(bufs, dataBuf)
	}

	return &vortex.Array{
		DType:   dt,
		Length:  n,
		Buffers: bufs,
	}
}
