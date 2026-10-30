package pmdoc

import (
	"fmt"

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
// The blank lines indented code keeps past where goldmark ends it are that code's, and space
// nothing (heldCodeLines, markBlocksAfterHeldCode).
//
// The lines alone do not decide a typed block holding a blank line in a list item, a blank line at
// the end of a quote after a list, or at or after a list in a typed block in a footnote
// definition, and this parser does not read the blank lines after a list item in a quote ending in
// a footnote definition whose typed block holds a fenced code block no fence closed
// (quotedDefinitionFenceBlank). A document holding such a shape is also refused where goldmark
// reads its blocks otherwise: an empty list item before a blank line and a block its outer item
// holds, where goldmark ends the outer item (emptyItemEndsOuterItem).
func browserListSpacing(root ast.Node, source []byte) error {
	lines := newSourceLines(source)
	_, lines.keptLast = root.Attribute(lastLineKeptAttr)
	lines.held = heldCodeLines(root, lines)
	markBlocksAfterHeldCode(root, lines)
	shape := browserOnlyShape(root)
	if shape == "" {
		if reason := readExactListSpacing(root, lines); reason != "" {
			return fmt.Errorf("%w: %s", ErrSchema, reason)
		}
		return nil
	}
	refuse := func(reason string) error {
		return fmt.Errorf("%w: %s, in a document holding %s, which is read as the browser editor reads it", ErrSchema, reason, shape)
	}
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

// goldmarkSpreadsItemsBeforeBlanks reports whether goldmark's looseness spreads each of list's
// items a blank line follows, the last one included: the list is loose and each holds more than
// one block (parseList).
func goldmarkSpreadsItemsBeforeBlanks(list *ast.List) bool {
	if list.IsTight {
		return false
	}
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		if next := item.NextSibling(); (next == nil || blankBefore(next)) && item.ChildCount() < 2 {
			return false
		}
	}
	return true
}

// readExactListSpacing reads, in a document holding no shape only the browser editor's parser and
// this one read alike, each list whose spacing the lines alone decide as that parser reads it
// (readListSpacing), and leaves every other list to goldmark's looseness. It names the two lists it
// cannot leave there, where that looseness reads an item the browser editor spreads as tight: one
// in a typed block in a footnote definition with a blank line at or after it
// (typedFootnoteListBlank), where that editor spreads the item a blank line follows and never the
// list, and goldmark's looseness then reads the list tight, or spreads it, or leaves that item
// holding one block unspread; and one whose item holds a typed block with a blank line inside it
// of a shape that editor reads as spacing the item (typedBlankInItem: typedCodeBlankInItem,
// unclosedTypedCodeBlankInItem and typedListBlankInItem).
func readExactListSpacing(root ast.Node, lines sourceLines) (refusal string) {
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.List:
			if !entering {
				return ast.WalkContinue, nil
			}
			reason := readListSpacing(node, lines)
			if reason == typedFootnoteListBlank && !goldmarkSpreadsItemsBeforeBlanks(node) {
				refusal = typedFootnoteListBlank
				return ast.WalkStop, nil
			}
			// A list the lines decide carries that reading already; only one left to goldmark's
			// looseness, which reads such an item tight, is refused.
			if reason != "" {
				if blank := typedBlankSpreadsATightItem(node, lines); blank != "" {
					refusal = blank
					return ast.WalkStop, nil
				}
			}
		}
		return ast.WalkContinue, nil
	})
	return refusal
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
	// In a typed block in a footnote definition, whatever quotes or typed blocks stand between, the
	// browser editor reads a blank line at or after a list as spacing it by what follows.
	if typed && footnoted && isAncestor(definition, directive) {
		blank := whitespaceLine
		if quoted {
			blank = quoteBlankLineAt(quoteDepth(list))
		}
		if lines.blankAtOrAfterTypedList(list, directive, blank) {
			return typedFootnoteListBlank
		}
	}
	switch {
	case typed && footnoted:
		// A list with a typed block and a footnote definition around it, whatever quotes stand
		// between, is spread by nothing: a blank line at or after it in a typed block in a
		// definition is refused above, and a definition in a typed block is refused where the tree
		// is read (parseBlocks).
		setSpread(list, false, spreadsNothing, lines)
	case quoted && footnoted:
		spread, lastSpread, reason := footnotedQuoteListSpread(list, quote, definition, lines)
		if reason != "" {
			return reason
		}
		setSpread(list, spread, func(item ast.Node) bool {
			// Blank lines after a list are that list's.
			if next := item.NextSibling(); next != nil {
				return blankBetweenBlocksBut(item, anyBlock, spreadsNothing) || blankBefore(next) && !endsWithList(item)
			}
			return blankBetweenBlocksBut(item, anyBlock, spreadsNothing) || lastSpread
		}, lines)
	case quoted:
		// A typed block's content is a document of its own to the browser editor's parser, whose
		// last line ends where the fence, or the block around the typed block, ends it, and the
		// blank lines that reach a list there are counted in that document (quotedListSpread).
		spread := quotedListSpread(list, quotedListIn(list, quote, directive, typed), lines)
		// The blank lines after an item ending in a footnote definition whose typed block ends in a
		// fenced code block no fence closed are that code's to the browser editor's parser, where
		// definitionEndSpreadsItem counts them as the definition's.
		for item := list.FirstChild(); item != nil; item = item.NextSibling() {
			if definitionEndSpreadsItem(item, list, quote, lines) && typedFenceKeepsBlankLinesAfter(item.LastChild()) {
				return quotedDefinitionFenceBlank
			}
		}
		definitionHolds := definitionBlanksInQuote(list, lines)
		setSpread(list, spread, func(item ast.Node) bool {
			return blankBetweenBlocksBut(item, anyBlock, definitionHolds) || definitionEndSpreadsItem(item, list, quote, lines) ||
				emptyDefinitionBeforeContainer(item)
		}, lines)
	case footnoted:
		setSpread(list, false, func(item ast.Node) bool {
			return blankBetweenBlocksBut(item, startsContainer, spreadsNothing) || blankAfterItem(item, definition)
		}, lines)
	default:
		spread := false
		for item := list.FirstChild().NextSibling(); item != nil; item = item.NextSibling() {
			spread = spread || blankBefore(item)
		}
		setSpread(list, spread, func(item ast.Node) bool { return blankBetweenBlocksBut(item, spreadsNothing, spreadsNothing) }, lines)
	}
	return ""
}

