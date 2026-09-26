package pmdoc

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// browserSpreadAttr carries a list's or a list item's spread, as the browser editor's parser reads
// it, on the goldmark node, where browserListSpacing computed it for parseList.
var browserSpreadAttr = []byte("pmdoc-browser-spread")

// browserListSpacing reads the lists of a document holding a shape the browser editor's parser
// and this one read alike only since this one learned it (browserOnlyShape) with that parser's
// spacing, or refuses the document where it cannot. Every other document keeps goldmark's
// looseness - a loose list's items holding more than one block are spread, and the list is spread
// when none of them is - which is how the documents Dispatch stores were read and rendered, though
// the browser editor reads some of them otherwise.
//
// That parser takes a list's spread from blank lines between its items and an item's from blank
// lines between its blocks, and, where the lines carry a quote's or a footnote definition's
// prefix, from what follows the list as well:
//   - outside quotes and footnote definitions, a list is spread when a blank line separates two of
//     its items and an item when one separates two of its blocks, as goldmark records on each
//     block (blankBefore), or when a typed block among them holds one, which is refused;
//   - in a footnote definition, a list is never spread, and an item is spread when a blank line
//     separates two of its blocks, or follows it before the next item or before a quote, list or
//     item the definition goes on with, which the renderer cannot write, so it is refused;
//   - in a quote, and in a typed block in a footnote definition, a blank line at or after the list
//     spreads it or its last item by what comes next, so a list there is read, spread nowhere,
//     only when no blank line lies between its first line and the next block but, in a quote, the
//     one the renderer writes before flow content the quote goes on with, and refused otherwise.
//
// Such a document is also refused where goldmark reads its blocks otherwise: an empty list item
// before a blank line and a block its outer item holds, where goldmark ends the outer item
// (emptyItemEndsOuterItem), and a footnote definition goldmark moves (movedFootnote).
func browserListSpacing(root ast.Node, source []byte) error {
	shape := browserOnlyShape(root)
	if shape == "" {
		return nil
	}
	refuse := func(reason string) error {
		return fmt.Errorf("%w: %s, in a document holding %s, which is read as the browser editor reads it", ErrSchema, reason, shape)
	}
	if reason := movedFootnote(root); reason != "" {
		return refuse(reason)
	}
	lines := newSourceLines(source)
	return ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var reason string
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.ListItem:
			if emptyItemEndsOuterItem(node, lines) {
				reason = "an empty list item followed by a blank line inside another list item, where goldmark ends the outer item and the browser editor does not"
			}
		case *ast.List:
			reason = readListSpacing(node, lines)
		}
		if reason != "" {
			return ast.WalkStop, refuse(reason)
		}
		return ast.WalkContinue, nil
	})
}

// emptyItemEndsOuterItem reports whether goldmark ends the list item around item, an empty one,
// at a blank line before a block the browser editor's parser keeps in it: one indented at least
// as far as the outer item's content.
func emptyItemEndsOuterItem(item *ast.ListItem, lines sourceLines) bool {
	outer, nested := ancestor[*ast.ListItem](item)
	if !nested || item.FirstChild() != nil {
		return false
	}
	next := nextBlock(item)
	return next != nil && blankBefore(next) && !isAncestor(outer, next) &&
		lines.textColumn(startOf(next)) >= lines.textColumn(startOf(outer.FirstChild()))
}

