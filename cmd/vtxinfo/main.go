// vtxinfo inspects Vortex (.vtx) files and prints their structure.
//
// Usage:
//
//	vtxinfo <file.vtx> [flags]
//
// Flags:
//
//	-json      Output as JSON
//	-layout    Show full layout tree
//	-segments  Show segment details
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	vortex "github.com/LaurieRhodes/vortex-go"
)

var (
	flagJSON     = flag.Bool("json", false, "output as JSON")
	flagLayout   = flag.Bool("layout", false, "show full layout tree")
	flagSegments = flag.Bool("segments", false, "show segment details")
)

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: vtxinfo <file.vtx> [flags]\n")
		os.Exit(1)
	}

	filename := flag.Arg(0)
	f, err := os.Open(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	vf, err := vortex.Open(f, fi.Size())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening vortex file: %v\n", err)
		os.Exit(1)
	}

	if *flagJSON {
		printJSON(vf, fi)
	} else {
		printText(vf, fi, filename)
	}
}

func printText(vf *vortex.File, fi os.FileInfo, filename string) {
	fmt.Printf("File:    %s\n", filename)
	fmt.Printf("Size:    %d bytes\n", fi.Size())
	fmt.Printf("Version: %d\n", vf.Version())
	fmt.Println()

	// Schema
	dt := vf.DType()
	fmt.Println("Schema:")
	printDType(dt, "  ")
	fmt.Println()

	// Summary
	layout := vf.Layout()
	fmt.Printf("Rows:    %d\n", layout.RowCount)
	footer := vf.Footer()
	fmt.Printf("Columns: %d\n", len(dt.FieldNames))
	fmt.Printf("Segments: %d\n", len(footer.SegmentSpecs))

	// Array encoding registry
	usedEncodings := collectUsedEncodings(vf, layout, footer)
	if len(usedEncodings) > 0 {
		fmt.Printf("Encodings used: %s\n", strings.Join(usedEncodings, ", "))
	}
	fmt.Println()

	// Layout specs
	fmt.Printf("Layout types: %s\n", strings.Join(footer.LayoutSpecs, ", "))
	fmt.Println()

	// Statistics
	stats := vf.Statistics()
	if stats != nil && len(stats.FieldStats) > 0 {
		fmt.Println("Statistics:")
		for i, fs := range stats.FieldStats {
			name := "?"
			if i < len(dt.FieldNames) {
				name = dt.FieldNames[i]
			}
			parts := []string{}
			if fs.NullCount != nil && *fs.NullCount > 0 {
				parts = append(parts, fmt.Sprintf("nulls=%d", *fs.NullCount))
			}
			if fs.HasMin() {
				parts = append(parts, fmt.Sprintf("min=%d bytes", len(fs.Min)))
			}
			if fs.HasMax() {
				parts = append(parts, fmt.Sprintf("max=%d bytes", len(fs.Max)))
			}
			if fs.IsSorted != nil && *fs.IsSorted {
				parts = append(parts, "sorted")
			}
			detail := ""
			if len(parts) > 0 {
				detail = " (" + strings.Join(parts, ", ") + ")"
			}
			fmt.Printf("  %s%s\n", name, detail)
		}
		fmt.Println()
	}

	// Layout tree
	if *flagLayout {
		fmt.Println("Layout tree:")
		printLayoutTree(vf, layout, footer, "  ")
		fmt.Println()
	}

	// Segments
	if *flagSegments {
		fmt.Println("Segments:")
		totalBytes := uint64(0)
		for i, seg := range footer.SegmentSpecs {
			fmt.Printf("  [%d] offset=%d length=%d align=2^%d\n",
				i, seg.Offset, seg.Length, seg.AlignmentExponent)
			totalBytes += uint64(seg.Length)
		}
		fmt.Printf("  Total segment data: %d bytes\n", totalBytes)
	}
}

func printDType(dt *vortex.DType, indent string) {
	if dt == nil {
		fmt.Printf("%s<nil>\n", indent)
		return
	}
	switch dt.Kind {
	case vortex.DTypeStruct:
		nullable := ""
		if dt.Nullable {
			nullable = "?"
		}
		fmt.Printf("%sstruct%s {\n", indent, nullable)
		for i, name := range dt.FieldNames {
			fmt.Printf("%s  %s: ", indent, name)
			printDTypeInline(dt.FieldTypes[i])
			fmt.Println()
		}
		fmt.Printf("%s}\n", indent)
	default:
		fmt.Printf("%s%s\n", indent, dt)
	}
}

func printDTypeInline(dt *vortex.DType) {
	if dt == nil {
		fmt.Print("<nil>")
		return
	}
	switch dt.Kind {
	case vortex.DTypeStruct:
		nullable := ""
		if dt.Nullable {
			nullable = "?"
		}
		fmt.Printf("struct%s{", nullable)
		for i, name := range dt.FieldNames {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("%s: ", name)
			printDTypeInline(dt.FieldTypes[i])
		}
		fmt.Print("}")
	case vortex.DTypeList:
		nullable := ""
		if dt.Nullable {
			nullable = "?"
		}
		fmt.Printf("list%s<", nullable)
		printDTypeInline(dt.ElementType)
		fmt.Print(">")
	default:
		fmt.Print(dt)
	}
}