// definitionBlanksInQuote accepts the blocks before which, in list's quote, the blank lines after a
// footnote definition one of its items holds are the definition's and spread nothing, as the
// browser editor's parser reads them: one blank line before anything but a quote, a list or a
// footnote definition (startsContainer), which that line spreads the item before, as two blank
// lines do before anything. An empty definition's own line is one of them (emptyDefinition).
func definitionBlanksInQuote(list ast.Node, lines sourceLines) func(ast.Node) bool {
	blank := quoteBlankLineAt(quoteDepth(list))
	return func(block ast.Node) bool {
		return !startsContainer(block) && lines.blanksBefore(startOf(block), blank)+emptyLines(block.PreviousSibling()) < 2
	}
}

// emptyDefinitionBeforeContainer reports whether item, in a quote, holds an empty footnote
// definition right before a quote, a list or a definition, with no blank line between: the
// browser editor's parser reads the definition's line as a blank line of the item, which spreads it
// before those as one blank line does (definitionBlanksInQuote).
func emptyDefinitionBeforeContainer(item ast.Node) bool {
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		if next := child.NextSibling(); next != nil && emptyDefinition(child) && startsContainer(next) && !blankBefore(next) {
			return true
		}
	}
	return false
}

// emptyDefinition reports whether node is a footnote definition holding no block, only the backlink
// goldmark may append, whose line the browser editor's parser reads, in a quote, as a blank line of
// the list item holding it.
func emptyDefinition(node ast.Node) bool {
	definition, ok := node.(*extensionast.Footnote)
	if !ok {
		return false
	}
	return definition.ChildCount() == 0
}

