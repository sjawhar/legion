package pmdoc

import (
	"strings"
	"unicode"
	"unicode/utf16"
)

// maxMultiblockUnits is the most text, in UTF-16 units, the browser editor's accept takes block
// content over across textblocks (proof-sdk marks.ts MAX_STRUCTURAL_MULTIBLOCK_CHARS).
const maxMultiblockUnits = 240

// MultiblockAligned reports whether the browser editor's accept takes block content over r
// across textblocks (proof-sdk marks.ts resolveStructuralMultiblockRange): r overlaps two
// textblocks, no more, their text is at most maxMultiblockUnits, and r starts within one
// character of the first's text, past its leading whitespace, and ends within one of the last's
// text, short of its trailing whitespace, or its text is theirs, whitespace aside. The editor takes
// a textblock's text as its content short of its last position, so r ending two characters before
// the last's end is aligned too; this keeps that reading, since it decides what the browser
// accepts.
func MultiblockAligned(doc *Node, r Range) bool {
	type textblock struct {
		from, to int
		// text is the textblock's text as the editor reads it, and covered is the part of its
		// content r covers.
		text, covered []uint16
		lead, trail   int
	}
	var blocks []textblock
	walk(doc, func(node *Node, _ []int, pos, end int) bool {
		if !isTextblock(node.Type) {
			return true
		}
		from, to := pos+1, end-2
		if r.To <= from || r.From >= to {
			return true
		}
		// An empty textblock's text is empty, as the editor reads it over a range that ends
		// before it starts.
		content := inlineUnits(node)
		text := content[:max(to-from, 0)]
		blocks = append(blocks, textblock{
			from: from, to: to, text: text,
			covered: content[max(r.From, from)-from : min(r.To, end-1)-from],
			lead:    spaceUnits(text, false), trail: spaceUnits(text, true),
		})
		return true
	})
	if len(blocks) != 2 {
		return false
	}
	first, last := blocks[0], blocks[1]
	startAligned := r.From >= max(0, first.from-1) && r.From <= min(first.to, first.from+first.lead+1)
	endAligned := r.To <= min(nodeSize(doc), last.to+1) && r.To >= max(last.from, last.to-last.trail-1)
	if !startAligned || !endAligned {
		covered := collapseSpace(string(utf16.Decode(first.covered)) + "\n" + string(utf16.Decode(last.covered)))
		if covered == "" || covered != collapseSpace(string(utf16.Decode(first.text))+"\n"+string(utf16.Decode(last.text))) {
			return false
		}
	}
	return len(first.text)+len(last.text) <= maxMultiblockUnits
}

// collapseSpace writes each run of whitespace in text as one space and trims its ends, as the
// editor's normalizeQuote does.
func collapseSpace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// inlineUnits is a textblock's content as the editor counts its positions: each text node's
// UTF-16 units, and a line feed for every other inline node.
func inlineUnits(textblock *Node) []uint16 {
	var units []uint16
	for _, child := range textblock.Children {
		if child.Type == "text" {
			units = append(units, utf16.Encode([]rune(child.Text))...)
			continue
		}
		for range nodeSize(child) {
			units = append(units, '\n')
		}
	}
	return units
}

// spaceUnits counts the UTF-16 units of whitespace text starts with, or with trailing, ends with.
func spaceUnits(text []uint16, trailing bool) int {
	runes := utf16.Decode(text)
	count := 0
	for index := range runes {
		if trailing {
			index = len(runes) - 1 - index
		}
		if !unicode.IsSpace(runes[index]) {
			break
		}
		count += len(utf16.Encode(runes[index : index+1]))
	}
	return count
}
