package pmdoc

import (
	"bytes"
	"slices"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// lazyAwareTable is goldmark's table extension with its paragraph transformer held off lazy
// continuation lines and off a header line holding one pipe alone (lazyTableRows).
type lazyAwareTable struct{}

// tableTransformer is goldmark's table paragraph transformer, which lazyTableRows runs and
// formsTable tries.
var tableTransformer = extension.NewTableParagraphTransformer()

func (lazyAwareTable) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithParagraphTransformers(util.Prioritized(lazyTableRows{table: tableTransformer}, 200)),
		parser.WithASTTransformers(util.Prioritized(extension.NewTableASTTransformer(), 0)),
	)
}

// lazyTableRows keeps a table's header and delimiter rows off lazy continuation lines, and opens
// no table whose header line holds one pipe and nothing else but spaces and tabs. In GFM, and in
// the browser editor's parser, a line that continues a paragraph in a list item, a quote or a
// footnote definition without that container's prefix only continues the paragraph; goldmark's
// transformer reads a paragraph's lines as rows wherever they came from, so `- a\n|-|` became a
// table in the list item. A lazy line is one the reader began at its line start although the
// paragraph's first line began after a container's prefix. The browser editor's parser reads a
// header line of a lone `|` as text, so `|\n-|` is the paragraph `| -|`, where goldmark read a
// table of one empty cell.
type lazyTableRows struct{ table parser.ParagraphTransformer }

func (t lazyTableRows) Transform(node *ast.Paragraph, reader gmtext.Reader, pc parser.Context) {
	source := reader.Source()
	lazy := lazyLines(node.Lines(), source)
	if lazy == nil && !holdsLonePipeLine(node.Lines(), source) {
		t.transform(node, reader, pc)
		return
	}
	// Read the paragraph as a table apart from the document to see which lines would be its
	// header and delimiter rows.
	trial := ast.NewParagraph()
	expanded := tabExpandedLines(node.Lines(), source)
	starts := make([]int, expanded.Len())
	for index := range starts {
		starts[index] = expanded.At(index).Start
	}
	trial.SetLines(expanded)
	holder := ast.NewDocument()
	holder.AppendChild(holder, trial)
	t.table.Transform(trial, reader, parser.NewContext())
	for child := holder.FirstChild(); child != nil; child = child.NextSibling() {
		table, ok := child.(*extensionast.Table)
		if !ok || table.FirstChild() == nil {
			continue
		}
		for index := 0; index+1 < node.Lines().Len(); index++ {
			line := node.Lines().At(index)
			if starts[index] != table.FirstChild().Pos() {
				continue
			}
			if lazy != nil && (lazy[index] || lazy[index+1]) || lonePipe(source[line.Start:line.Stop]) {
				return
			}
		}
	}
	t.transform(node, reader, pc)
}

// transform is goldmark's table transformer, reading the paragraph's lines with their indentation
// expanded where it holds a tab (tabExpandedLines); a paragraph that stays one keeps its lines. A
// table that takes the paragraph's first line takes the paragraph's place, starting where its text
// does and after the blank line before it if there was one; goldmark gives it neither. The browser
// list spacing reads both, the blank line through blankAfterAttr, since goldmark's looseness, which
// reads HasBlankPreviousLines, has never seen it.
func (t lazyTableRows) transform(node *ast.Paragraph, reader gmtext.Reader, pc parser.Context) {
	lines := node.Lines()
	first := lines.At(0)
	start := first.TrimLeftSpace(reader.Source()).Start
	blank := node.HasBlankPreviousLines()
	parent, previous := node.Parent(), node.PreviousSibling()
	// Goldmark's transformer consumes the lines it takes as rows, so where they start is read before
	// it runs. A table takes the paragraph's lines from its header row to the paragraph's end.
	starts := make([]int, lines.Len())
	for index := range starts {
		starts[index] = lineStart(reader.Source(), lines.At(index).Start)
	}
	segments := lines.Sliced(0, lines.Len())
	if read, ok := node.Attribute(readLinesAttr); ok {
		segments = read.([]gmtext.Segment)
	}
	lazy, _ := node.Attribute(lazyLinesAttr)
	lazyStarts, _ := lazy.([]int)
	node.SetLines(tabExpandedLines(lines, reader.Source()))
	t.table.Transform(node, reader, pc)
	if node.Parent() != nil {
		if kept := node.Lines().Len(); kept == len(starts) {
			node.SetLines(lines)
		} else if table, ok := node.NextSibling().(*extensionast.Table); ok {
			markLazyRows(table, starts[kept:], lazyStarts)
			markBlockRows(table, segments[kept:], reader.Source())
		}
		return
	}
	place := parent.FirstChild()
	if previous != nil {
		place = previous.NextSibling()
	}
	if table, ok := place.(*extensionast.Table); ok {
		table.SetPos(start)
		table.SetAttribute(blankAfterAttr, blank)
		markLazyRows(table, starts, lazyStarts)
		markBlockRows(table, segments, reader.Source())
	}
}

// lazyRowAttr marks a table a lazy continuation line is a body row of (markLazyRows): goldmark
// continued the paragraph the table was made of with the line, where the browser editor's parser
// ends the table, and every container the line does not continue, before it.
var lazyRowAttr = []byte("pmdoc-lazy-row")