// emptyLines is the blank line node's own line counts as, 1 for an empty footnote definition
// (emptyDefinition), 0 otherwise.
func emptyLines(node ast.Node) int {
	if emptyDefinition(node) {
		return 1
	}
	return 0
}

// definitionEndSpreadsItem reports whether the blank lines after item, in a list in quote, spread
// it as the browser editor's parser reads them where item ends in a footnote definition: they are
// the definition's, so one spreads the item before a quote, a list or a definition after the list,
// two spread it before anything - the next item and the end of the quote as well - and neither
// spreads the list (quotedListSpread). An empty definition's own line is one of them
// (emptyDefinition).
func definitionEndSpreadsItem(item ast.Node, list *ast.List, quote ast.Node, lines sourceLines) bool {
	if !itemEndsInDefinition(item) {
		return false
	}
	depth := quoteDepth(list)
	blank := quoteBlankLineAt(depth)
	own := emptyLines(item.LastChild())
	if next := item.NextSibling(); next != nil {
		return lines.blanksBefore(startOf(next), blank)+own >= 2
	}
	next := nextBlock(list)
	if next == nil || !isAncestor(quote, next) {
		return lines.blanksEnding(next, outerBlankLineAt(depth), blank)+own >= 2
	}
	blanks := lines.blanksBefore(startOf(next), blank) + own
	switch next.(type) {
	case *ast.Blockquote, *ast.List, *extensionast.Footnote:
		return blanks >= 1
	}
	return blanks >= 2
}

// itemEndsInDefinition reports whether item's last block is a footnote definition.
func itemEndsInDefinition(item ast.Node) bool {
	_, definition := item.LastChild().(*extensionast.Footnote)
	return definition
}

// typedFootnoteListBlank is the refusal of a blank line at or after a list in a typed block in a
// footnote definition (blankAtOrAfterTypedList).
const typedFootnoteListBlank = "a blank line at or after a list in a typed block in a footnote definition, which the browser editor reads as spacing the list by what follows it"

// quotedDefinitionFenceBlank is the refusal of blank lines after a list item in a quote that ends in
// a footnote definition whose typed block ends in a fenced code block no fence closed
// (typedFenceKeepsBlankLinesAfter), where they would spread the item (definitionEndSpreadsItem).
const quotedDefinitionFenceBlank = "blank lines after a list item in a quote ending in a footnote definition whose typed block holds a fenced code block no fence closed, which the browser editor reads as that code's lines and not as spacing the item"

// typedCodeBlankInItem, unclosedTypedCodeBlankInItem and typedListBlankInItem are the refusals of
// the blank lines inside a typed block in a list item that the browser editor reads as spacing
// the item and goldmark reads as no block's (typedBlankInItem), one for each shape measured at
// the fork point and at this head (PR #2507). The first two are indented code ending in a
// whitespace-only line the code does not hold, in a block a fence closes and in one nothing in
// the document follows: indented code keeps a blank line of four columns or more past its
// container and ends before a shorter one, in both parsers (codeColumns), so that line is a blank
// line of the list item to the browser editor, which spreads the item by it, while goldmark drops
// the line and leaves the item to its looseness, which reads it tight. The third is a blank line
// between a list the typed block ends with and the block's own end, which that editor reads the
// same way while leaving the list inside the block tight.
//
// A blank line anywhere else in a typed block in a list item is left to that looseness, since the
// writer writes such a line - a tight item's typed block holding two blocks is written that way -
// and the documents Dispatch stores were read with it. The writer writes no indented code and no
// blank line before a block's closing fence, so no document it wrote holds any of these shapes,
// and a caller who writes one can write the code fenced, the line as wide as the code, or no
// blank line where the block ends.
const (
	typedCodeBlankInItem         = "indented code in a typed block in a list item ending in a whitespace-only line shorter than the code's indentation, which the browser editor reads as a blank line spacing the item"
	unclosedTypedCodeBlankInItem = "indented code in a typed block no fence closes, in a list item, ending in a whitespace-only line shorter than the code's indentation with nothing in the document past the block, which the browser editor reads as a blank line spacing the item"
	typedListBlankInItem         = "a blank line between a list a typed block in a list item ends with and the block's own end, which the browser editor reads as a blank line spacing the item around the block"
)

