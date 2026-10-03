package pmdoc

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// segmentsText is the text segments cover in source. Goldmark's own Segments.Value appends a
// forced line feed into the buffer it reads, past the segment's end; this copies instead.
func segmentsText(segments *gmtext.Segments, source []byte) string {
	var text strings.Builder
	for index := 0; index < segments.Len(); index++ {
		segment := segments.At(index)
		text.WriteString(strings.Repeat(" ", segment.Padding))
		value := source[segment.Start:segment.Stop]
		text.Write(value)
		if segment.ForceNewline && (len(value) == 0 || value[len(value)-1] != '\n') {
			text.WriteByte('\n')
		}
	}
	return text.String()
}

// parse parses source. unclosedFrontmatter says source opens a document with a front-matter opener
// no later line closes, after which no container opens at the document's level
// (frontmatterAttempt). Its tables' short rows are padded, and its elements counted, on budget;
// firstLine is the number source's first line has in what the caller wrote, which a refusal of
// those elements names its line from.
func (reader markdownReader) parse(source []byte, unclosedFrontmatter bool, budget *TablePaddingBudget, firstLine int) (ast.Node, error) {
	root, count, pc := reader.read(source, unclosedFrontmatter, budget)
	if refusal := count.refusal(root, source, firstLine); refusal != nil {
		return nil, refusal
	}
	if err, _ := pc.Get(tablePaddingErrorKey).(error); err != nil {
		return nil, err
	}
	return root, nil
}

// read is goldmark's tree of source, its tables padded and its elements counted on budget, with the
// count and the parser context it kept, before anything refuses it.
func (reader markdownReader) read(source []byte, unclosedFrontmatter bool, budget *TablePaddingBudget) (ast.Node, *parseCount, parser.Context) {
	pc := parser.NewContext()
	if unclosedFrontmatter {
		pc.Set(unclosedFrontmatterKey, true)
	}
	pc.Set(tablePaddingBudgetKey, budget)
	count := budget.elements.countParse(pc)
	return withLineStarts(reader.md.Parser(), source, pc), count, pc
}

// unclosedFrontmatterKey marks the parse of a document that opens with a front-matter opener no
// later line closes.
var unclosedFrontmatterKey = parser.NewContextKey()

// frontmatterAttempt is a container's parser - a list's, a quote's or a footnote definition's -
// that opens nothing at the document's level in a document opening with a front-matter opener no
// later line closes (unclosedFrontmatterKey). The browser editor's parser tries front matter from
// that opener to the document's end, and front matter is a construct no container opens in, so on
// none of those lines does a container open at the document's level. With no line closing it, that
// parser reads the lines again as the blocks they form without those containers: `---\n- a\n- b`
// is a rule and one paragraph holding both lines, and `---\n> a` a rule and the paragraph `> a`.
// Inside a typed block they open as anywhere else.
type frontmatterAttempt struct{ parser.BlockParser }

func (p frontmatterAttempt) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	if parent.Kind() == ast.KindDocument && pc.Get(unclosedFrontmatterKey) != nil {
		return nil, parser.NoChildren
	}
	return p.BlockParser.Open(parent, reader, pc)
}

