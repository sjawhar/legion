package pmdoc

import (
	"fmt"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// browserSpreadAttr carries a list's or a list item's spread, as the browser editor's parser reads
// it, on the goldmark node, where browserListSpacing computed it for parseList.
var browserSpreadAttr = []byte("pmdoc-browser-spread")

// browserListSpacing records on every list and every list item the spread the browser editor's
// parser reads for it (readSpreads, prepare_list.go), which is a port of that parser's own two
// steps: micromark's container machine, which says which lines each container holds and what is
// left of each line once their prefixes are stripped, and mdast-util-from-markdown's prepareList,
// which reads the spread off that. No list is left to goldmark's looseness and no spacing is
// refused.
//
// The blank lines indented code keeps past where goldmark ends it are that code's, and space
// nothing (heldCodeLines).
//
// One shape is still refused, where goldmark reads the blocks themselves otherwise than that
// parser does: an empty list item before a blank line and a block its outer item holds, where
// goldmark ends the outer item (emptyItemEndsOuterItem).
func browserListSpacing(root ast.Node, source []byte) error {
	lines := newSourceLines(source)
	lines.held = heldCodeLines(root, lines)
	spread := readSpreads(root, lines)
	return ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.ListItem:
			node.SetAttribute(browserSpreadAttr, spread[node])
			if emptyItemEndsOuterItem(node, lines) {
				return ast.WalkStop, fmt.Errorf("%w: an empty list item followed by a blank line inside another list item, where goldmark ends the outer item and the browser editor does not", ErrSchema)
			}
		case *ast.List:
			node.SetAttribute(browserSpreadAttr, spread[node])
		}
		return ast.WalkContinue, nil
	})
}

// emptyItemEndsOuterItem reports whether goldmark ends a list item around item, an empty one, at a
// blank line before a block the browser editor's parser keeps in it: one indented at least as far
// as that item's content. Goldmark ends every item around the empty one there, so each is checked,
// out to the first that goldmark's tree holds the block in.
func emptyItemEndsOuterItem(item *ast.ListItem, lines sourceLines) bool {
	if item.FirstChild() != nil {
		return false
	}
	next := nextBlock(item)
	if next == nil {
		return false
	}
	blanks := lines.blanksBefore(startOf(next), markersOrWhitespace)
	if blanks == 0 {
		return false
	}
	line := lines.lineOf(startOf(next))
	for outer, nested := ancestor[*ast.ListItem](item); nested && !isAncestor(outer, next); outer, nested = ancestor[*ast.ListItem](outer) {
		// That parser goes on with an item only on lines carrying a marker for each quote around
		// it, and measures the next block's text past those markers, where a quote the line opens
		// further in is the item's when its marker stands at the item's content or past it. A line
		// with fewer markers ends the quotes it lacks, and the item with them. Goldmark ends the
		// quotes around the empty item at the blank line and re-opens one for the next line, so the
		// next block's own quotes do not say which quotes those are.
		depth := quoteDepth(outer)
		carries := func(line []byte) bool { return leadingQuoteMarkers(line) >= depth }
		if lines.blanksBefore(startOf(next), func(line []byte) bool { return markersOrWhitespace(line) && carries(line) }) < blanks || !carries(lines.text(line)) {
			continue
		}
		if lines.textColumn(lines.pastQuoteMarkers(lines.starts[line], depth)) >= lines.itemContentColumn(outer) {
			return true
		}
	}
	return false
}

// browserOnlyShape names the first shape in the document that the browser editor's parser and
// this one read alike only since this one reads it as that parser does - a container holding
// nothing, or a list item opening with another block, read holding an empty paragraph
// (emptyParagraphFirst); a table with no body row; a footnote definition ending in a block other
// than a paragraph, whether or not anything refers to it - or "" when it holds none.
func browserOnlyShape(root ast.Node) string {
	var shape string
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.ListItem:
			if first := node.FirstChild(); first == nil {
				shape = "an empty list item"
			} else if !isParagraph(first) {
				shape = "a list item that opens with a block other than a paragraph"
			}
		case *ast.Blockquote:
			if node.FirstChild() == nil {
				shape = "an empty quote"
			}
		case *typedDirective:
			if node.FirstChild() == nil {
				shape = "an empty typed block"
			}
		case *extensionast.Footnote:
			if last := node.LastChild(); last != nil && !isParagraph(last) {
				shape = "a footnote definition that ends in a block other than a paragraph"
			}
		case *extensionast.Table:
			if _, ok := node.LastChild().(*extensionast.TableRow); !ok {
				shape = "a table with no body row"
			}
		}
		if shape != "" {
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	return shape
}

// heldCodeLines is the lines each indented code block holds past where goldmark ends it
// (codeHeldAttr): the code's text, which spaces nothing, so no counter of blank lines counts them
// (sourceLines.held).
func heldCodeLines(root ast.Node, lines sourceLines) map[int]bool {
	var held map[int]bool
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.CodeBlock:
			from, ok := node.Attribute(codeHeldAttr)
			if !ok {
				return ast.WalkContinue, nil
			}
			if held == nil {
				held = map[int]bool{}
			}
			segments := node.Lines()
			for index := from.(int); index < segments.Len(); index++ {
				held[lines.lineOf(segments.At(index).Start)] = true
			}
		}
		return ast.WalkContinue, nil
	})
	return held
}

// startsContainer reports whether block opens a container the browser editor's parser keeps open
// across lines by their prefix - a quote, a list, a list item or a footnote definition - rather
// than flow content.
func startsContainer(block ast.Node) bool {
	switch block.(type) {
	case *ast.Blockquote, *ast.List, *ast.ListItem, *extensionast.Footnote:
		return true
	}
	return false
}

func isParagraph(block ast.Node) bool {
	switch block.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return true
	}
	return false
}

// ancestor is node's nearest ancestor of type T.
func ancestor[T ast.Node](node ast.Node) (T, bool) {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if found, ok := parent.(T); ok {
			return found, true
		}
	}
	var none T
	return none, false
}

func isAncestor(ancestor, node ast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if parent == ancestor {
			return true
		}
	}
	return false
}

// nextBlock is the block that follows node's last line in the document, or nil at its end: node's
// next sibling, or that of its nearest ancestor that has one.
func nextBlock(node ast.Node) ast.Node {
	for ; node != nil; node = node.Parent() {
		if next := node.NextSibling(); next != nil {
			return next
		}
	}
	return nil
}

// startOf is where block starts in the source: a paragraph's text where its first line does, and
// any other block where goldmark records opening it. Goldmark's record counts a tab before the
// block as the columns it spans, and it records nothing on the text block that replaces a tight
// list item's paragraph.
func startOf(block ast.Node) int {
	if isParagraph(block) || block.Pos() < 0 {
		return block.Lines().At(0).Start
	}
	return block.Pos()
}