// typedBlankSpreadsATightItem is why an item of list holds a typed block with such a blank line
// inside it (typedBlankInTheItem) where goldmark's looseness leaves that item unspread: a tight
// list's item, or one holding a single block (parseList), which is the item the browser editor
// reads spread. It is "" where no item of the list holds one.
func typedBlankSpreadsATightItem(list *ast.List, lines sourceLines) string {
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		if list.IsTight || item.ChildCount() < 2 {
			if reason := typedBlankInTheItem(item, lines); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// typedBlankInTheItem is why a typed block container holds holds a blank line only the browser
// editor reads as spacing the list item (typedBlankInItem), or "" where it holds none. It looks
// through the typed blocks and the footnote definitions inside it, whose lines are the list
// item's own, and through no quote or list, which that parser counts a blank line inside as that
// quote's or that list's.
func typedBlankInTheItem(container ast.Node, lines sourceLines) string {
	for child := container.FirstChild(); child != nil; child = child.NextSibling() {
		switch child := child.(type) {
		case *typedDirective:
			if reason := lines.typedBlankInItem(child); reason != "" {
				return reason
			}
			if reason := typedBlankInTheItem(child, lines); reason != "" {
				return reason
			}
		case *extensionast.Footnote:
			if reason := typedBlankInTheItem(child, lines); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// blankAtOrAfterTypedList reports whether a line blank(line) holds stands from list's start up to
// the next block, or to the closing fence of directive, the typed block around it, where nothing
// in directive follows the list: blank lines past the fence stand outside it.
func (l sourceLines) blankAtOrAfterTypedList(list ast.Node, directive *typedDirective, blank func([]byte) bool) bool {
	last := len(l.starts)
	if next := nextBlock(list); directive.Closed && (next == nil || !isAncestor(directive, next)) {
		last = l.lineOf(directive.closer)
	} else if next != nil {
		last = l.lineOf(startOf(next))
	}
	return len(l.blanks(l.lineOf(startOf(list)), last, blank)) > 0
}

// quotedList is where a list in a quote stands among the containers that decide its spacing, read
// once for the list (readListSpacing): quote is the quote around it, directive the nearest typed
// block around it or nil, inDirective whether that block stands inside the quote - its content is
// a document of its own to the browser editor's parser, which the blank lines reaching the list
// are counted in - and inQuotedTyped whether the list's quote stands in a typed block that stands
// in a quote itself, where that parser ends the list where the block's content starts on the line
// after its blank lines (quotedInTypedBlockSpread). The two cannot both hold.
type quotedList struct {
	quote         ast.Node
	directive     *typedDirective
	inDirective   bool
	inQuotedTyped bool
}

// quotedListIn is where list, in quote, stands among those containers, directive being the
// nearest typed block around it and typed whether there is one.
func quotedListIn(list *ast.List, quote ast.Node, directive *typedDirective, typed bool) quotedList {
	inDirective := typed && isAncestor(quote, directive)
	return quotedList{
		quote:         quote,
		directive:     directive,
		inDirective:   inDirective,
		inQuotedTyped: typed && !inDirective && quoteDepth(directive) > 0,
	}
}

// quotedListSpread is the spread of a list in a quote, standing as in says, as the browser
// editor's parser reads it: spread by a blank line after an item that does not end in a list, or,
// when the last item does not, by blank lines between it and what the quote goes on with: one
// before a quote, a list, an item or a footnote definition - and before a quote, a list or a
// footnote definition that opens right after them in a quote around the list's - and two before
// anything else or at the quote's end; in a typed block inside the quote, one before anything and
// two at its fence, three where no fence closes it and the last of them holds nothing past the
// quote's markers, and one more of each after an item ending in a quote; with the list's quote in
// a typed block that stands in a quote, as quotedInTypedBlockSpread reads.
func quotedListSpread(list *ast.List, in quotedList, lines sourceLines) bool {
	// Blank lines after an item that ends in a list are that list's. In a typed block inside the
	// quote, one after an item that ends in a quote is that quote's, and more spread the list.
	// A blank line of a quote around the list's own holds fewer markers, and the list's quote has
	// ended at it; where the typed block the list's quote stands in stands in a quote itself and
	// goes on past such lines, to a block inside it or to its fence, they are the list's too, after
	// one of its own (blanksThroughOuterQuotes).
	depth := quoteDepth(list)
	blank := quoteBlankLineAt(depth)
	least := func(item ast.Node) int {
		if _, endsInQuote := item.LastChild().(*ast.Blockquote); (in.inDirective || in.inQuotedTyped) && endsInQuote {
			return 2
		}
		return 1
	}
	for item := list.FirstChild(); item.NextSibling() != nil; item = item.NextSibling() {
		next := item.NextSibling()
		if blankBefore(next) && !endsWithList(item) && !itemEndsInDefinition(item) && lines.blanksBefore(startOf(next), blank) >= least(item) {
			return true
		}
	}
	last := list.LastChild()
	// The blank lines after a list ending in a fenced code block no fence closed are that block's,
	// or its quote's, and space no list, and those after one ending in a footnote definition are the
	// definition's (definitionEndSpreadsItem).
	if endsWithList(last) || endsInOpenFence(list) || itemEndsInDefinition(last) {
		return false
	}
	next := nextBlock(list)
	if in.inQuotedTyped {
		return lines.quotedInTypedBlockSpread(list, in.directive, next, least(last))
	}
	threshold := 2
	if in.inDirective {
		threshold = 1
	}
	threshold = max(threshold, least(last))
	within := in.quote
	if in.inDirective {
		within = in.directive
	}
	// The next item of a list around this one, but not past the closing fence of the list's own
	// typed block: the typed block's content is a document of its own to the browser editor's
	// parser, which ends at that fence, so blank lines after it are not the list's, and the fence
	// decides below.
	if _, enclosingItem := next.(*ast.ListItem); enclosingItem && (!in.inDirective || !in.directive.Closed || isAncestor(within, next)) {
		return lines.blanksBefore(startOf(next), blank) >= threshold
	}
	if next == nil || !isAncestor(within, next) {
		// At the end of the quote, or of a typed block inside it before its closing fence. A typed
		// block no fence closes runs to the quote's end - or to the fence of a typed block around
		// that quote - which takes a third blank line where the last holds nothing past the
		// quote's markers and their spaces, the browser editor's parser reading that line as no
		// line of the typed block, but for a container opening on the line right after them, which
		// the last one spaces too; and an item ending in a quote gives that quote one more (least).
		blanks, end, outer := lines.blanksEndingTheList(list, next, depth)
		// A quote, a list or a footnote definition opening in a quote around the list's, on the
		// line right after its blank lines, ends the list past that quote's markers on that line,
		// so the browser editor's parser spaces the list by one blank line.
		if !in.inDirective && opensInQuoteAround(next, list) && lines.blanksBefore(startOf(next), outer) == 0 {
			return blanks >= 1
		}
		need := 2
		if typed, ok := within.(*typedDirective); ok {
			if ending, ok := fenceEnding(typed); ok && (next == nil || !isAncestor(ending, next)) {
				blanks = lines.blanksThroughOuterQuotes(ending.closer, depth)
			} else if (next == nil || !startsContainer(next) || lines.blanksBefore(startOf(next), blank) != blanks) &&
				lines.holdsOnlyQuoteMarkers(lines.lastBefore(end, outer), depth) {
				need = 3
			}
			need += least(last) - 1
		} else if typed, ok := ancestor[*typedDirective](within); ok {
			// A quote in a typed block that nothing after the list goes on with ends by the fence
			// that ends the typed block, or at a line before it without the quote's markers.
			if ending, ok := fenceEnding(typed); ok && (next == nil || !isAncestor(ending, next)) {
				blanks = lines.blanksEndingBefore(lines.lineOf(ending.closer), outer, blank)
			}
		}
		return blanks >= need
	}
	blanks := lines.blanksBefore(startOf(next), blank)
	if in.inDirective {
		blanks = lines.blanksThroughOuterQuotes(startOf(next), depth)
	}
	switch {
	case blanks == 0:
		return false
	case startsContainer(next):
		return blanks >= least(last)
	default:
		return blanks >= threshold
	}
}

// quotedInTypedBlockSpread is the spread of list, whose quote stands in typed, a typed block that
// stands in a quote, after its last item, with next the block that follows the list. The browser
// editor's parser ends the list on the first line after its blank lines, and where that line goes
// on with typed, a block or a blank line of typed's, it ends it where typed's content starts on
// that line, past the markers of the quotes around typed, so that the line endings of the blank
// lines and of the item's last line before them space the list: least blank lines do. At typed's
// closing fence, and past typed, the list ends before that line, and least+1 do. least is 2 after
// an item ending in a quote, whose own end the first blank line holds, and 1 otherwise.
func (l sourceLines) quotedInTypedBlockSpread(list *ast.List, typed *typedDirective, next ast.Node, least int) bool {
	depth := quoteDepth(list)
	blanks, end, outer := l.blanksEndingTheList(list, next, depth)
	ending := l.lastBefore(end, outer) + 1
	goesOn := ending < end && leadingQuoteMarkers(l.text(ending)) >= quoteDepth(typed) ||
		ending == end && next != nil && isAncestor(typed, next)
	if goesOn {
		return blanks >= least
	}
	return blanks >= least+1
}

// blanksEndingTheList is how many blank lines end list in depth quotes before next, the block that
// follows it (nil at the source's end), with end the line its container ends before (endBefore)
// and outer the blank line of the quotes around it, which both callers read on as well.
func (l sourceLines) blanksEndingTheList(list *ast.List, next ast.Node, depth int) (blanks, end int, outer func([]byte) bool) {
	end = l.endBefore(list, next)
	outer = outerBlankLineAt(depth)
	return l.blanksEndingBefore(end, outer, quoteBlankLineAt(depth)), end, outer
}

// endBefore is the line that node's container ends before where next follows it: next's line, or
// the end of the source where next is nil, or the closing fence of the nearest typed block around
// node that a fence closes and next stands past, whichever comes first.
func (l sourceLines) endBefore(node, next ast.Node) int {
	end := len(l.starts)
	if next != nil {
		end = l.lineOf(startOf(next))
	}
	for typed, ok := ancestor[*typedDirective](node); ok; typed, ok = ancestor[*typedDirective](typed) {
		if typed.Closed && (next == nil || !isAncestor(typed, next)) {
			return min(end, l.lineOf(typed.closer))
		}
	}
	return end
}

// opensInQuoteAround reports whether next opens a quote, a list or a footnote definition inside a
// quote around list, whose markers next's line carries.
func opensInQuoteAround(next ast.Node, list *ast.List) bool {
	switch next.(type) {
	case *ast.Blockquote, *ast.List, *extensionast.Footnote:
	default:
		return false
	}
	for parent := list.Parent(); parent != nil; parent = parent.Parent() {
		if _, quote := parent.(*ast.Blockquote); quote && isAncestor(parent, next) {
			return true
		}
	}
	return false
}

// fenceEnding is the typed block whose closing fence ends typed: typed itself where a fence closes
// it, or, where none does and typed stands directly in another typed block, the block whose fence
// ends that one, since the browser editor's parser ends a typed block no fence closes with the
// block around it. ok is false where no fence ends typed.
func fenceEnding(typed *typedDirective) (*typedDirective, bool) {
	for !typed.Closed {
		outer, ok := typed.Parent().(*typedDirective)
		if !ok {
			return nil, false
		}
		typed = outer
	}
	return typed, true
}

// footnotedQuoteListSpread is the spread of a list in a quote and a footnote definition, and of its
// last item, as the browser editor's parser reads them; an item before it is spread by a blank
// line before the next item. Blank lines between the list and what the nearer of the two goes on
// with space the last item, one before a quote or a list, and two before anything else when the
// quote stands inside the definition; with the definition inside the quote, two before anything
// else space the list. Blank lines after a last item that ends in a list are that list's, and
// those after one ending in a typed block that ends in a fenced code block no fence closed are
// that code's (keepsBlankLinesAfter).
func footnotedQuoteListSpread(list *ast.List, quote, definition ast.Node, lines sourceLines) (spread, lastSpread bool, reason string) {
	last := list.LastChild()
	if endsWithList(last) {
		return false, false, ""
	}
	if typedFenceKeepsBlankLinesAfter(last) {
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

// setSpread records list's spread and each item's, itemSpread's or, where the item holds a footnote
// definition whose first line holds nothing, spread (holdsDefinitionOpeningOnALaterLine).
func setSpread(list *ast.List, spread bool, itemSpread func(ast.Node) bool, lines sourceLines) {
	list.SetAttribute(browserSpreadAttr, spread)
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		item.SetAttribute(browserSpreadAttr, itemSpread(item) || holdsDefinitionOpeningOnALaterLine(item, lines))
	}
}

// holdsDefinitionOpeningOnALaterLine reports whether item holds, as one of its own blocks, a
// footnote definition whose first line holds nothing past its `]:` and whose first block starts on
// a later line: the browser editor's parser reads that first line as a blank line of the item,
// which spreads it. An empty definition, and one inside a quote in the item, spread nothing.
func holdsDefinitionOpeningOnALaterLine(item ast.Node, lines sourceLines) bool {
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		if definition, ok := child.(*extensionast.Footnote); ok {
			if first := definition.FirstChild(); first != nil && first.Type() == ast.TypeBlock && lines.lineOf(startOf(first)) > lines.lineOf(definition.Pos()) {
				return true
			}
		}
	}
	return false
}

func spreadsNothing(ast.Node) bool { return false }

func anyBlock(ast.Node) bool { return true }

// blankBetweenBlocksBut reports whether a blank line separates two of item's blocks, or two blocks
// of a footnote definition it holds, which the browser editor's parser reads as the item's lines -
// but for the blank lines after a list the item holds, before a block listHolds accepts, which are
// that list's: in a quote, before any block (anyBlock); in a footnote definition, before a quote,
// a list or a footnote definition (startsContainer); elsewhere, before none (spreadsNothing). The
// blank lines after a footnote definition the item holds, before a block definitionHolds accepts,
// are the definition's the same way (definitionBlanksInQuote).
func blankBetweenBlocksBut(item ast.Node, listHolds, definitionHolds func(ast.Node) bool) bool {
	for child := item.FirstChild(); child != nil; child = child.NextSibling() {
		_, afterList := child.PreviousSibling().(*ast.List)
		_, afterDefinition := child.PreviousSibling().(*extensionast.Footnote)
		if child != item.FirstChild() && blankBefore(child) && !(afterList && listHolds(child)) && !(afterDefinition && definitionHolds(child)) {
			return true
		}
		// In a footnote definition the item holds, blank lines after a list are that list's where
		// they are in the item's own blocks, and before a quote, a list or a footnote definition, as
		// in any footnote definition.
		if _, definition := child.(*extensionast.Footnote); definition && blankBetweenBlocksBut(child, func(block ast.Node) bool { return listHolds(block) || startsContainer(block) }, spreadsNothing) {
			return true
		}
	}
	return false
}

// blankAfterAttr marks a block goldmark made from a paragraph that followed a blank line, which
// its HasBlankPreviousLines does not carry over (lazyTableRows.transform), and the block right
// after indented code's held lines, which follows no blank line (markBlocksAfterHeldCode).
var blankAfterAttr = []byte("pmdoc-blank-after")

// blankBefore reports whether a blank line comes right before block. A line indented code holds
// past where goldmark ends it is the code's, so the block right after one follows none.
func blankBefore(block ast.Node) bool {
	if blank, ok := block.Attribute(blankAfterAttr); ok {
		return blank.(bool)
	}
	if keepsBlankLinesAfter(block.PreviousSibling()) {
		return false
	}
	return block.HasBlankPreviousLines()
}

// heldCodeLines is the lines each indented code block holds past where goldmark ends it
// (codeHeldAttr): the code's text, which spaces nothing, so no counter of blank lines counts them
// (sourceLines.held).
func heldCodeLines(root ast.Node, lines sourceLines) map[int]bool {
	var held map[int]bool
	eachHeldCode(root, func(code *ast.CodeBlock, from int) {
		if held == nil {
			held = map[int]bool{}
		}
		segments := code.Lines()
		for index := from; index < segments.Len(); index++ {
			held[lines.lineOf(segments.At(index).Start)] = true
		}
	})
	return held
}

// markBlocksAfterHeldCode records on the block on the line right after an indented code block's
// last held line that it follows no blank line (blankAfterAttr), whatever goldmark records on it,
// even where that line holds only its quotes' markers (blankBefore). It runs after the parse, so it
// decides over what lazyTableRows.transform records there.
func markBlocksAfterHeldCode(root ast.Node, lines sourceLines) {
	eachHeldCode(root, func(code *ast.CodeBlock, _ int) {
		segments := code.Lines()
		last := lines.lineOf(segments.At(segments.Len() - 1).Start)
		if next := nextBlock(code); next != nil && lines.lineOf(startOf(next)) == last+1 {
			next.SetAttribute(blankAfterAttr, false)
		}
	})
}

// eachHeldCode calls visit with each indented code block that holds lines past where goldmark
// ends it, and the index of the first of them (codeHeldAttr).
func eachHeldCode(root ast.Node, visit func(code *ast.CodeBlock, from int)) {
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
			return ast.WalkSkipChildren, nil
		case *ast.CodeBlock:
			if from, ok := node.Attribute(codeHeldAttr); ok {
				visit(node, from.(int))
			}
		}
		return ast.WalkContinue, nil
	})
}

// keepsBlankLinesAfter reports whether node ends in a fenced code block no fence closed that takes
// the blank lines after node as its own lines, so that they spread nothing: every container
// between the block and node, node included, continues across a blank line, and the browser
// editor's parser reads such a line as the code's, even where the code's text drops it
// (blankTaker). A quote between ends at a blank line without its marker, so the line is not its
// code's.
func keepsBlankLinesAfter(node ast.Node) bool {
	if !endsInOpenFence(node) {
		return false
	}
	for container := lastFencedCode(node).Parent(); container != node.Parent(); container = container.Parent() {
		if _, quote := container.(*ast.Blockquote); quote {
			return false
		}
	}
	return true
}

// typedFenceKeepsBlankLinesAfter reports whether node ends in a fenced code block no fence closed,
// inside a typed block node holds, that takes the blank lines after node as its own lines
// (keepsBlankLinesAfter).
func typedFenceKeepsBlankLinesAfter(node ast.Node) bool {
	if !keepsBlankLinesAfter(node) {
		return false
	}
	typed, ok := ancestor[*typedDirective](lastFencedCode(node))
	return ok && isAncestor(node, typed)
}

// endsInOpenFence reports whether node ends in a fenced code block no fence closed, and that runs
// to node's end: a typed block between them, node included, that its own fence closes ends the code
// there, so the lines after it are not the code's.
func endsInOpenFence(node ast.Node) bool {
	code := lastFencedCode(node)
	if code == nil {
		return false
	}
	if _, closed := code.Attribute(fenceClosedAttr); closed {
		return false
	}
	for container := code.Parent(); container != node.Parent(); container = container.Parent() {
		if typed, ok := container.(*typedDirective); ok && typed.Closed {
			return false
		}
	}
	return true
}

// lastFencedCode is the fenced code block node ends in, through the last block of each container
// it ends in, or nil when it ends in no fenced code block.
func lastFencedCode(node ast.Node) *ast.FencedCodeBlock {
	for node != nil && node.Type() == ast.TypeBlock {
		if code, ok := node.(*ast.FencedCodeBlock); ok {
			return code
		}
		node = node.LastChild()
	}
	return nil
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