// bareMarkerLine is a line holding only a list marker.
var bareMarkerLine = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])[ \t]*$`)

// emptyItemGuard is a list parser that opens no list whose first item could not interrupt a
// paragraph - an empty item, a marker with nothing after it on its line, or an ordered item
// numbered anything but a lone `1` (orderedCannotInterrupt) - where it would, as the browser
// editor's parser reads it. Goldmark refuses one only while the paragraph is the block last opened,
// and there lets an item numbered `01.` interrupt it; that parser also refuses one opening a
// container on a line that already interrupted the paragraph, so `- a\n  - -` is an item holding
// the text `-`, `- a\n  > -` a quote holding it, and `a\n> 2. b` a quote holding the paragraph
// `2. b`, and refuses one after indented code (interruptsOpenBlock), so `    code\n> 2. b` is a
// quote holding that paragraph too.
type emptyItemGuard struct{ parser.BlockParser }

func (p emptyItemGuard) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, segment := reader.PeekLine()
	if !bareMarkerLine.Match(bytes.TrimRight(line, "\n")) && !orderedCannotInterrupt(line) {
		return p.BlockParser.Open(parent, reader, pc)
	}
	if last, paragraph := pc.LastOpenedBlock().Node.(*ast.Paragraph); paragraph && last.Parent() == parent {
		return nil, parser.NoChildren
	}
	if interruptsOpenBlock(parent, reader.Source(), lineStart(reader.Source(), segment.Start)) {
		return nil, parser.NoChildren
	}
	return p.BlockParser.Open(parent, reader, pc)
}

// Continue keeps the list open for a line holding its empty last item's content, as the browser
// editor's parser reads it: an item whose marker line holds nothing takes its content from the
// next line when that line, with no blank line between, reaches the item's content column, a list
// marker there included. Goldmark closes the list when that marker cannot continue it - another
// bullet character, or an ordered marker under a bullet - so `-\n  1.` read as two lists and
// `> 1.\n>    2) a` as two quoted lists.
func (p emptyItemGuard) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	state := p.BlockParser.Continue(node, reader, pc)
	item, ok := node.LastChild().(*ast.ListItem)
	if state != parser.Close || !ok || item.ChildCount() != 0 {
		return state
	}
	line, segment := reader.PeekLine()
	source := reader.Source()
	start := lineStart(source, segment.Start)
	if util.IsBlank(line) || start == 0 || markersOrWhitespace(source[lineStart(source, start-1):start]) {
		return state
	}
	if indent, _ := util.IndentWidth(line, reader.LineOffset()); indent < item.Offset {
		return state
	}
	return parser.Continue | parser.HasChildren
}

// orderedMarkerStart is an ordered list marker opening a line, its number captured.
var orderedMarkerStart = regexp.MustCompile(`^ {0,3}([0-9]{1,9})[.)](?:[ \t]|\n|$)`)

// orderedCannotInterrupt reports whether line opens an ordered list item the browser editor's
// parser lets interrupt no paragraph: one numbered anything but a lone `1`, so `01.`, `001)` and
// `10.` as well as `2.`, where goldmark takes the number's value and lets `01.` interrupt one.
func orderedCannotInterrupt(line []byte) bool {
	match := orderedMarkerStart.FindSubmatch(line)
	return match != nil && string(match[1]) != "1"
}

// tabIndented is a block parser reading a line whose indentation holds a tab as CommonMark and the
// browser editor's parser do, when what follows the indentation matches opens: the tab spans the
// columns to the next multiple of four, so after a quote's `> ` it spans two. Goldmark measures such
// a line's indentation as if it began the line, or takes a list marker or a setext underline only
// after spaces, and read `> \t- a` and `> a\n> \t===` as paragraph text; and it counts a fence's
// indentation in bytes, a tab as one, so it kept a column of each code line's indentation that the
// fence's own removes (`> \t```` over `> \tc` read ` c`). Before the parser looks at such a line,
// its indentation is taken as the columns it spans, as padding. With no opens, the parser reads
// the line as goldmark's does, and only the block's position is corrected (Open).
type tabIndented struct {
	parser.BlockParser
	opens *regexp.Regexp
}

// Open also leaves goldmark the block's position where its text starts: goldmark places an opened
// block at the line's start plus the block offset, which counts the columns a tab spans as bytes,
// so a block after a tab stood a character or two past its text, or on the next line.
func (p tabIndented) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, start := reader.PeekLine()
	if p.opens != nil {
		expandTabIndentation(reader, p.opens)
	}
	line, segment := reader.PeekLine()
	text := util.FirstNonSpacePosition(line)
	if text >= 0 {
		pc.SetBlockOffset(text)
	}
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil && text >= segment.Padding {
		pc.SetBlockOffset(segment.Start + text - segment.Padding - start.Start)
	}
	return node, state
}

func (p tabIndented) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	if p.opens != nil {
		expandTabIndentation(reader, p.opens)
	}
	return p.BlockParser.Continue(node, reader, pc)
}

// endsContainers is a container's block parser - a list's, a list item's, a quote's or a footnote
// definition's - recording the line its container's lines last ended at (recordEndedContainer),
// by which a paragraph inside tells a lazy continuation line from one that continues it
// (lazilyEnded).
type endsContainers struct{ parser.BlockParser }

func (p endsContainers) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	state := p.BlockParser.Continue(node, reader, pc)
	recordEndedContainer(node, state, reader, pc)
	return state
}

// endedContainerKey holds the list, list item, quote or footnote definition whose lines last ended
// at a line that is not blank, and that line: goldmark reads such a line as continuing a paragraph
// the container holds (lazyTypedParagraph) where it opens no block.
var endedContainerKey = parser.NewContextKey()

