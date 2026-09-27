package pmdoc

import "strings"

// blanksAfterList is how many blank lines separate list from next, the block after it: as many as
// spread what they space there (spacingAfterList) when it is spread, and one everywhere else,
// which spaces nothing - but none where next can stand on the line after the list, since it is a
// list, which opens there whatever its first item, or opens after a paragraph as well
// (opensAfterParagraph), or the list ends in no paragraph it would continue (endsInParagraph), and
// either a single blank line would spread what it spaces, or the list stands in a typed block in
// a footnote definition outside quotes, where the browser editor reads any blank line after it as
// spreading it, or, inside a list item, the list ends in an empty item, at a blank line after which
// goldmark ends the list item around it or a quote the list stands in (emptyItemEndsOuterItem).
// exact reports whether the count is one of those two; the one blank line everywhere else spaces
// nothing, so a list item may write none instead.
func (r *renderer) blanksAfterList(list, next *Node, prefix string) (blanks int, exact bool) {
	spaced, spacing := r.spacingAfterList(list, next, prefix)
	typedInFootnote := r.inFootnote && r.typedPrefix != nil && !strings.Contains(prefix, ">")
	switch {
	case spaced != nil && spaced.Attrs["spread"] == true:
		return spacing, true
	case (isList(next) || opensAfterParagraph(next) || !endsInParagraph(list)) && (spaced != nil && spacing == 1 || typedInFootnote || r.itemDepth > 0 && endsInEmptyItem(list)):
		return 0, true
	default:
		return 1, false
	}
}

// spacingAfterList is what the browser editor's parser reads blank lines after list, before next,
// as spacing (spacedAfter), and how many it takes, where the lines carry a quote's or a footnote
// definition's prefix; nil where they space nothing, or what they space is spread without them
// (spreadByItsOwnLines). In a quote they space the list, one before a quote or a list (or
// anything, in a typed block inside the quote) and two before anything else; in a footnote
// definition, its last item, one before a quote or a list; in both, the last item, one before a
// quote or a list and two before anything else, which with the definition inside the quote space
// the list instead.
func (r *renderer) spacingAfterList(list, next *Node, prefix string) (*Node, int) {
	quotes := strings.Count(prefix, ">")
	container := next.Type == "blockquote" || next.Type == "footnote_definition" || isList(next)
	inTyped := r.inQuotedTypedBlock(prefix)
	var spaced *Node
	blanks := 1
	switch {
	case quotes > 0 && r.inFootnote && container:
		spaced = spacedAfter(list, false)
	case quotes > 0 && r.inFootnote:
		// The quote stands inside the definition when the definition's lines carry fewer quote
		// markers than the list's.
		spaced, blanks = spacedAfter(list, quotes == r.footnoteQuotes), 2
	case quotes > 0 && (container || inTyped):
		spaced = spacedAfter(list, true)
		// In a typed block inside the quote, a quote the last item ends in takes one blank line.
		if last := spaced.Children[len(spaced.Children)-1]; inTyped && last.Children[len(last.Children)-1].Type == "blockquote" {
			blanks = 2
		}
	case quotes > 0:
		spaced, blanks = spacedAfter(list, true), 2
	case r.inFootnote && container:
		spaced = spacedAfter(list, false)
	}
	if spaced == nil || spreadByItsOwnLines(spaced, quotes > 0 && !r.inFootnote, r.itemDepth > 0) {
		return nil, 0
	}
	return spaced, blanks
}

// inQuotedTypedBlock reports whether lines with prefix stand in a typed block inside a quote, with
// no quote opened inside the typed block.
func (r *renderer) inQuotedTypedBlock(prefix string) bool {
	quotes := strings.Count(prefix, ">")
	return quotes > 0 && r.typedPrefix != nil && strings.Count(*r.typedPrefix, ">") == quotes
}

// spreadByItsOwnLines reports whether a spread list or list item is written spread without a blank
// line after it: an item by a blank line between two of its blocks, and a list in a quote outside
// footnote definitions (quoted) by one after an item that neither ends in a list nor, in a list
// item (nested), is empty (writesBlankAfterItem).
func spreadByItsOwnLines(spaced *Node, quoted, nested bool) bool {
	if spaced.Attrs["spread"] != true {
		return false
	}
	if spaced.Type == "list_item" {
		return len(spaced.Children) > 1
	}
	if !quoted {
		return false
	}
	for _, item := range spaced.Children[:len(spaced.Children)-1] {
		if !isList(item.Children[len(item.Children)-1]) && !(nested && holdsOnlyAnEmptyParagraph(item)) {
			return true
		}
	}
	return false
}

// spacedAfter is what the browser editor's parser spaces by blank lines after list: in a quote, the
// list, and in a footnote definition, its last item - or, when the last item ends in a list, what
// that list's blank lines space.
func spacedAfter(list *Node, quoted bool) *Node {
	last := list.Children[len(list.Children)-1]
	switch inner := last.Children[len(last.Children)-1]; {
	case isList(inner):
		return spacedAfter(inner, quoted)
	case quoted:
		return list
	default:
		return last
	}
}

