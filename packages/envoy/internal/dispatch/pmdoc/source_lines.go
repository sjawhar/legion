package pmdoc

import (
	"bytes"
	"sort"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// itemContentColumn is the column item's content starts at as the browser editor's parser measures
// it: where its first block's text does (browserTextColumn), except where the rest of the item's
// marker line is blank or indented code, five columns or more, where it is one column past the
// marker, however far in the first block then stands.
func (l sourceLines) itemContentColumn(item ast.Node) int {
	start := startOf(item)
	line := l.starts[l.lineOf(start)]
	marker := start
	for marker < len(l.source) && (l.source[marker] == ' ' || l.source[marker] == '\t') {
		marker++
	}
	end := marker + len(listMarker.Find(l.source[marker:]))
	text := end
	for text < len(l.source) && (l.source[text] == ' ' || l.source[text] == '\t') {
		text++
	}
	markerEnd, textStart := columnOf(l.source[line:end]), columnOf(l.source[line:text])
	if text == len(l.source) || l.source[text] == '\n' || textStart-markerEnd >= 5 {
		return markerEnd + 1
	}
	return browserTextColumn(item.FirstChild(), l)
}

// leadingQuoteMarkers is how many quote markers open line, each with the spaces and tabs before it.
func leadingQuoteMarkers(line []byte) int {
	count := 0
	for {
		line = bytes.TrimLeft(line, " \t")
		if len(line) == 0 || line[0] != '>' {
			return count
		}
		count++
		line = line[1:]
	}
}

// quoteContentColumn is the column the content of count quotes starts at on position's line: past
// each quote marker, the spaces and tabs before it, and one column of the space or tab after it -
// the rest of a tab there is the content's indentation.
func (l sourceLines) quoteContentColumn(position, count int) int {
	index, column := l.starts[l.lineOf(position)], 0
	advance := func() {
		if l.source[index] == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
		index++
	}
	for ; count > 0; count-- {
		for index < len(l.source) && (l.source[index] == ' ' || l.source[index] == '\t') {
			advance()
		}
		if index >= len(l.source) || l.source[index] != '>' {
			break
		}
		advance()
		if index < len(l.source) && (l.source[index] == ' ' || l.source[index] == '\t') {
			if count == 1 {
				return column + 1
			}
			advance()
		}
	}
	return column
}

// pastQuoteMarkers is position past count quote markers on its line, each with the spaces and tabs
// before it and the space after it.
func (l sourceLines) pastQuoteMarkers(position, count int) int {
	for ; count > 0; count-- {
		for position < len(l.source) && (l.source[position] == ' ' || l.source[position] == '\t') {
			position++
		}
		if position >= len(l.source) || l.source[position] != '>' {
			break
		}
		position++
		if position < len(l.source) && l.source[position] == ' ' {
			position++
		}
	}
	return position
}

// browserTextColumn is the column block's text starts at as the browser editor's parser measures
// it: on a footnote definition's first line, the definition's text starts where its later lines'
// text does, four columns past where its container's content starts, whatever the spaces before
// its `[^` and after its `]:`.
func browserTextColumn(block ast.Node, lines sourceLines) int {
	column := lines.textColumn(startOf(block))
	definition, footnoted := ancestor[*extensionast.Footnote](block)
	if !footnoted || lines.lineOf(definition.Pos()) != lines.lineOf(startOf(block)) {
		return column
	}
	base := lines.textColumn(definition.Pos())
	switch definition.Parent().(type) {
	case *ast.Document:
		base = 0
	case *ast.Blockquote:
		base = lines.quoteContentColumn(definition.Pos(), quoteDepth(definition))
	}
	return column - lines.textColumn(startOf(definition.FirstChild())) + base + 4
}

// sourceLines is where each line of a source starts, and which of its lines indented code holds
// past where goldmark ends the code (held): those are the code's, not blank, wherever a blank line
// is counted (heldCodeLines).
type sourceLines struct {
	source []byte
	starts []int
	held   map[int]bool
	// keptLast is whether lastLineColumns kept the last line (lastLineKeptAttr).
	keptLast bool
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

// blanksOutsideCode is blanks less the lines a code block inside within holds: a blank line of code
// is the code's text, which spaces nothing. Indented code's held lines (codeHeldAttr) are not the
// code's here, nor the columns of a last line lastLineColumns kept: the refusal that calls it, of
// a blank line the lines alone do not decide (blankInside), reads them on asHeldBefore's lines as
// the blank lines they were before the code held them, so it refuses what it refused then. It also
// refuses a document the reader before PR #2481 misread, whose last whitespace-only line a typed
// block's indentation now takes out of its fenced code (typedDirectiveParser.Continue).
func (l sourceLines) blanksOutsideCode(within ast.Node, blanks []int) []int {
	code := map[int]bool{}
	_ = ast.Walk(within, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			segments := node.Lines()
			end := segments.Len()
			if from, held := node.Attribute(codeHeldAttr); held {
				end = from.(int)
			}
			for index := range end {
				code[l.lineOf(segments.At(index).Start)] = true
			}
			if l.keptLast {
				// The last line lastLineColumns kept, which goldmark dropped, was no code's.
				delete(code, len(l.starts)-1)
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	var outside []int
	for _, line := range blanks {
		if !code[line] {
			outside = append(outside, line)
		}
	}
	return outside
}

// blanks is the lines from first up to last that are blank.
func (l sourceLines) blanks(first, last int, blank func([]byte) bool) []int {
	var blanks []int
	for line := first; line < last; line++ {
		if l.blank(line, blank) {
			blanks = append(blanks, line)
		}
	}
	return blanks
}

// blanksBefore is how many blank lines come right before position's line.
func (l sourceLines) blanksBefore(position int, blank func([]byte) bool) int {
	count := 0
	for line := l.lineOf(position) - 1; line >= 0 && l.blank(line, blank); line-- {
		count++
	}
	return count
}

// blanksEnding is how many blank lines end the container before next (nil for the source's
// end): the lines blank(line) holds that come right before the lines past(line) holds, if any,
// right before next's line - the whitespace-only lines, or those and the blank lines of the
// quotes around the container.
func (l sourceLines) blanksEnding(next ast.Node, past, blank func([]byte) bool) int {
	line := len(l.starts)
	if next != nil {
		line = l.lineOf(startOf(next))
	}
	return l.blanksEndingBefore(line, past, blank)
}

// blanksEndingBefore is blanksEnding for a container ending before line.
func (l sourceLines) blanksEndingBefore(line int, past, blank func([]byte) bool) int {
	count := 0
	for line = l.lastBefore(line, past); line >= 0 && l.blank(line, blank); line-- {
		count++
	}
	return count
}

// lastBefore is the last line before line that past(line) does not hold, past the lines it holds
// right before line, or -1.
func (l sourceLines) lastBefore(line int, past func([]byte) bool) int {
	line--
	for line >= 0 && l.blank(line, past) {
		line--
	}
	return line
}

// holdsOnlyBlockIndent reports whether line opens with depth quote markers, each with the space
// after it, and holds past them nothing but indentation the containers between those quotes and
// typed's content strip from every line they continue: as far past the quotes' content as typed's
// own opening line starts, its list item's indentation and the block's own among it
// (typedDirectiveParser.Continue). What is left of such a line for typed's content is its line
// ending alone, so the browser editor's parser reads it as no line of typed.
func (l sourceLines) holdsOnlyBlockIndent(line, depth int, typed *typedDirective) bool {
	if line < 0 || leadingQuoteMarkers(l.text(line)) != depth {
		return false
	}
	start := l.starts[line]
	past := l.pastQuoteMarkers(start, depth)
	if !whitespaceLine(l.text(line)[past-start:]) {
		return false
	}
	indent := l.textColumn(startOf(typed)) - l.quoteContentColumn(startOf(typed), depth)
	return l.textColumn(past)-l.quoteContentColumn(start, depth) <= indent
}

// blanksThroughOuterQuotes is how many blank lines come right before position's line in depth
// quotes: those blank in them, and the blank lines of the quotes around them, with fewer markers,
// that follow at least one of those.
func (l sourceLines) blanksThroughOuterQuotes(position, depth int) int {
	line := l.lineOf(position) - 1
	outer := 0
	for ; line >= 0 && l.blank(line, quoteBlankLine) && bytes.Count(l.text(line), []byte(">")) < depth; line-- {
		outer++
	}
	inner := 0
	for blank := quoteBlankLineAt(depth); line >= 0 && l.blank(line, blank); line-- {
		inner++
	}
	if inner == 0 {
		return 0
	}
	return inner + outer
}

// asHeldBefore is l reading indented code's held lines as blank, as it read them before the code
// held them (blanksOutsideCode).
func (l sourceLines) asHeldBefore() sourceLines {
	l.held = nil
	return l
}

// blank reports whether line is blank as blank reads it and not a line indented code holds.
func (l sourceLines) blank(line int, blank func([]byte) bool) bool {
	return !l.held[line] && blank(l.text(line))
}

// blankInside reports whether a line between typed block directive's opening and closing fences
// is blank, outside the code it holds; one that runs to its parent's end runs to the next block. A
// blank line of code is the code's text, as are the blank lines a fenced code block no fence closed
// takes up to the next block, which space nothing (keepsBlankLinesAfter); indented code's held
// lines and a last line lastLineColumns kept are read as the blank lines they were before the code
// held them (asHeldBefore, blanksOutsideCode).
func (l sourceLines) blankInside(directive *typedDirective) bool {
	l = l.asHeldBefore()
	// In a quote, a line blank but for the quote's markers is blank, and a line without them ends
	// the quote and the typed block with it.
	blank := whitespaceLine
	if _, quoted := ancestor[*ast.Blockquote](directive); quoted {
		blank = quoteBlankLine
	}
	if !directive.Closed {
		return len(l.blanksOutsideCode(directive, l.blankLines(directive, nextBlock(directive), blank))) > 0
	}
	return len(l.blanksOutsideCode(directive, l.blanks(l.lineOf(directive.Pos())+1, l.lineOf(directive.closer), blank))) > 0
}

// typedEndBlankSpreadsItem reports whether the blank lines that end typed block directive, which
// stands in a list item, are blank lines of that item to the browser editor's parser, which
// spreads the item by them (typedEndBlankInItem), where goldmark reads them as no block's.
//
// That parser counts a blank line toward a list item's spread where the line stands in the item
// itself: where no list or quote the item holds is open across it, and where it is not one of the
// line endings that end the item, which the item drops. A typed block is no such container to it -
// its content is a document of its own, spliced back into the lines around it - so the blank lines
// that end one, above its closing fence, are the item's and spread it, however deep in typed
// blocks they stand, while goldmark reads them as no block's and leaves the item to its looseness,
// which reads it tight.
//
// Three shapes keep those lines from the item, each measured at the fork point and at this head
// (PR #2507):
//
//   - A line the containers around the block do not read as blank is no line of the block's end: in
//     a quote, one without the quote's markers, which ends the quote and the block with it, and
//     anywhere, one indented code holds (codeColumns), which is that code's text.
//   - A block ending in a fenced code block no fence closed takes them as that code's lines
//     (keepsBlankLinesAfter).
//   - In a quote, a list the block ends with takes them: that parser ends a list inside a typed
//     block where the block's content starts on the line that ends it, past the markers of the
//     quotes around the block, so a blank line of the quote falls inside the list. Outside a quote
//     there are no markers to step over, the list ends before the line, and the line is the item's.
//
// A block no fence closes that runs to the end of the document needs two such lines rather than
// one in a quote: that parser ends the block before the last of them, which holds nothing past the
// quotes' markers, so that line ends the list item and the item drops it. Where a block in the
// document follows such a block instead, the blank lines before that block end the list item, as
// this reader reads them too.
func (l sourceLines) typedEndBlankSpreadsItem(directive *typedDirective) bool {
	end, closed := len(l.starts), false
	if ending, ok := fenceEnding(directive); ok {
		end, closed = l.lineOf(ending.closer), true
	} else if nextBlock(directive) != nil {
		return false
	}
	depth := quoteDepth(directive)
	blank := whitespaceLine
	if depth > 0 {
		blank = quoteBlankLineAt(depth)
	}
	first := end
	for first-1 > l.lineOf(startOf(directive)) && l.blank(first-1, blank) {
		first--
	}
	if first == end || typedEndKeepsBlankLines(directive, depth) {
		return false
	}
	need := 1
	if depth > 0 && !closed {
		need = 2
	}
	return end-first >= need
}

// typedEndKeepsBlankLines reports whether a block directive ends with takes the blank lines that
// end directive as its own, so that they are no lines of the list item around it: a fenced code
// block no fence closed, wherever it stands, and, in depth quotes, a list, which that parser ends
// past those quotes' markers on the line after it (typedEndBlankSpreadsItem). A typed block no
// fence closes ends with the block around it, so the list it ends with is the outer block's too.
func typedEndKeepsBlankLines(directive *typedDirective, depth int) bool {
	last := directive.LastChild()
	if last == nil {
		return false
	}
	if keepsBlankLinesAfter(last) {
		return true
	}
	if depth == 0 {
		return false
	}
	for {
		inner, ok := last.(*typedDirective)
		if !ok || inner.Closed || inner.LastChild() == nil {
			break
		}
		last = inner.LastChild()
	}
	_, list := last.(*ast.List)
	return list
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

// outerBlankLineAt reports whether a line stands blank outside depth quotes: whitespace alone, or a
// blank line of a quote around them, with fewer markers.
func outerBlankLineAt(depth int) func([]byte) bool {
	return func(line []byte) bool {
		return whitespaceLine(line) || quoteBlankLine(line) && bytes.Count(line, []byte(">")) < depth
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