type endedContainer struct {
	node ast.Node
	line int
}

func recordEndedContainer(node ast.Node, state parser.State, reader gmtext.Reader, pc parser.Context) {
	if line, _ := reader.PeekLine(); state&parser.Close == 0 || util.IsBlank(line) {
		return
	}
	row, _ := reader.Position()
	pc.Set(endedContainerKey, endedContainer{node: node, line: row})
}

// lazyTypedParagraphAttr marks a typed block holding a paragraph goldmark continued with a line from
// outside a container around the typed block. The browser editor's parser continues no paragraph
// in a typed block so; the line ends the typed block there (parseTypedDirective refuses it).
var lazyTypedParagraphAttr = []byte("pmdoc-lazy-typed-paragraph")

// lazilyEnded is the container around paragraph whose lines ended at the line goldmark is
// continuing paragraph with (endedContainerKey): goldmark's lazy continuation line, which continues
// the paragraph and no container that ended there. It is nil where the line continues them all.
func lazilyEnded(paragraph ast.Node, reader gmtext.Reader, pc parser.Context) ast.Node {
	ended, ok := pc.Get(endedContainerKey).(endedContainer)
	if row, _ := reader.Position(); !ok || ended.line != row || !isAncestor(ended.node, paragraph) {
		return nil
	}
	return ended.node
}

// lazyTypedParagraph marks the typed block between paragraph and ended, the container a lazy
// continuation line ended (lazilyEnded), if one stands there (lazyTypedParagraphAttr).
func lazyTypedParagraph(paragraph, ended ast.Node) {
	if ended == nil {
		return
	}
	for parent := paragraph.Parent(); parent != ended; parent = parent.Parent() {
		if typed, ok := parent.(*typedDirective); ok {
			typed.SetAttribute(lazyTypedParagraphAttr, true)
			return
		}
	}
}

// lazyLinesAttr holds, on a paragraph, where each of its lazy continuation lines starts (lazilyEnded),
// so a table made of the paragraph can tell which of its rows are such lines (lazyTableRows).
var lazyLinesAttr = []byte("pmdoc-lazy-lines")

// listItemColumns is goldmark's list item parser measuring the spaces and tabs after an item's
// marker from the column they stand at. Goldmark measures them from their place in what is left of
// the line, as if it began where the containers around the item leave off, so inside a quote or a
// list item whose text does not start at a multiple of four columns a tab there spans the wrong
// columns: `> -\t\ta` read `  a` as its code, where the browser editor's parser reads `a`.
type listItemColumns struct{ parser.BlockParser }

// listMarkerOpening is a list item's marker opening what is left of a line.
var listMarkerOpening = regexp.MustCompile(`^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])`)

func (p listItemColumns) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	// The line starts with a partly taken tab's padding as spaces, at the reader's offset.
	line, _ := reader.PeekLine()
	column := reader.LineOffset()
	marker := listMarkerOpening.FindIndex(line)
	if marker == nil || column%4 == 0 {
		return p.BlockParser.Open(parent, reader, pc)
	}
	after := line[marker[1]:]
	if indent := bytes.IndexFunc(after, func(char rune) bool { return char != ' ' && char != '\t' }); bytes.IndexByte(after[:max(indent, 0)], '\t') < 0 {
		return p.BlockParser.Open(parent, reader, pc)
	}
	row, position := reader.Position()
	node, state := p.BlockParser.Open(parent, reader, pc)
	item, ok := node.(*ast.ListItem)
	if !ok || state != parser.HasChildren {
		return node, state
	}
	// Goldmark's placement of the item's text, with the tabs' stops where they are.
	reader.SetPosition(row, position)
	from := column + marker[1]
	offset, _ := util.IndentWidth(after, from)
	if offset > 4 {
		offset = 1
	}
	advance, padding := util.IndentPosition(after, from, offset)
	item.Offset = marker[1] + offset
	reader.AdvanceAndSetPadding(marker[1]+advance, padding)
	return node, state
}

