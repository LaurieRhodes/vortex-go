package vortex

import (
	"io"
	"sort"
)

// prefetchMergeGap is the maximum gap between two needed byte ranges for
// them to be coalesced into a single read. Below this threshold, reading
// the (unneeded) bytes in between costs less than a second syscall would;
// above it, a separate read avoids wasting bandwidth/memory on a large
// unused gap. 128KB is a conservative middle ground for typical extent
// file layouts (segments for a handful of projected columns' chunks,
// usually within a few hundred KB of each other in a file written
// column-major per chunk).
const prefetchMergeGap = 128 * 1024

// segRange is one byte range a scan will need to read: one (column,
// chunk) segment, identified before any reads happen by walking the
// scan's column layouts and consulting zone-pruning results.
type segRange struct {
	offset int64
	length int64
}

// segmentPrefetch holds a small number of already-read, coalesced byte
// ranges covering exactly the segments a scan will touch after column
// projection and zone pruning. readSegmentSpec consults it first, so
// per-segment reads during iteration become zero-copy slices instead of
// individual ReadAt syscalls — this is what lets a fully-projected,
// unfiltered scan approach "read the whole file in one shot" performance
// (few, large coalesced reads) while a selective/prunable scan still
// only pays for the bytes it actually needs (pruned chunks are never
// added to the needed-ranges list in the first place).
type segmentPrefetch struct {
	starts []int64
	bufs   [][]byte // bufs[i] covers [starts[i], starts[i]+len(bufs[i]))
}

// slice returns a zero-copy view of [offset, offset+length) if it falls
// entirely within one of the prefetched ranges. Returns ok=false if not
// covered (e.g. a layout the range-gathering pass didn't anticipate),
// in which case the caller falls back to a direct read.
func (p *segmentPrefetch) slice(offset, length int64) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	for i, start := range p.starts {
		end := start + int64(len(p.bufs[i]))
		if offset >= start && offset+length <= end {
			o := offset - start
			return p.bufs[i][o : o+length], true
		}
	}
	return nil, false
}

// buildSegmentPrefetch sorts the needed ranges, merges any within
// prefetchMergeGap of each other, and issues one ReadAt per merged
// region. Returns (nil, nil) for an empty input — callers should treat
// a nil *segmentPrefetch as "no prefetch available", falling back to
// the standard per-segment read path.
func buildSegmentPrefetch(r io.ReaderAt, needed []segRange) (*segmentPrefetch, error) {
	if len(needed) == 0 {
		return nil, nil
	}
	sort.Slice(needed, func(i, j int) bool { return needed[i].offset < needed[j].offset })

	type mrange struct{ start, end int64 }
	var merged []mrange
	cur := mrange{needed[0].offset, needed[0].offset + needed[0].length}
	for _, s := range needed[1:] {
		if s.offset <= cur.end+prefetchMergeGap {
			if e := s.offset + s.length; e > cur.end {
				cur.end = e
			}
			continue
		}
		merged = append(merged, cur)
		cur = mrange{s.offset, s.offset + s.length}
	}
	merged = append(merged, cur)

	p := &segmentPrefetch{}
	for _, m := range merged {
		buf := make([]byte, m.end-m.start)
		if _, err := r.ReadAt(buf, m.start); err != nil {
			return nil, err
		}
		p.starts = append(p.starts, m.start)
		p.bufs = append(p.bufs, buf)
	}
	return p, nil
}

// segRangeForLayout resolves a Flat layout's single backing segment to
// its (offset, length) byte range. Flat layouts reference exactly one
// segment (see ReadFlatSegment, which only ever reads Segments[0]; any
// further internal structure — e.g. multiple buffers for a struct/dict
// array — is derived by slicing within that one segment, not additional
// file reads).
func segRangeForLayout(f *File, layout *Layout) (segRange, bool) {
	if layout == nil || len(layout.Segments) == 0 {
		return segRange{}, false
	}
	segIdx := layout.Segments[0]
	if int(segIdx) >= len(f.footer.SegmentSpecs) {
		return segRange{}, false
	}
	spec := f.footer.SegmentSpecs[segIdx]
	return segRange{offset: int64(spec.Offset), length: int64(spec.Length)}, true
}
