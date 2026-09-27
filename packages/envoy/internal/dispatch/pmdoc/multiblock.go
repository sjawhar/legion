package pmdoc

// maxMultiblockUnits is the most text, in UTF-16 units, over which the browser editor's accept
// takes block content across textblocks (proof-sdk marks.ts MAX_STRUCTURAL_MULTIBLOCK_CHARS).
const maxMultiblockUnits = 240

// MultiblockRange is the content of the textblocks the browser editor's accept replaces when it
// takes block content over r across textblocks (proof-sdk marks.ts
// resolveStructuralMultiblockRange), from the first one's start to the last one's end. It reports
// false where the editor takes none because it reads other than two textblocks under r, or more than
// maxMultiblockUnits of their text. The editor reads a textblock's text as its content short of its
// last position, so it skips a one-character textblock at r's edge. Whether it takes a range other
// than the one returned depends on the editor's edge tolerance; the one returned it always takes.
func MultiblockRange(doc *Node, r Range) (Range, bool) {
	var blocks []Range
	units := 0
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if !isTextblock(node.Type) {
			return true
		}
		from, to := pos+1, end-2
		if r.To <= from || r.From >= to {
			return true
		}
		blocks = append(blocks, Range{From: from, To: end - 1})
		units += max(to-from, 0)
		return len(blocks) <= 2
	})
	if len(blocks) != 2 || units > maxMultiblockUnits {
		return Range{}, false
	}
	return Range{From: blocks[0].From, To: blocks[1].To}, true
}