// blanksEndingQuotedList writes the two blank lines that, at the end of a quote or of a typed block
// inside one, spread the list its blocks end in, where nothing else does.
func (r *renderer) blanksEndingQuotedList(blocks []*Node, prefix string) {
	last := blocks[len(blocks)-1]
	if !isList(last) || r.inFootnote || !strings.Contains(prefix, ">") {
		return
	}
	if spaced := spacedAfter(last, true); spaced.Attrs["spread"] == true && !spreadByItsOwnLines(spaced, true, r.itemDepth > 0) {
		r.writeSyntax(strings.Repeat("\n"+strings.TrimRight(prefix, " "), 2))
	}
}

// endsInParagraph reports whether list's last item ends in a paragraph holding text, directly or in
// a list or quote it ends in, which a line of text after the list would continue.
func endsInParagraph(list *Node) bool {
	last := list.Children[len(list.Children)-1]
	return blockEndsInParagraph(last.Children[len(last.Children)-1])
}

// blockEndsInParagraph reports whether block is a paragraph holding text, or a list or quote whose
// last block ends in one.
func blockEndsInParagraph(block *Node) bool {
	switch {
	case isList(block):
		return endsInParagraph(block)
	case block.Type == "blockquote":
		return blockEndsInParagraph(block.Children[len(block.Children)-1])
	default:
		return block.Type == "paragraph" && len(block.Children) > 0
	}
}

// endsInEmptyItem reports whether list's last item holds only an empty paragraph.
func endsInEmptyItem(list *Node) bool {
	return holdsOnlyAnEmptyParagraph(list.Children[len(list.Children)-1])
}

func isList(n *Node) bool {
	return n.Type == "bullet_list" || n.Type == "ordered_list"
}

// opensAfterParagraph reports whether block opens on the line after a paragraph without a blank
// line: a quote, a code block (written fenced), a heading written on one line, a rule, a typed
// block, a footnote definition, a bullet list, or an ordered list starting at one, whose first item
// holds something (an empty item interrupts no paragraph, emptyItemGuard).
func opensAfterParagraph(block *Node) bool {
	if _, typed := typedBlock(block.Type); typed {
		return true
	}
	switch block.Type {
	case "blockquote", "code_block", "hr", "footnote_definition":
		return true
	case "heading":
		return !holdsHardBreak(block.Children)
	case "bullet_list", "ordered_list":
		if block.Type == "ordered_list" && int(num(block.Attrs["order"], 1)) != 1 {
			return false
		}
		return !holdsOnlyAnEmptyParagraph(block.Children[0])
	}
	return false
}

// writesBlankAfterItem reports whether blank lines follow item, before the next item of list. The
// browser editor's parser reads them as spreading the list, except where the lines carry a quote's
// or a footnote definition's prefix: in a footnote definition, a quote around it or inside it
// included, they spread the item, and are written only where its own lines do not already
// (spreadByItsOwnLines); in a quote blank lines after an item that ends in a list are that list's,
// two of them spreading the list it ends in (spacedAfter).
func (r *renderer) writesBlankAfterItem(list, item *Node, prefix string) bool {
	quoted := strings.Contains(prefix, ">")
	switch last := item.Children[len(item.Children)-1]; {
	case r.inFootnote && quoted && isList(last):
		// The blank lines are the list's, spacing what they would after its last item there.
		return spacedAfter(last, strings.Count(prefix, ">") == r.footnoteQuotes).Attrs["spread"] == true
	case r.inFootnote:
		return item.Attrs["spread"] == true && !spreadByItsOwnLines(item, false, false)
	case quoted && isList(last):
		return spacedAfter(last, true).Attrs["spread"] == true
	case quoted && r.itemDepth > 0 && holdsOnlyAnEmptyParagraph(item):
		// Goldmark ends the list item around this list at a blank line after an empty item
		// (emptyItemEndsOuterItem); in a quote, blank lines after another item or after the list
		// spread it instead (spreadByItsOwnLines).
		return false
	case r.itemDepth > 0 && holdsOnlyAnEmptyParagraph(item) && spreadAtAnotherItem(list):
		// Goldmark ends the list item around this list at a blank line after an empty item
		// (emptyItemEndsOuterItem), and one after another item spreads the list as well.
		return false
	default:
		return list.Attrs["spread"] == true
	}
}

// spreadAtAnotherItem reports whether list is spread and one of its items but the last is neither
// empty nor ends in a list, so that blank lines after it spread the list.
func spreadAtAnotherItem(list *Node) bool {
	if list.Attrs["spread"] != true {
		return false
	}
	for _, item := range list.Children[:len(list.Children)-1] {
		if !holdsOnlyAnEmptyParagraph(item) && !isList(item.Children[len(item.Children)-1]) {
			return true
		}
	}
	return false
}