// Continue takes no more than the item's own columns from a blank line, as the browser editor's
// parser does, and leaves the rest of the line to the blocks inside the item. Goldmark takes the
// whole line, so a fenced or indented code block in a list item lost what a blank line held past
// the item's indentation: `- a\n\n  ```\n  c\n    \n  ```` holds the code `c\n  `.
func (p listItemColumns) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	line, _ := reader.PeekLine()
	if !util.IsBlank(line) {
		return p.BlockParser.Continue(node, reader, pc)
	}
	if position, padding := util.IndentPosition(line, reader.LineOffset(), node.(*ast.ListItem).Offset); position >= 0 {
		reader.AdvanceAndSetPadding(position, padding)
	} else {
		reader.AdvanceToEOL()
	}
	return parser.Continue | parser.HasChildren
}

// fenceClosure is goldmark's fenced code parser recording on a block whether a closing fence closed
// it (fenceClosedAttr), which the browser editor's parser reads its trailing blank line by
// (fencedCodeText).
type fenceClosure struct{ parser.BlockParser }

var fenceClosedAttr = []byte("pmdoc-fence-closed")

func (p fenceClosure) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	state := p.BlockParser.Continue(node, reader, pc)
	if state == parser.Close {
		node.SetAttribute(fenceClosedAttr, true)
	}
	return state
}

// codeColumns is goldmark's indented code parser measuring a blank line's indentation from the
// column it stands at, as it measures a line of code's: four columns are the code's indentation,
// and what the line holds past them, a tab split there written as the columns it has left, is the
// code's text. Goldmark counts every tab on a blank line as four columns wherever it stands, so in
// a list item, where the line starts past the item's columns, a tab short of the code's indentation
// left a space in the code (`- x\n  \n      a\n   \t\n      b` read `a\n \nb`).
type codeColumns struct{ parser.BlockParser }

func (p codeColumns) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	line, segment := reader.PeekLine()
	if !util.IsBlank(line) {
		return p.BlockParser.Continue(node, reader, pc)
	}
	if position, padding := util.IndentPosition(line, reader.LineOffset(), 4); position >= 0 {
		reader.AdvanceAndSetPadding(position, padding)
		_, segment = reader.PeekLine()
	} else {
		// Nothing past the code's indentation: the line is empty but for its line feed.
		start := segment.Stop
		if start > segment.Start && reader.Source()[start-1] == '\n' {
			start--
		}
		segment = gmtext.NewSegment(start, segment.Stop)
	}
	node.Lines().Append(segment)
	reader.AdvanceToEOL()
	return parser.Continue | parser.NoChildren
}

// setPadding sets reader's padding and drops the line it has peeked, which goldmark's
// SetPadding keeps: a zero advance drops it.
func setPadding(reader gmtext.Reader, padding int) {
	reader.SetPadding(padding)
	reader.Advance(0)
}

var (
	// fenceStart is a code fence's opening run of backticks or tildes.
	fenceStart = regexp.MustCompile("^(?:```|~~~)")
	// listMarkerStart is a list marker opening a line's text: a bullet, or an ordered item's number
	// and delimiter, followed by a space, a tab or the line's end.
	listMarkerStart = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])(?:[ \t]|\n|$)`)
	// listMarker is a list item's marker alone: a bullet, or an ordered item's number and
	// delimiter.
	listMarker = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])`)
	// setextUnderline is a setext heading's underline.
	setextUnderline = regexp.MustCompile(`^(?:=+|-+)[ \t]*(?:\n|$)`)
)

// expandTabIndentation takes the indentation of reader's line as the columns it spans, as padding,
// when it holds a tab, spans fewer than four columns, and what follows it matches opens; the line
// reads the same, with spaces where a tab stood.
func expandTabIndentation(reader gmtext.Reader, opens *regexp.Regexp) {
	line, _ := reader.PeekLine()
	end := 0
	for end < len(line) && (line[end] == ' ' || line[end] == '\t') {
		end++
	}
	if bytes.IndexByte(line[:end], '\t') < 0 || !opens.Match(line[end:]) {
		return
	}
	if width, _ := util.IndentWidth(line, reader.LineOffset()); width < 4 {
		reader.AdvanceAndSetPadding(end, width)
	}
}

// tabExpandedLines is lines with each one's indentation, where it holds a tab, taken as the columns
// it spans on its source line, as padding: goldmark's table transformer measures a row's indentation
// as if the row began its line, so `> \t| a |` read as paragraph text.
func tabExpandedLines(lines *gmtext.Segments, source []byte) *gmtext.Segments {
	expanded := gmtext.NewSegments()
	for index := range lines.Len() {
		expanded.Append(tabExpandedLine(lines.At(index), source))
	}
	return expanded
}

