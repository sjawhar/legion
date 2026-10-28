package pmdoc

import (
	"fmt"
	"testing"
)

// BenchmarkFindMarks compares one FindMarks walk with the base implementation's FindMark and
// MarkAttrs pair per key (frozenFindMark, frozenMarkAttrs), the loop a refresh ran before, at 10,
// 40 and 80 open anchors on real-size documents.
func BenchmarkFindMarks(b *testing.B) {
	for _, size := range realSizeDocuments {
		// Each size is marked once and every count looks up the first refs of the one marked
		// document, so the counts differ only in the keys asked for: marking the document again
		// per count would change its runs too, and the timings would not compare.
		doc := parseRealSize(b, size.bytes, size.seed)
		refs := placeAnchorMarks(doc, 100, size.seed)
		if len(refs) < 80 {
			b.Fatalf("%s: %d distinct marks placed, want at least 80", size.name, len(refs))
		}
		for _, count := range []int{10, 40, 80} {
			keys := refs[:count]
			b.Run(fmt.Sprintf("%s/%d/FindMarks", size.name, count), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if found := FindMarks(doc, keys); len(found) != count {
						b.Fatalf("found %d of %d marks", len(found), count)
					}
				}
			})
			// FindMark and MarkAttrs are lookups in FindMarks, so the per-key loop is measured on
			// the frozen copies.
			b.Run(fmt.Sprintf("%s/%d/FindMarkLoop", size.name, count), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for _, key := range keys {
						if _, _, ok := frozenFindMark(doc, key.Type, key.ID); !ok {
							b.Fatalf("mark %s %s not found", key.Type, key.ID)
						}
						if _, ok := frozenMarkAttrs(doc, key.Type, key.ID); !ok {
							b.Fatalf("mark %s %s has no attributes", key.Type, key.ID)
						}
					}
				}
			})
		}
	}
}
