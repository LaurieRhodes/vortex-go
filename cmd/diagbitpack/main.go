package main

import (
	"encoding/binary"
	"fmt"
	"os"

	vortex "github.com/LaurieRhodes/vortex-go"
	_ "github.com/LaurieRhodes/vortex-go/encoding"
	"github.com/LaurieRhodes/vortex-go/internal/proto/encodings"
	"google.golang.org/protobuf/proto"
)

func main() {
	fname := os.Args[1]
	f, _ := os.Open(fname)
	fi, _ := f.Stat()
	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		fmt.Println("Open error:", err)
		return
	}

	fmt.Printf("Schema: %s\n", vf.DType())
	fmt.Printf("Rows: %d\n", vf.Layout().RowCount)
	fmt.Printf("ArraySpecs: %v\n", vf.Footer().ArraySpecs)

	root := vf.Layout()
	if !root.IsStruct() {
		return
	}
	for ci, child := range root.Children {
		name := vf.DType().FieldNames[ci]
		colDtype := vf.DType().FieldTypes[ci]
		fmt.Printf("\n=== Column %d: %s (%s) ===\n", ci, name, colDtype)

		data, dictValues := vortex.NavigateToData(child)
		if dictValues != nil {
			fmt.Printf("  Dict layout: values=%s, codes=%s\n", dictValues.EncodingID, data.EncodingID)
		}

		flats := vortex.CollectFlatLayouts(data)
		for fi, flat := range flats {
			fmt.Printf("\n  Flat[%d]: rows=%d\n", fi, flat.RowCount)
			node, bufs, err := vf.ReadFlatSegment(flat)
			if err != nil {
				fmt.Printf("    Error: %v\n", err)
				continue
			}
			dumpNode(node, bufs, colDtype, flat.RowCount, "    ")
		}
	}
}

func dumpNode(node *vortex.ArrayNode, bufs [][]byte, dtype *vortex.DType, rowCount uint64, indent string) {
	fmt.Printf("%s[%s] children=%d bufs=%v meta=%db\n",
		indent, node.EncodingID, len(node.Children), node.BufferIndices, len(node.Metadata))

	for i, bi := range node.BufferIndices {
		if int(bi) < len(bufs) {
			b := bufs[bi]
			fmt.Printf("%s  buf[%d]: %d bytes", indent, i, len(b))
			if len(b) <= 48 {
				fmt.Printf(" hex=%x", b)
			}
			fmt.Println()
		}
	}

	if node.EncodingID == "fastlanes.bitpacked" && len(node.Metadata) > 0 {
		var meta encodings.BitPackedMetadata
		if err := proto.Unmarshal(node.Metadata, &meta); err == nil {
			fmt.Printf("%s  BP: bw=%d, offset=%d, patches=%v\n",
				indent, meta.BitWidth, meta.Offset, meta.Patches != nil)

			if len(node.BufferIndices) > 0 && int(node.BufferIndices[0]) < len(bufs) {
				buf := bufs[node.BufferIndices[0]]
				bw := int(meta.BitWidth)
				if bw > 0 && dtype.Kind == vortex.DTypePrimitive {
					elemSize := vortex.PTypeSize(dtype.PType)
					nLanes := 1024 / (elemSize * 8)
					fmt.Printf("%s  elemSize=%d, nLanes=%d, bufLen=%d\n", indent, elemSize, nLanes, len(buf))

					fmt.Printf("%s  Typed decode [0..30]: ", indent)
					for i := 0; i < 30 && i < int(rowCount); i++ {
						v := extractBitsTyped(buf, i, nLanes, bw, elemSize)
						fmt.Printf("%d ", v)
					}
					fmt.Println()
				}
			}
		}
	}

	for i, child := range node.Children {
		fmt.Printf("%s  child[%d]:\n", indent, i)
		dumpNode(child, bufs, dtype, rowCount, indent+"    ")
	}
}

var flOrder = [8]int{0, 4, 2, 6, 1, 5, 3, 7}

func extractBitsTyped(packed []byte, idx, nLanes, bw, elemSize int) uint64 {
	// FL_ORDER transposition (spiraldb/fastlanes crate).
	lane := idx % nLanes
	s := idx / 128
	flVal := (idx - s*128 - lane) / 16
	o := flOrder[flVal]
	row := o*8 + s

	tBits := elemSize * 8
	startBit := row * bw
	word := startBit / tBits
	shift := startBit % tBits

	physByte := (word*nLanes + lane) * elemSize

	mask := uint64((1 << bw) - 1)
	var raw uint64

	if physByte+elemSize <= len(packed) {
		switch elemSize {
		case 1:
			raw = uint64(packed[physByte])
		case 2:
			raw = uint64(binary.LittleEndian.Uint16(packed[physByte:]))
		case 4:
			raw = uint64(binary.LittleEndian.Uint32(packed[physByte:]))
		case 8:
			raw = binary.LittleEndian.Uint64(packed[physByte:])
		}
	}

	if shift+bw > tBits {
		nextPhysByte := ((word+1)*nLanes + lane) * elemSize
		if nextPhysByte+elemSize <= len(packed) {
			var next uint64
			switch elemSize {
			case 1:
				next = uint64(packed[nextPhysByte])
			case 2:
				next = uint64(binary.LittleEndian.Uint16(packed[nextPhysByte:]))
			case 4:
				next = uint64(binary.LittleEndian.Uint32(packed[nextPhysByte:]))
			case 8:
				next = binary.LittleEndian.Uint64(packed[nextPhysByte:])
			}
			raw |= next << uint(tBits)
		}
	}

	return (raw >> uint(shift)) & mask
}