// tabExpandedLine is one line of tabExpandedLines.
func tabExpandedLine(line gmtext.Segment, source []byte) gmtext.Segment {
	end := line.Start
	for end < line.Stop && (source[end] == ' ' || source[end] == '\t') {
		end++
	}
	if line.Padding == 0 && bytes.IndexByte(source[line.Start:end], '\t') >= 0 {
		from := lineStart(source, line.Start)
		width, _ := util.IndentWidth(source[line.Start:end], columnOf(source[from:line.Start]))
		line = gmtext.Segment{Start: end, Stop: line.Stop, Padding: width, ForceNewline: line.ForceNewline}
	}
	return line
}

// columnOf is the column text, the start of a line, ends at, a tab advancing to the next multiple
// of four.
func columnOf(text []byte) int {
	column := 0
	for _, char := range text {
		if char == '\t' {
			column += 4 - column%4
		} else {
			column++
		}
	}
	return column
}

// interruptsOpenBlock reports whether a list opening in parent on the line starting at start
// interrupts a block the browser editor's parser holds open there, other than a paragraph parent
// holds, which goldmark names: a paragraph ending on the line before, reached across the containers
// the line opens first, that forms no table, or an indented code block with only blank lines after
// it. That parser decides once per line whether it interrupts, from the construct it holds open,
// before opening the containers the line starts with, and keeps indented code open to the line -
// except code right after a list, or right after a quote with no blank line between them. The list
// stays open across the blank lines before the code, and the quote across the code's first line;
// neither continues the line after it, and that parser ends indented code at a line no open
// container continues, so the code has ended and the line interrupts nothing (which is also why it
// reads a second code line there as a second code block). A container opened on a line after the
// code, such as a typed block's fence, starts a flow of its own that holds nothing open.
func interruptsOpenBlock(parent ast.Node, source []byte, start int) bool {
	previous := parent.LastChild()
	opensContainers := previous == nil
	for node := parent; previous == nil && node != nil && node.Kind() != ast.KindDocument; node = node.Parent() {
		previous = node.PreviousSibling()
	}
	switch previous := previous.(type) {
	case *ast.Paragraph:
		// A table goldmark makes of the paragraph is no paragraph to that parser, and holds
		// nothing open a line could interrupt.
		if !opensContainers || previous.Lines().Len() == 0 {
			return false
		}
		if table, _ := formsTable(previous, source); table {
			return false
		}
		last := previous.Lines().At(previous.Lines().Len() - 1)
		return bytes.Count(source[last.Start:start], []byte("\n")) <= 1
	case *ast.CodeBlock:
		switch previous.PreviousSibling().(type) {
		case *ast.List:
			return false
		case *ast.Blockquote:
			if !previous.HasBlankPreviousLines() {
				return false
			}
		}
		if previous.Lines().Len() == 0 {
			return false
		}
		last := previous.Lines().At(previous.Lines().Len() - 1)
		between := source[last.Start:start]
		between = between[bytes.IndexByte(between, '\n')+1:]
		for _, line := range bytes.Split(bytes.TrimSuffix(between, []byte("\n")), []byte("\n")) {
			if len(line) > 0 && !markersOrWhitespace(line) {
				return false
			}
		}
		return true
	}
	return false
}

// taskList reads a task list item's marker as the browser editor's parser does: `[ ]`, `[x]` or
// `[X]` opening a list item's first paragraph, followed by a space or a tab and then more text on
// the line, or by a line ending, after any spaces and tabs, that the paragraph continues past. The
// marker takes the one space or tab after it, or the line ending when the line holds nothing more
// but a hard break's spaces, which stay a hard break. Goldmark's reads a marker with anything after it, and takes every space
// after it, so it read a link opening a list item (`- [x](https://…)`) as a checked task.
type taskList struct{}

func (taskList) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(taskMarkerParser{}, 0)))
}

type taskMarkerParser struct{}

func (taskMarkerParser) Trigger() []byte { return []byte{'['} }

