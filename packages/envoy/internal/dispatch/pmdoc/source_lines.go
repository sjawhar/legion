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

// sourceLines is where each line of a source starts, which of its lines indented code holds past
// where goldmark ends the code (held): those are the code's, not blank, wherever a blank line is
// counted (heldCodeLines).
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

// blanksOutsideCode is blanks less the lines a code block inside within holds: a blank line of code
// is the code's text, which spaces nothing. The columns of a last line lastLineColumns kept are no
// code's, which a document the reader before PR #2481 misread turns on: its last whitespace-only
// line a typed block's indentation now takes out of its fenced code
// (typedDirectiveParser.Continue). Indented code's held lines (codeHeldAttr) need no mention here,
// since sourceLines.blank passes over them for every reader of the source.
func (l sourceLines) blanksOutsideCode(within ast.Node, blanks []int) []int {
	code := map[int]bool{}
	_ = ast.Walk(within, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			segments := node.Lines()
			for index := range segments.Len() {
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
// ending alone, so the browser editor's parser reads it as no line of typed. A line holding a
// marker and a tab alone is that parser's own case (markerTabEndsLine): it never reads such a line
// as typed's, whatever the columns the tab spans.
func (l sourceLines) holdsOnlyBlockIndent(line, depth int, typed *typedDirective) bool {
	if line < 0 || leadingQuoteMarkers(l.text(line)) != depth {
		return false
	}
	start := l.starts[line]
	past := l.pastQuoteMarkers(start, depth)
	if !whitespaceLine(l.text(line)[past-start:]) {
		return false
	}
	if depth > 0 && l.markerTabEndsLine(start, depth) {
		return false
	}
	indent := l.textColumn(startOf(typed)) - l.quoteContentColumn(startOf(typed), depth)
	return l.textColumn(past)-l.quoteContentColumn(start, depth) <= indent
}

// markerTabEndsLine reports whether position's line holds count quote markers and a tab alone, the
// tab right after the innermost marker and the line ending right after the tab. The quote takes one
// column of such a tab, and the browser editor's parser leaves what is left of it in the line rather
// than as indentation a block inside the quote strips from the lines it continues: measured against
// the headless engine, such a line is a line of a typed block's content at every indentation the
// block can carry, where a line of spaces at or within that indentation is none, and so is a line
// whose tab another character follows.
func (l sourceLines) markerTabEndsLine(position, count int) bool {
	for ; count > 0; count-- {
		for position < len(l.source) && (l.source[position] == ' ' || l.source[position] == '\t') {
			position++
		}
		if position >= len(l.source) || l.source[position] != '>' {
			return false
		}
		position++
		if count == 1 {
			if position >= len(l.source) || l.source[position] != '\t' {
				return false
			}
			position++
			return position >= len(l.source) || l.source[position] == '\n'
		}
		if position < len(l.source) && l.source[position] == ' ' {
			position++
		}
	}
	return false
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

// blank reports whether line is blank as blank reads it and not a line indented code holds.
func (l sourceLines) blank(line int, blank func([]byte) bool) bool {
	return !l.held[line] && blank(l.text(line))
}

// typedBlankSpreadsItem reports whether a blank line inside typed block directive, which stands in
// a list item, is a blank line of that item to the browser editor's parser, which spreads the item
// by it (typedBlankInItem), where goldmark reads it as no block's.
//
// That parser counts a blank line toward a list item's spread where the line stands in the item
// itself: where no list or quote the item holds is open across it. A typed block is no such
// container to it - its content is a document of its own, spliced back into the lines around it -
// so every blank line inside one, wherever it stands in the block and however deep in typed blocks
// the block stands, is the item's and spreads it, while goldmark reads it as no block's and leaves
// the item to its looseness, which reads it tight.
//
// The block ends where typedBlockEnd ends it: at a closing fence - its own, or that of a typed
// block around it, since that parser ends a typed block no fence closes with the block around it -
// or at the next block, or at the end of the source.
//
// Three shapes keep a line from the item (typedKeptLines):
//
//   - A line the containers around the block do not read as blank is no blank line of the block:
//     in a quote, one without the quote's markers, which ends the quote and the block with it,
//     and anywhere, one a code block holds (blanksOutsideCode), or one indented code holds past
//     where goldmark ends it (codeColumns), which is that code's text.
//   - A block inside the block takes it: a fenced code block no fence closed the block ends with,
//     which that parser reads the lines after as that code's, a list or a quote open across the
//     line, and, in a quote, a list the line follows, which that parser ends past the quotes'
//     markers on the line after it.
//   - The line endings that end the list item, which the item drops: where no fence closes the
//     block, the last of the blank lines ending it is the item's ending rather than a blank line
//     of the item, except at the end of a quote-less document and before the list's next item,
//     where that parser ends the item past them (typedBlankEndNeeds).
func (l sourceLines) typedBlankSpreadsItem(directive *typedDirective) bool {
	next := nextBlock(directive)
	end, byFence := l.typedBlockEnd(directive, next)
	depth := quoteDepth(directive)
	blank := blankLineAt(depth)
	first := l.lineOf(startOf(directive)) + 1
	blanks := l.blanksOutsideCode(directive, l.blanks(first, end, blank))
	if len(blanks) == 0 {
		return false
	}
	kept := l.typedKeptLines(directive, end)
	ending := 0
	for index := len(blanks) - 1; index >= 0 && blanks[index] == end-1-ending; index-- {
		ending++
	}
	for _, line := range blanks[:len(blanks)-ending] {
		if !kept[line] {
			return true
		}
	}
	if ending == 0 || kept[end-1] {
		return false
	}
	// A whitespace-only line wider than the block's own indentation holds content for the block to
	// that parser, which a paragraph after the block continues lazily, so the item's lines carry no
	// blank line there. A container opening after such a line, and the end of the source, leave it
	// the item's (typedBlankEndNeeds). Where the block's content ends in a list, such a line is
	// the item's all the same when its whitespace ends in a tab: measured against the headless
	// engine, a line of `\t` or ` \t` there spreads the item, while `   ` and `\t ` do not. In a
	// quote every such line is the item's, the quote's own content ending at it.
	if depth == 0 && next != nil && isParagraph(next) && !l.holdsOnlyBlockIndent(end-1, depth, directive) &&
		!(endsInList(directive) && l.whitespaceEndsInTab(end-1)) {
		return false
	}
	return ending >= typedBlankEndNeeds(byFence, depth, next)
}

// endsInList reports whether node's content ends in a list.
func endsInList(node ast.Node) bool {
	_, list := node.LastChild().(*ast.List)
	return list
}

// whitespaceEndsInTab reports whether line's text ends in a tab, its line ending apart.
func (l sourceLines) whitespaceEndsInTab(line int) bool {
	text := bytes.TrimRight(l.text(line), "\n")
	return len(text) > 0 && text[len(text)-1] == '\t'
}

// typedBlankInside reports whether any blank line stands inside typed block directive, between
// its opening line and where its lines end (typedBlockEnd), outside the code it holds. It is
// what a footnote definition's typed block is judged by, where this reader decides no such line
// (holdsTypedBlankInDefinition, typedDefinitionBlankInItem).
func (l sourceLines) typedBlankInside(directive *typedDirective) bool {
	end, _ := l.typedBlockEnd(directive, nextBlock(directive))
	first := l.lineOf(startOf(directive)) + 1
	return len(l.blanksOutsideCode(directive, l.blanks(first, end, blankLineAt(quoteDepth(directive))))) > 0
}

// typedBlankEndNeeds is how many blank lines ending a typed block in a list item the browser
// editor's parser reads as blank lines of that item (typedBlankSpreadsItem): one above a closing
// fence, which is no ending of the item; one where the item ends with the source outside a quote,
// or where the list's next item follows, which that parser reads past the lines; one before a
// quote, a list or a footnote definition, which opens on the line right after them and takes none
// of them as its own; and two otherwise, the last being the item's own ending, which it drops.
func typedBlankEndNeeds(byFence bool, depth int, next ast.Node) int {
	if byFence {
		return 1
	}
	if _, item := next.(*ast.ListItem); item {
		return 1
	}
	if next == nil && depth == 0 {
		return 1
	}
	if next != nil && startsContainer(next) {
		return 1
	}
	return 2
}

// typedBlockEnd is the line typed block directive's lines end before, next being the block that
// follows it: a closing fence, that of directive or of a typed block around it that next stands
// past, whichever comes first, else next's line, else the end of the source. byFence is whether a
// fence decided it.
func (l sourceLines) typedBlockEnd(directive *typedDirective, next ast.Node) (end int, byFence bool) {
	end = len(l.starts)
	if next != nil {
		end = l.lineOf(startOf(next))
	}
	for typed, ok := directive, true; ok; typed, ok = typed.Parent().(*typedDirective) {
		if typed.Closed && (next == nil || !isAncestor(typed, next)) {
			if closer := l.lineOf(typed.closer); closer < end {
				return closer, true
			}
			return end, false
		}
	}
	return end, false
}

// typedKeptLines is the lines up to end that a block inside typed block directive takes as its
// own, so that they are no lines of the list item around it (typedBlankSpreadsItem): a fenced
// code block no fence closed that the block ends with, which that parser reads the lines after
// it as; a list or a quote open across the line; and, in a quote, a list the line follows, which
// that parser ends on the line after it, past the markers of the quotes around the block, so
// that the line falls inside the list.
//
// A quote between such a block and the typed block ends at the line and takes the block with it,
// so the line is the item's after all: the fence is asked of the typed block itself
// (keepsBlankLinesAfter, which refuses a quote between), and a list the line follows is read only
// where no quote stands between the two.
//
// It walks the block's nodes once, rather than once for each blank line inside it, since a block
// holding one list item per line holds as many blank lines as nodes.
func (l sourceLines) typedKeptLines(directive *typedDirective, end int) map[int]bool {
	kept := map[int]bool{}
	mark := func(from, to int) {
		for line := max(from, 0); line < to; line++ {
			kept[line] = true
		}
	}
	if keepsBlankLinesAfter(directive) {
		mark(l.lineOf(startOf(lastFencedCode(directive)))+1, end)
	}
	blank := blankLineAt(quoteDepth(directive))
	quoted := quoteDepth(directive) > 0
	quotes := 0
	_ = ast.Walk(directive, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		_, quote := node.(*ast.Blockquote)
		if !entering {
			if quote {
				quotes--
			}
			return ast.WalkContinue, nil
		}
		list, listed := node.(*ast.List)
		inner := quotes
		if quote {
			quotes++
		}
		if !quote && !listed {
			return ast.WalkContinue, nil
		}
		// Open across every line before the last block it holds: that parser is still inside it.
		mark(l.lineOf(startOf(node))+1, min(l.lastStartOf(node), end))
		if !listed || !quoted || inner > 0 {
			return ast.WalkContinue, nil
		}
		// The blank lines right after a list in a quote are the list's, since that parser ends it
		// on the line after them, past the quotes' markers.
		after := l.lastLineOf(list) + 1
		line := after
		for line < end && l.blank(line, blank) {
			line++
		}
		mark(after, line)
		return ast.WalkContinue, nil
	})
	return kept
}

// lastLineOf is the last line of the source a block container holds stands on, container's own
// first line where it holds none.
func (l sourceLines) lastLineOf(container ast.Node) int {
	last := l.lineOf(startOf(container))
	_ = ast.Walk(container, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && node.Type() == ast.TypeBlock {
			last = max(last, l.lineOf(startOf(node)))
			if lines := node.Lines(); lines.Len() > 0 {
				last = max(last, l.lineOf(lines.At(lines.Len()-1).Start))
			}
		}
		return ast.WalkContinue, nil
	})
	return last
}

// lastStartOf is the line the last block container holds starts on, container's own line where it
// holds none.
func (l sourceLines) lastStartOf(container ast.Node) int {
	last := l.lineOf(startOf(container))
	_ = ast.Walk(container, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && node != container && node.Type() == ast.TypeBlock {
			last = max(last, l.lineOf(startOf(node)))
		}
		return ast.WalkContinue, nil
	})
	return last
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

// blankLineAt is what a blank line is at depth quotes: whitespace alone outside them, and the
// quotes' markers and whitespace inside them (quoteBlankLineAt).
func blankLineAt(depth int) func([]byte) bool {
	if depth == 0 {
		return whitespaceLine
	}
	return quoteBlankLineAt(depth)
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