// browserOnlyShape names the first shape in the document that the browser editor's parser and
// this one read alike only since this one reads it as that parser does - a container holding
// nothing, or a list item opening with another block, read holding an empty paragraph
// (emptyParagraphFirst); a table with no body row; a footnote definition ending in a block other
// than a paragraph, after which goldmark puts the definition's backlink - or "" when it holds none.
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
		case *extensionast.FootnoteBacklink:
			if _, ok := node.Parent().(*extensionast.Footnote); ok {
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

// movedFootnote names how goldmark moves a footnote definition the browser editor's parser keeps
// where it stands, or returns "" when it moves none: goldmark gathers every definition it keeps
// at the document's end, in the order of their first references, and this parser writes the ones
// nothing refers to after them (droppedDefinitions).
func movedFootnote(root ast.Node) string {
	value, _ := root.Attribute(openedDefinitionsAttr)
	opened, _ := value.([]openedDefinition)
	for _, definition := range opened {
		if definition.nested {
			return "a footnote definition inside another block, which goldmark moves to the document's end"
		}
	}
	var written []ast.Node
	if list, ok := root.LastChild().(*extensionast.FootnoteList); ok {
		for definition := list.FirstChild(); definition != nil; definition = definition.NextSibling() {
			written = append(written, definition)
		}
	}
	written = append(written, droppedDefinitions(root)...)
	if len(written) == 0 {
		return ""
	}
	for index := 1; index < len(written); index++ {
		if written[index].Pos() < written[index-1].Pos() {
			return "footnote definitions out of the order of their first references, with those nothing refers to last, which goldmark writes them in"
		}
	}
	for child := root.FirstChild(); child != nil; child = child.NextSibling() {
		if _, gathered := child.(*extensionast.FootnoteList); !gathered && startOf(child) > written[0].Pos() {
			return "a block after a footnote definition, which goldmark moves the definition past"
		}
	}
	return ""
}

// readListSpacing records list's and its items' spread as the browser editor's parser reads them
// (browserListSpacing), or names why the document is refused.
func readListSpacing(list *ast.List, lines sourceLines) string {
	quote, quoted := ancestor[*ast.Blockquote](list)
	_, typed := ancestor[*typedDirective](list)
	definition, footnoted := ancestor[*extensionast.Footnote](list)
	switch {
	case quoted:
		// One blank line before flow content the quote goes on with - the renderer's spacing -
		// spreads nothing, unless a typed block holds the list.
		next := nextBlock(list)
		blanks := lines.blankLines(list, next, quoteBlankLine)
		spacedBeforeFlow := len(blanks) == 1 && next != nil && blanks[0] == lines.lineOf(startOf(next))-1 &&
			!startsContainer(next) && isAncestor(quote, next) && !typed
		if len(blanks) > 0 && !spacedBeforeFlow {
			return "a blank line in a quote at or after a list, which the browser editor reads as spacing the list by what follows it"
		}
		setSpread(list, false, spreadsNothing)
		return ""
	case footnoted && typed:
		if len(lines.blankLines(list, nextBlock(list), whitespaceLine)) > 0 {
			return "a blank line at or after a list in a typed block in a footnote definition, which the browser editor reads as spacing the list by what follows it"
		}
		setSpread(list, false, spreadsNothing)
		return ""
	}
	spread := false
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		spread = spread || item != list.FirstChild() && blankBefore(item)
		for child := item.FirstChild(); child != nil; child = child.NextSibling() {
			if directive, ok := child.(*typedDirective); ok && lines.blankInside(directive) {
				return "a typed block holding a blank line inside a list item, which the browser editor reads as spacing the item"
			}
		}
	}
	if footnoted {
		if spread {
			return "a blank line between two items of a list in a footnote definition, which the browser editor reads as spacing the item before it"
		}
		if next := nextBlock(list); next != nil && blankBefore(next) && startsContainer(next) && isAncestor(definition, next) {
			return "a blank line after a list in a footnote definition, before the quote, list or item the definition goes on with, which the browser editor reads as spacing the list's last item"
		}
	}
	setSpread(list, spread, blankBetweenBlocks)
	return ""
}

func setSpread(list *ast.List, spread bool, itemSpread func(ast.Node) bool) {
	list.SetAttribute(browserSpreadAttr, spread)
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		item.SetAttribute(browserSpreadAttr, itemSpread(item))
	}
}

func spreadsNothing(ast.Node) bool { return false }

// blankBetweenBlocks reports whether a blank line separates two of item's blocks.
func blankBetweenBlocks(item ast.Node) bool {
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		if child != item.FirstChild() && blankBefore(child) {
			return true
		}
	}
	return false
}

