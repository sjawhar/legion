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

// browserListSpacing reads every document's lists with the browser editor's parser's spacing,
// wherever the lines alone decide it (readListSpacing). Where they do not, a document holding a
// shape the two parsers read alike only since this one learned it (browserOnlyShape) is refused,
// and every other document keeps goldmark's looseness - a loose list's items holding more than one
// block are spread, and the list is spread when none of them is - which is how the documents
// Dispatch stores were read.
//
// That parser takes a list's spread from blank lines between its items and an item's from blank
// lines between its blocks, and, where the lines carry a quote's or a footnote definition's
// prefix, from what follows the list as well:
//   - outside quotes and footnote definitions, a list is spread when a blank line separates two of
//     its items and an item when one separates two of its blocks, as goldmark records on each
//     block (blankBefore);
//   - in a footnote definition, a list is never spread, and an item is spread by a blank line
//     between two of its blocks or after it, before the next item or before a quote or list the
//     definition goes on with (blankAfterItem);
//   - in a quote, a list is spread by a blank line after an item, or by blank lines after its
//     last item before what the quote goes on with (quotedListSpread);
//   - in a quote and a footnote definition, as footnotedQuoteListSpread reads;
//   - and blank lines after an item that ends in a quote or a list are that block's.
//
// The lines alone do not decide a typed block holding a blank line in a list item, a blank line at
// the end of a quote after a list, or at or after a list in a typed block in a footnote
// definition. A document holding such a shape is also refused where goldmark reads its blocks
// otherwise: an empty list item before a blank line and a block its outer item holds, where
// goldmark ends the outer item (emptyItemEndsOuterItem).
func browserListSpacing(root ast.Node, source []byte) error {
	shape := browserOnlyShape(root)
	if shape == "" {
		readExactListSpacing(root, newSourceLines(source))
		return nil
	}
	refuse := func(reason string) error {
		return fmt.Errorf("%w: %s, in a document holding %s, which is read as the browser editor reads it", ErrSchema, reason, shape)
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

// readExactListSpacing reads, in a document holding no shape only the browser editor's parser and
// this one read alike, each list whose spacing the lines alone decide as that parser reads it
// (readListSpacing), and leaves every other list to goldmark's looseness.
func readExactListSpacing(root ast.Node, lines sourceLines) {
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.List:
			if entering {
				readListSpacing(node, lines)
			}
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
	return next != nil && lines.blanksBefore(startOf(next), markersOrWhitespace) > 0 && !isAncestor(outer, next) &&
		lines.textColumn(startOf(next)) >= browserTextColumn(outer.FirstChild(), lines)
}

// browserTextColumn is the column block's text starts at as the browser editor's parser measures
// it: on a footnote definition's first line, the definition's text starts four columns past the
// definition, where its later lines' text does, whatever the spaces after its `]:`.
func browserTextColumn(block ast.Node, lines sourceLines) int {
	column := lines.textColumn(startOf(block))
	definition, footnoted := ancestor[*extensionast.Footnote](block)
	if !footnoted || lines.lineOf(definition.Pos()) != lines.lineOf(startOf(block)) {
		return column
	}
	return column - lines.textColumn(startOf(definition.FirstChild())) + lines.textColumn(definition.Pos()) + 4
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
			// Goldmark appends a backlink after a referenced definition's last block when that
			// block is not a paragraph.
			last := node.LastChild()
			if _, backlink := last.(*extensionast.FootnoteBacklink); backlink {
				last = last.PreviousSibling()
			}
			if last != nil && !isParagraph(last) {
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

// readListSpacing records list's and its items' spread as the browser editor's parser reads them
// (browserListSpacing), or names why they cannot be read from the lines alone.
func readListSpacing(list *ast.List, lines sourceLines) string {
	quote, quoted := ancestor[*ast.Blockquote](list)
	directive, typed := ancestor[*typedDirective](list)
	definition, footnoted := ancestor[*extensionast.Footnote](list)
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		if holdsTypedBlockWithBlankLine(item, lines) {
			return "a typed block holding a blank line inside a list item, which the browser editor reads as spacing the item"
		}
	}
	switch {
	case quoted && footnoted:
		spread, lastSpread, reason := footnotedQuoteListSpread(list, quote, definition, lines)
		if reason != "" {
			return reason
		}
		setSpread(list, spread, func(item ast.Node) bool {
			// Blank lines after a list are that list's.
			if next := item.NextSibling(); next != nil {
				return blankBetweenBlocksBut(item, anyBlock) || blankBefore(next) && !endsWithList(item)
			}
			return blankBetweenBlocksBut(item, anyBlock) || lastSpread
		})
	case quoted:
		// The browser editor spaces a list in a typed block inside a quote by one blank line,
		// whether the list's own quote holds the typed block or stands inside it.
		directiveQuoted := false
		if typed {
			_, directiveQuoted = ancestor[*ast.Blockquote](directive)
		}
		spread, reason := quotedListSpread(list, quote, directive, directiveQuoted, lines)
		if reason != "" {
			return reason
		}
		setSpread(list, spread, func(item ast.Node) bool { return blankBetweenBlocksBut(item, anyBlock) })
	case footnoted && typed:
		// Blank lines past the typed block's closing fence stand outside it.
		last := len(lines.starts)
		if next := nextBlock(list); directive.Closed && (next == nil || !isAncestor(directive, next)) {
			last = lines.lineOf(directive.closer)
		} else if next != nil {
			last = lines.lineOf(startOf(next))
		}
		if len(lines.blanks(lines.lineOf(startOf(list)), last, whitespaceLine)) > 0 {
			return "a blank line at or after a list in a typed block in a footnote definition, which the browser editor reads as spacing the list by what follows it"
		}
		setSpread(list, false, spreadsNothing)
	case footnoted:
		setSpread(list, false, func(item ast.Node) bool {
			return blankBetweenBlocksBut(item, startsContainer) || blankAfterItem(item, definition)
		})
	default:
		spread := false
		for item := list.FirstChild().NextSibling(); item != nil; item = item.NextSibling() {
			spread = spread || blankBefore(item)
		}
		setSpread(list, spread, func(item ast.Node) bool { return blankBetweenBlocksBut(item, spreadsNothing) })
	}
	return ""
}

// quotedListSpread is a list's spread in a quote, as the browser editor's parser reads it: spread
// by a blank line after an item that does not end in a list, or, when the last item does not, by
// blank lines between it and what the quote goes on with: one before a quote, a list, an item or a
// footnote definition, and two before anything else or at the quote's end; in a typed block inside
// the quote, one before anything and two at its end.
func quotedListSpread(list *ast.List, quote, directive ast.Node, inDirective bool, lines sourceLines) (bool, string) {
	// Blank lines after an item that ends in a list are that list's. In a typed block inside the
	// quote, one after an item that ends in a quote is that quote's, and more spread the list.
	// A blank line of a quote around the list's own holds fewer markers, and the list's quote has
	// ended at it.
	depth := quoteDepth(list)
	blank := quoteBlankLineAt(depth)
	least := func(item ast.Node) int {
		if _, endsInQuote := item.LastChild().(*ast.Blockquote); inDirective && endsInQuote {
			return 2
		}
		return 1
	}
	for item := list.FirstChild(); item.NextSibling() != nil; item = item.NextSibling() {
		next := item.NextSibling()
		if blankBefore(next) && !endsWithList(item) && lines.blanksBefore(startOf(next), blank) >= least(item) {
			return true, ""
		}
	}
	last := list.LastChild()
	if endsWithList(last) {
		return false, ""
	}
	threshold := 2
	if inDirective {
		threshold = 1
	}
	threshold = max(threshold, least(last))
	next := nextBlock(list)
	if _, enclosingItem := next.(*ast.ListItem); enclosingItem {
		// The next item of a list around this one.
		return lines.blanksBefore(startOf(next), blank) >= threshold, ""
	}
	within := quote
	if inDirective {
		within = directive
	}
	if next == nil || !isAncestor(within, next) {
		// At the end of the quote, or of a typed block inside it before its closing fence.
		blanks := lines.blanksEnding(next, func(line []byte) bool {
			return whitespaceLine(line) || quoteBlankLine(line) && bytes.Count(line, []byte(">")) < depth
		}, blank)
		if typed, ok := within.(*typedDirective); ok && typed.Closed {
			blanks = lines.blanksBefore(typed.closer, blank)
		} else if typed, ok := ancestor[*typedDirective](within); ok && typed.Closed && (next == nil || !isAncestor(typed, next)) {
			// A quote in a typed block that nothing after the list goes on with ends at its fence.
			blanks = lines.blanksBefore(typed.closer, blank)
		}
		return blanks >= 2, ""
	}
	blanks := lines.blanksBefore(startOf(next), blank)
	switch {
	case blanks == 0:
		return false, ""
	case startsContainer(next):
		return blanks >= least(last), ""
	default:
		return blanks >= threshold, ""
	}
}

// footnotedQuoteListSpread is the spread of a list in a quote and a footnote definition, and of its
// last item, as the browser editor's parser reads them; an item before it is spread by a blank
// line before the next item. Blank lines between the list and what the nearer of the two goes on
// with space the last item, one before a quote or a list, and two before anything else when the
// quote stands inside the definition; with the definition inside the quote, two before anything
// else space the list. Blank lines after a last item that ends in a list are that list's.
func footnotedQuoteListSpread(list *ast.List, quote, definition ast.Node, lines sourceLines) (spread, lastSpread bool, reason string) {
	if endsWithList(list.LastChild()) {
		return false, false, ""
	}
	quoteInside := isAncestor(definition, quote)
	within := definition
	if quoteInside {
		within = quote
	}
	next := nextBlock(list)
	if _, enclosingItem := next.(*ast.ListItem); enclosingItem {
		// The next item of a list around this one: two blank lines before it space it as they
		// space anything else.
		spaced := lines.blanksBefore(startOf(next), quoteBlankLine) >= 2
		return !quoteInside && spaced, quoteInside && spaced, ""
	}
	if next == nil || !isAncestor(within, next) {
		if lines.blanksEnding(next, whitespaceLine, quoteBlankLine) > 0 {
			return false, false, "a blank line at the end of a quote or a footnote definition after a list, which the browser editor reads as spacing the list"
		}
		return false, false, ""
	}
	blanks := lines.blanksBefore(startOf(next), quoteBlankLine)
	if startsContainer(next) {
		return false, blanks > 0, ""
	}
	return !quoteInside && blanks >= 2, quoteInside && blanks >= 2, ""
}

// blankAfterItem reports whether a blank line follows item, in a footnote definition, before the
// next item or before a quote, list or item the definition goes on with.
func blankAfterItem(item, definition ast.Node) bool {
	if next := item.NextSibling(); next != nil {
		return blankBefore(next)
	}
	if endsWithList(item) {
		return false
	}
	next := nextBlock(item.Parent())
	if _, enclosingItem := next.(*ast.ListItem); enclosingItem {
		return false
	}
	return next != nil && blankBefore(next) && startsContainer(next) && isAncestor(definition, next)
}

// endsWithList reports whether item's last block is a list, whose own lines the browser editor's
// parser reads the blank lines after it as belonging to: a list item goes on across blank lines,
// where a quote ends at one.
func endsWithList(item ast.Node) bool {
	_, list := item.LastChild().(*ast.List)
	return list
}

// holdsTypedBlockWithBlankLine reports whether container holds a typed block with a blank line
// inside it, directly or in a footnote definition it holds, whose lines are the container's.
func holdsTypedBlockWithBlankLine(container ast.Node, lines sourceLines) bool {
	for child := container.FirstChild(); child != nil; child = child.NextSibling() {
		switch child := child.(type) {
		case *typedDirective:
			if lines.blankInside(child) {
				return true
			}
		case *extensionast.Footnote:
			if holdsTypedBlockWithBlankLine(child, lines) {
				return true
			}
		}
	}
	return false
}

func setSpread(list *ast.List, spread bool, itemSpread func(ast.Node) bool) {
	list.SetAttribute(browserSpreadAttr, spread)
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		item.SetAttribute(browserSpreadAttr, itemSpread(item))
	}
}

func spreadsNothing(ast.Node) bool { return false }

func anyBlock(ast.Node) bool { return true }

// blankBetweenBlocksBut reports whether a blank line separates two of item's blocks, or two blocks
// of a footnote definition it holds, which the browser editor's parser reads as the item's lines -
// but for the blank lines after a list the item holds, before a block listHolds accepts, which are
// that list's: in a quote, before any block (anyBlock); in a footnote definition, before a quote,
// a list or a footnote definition (startsContainer); elsewhere, before none (spreadsNothing).
func blankBetweenBlocksBut(item ast.Node, listHolds func(ast.Node) bool) bool {
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		// Goldmark appends a backlink, an inline node, after a referenced footnote definition's
		// last block when that block is not a paragraph; it is no block of the item's.
		if _, backlink := child.(*extensionast.FootnoteBacklink); backlink {
			continue
		}
		_, afterList := child.PreviousSibling().(*ast.List)
		if child != item.FirstChild() && blankBefore(child) && !(afterList && listHolds(child)) {
			return true
		}
		// In a footnote definition the item holds, blank lines after a list are that list's where
		// they are in the item's own blocks, and before a quote, a list or a footnote definition, as
		// in any footnote definition.
		if _, definition := child.(*extensionast.Footnote); definition && blankBetweenBlocksBut(child, func(block ast.Node) bool { return listHolds(block) || startsContainer(block) }) {
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
// next sibling, or that of its nearest ancestor that has one, past a footnote definition's
// backlink.
func nextBlock(node ast.Node) ast.Node {
	for ; node != nil; node = node.Parent() {
		for next := node.NextSibling(); next != nil; next = next.NextSibling() {
			if _, backlink := next.(*extensionast.FootnoteBacklink); !backlink {
				return next
			}
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
	return columnOf(l.source[l.starts[l.lineOf(position)]:position])
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

// blanksBefore is how many blank lines come right before position's line.
func (l sourceLines) blanksBefore(position int, blank func([]byte) bool) int {
	count := 0
	for line := l.lineOf(position) - 1; line >= 0 && blank(l.text(line)); line-- {
		count++
	}
	return count
}

// blanksEnding is how many blank lines end the container before next (nil for the source's
// end): the lines blank(line) holds that come right before the lines past(line) holds, if any,
// right before next's line - the whitespace-only lines, or those and the blank lines of the
// quotes around the container.
func (l sourceLines) blanksEnding(next ast.Node, past, blank func([]byte) bool) int {
	line := len(l.starts) - 1
	if next != nil {
		line = l.lineOf(startOf(next)) - 1
	}
	for line >= 0 && past(l.text(line)) {
		line--
	}
	count := 0
	for ; line >= 0 && blank(l.text(line)); line-- {
		count++
	}
	return count
}

// blankInside reports whether a line between typed block directive's opening and closing fences
// is blank; one that runs to its parent's end runs to the next block.
func (l sourceLines) blankInside(directive *typedDirective) bool {
	// In a quote, a line blank but for the quote's markers is blank, and a line without them ends
	// the quote and the typed block with it.
	blank := whitespaceLine
	if _, quoted := ancestor[*ast.Blockquote](directive); quoted {
		blank = quoteBlankLine
	}
	if !directive.Closed {
		return len(l.blankLines(directive, nextBlock(directive), blank)) > 0
	}
	return len(l.blanks(l.lineOf(directive.Pos())+1, l.lineOf(directive.closer), blank)) > 0
}

// markersOrWhitespace reports whether line holds nothing but whitespace and quote markers.
func markersOrWhitespace(line []byte) bool {
	return len(bytes.Trim(line, " \t>\n")) == 0
}

func whitespaceLine(line []byte) bool {
	return len(bytes.Trim(line, " \t\n")) == 0
}

// quoteBlankLineAt is quoteBlankLine in depth quotes: a line blank but for exactly depth quote
// markers.
func quoteBlankLineAt(depth int) func([]byte) bool {
	return func(line []byte) bool {
		return bytes.Count(line, []byte(">")) == depth && quoteBlankLine(line)
	}
}

// quoteDepth is how many quotes node stands in.
func quoteDepth(node ast.Node) int {
	depth := 0
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, quote := parent.(*ast.Blockquote); quote {
			depth++
		}
	}
	return depth
}

// quoteBlankLine reports whether line is a blank line inside a quote: quote markers and whitespace.
func quoteBlankLine(line []byte) bool {
	return bytes.IndexByte(line, '>') >= 0 && len(bytes.Trim(line, " \t>\n")) == 0
}