func (taskMarkerParser) Parse(parent ast.Node, block gmtext.Reader, _ parser.Context) ast.Node {
	if _, ok := parent.Parent().(*ast.ListItem); !ok || parent.Parent().FirstChild() != parent || parent.HasChildren() {
		return nil
	}
	line, _ := block.PeekLine()
	if len(line) < 4 || !strings.ContainsRune(" \txX", rune(line[1])) || line[2] != ']' {
		return nil
	}
	rest := line[3:]
	blank := len(rest) - len(bytes.TrimLeft(rest, " \t"))
	width := 4
	switch {
	case blank > 0 && !util.IsBlank(rest):
	case rest[blank] == '\n':
		row, segment := block.Position()
		block.AdvanceLine()
		next, _ := block.PeekLine()
		block.SetPosition(row, segment)
		if next == nil {
			return nil
		}
		width = len(line)
		if spaces := len(rest) - len(bytes.TrimLeft(rest, " ")); spaces >= 2 {
			width = 3
		}
	default:
		return nil
	}
	block.Advance(width)
	return extensionast.NewTaskCheckBox(line[1] == 'x' || line[1] == 'X')
}

// lineRecordingParagraph is goldmark's paragraph parser, recording each line it takes as the
// containers left it, before the paragraph trims its leading whitespace, keyed by where the
// trimmed line starts. The browser editor's parser keeps that whitespace in a code span
// (multilineCodeSpanText). Recording as the lines are taken keeps them whatever the paragraph
// becomes: a tight list item's text block, or a setext heading, whose paragraph is trimmed before
// any paragraph transformer sees it.
type lineRecordingParagraph struct{ parser.BlockParser }

// untrimmedLinesKey holds the recorded lines while a document parses, and untrimmedLinesAttr on
// the parsed document root afterwards (withLineStarts).
var (
	untrimmedLinesKey  = parser.NewContextKey()
	untrimmedLinesAttr = []byte("pmdoc-untrimmed-lines")
)

func (p lineRecordingParagraph) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	_, segment := reader.PeekLine()
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil {
		recordLine(segment, reader.Source(), pc)
	}
	return node, state
}

func (p lineRecordingParagraph) Continue(node ast.Node, reader gmtext.Reader, pc parser.Context) parser.State {
	_, segment := reader.PeekLine()
	ended := lazilyEnded(node, reader, pc)
	lazyTypedParagraph(node, ended)
	state := p.BlockParser.Continue(node, reader, pc)
	if state != parser.Close {
		recordLine(segment, reader.Source(), pc)
		if ended != nil {
			lazy, _ := node.Attribute(lazyLinesAttr)
			starts, _ := lazy.([]int)
			node.SetAttribute(lazyLinesAttr, append(starts, lineStart(reader.Source(), segment.Start)))
		}
	}
	return state
}

func recordLine(line gmtext.Segment, source []byte, pc parser.Context) {
	recorded, _ := pc.Get(untrimmedLinesKey).(map[int]gmtext.Segment)
	if recorded == nil {
		recorded = make(map[int]gmtext.Segment)
		pc.Set(untrimmedLinesKey, recorded)
	}
	recorded[line.TrimLeftSpace(source).Start] = line
}

// withLineStarts parses source with context, with each footnote definition where it is written
// (definitionsInPlace), and leaves the lines lineRecordingParagraph recorded on the document root.
func withLineStarts(p parser.Parser, source []byte, context parser.Context) ast.Node {
	root := p.Parse(gmtext.NewReader(source), parser.WithContext(context))
	definitionsInPlace(root, context)
	if recorded := context.Get(untrimmedLinesKey); recorded != nil {
		root.SetAttribute(untrimmedLinesAttr, recorded)
	}
	return root
}