func printLayoutTree(vf *vortex.File, l *vortex.Layout, footer *vortex.Footer, indent string) {
	segs := ""
	if len(l.Segments) > 0 {
		segStrs := make([]string, len(l.Segments))
		for i, s := range l.Segments {
			segStrs[i] = fmt.Sprintf("%d", s)
		}
		segs = fmt.Sprintf(" segs=[%s]", strings.Join(segStrs, ","))
	}
	fmt.Printf("%s%s rows=%d%s\n", indent, l.EncodingID, l.RowCount, segs)

	// If it's a flat layout, show the array encoding tree inside it.
	if l.IsFlat() && len(l.Segments) > 0 {
		node, _, err := vf.ReadFlatSegment(l)
		if err == nil {
			printArrayNodeTree(node, indent+"  ")
		}
	}

	for _, child := range l.Children {
		printLayoutTree(vf, child, footer, indent+"  ")
	}
}

func printArrayNodeTree(n *vortex.ArrayNode, indent string) {
	bufs := ""
	if len(n.BufferIndices) > 0 {
		bufStrs := make([]string, len(n.BufferIndices))
		for i, b := range n.BufferIndices {
			bufStrs[i] = fmt.Sprintf("%d", b)
		}
		bufs = fmt.Sprintf(" bufs=[%s]", strings.Join(bufStrs, ","))
	}
	meta := ""
	if len(n.Metadata) > 0 {
		meta = fmt.Sprintf(" meta=%db", len(n.Metadata))
	}
	fmt.Printf("%s[%s]%s%s\n", indent, n.EncodingID, bufs, meta)
	for _, child := range n.Children {
		printArrayNodeTree(child, indent+"  ")
	}
}

// collectUsedEncodings walks the layout tree and flat segments to find
// which array encodings are actually referenced.
func collectUsedEncodings(vf *vortex.File, layout *vortex.Layout, footer *vortex.Footer) []string {
	seen := map[string]bool{}
	walkLayoutEncodings(vf, layout, seen)
	result := make([]string, 0, len(seen))
	for enc := range seen {
		result = append(result, enc)
	}
	// Sort for stable output.
	sortStrings(result)
	return result
}

func walkLayoutEncodings(vf *vortex.File, l *vortex.Layout, seen map[string]bool) {
	if l.IsFlat() && len(l.Segments) > 0 {
		node, _, err := vf.ReadFlatSegment(l)
		if err == nil {
			walkArrayNodeEncodings(node, seen)
		}
	}
	for _, child := range l.Children {
		walkLayoutEncodings(vf, child, seen)
	}
}

func walkArrayNodeEncodings(n *vortex.ArrayNode, seen map[string]bool) {
	seen[n.EncodingID] = true
	for _, child := range n.Children {
		walkArrayNodeEncodings(child, seen)
	}
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[i] > s[j] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// --- JSON output ---

type jsonOutput struct {
	File       string          `json:"file"`
	Size       int64           `json:"size"`
	Version    uint16          `json:"version"`
	Rows       uint64          `json:"rows"`
	Schema     *jsonDType      `json:"schema"`
	Segments   int             `json:"segments"`
	Encodings  []string        `json:"encodings_used"`
	LayoutTree *jsonLayoutNode `json:"layout,omitempty"`
}

type jsonDType struct {
	Kind     string      `json:"kind"`
	Nullable bool        `json:"nullable,omitempty"`
	PType    string      `json:"ptype,omitempty"`
	Fields   []jsonField `json:"fields,omitempty"`
}

type jsonField struct {
	Name string     `json:"name"`
	Type *jsonDType `json:"type"`
}

type jsonLayoutNode struct {
	Encoding string            `json:"encoding"`
	Rows     uint64            `json:"rows"`
	Segments []uint32          `json:"segments,omitempty"`
	Children []*jsonLayoutNode `json:"children,omitempty"`
}

func printJSON(vf *vortex.File, fi os.FileInfo) {
	layout := vf.Layout()
	footer := vf.Footer()
	dt := vf.DType()

	out := jsonOutput{
		File:      fi.Name(),
		Size:      fi.Size(),
		Version:   vf.Version(),
		Rows:      layout.RowCount,
		Schema:    dtypeToJSON(dt),
		Segments:  len(footer.SegmentSpecs),
		Encodings: collectUsedEncodings(vf, layout, footer),
	}

	if *flagLayout {
		out.LayoutTree = layoutToJSON(layout)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func dtypeToJSON(dt *vortex.DType) *jsonDType {
	if dt == nil {
		return nil
	}
	jdt := &jsonDType{
		Kind:     dt.Kind.String(),
		Nullable: dt.Nullable,
	}
	if dt.Kind == vortex.DTypePrimitive {
		jdt.PType = dt.PType.String()
	}
	if dt.Kind == vortex.DTypeStruct {
		for i, name := range dt.FieldNames {
			jdt.Fields = append(jdt.Fields, jsonField{
				Name: name,
				Type: dtypeToJSON(dt.FieldTypes[i]),
			})
		}
	}
	return jdt
}

func layoutToJSON(l *vortex.Layout) *jsonLayoutNode {
	node := &jsonLayoutNode{
		Encoding: l.EncodingID,
		Rows:     l.RowCount,
	}
	if len(l.Segments) > 0 {
		node.Segments = l.Segments
	}
	for _, child := range l.Children {
		node.Children = append(node.Children, layoutToJSON(child))
	}
	return node
}