// markLazyRows marks table (lazyRowAttr) where one of its body rows - the lines rows starts after
// its header and delimiter rows - starts at one of lazy, where the paragraph's lazy lines start.
func markLazyRows(table *extensionast.Table, rows, lazy []int) {
	if len(rows) < 3 {
		return
	}
	for _, row := range rows[2:] {
		if slices.Contains(lazy, row) {
			table.SetAttribute(lazyRowAttr, true)
			return
		}
	}
}

// underlineAfterTable is goldmark's setext heading parser opening no heading under a paragraph its
// table transformer reads a table in (formsTable). Goldmark's table is a paragraph, which the
// underline would make a heading, and where the table takes the paragraph's every line goldmark
// writes the underline as a paragraph of its own, or moves the lines before the table after it as
// the heading. The browser editor's parser, whose table is no paragraph, reads the line as the
// table's row, as it reads any line that opens no other block, so the line stays the paragraph's -
// all but a lone `-`, which opens an empty list item there, as goldmark's own handling leaves it.
// That handling closes the paragraph, trimming the whitespace every line opens with, before the
// table transformer reads it, so the lines are kept as read (readLinesAttr) for markBlockRows.
type underlineAfterTable struct{ parser.BlockParser }

func (p underlineAfterTable) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	paragraph, ok := pc.LastOpenedBlock().Node.(*ast.Paragraph)
	if !ok || paragraph.Parent() != parent || !formsTable(paragraph, reader.Source()) {
		return p.BlockParser.Open(parent, reader, pc)
	}
	if line, _ := reader.PeekLine(); !bareMarkerLine.Match(bytes.TrimRight(line, "\n")) {
		return nil, parser.NoChildren
	}
	// A copy, since the close trims the paragraph's lines in place.
	lines := slices.Clone(paragraph.Lines().Sliced(0, paragraph.Lines().Len()))
	node, state := p.BlockParser.Open(parent, reader, pc)
	if node != nil {
		paragraph.SetAttribute(readLinesAttr, lines)
	}
	return node, state
}

// readLinesAttr holds, on a paragraph goldmark closes for a setext underline, its lines as read,
// before that close trims them (underlineAfterTable).
var readLinesAttr = []byte("pmdoc-read-lines")

// formsTable reports whether goldmark's table transformer reads a table in paragraph's lines,
// trying it on a copy apart from the document.
func formsTable(paragraph *ast.Paragraph, source []byte) bool {
	trial := ast.NewParagraph()
	trial.SetLines(tabExpandedLines(paragraph.Lines(), source))
	holder := ast.NewDocument()
	holder.AppendChild(holder, trial)
	tableTransformer.Transform(trial, gmtext.NewReader(source), parser.NewContext())
	for child := holder.FirstChild(); child != nil; child = child.NextSibling() {
		if _, ok := child.(*extensionast.Table); ok {
			return true
		}
	}
	return false
}

// blockRowAttr marks a table goldmark took a body row of from a line the browser editor's parser
// reads as opening another block after the table (markBlockRows).
var blockRowAttr = []byte("pmdoc-block-row")

// markBlockRows marks table (blockRowAttr) where one of its body rows - the lines past its header
// and delimiter rows, the first two of lines - is a line the browser editor's
// parser reads as another block: a list item, whatever its marker, or indented code, four columns
// or more past its container's content. Goldmark's table is a paragraph, so a line that cannot
// interrupt one - an empty item, an ordered item numbered other than 1, indented code - becomes its
// row, where that parser's table, which is no paragraph, ends.
func markBlockRows(table *extensionast.Table, lines []gmtext.Segment, source []byte) {
	for index := 2; index < len(lines); index++ {
		segment := lines[index]
		begin := lineStart(source, segment.Start)
		text := segment.Start
		for text < segment.Stop && (source[text] == ' ' || source[text] == '\t') {
			text++
		}
		indent := columnOf(source[begin:text]) - columnOf(source[begin:segment.Start]) + segment.Padding
		if indent >= 4 || listMarkerStart.Match(source[text:segment.Stop]) {
			table.SetAttribute(blockRowAttr, true)
			return
		}
	}
}

// holdsLonePipeLine reports whether one of a paragraph's lines holds one pipe and nothing else but
// spaces and tabs (lonePipe).
func holdsLonePipeLine(lines *gmtext.Segments, source []byte) bool {
	for index := 0; index < lines.Len(); index++ {
		if line := lines.At(index); lonePipe(source[line.Start:line.Stop]) {
			return true
		}
	}
	return false
}

// lonePipe reports whether line holds one pipe and nothing else but spaces, tabs and its line
// ending.
func lonePipe(line []byte) bool {
	return string(bytes.Trim(line, " \t\n")) == "|"
}

// lazyLines reports which of a paragraph's lines are lazy continuation lines, or nil when none is.
func lazyLines(lines *gmtext.Segments, source []byte) []bool {
	if lines.Len() < 2 {
		return nil
	}
	if first := lines.At(0); first.Start == lineStart(source, first.Start) {
		return nil
	}
	var lazy []bool
	for index := 1; index < lines.Len(); index++ {
		if line := lines.At(index); line.Start == lineStart(source, line.Start) {
			if lazy == nil {
				lazy = make([]bool, lines.Len())
			}
			lazy[index] = true
		}
	}
	return lazy
}