// multilineCodeSpanText is the text of a code span that runs over more than one line, as the
// browser editor's parser reads it: each later line from just after the containers' prefix, where
// goldmark's paragraph trimmed the whitespace before it - a line holding only whitespace before
// the closer included, for which goldmark keeps no text at all - and then one space or line ending
// taken from each end when both ends hold one and something else stands between, after the whole
// span is put together. The columns left of a tab a container's marker took part of are written as
// spaces, but that parser reads them as text, not spaces: they stand between the ends, and where
// they end the span nothing is taken off. ok is false for a span on one line, whose goldmark
// reading stands.
func multilineCodeSpanText(span *ast.CodeSpan, source []byte) (text string, ok bool) {
	first, _ := span.FirstChild().(*ast.Text)
	last, _ := span.LastChild().(*ast.Text)
	if first == nil || last == nil {
		return "", false
	}
	// Goldmark takes one character off each end when both are whitespace; put them back.
	trimmed := first.Segment.Start > 0 && source[first.Segment.Start-1] != '`'
	stop := last.Segment.Stop
	if trimmed {
		stop++
	}
	closerAlone := stop > last.Segment.Start && source[stop-1] == '\n'
	if first == last && !closerAlone {
		return "", false
	}
	var content strings.Builder
	split, endsSplit := false, false
	indent := func(start int) {
		columns, whitespace := untrimmedIndent(span, start, source)
		content.WriteString(strings.Repeat(" ", columns) + whitespace)
		split = split || columns > 0
		endsSplit = columns > 0 && whitespace == ""
	}
	for child := span.FirstChild(); child != nil; child = child.NextSibling() {
		line := child.(*ast.Text).Segment
		from, to := line.Start, line.Stop
		if child == span.FirstChild() && trimmed {
			from--
		} else if child != span.FirstChild() {
			indent(line.Start)
		}
		if child == span.LastChild() && trimmed {
			to++
		}
		content.Write(source[from:to])
		endsSplit = endsSplit && from == to
	}
	if closerAlone {
		endsSplit = false
		if next := bytes.IndexByte(source[stop:], '`'); next >= 0 {
			indent(stop + next)
		}
	}
	text = content.String()
	if !endsSplit && (codeSpanPadded(text) || split && len(text) >= 2 && isCodePadding(text[0]) && isCodePadding(text[len(text)-1])) {
		text = text[1 : len(text)-1]
	}
	return text, true
}

// codeSpanPadded reports whether a code span's text is one the parser takes a character off each
// end of: both ends a space or a line feed, and something else between.
func codeSpanPadded(text string) bool {
	return len(text) >= 2 && isCodePadding(text[0]) && isCodePadding(text[len(text)-1]) && strings.Trim(text, " \n") != ""
}

// isCodePadding reports whether char is a space or a line feed, what a code span sheds at each end.
func isCodePadding(char byte) bool { return char == ' ' || char == '\n' }

// untrimmedIndent is the whitespace goldmark's paragraph trimmed from the line whose text now
// starts at start: what lies between the containers' prefix and it (lineRecordingParagraph), as
// the columns left of a tab a container's marker took part of, and the whitespace after them.
func untrimmedIndent(node ast.Node, start int, source []byte) (columns int, whitespace string) {
	root := node
	for root.Parent() != nil {
		root = root.Parent()
	}
	attribute, _ := root.Attribute(untrimmedLinesAttr)
	recorded, _ := attribute.(map[int]gmtext.Segment)
	line, ok := recorded[start]
	if !ok {
		return 0, ""
	}
	return line.Padding, string(source[line.Start:start])
}

// lineStart is where the line holding position begins in source.
func lineStart(source []byte, position int) int {
	for position > 0 && source[position-1] != '\n' {
		position--
	}
	return position
}

// insideImage reports whether node is part of an image's alt text.
func insideImage(node ast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, ok := parent.(*ast.Image); ok {
			return true
		}
	}
	return false
}

// parseFrontmatterBlock reads the front matter a document opens with in source, as the
// browser editor's parser does: a first line that is `---` and any spaces or tabs opens it, and
// the first later line that is the same closes it. The node's text is the lines between, joined
// with line feeds between plain `---` fences, as that parser stores it. rest is where the
// document after the closing line begins; a document with no closed front matter has none, and
// rest is 0. unclosed reports an opener nothing closes, which that parser tries as front matter to
// the document's end (blockReader.parse).
func parseFrontmatterBlock(source []byte) (front *Node, rest int, unclosed bool) {
	var content []string
	for start, index := 0, 0; start < len(source); index++ {
		end := len(source)
		if next := bytes.IndexByte(source[start:], '\n'); next >= 0 {
			end = start + next + 1
		}
		line := strings.TrimRight(string(bytes.TrimSuffix(source[start:end], []byte("\n"))), " \t")
		switch {
		case index == 0 && line != "---":
			return nil, 0, false
		case index > 0 && line == "---":
			text := "---\n---"
			if joined := strings.Join(content, "\n"); joined != "" {
				text = "---\n" + joined + "\n---"
			}
			return &Node{Type: "frontmatter", Children: []*Node{{Type: "text", Text: text}}}, end, false
		case index > 0:
			content = append(content, string(bytes.TrimSuffix(source[start:end], []byte("\n"))))
		}
		start = end
	}
	return nil, 0, len(source) > 0
}