// blankAfterAttr marks a block goldmark made from a paragraph that followed a blank line, which
// its HasBlankPreviousLines does not carry over (lazyTableRows.transform).
var blankAfterAttr = []byte("pmdoc-blank-after")

// blankBefore reports whether a blank line comes right before block.
func blankBefore(block ast.Node) bool {
	if blank, ok := block.Attribute(blankAfterAttr); ok {
		return blank.(bool)
	}
	return block.HasBlankPreviousLines()
}

// startsContainer reports whether block opens a container the browser editor's parser keeps open
// across lines by their prefix - a quote, a list or a list item - rather than flow content.
func startsContainer(block ast.Node) bool {
	switch block.(type) {
	case *ast.Blockquote, *ast.List, *ast.ListItem:
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
// next sibling, or that of its nearest ancestor that has one, past a footnote definition's
// backlink and into the definitions goldmark gathered at the document's end (movedFootnote has
// found them there in the order they are written).
func nextBlock(node ast.Node) ast.Node {
	for ; node != nil; node = node.Parent() {
		for next := node.NextSibling(); next != nil; next = next.NextSibling() {
			switch next.(type) {
			case *extensionast.FootnoteBacklink:
				continue
			case *extensionast.FootnoteList:
				return next.FirstChild()
			}
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

// sourceLines is where each line of a source starts.
type sourceLines struct {
	source []byte
	starts []int
}

func newSourceLines(source []byte) sourceLines {
	starts := []int{0}
	for index, char := range source {
		if char == '\n' && index+1 < len(source) {
			starts = append(starts, index+1)
		}
	}
	return sourceLines{source: source, starts: starts}
}

func (l sourceLines) lineOf(position int) int {
	return sort.SearchInts(l.starts, position+1) - 1
}

func (l sourceLines) text(line int) []byte {
	end := len(l.source)
	if line+1 < len(l.starts) {
		end = l.starts[line+1]
	}
	return l.source[l.starts[line]:end]
}

// textColumn is the column the text from position on stands at on its line, past any spaces and
// tabs, a tab advancing to the next multiple of four, as markdown measures indentation. Goldmark
// starts a block indented four columns or more, an indented code block, at its line's start.
func (l sourceLines) textColumn(position int) int {
	for position < len(l.source) && (l.source[position] == ' ' || l.source[position] == '\t') {
		position++
	}
	column := 0
	for _, char := range l.source[l.starts[l.lineOf(position)]:position] {
		if char == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
	}
	return column
}

// blankLines is the blank lines from from's first line up to until's, until nil standing for the
// end of the source. From's first line holds its start, which is not blank; beginning there rather
// than after it keeps the next line when goldmark records from past a line holding a tab.
func (l sourceLines) blankLines(from, until ast.Node, blank func([]byte) bool) []int {
	last := len(l.starts)
	if until != nil {
		last = l.lineOf(startOf(until))
	}
	return l.blanks(l.lineOf(startOf(from)), last, blank)
}

// blanks is the lines from first up to last that are blank.
func (l sourceLines) blanks(first, last int, blank func([]byte) bool) []int {
	var blanks []int
	for line := first; line < last; line++ {
		if blank(l.text(line)) {
			blanks = append(blanks, line)
		}
	}
	return blanks
}

// blankInside reports whether a line between typed block directive's opening and closing fences
// is blank; one that runs to its parent's end runs to the next block.
func (l sourceLines) blankInside(directive *typedDirective) bool {
	if !directive.Closed {
		return len(l.blankLines(directive, nextBlock(directive), whitespaceLine)) > 0
	}
	return len(l.blanks(l.lineOf(directive.Pos())+1, l.lineOf(directive.closer), whitespaceLine)) > 0
}

func whitespaceLine(line []byte) bool {
	return len(bytes.Trim(line, " \t\n")) == 0
}

// quoteBlankLine reports whether line is a blank line inside a quote: quote markers and whitespace.
func quoteBlankLine(line []byte) bool {
	return bytes.IndexByte(line, '>') >= 0 && len(bytes.Trim(line, " \t>\n")) == 0
}
