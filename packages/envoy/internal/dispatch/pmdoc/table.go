package pmdoc

import (
	"bytes"
	"fmt"
	"regexp"
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

// tableTransformer is goldmark's table paragraph transformer, which lazyTableRows runs where
// findTableRows reads a table the budget pays for.
var tableTransformer = extension.NewTableParagraphTransformer()

// maxTablePaddingCells is how many empty cells the tables in the markdown one caller write sends
// - a document, the operations of an edit batch, an accepted suggestion - may have added to their
// short rows, and an accept or a reject to the rows of the tables its splice cut (PadTables).
const maxTablePaddingCells = 10_000

// ErrTablePadding is the refusal (ErrSchema) of markdown whose tables' short rows would be padded
// past their budget's limit, before the cells are allocated.
var ErrTablePadding = fmt.Errorf("%w: table padding", ErrSchema)

var (
	tablePaddingErrorKey  = parser.NewContextKey()
	tablePaddingBudgetKey = parser.NewContextKey()
)

type tablePadding struct {
	written int
	implied int
}

func (p tablePadding) cells() int {
	return p.implied - p.written
}

// TablePaddingBudget bounds the empty cells reading or padding tables may add to their short rows.
// A caller write takes one (NewTablePaddingBudget) and spends it on every parse of the markdown it
// sends and every table it pads, so the cells it costs are bounded however many operations carry
// them. Reading back a rendering spends a budget of its own (readBackPaddingBudget).
type TablePaddingBudget struct {
	cells  int
	tables int
	limit  int
	scope  string
}

// NewTablePaddingBudget is the budget of one caller write, maxTablePaddingCells cells.
func NewTablePaddingBudget() *TablePaddingBudget {
	return &TablePaddingBudget{limit: maxTablePaddingCells, scope: "this write"}
}

// readBackPaddingBudget is the budget of one parse of a rendering. The renderer writes a row short
// where its table's spans are left unwritten (renderSpanless) or past the cells its spans may add
// (maxSpanCells), or where the tree holds a short row, so the cells its parse pads are the tree's
// own, not a caller's: a table the browser editor pads whose spans the renderer writes whole pads
// at most maxSpanCells cells in any read-back.
func readBackPaddingBudget() *TablePaddingBudget {
	return &TablePaddingBudget{limit: maxSpanCells, scope: "this read-back"}
}

// quotePaddingBudget is the budget of a quote read as markdown (renderedMarkdownQuote), which
// matches by text: the cells a short row is padded with hold none, so a quote pads none.
func quotePaddingBudget() *TablePaddingBudget {
	return &TablePaddingBudget{limit: 0, scope: "a quote"}
}

func (b *TablePaddingBudget) add(padding tablePadding) error {
	b.tables++
	b.cells += padding.cells()
	if b.cells <= b.limit {
		return nil
	}
	return fmt.Errorf(
		"%w: table %d writes %d cells, which padding its rows to the table's width would make %d, adding %d; tables in %s would add %d cells (limit %d)",
		ErrTablePadding, b.tables, padding.written, padding.implied, padding.cells(), b.scope, b.cells, b.limit,
	)
}

func recordTablePadding(pc parser.Context, padding tablePadding) bool {
	if err := pc.Get(tablePaddingBudgetKey).(*TablePaddingBudget).add(padding); err != nil {
		pc.Set(tablePaddingErrorKey, err)
		return true
	}
	return false
}

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
// table of one empty cell. Where goldmark would read a table (findTableRows), the budget pays for
// its padded cells before goldmark's transformer builds them.
type lazyTableRows struct{ table parser.ParagraphTransformer }

func (t lazyTableRows) Transform(node *ast.Paragraph, reader gmtext.Reader, pc parser.Context) {
	source := reader.Source()
	rows, found, _ := findTableRows(node.Lines(), source, 1)
	if !found {
		return
	}
	header := node.Lines().At(rows.header)
	if lazy := lazyLines(node.Lines(), source); lazy != nil && (lazy[rows.header] || lazy[rows.header+1]) || lonePipe(source[header.Start:header.Stop]) {
		return
	}
	if recordTablePadding(pc, rows.padding(node.Lines(), source)) {
		return
	}
	t.transform(node, reader, pc)
}

// tableRows is the table goldmark's transformer reads in a paragraph's lines (findTableRows): the
// line its header row is, and the width of its delimiter row.
type tableRows struct{ header, width int }

// findTableRows reads lines as goldmark's table transformer does (extension/table.go Transform),
// their indentation expanded where it holds a tab (tabExpandedLine), as transform hands them to it:
// the first line after the first that reads as a delimiter row (tableDelimiterWidth) makes a table
// under the line before it, its header, unless the header holds more cells than the delimiter row.
// It reads lines for that delimiter row from from on, and stops there; next is where a read of
// lines added after these resumes, or -1 once the delimiter row is read.
func findTableRows(lines *gmtext.Segments, source []byte, from int) (rows tableRows, found bool, next int) {
	for delimiter := max(from, 1); delimiter < lines.Len(); delimiter++ {
		width, ok := tableDelimiterWidth(tabExpandedLine(lines.At(delimiter), source), source)
		if !ok {
			continue
		}
		rows = tableRows{header: delimiter - 1, width: width}
		return rows, tableRowWidth(tabExpandedLine(lines.At(rows.header), source), source) <= rows.width, -1
	}
	return tableRows{}, false, lines.Len()
}

// padding is the cells goldmark's transformer writes for the table in lines and the cells it holds
// once padded: every row from the header on, the delimiter row aside, holds as many cells as the
// delimiter row, the cells it writes past that dropped and the ones short of it padded (parseRow).
func (r tableRows) padding(lines *gmtext.Segments, source []byte) tablePadding {
	var padding tablePadding
	for row := r.header; row < lines.Len(); row++ {
		if row == r.header+1 {
			continue
		}
		padding.written += min(tableRowWidth(tabExpandedLine(lines.At(row), source), source), r.width)
		padding.implied += r.width
	}
	return padding
}

// tableDelimiterCell is a delimiter row's cell as goldmark's transformer reads one: hyphens, a
// colon at either end or both, and whitespace around them (tableDelimLeft, tableDelimRight,
// tableDelimCenter, tableDelimNone).
var tableDelimiterCell = regexp.MustCompile(`^\s*:?-+:?\s*$`)

// tableDelimiterWidth is the width of segment read as goldmark's delimiter row (isTableDelim,
// parseDelimiter), or false when it is no delimiter row.
func tableDelimiterWidth(segment gmtext.Segment, source []byte) (int, bool) {
	line := segment.Value(source)
	if indent, _ := util.IndentWidth(line, 0); indent > 3 {
		return 0, false
	}
	allHyphens := true
	for _, char := range line {
		if char != '-' {
			allHyphens = false
		}
		if !(util.IsSpace(char) || char == '-' || char == '|' || char == ':') {
			return 0, false
		}
	}
	if allHyphens {
		return 0, false
	}
	cells := bytes.Split(line, []byte{'|'})
	if util.IsBlank(cells[0]) {
		cells = cells[1:]
	}
	if len(cells) > 0 && util.IsBlank(cells[len(cells)-1]) {
		cells = cells[:len(cells)-1]
	}
	if len(cells) == 0 {
		return 0, false
	}
	for _, cell := range cells {
		if !tableDelimiterCell.Match(cell) {
			return 0, false
		}
	}
	return len(cells), true
}

func tableRowWidth(segment gmtext.Segment, source []byte) int {
	segment = segment.TrimLeftSpace(source)
	segment = segment.TrimRightSpace(source)
	line := segment.Value(source)
	position, limit := 0, len(line)
	if limit > 0 && line[position] == '|' {
		position++
	}
	if limit > 0 && line[limit-1] == '|' {
		limit--
	}
	width := 0
	for position < limit {
		width++
		closure := position
		for closure < limit && (line[closure] != '|' || closure > 0 && line[closure-1] == '\\') {
			closure++
		}
		position = closure + 1
	}
	return width
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
// table transformer reads a table in (scanForTable). Goldmark's table is a paragraph, which the
// underline would make a heading, and where the table takes the paragraph's every line goldmark
// writes the underline as a paragraph of its own, or moves the lines before the table after it as
// the heading. The browser editor's parser, whose table is no paragraph, reads the line as the
// table's row, as it reads any line that opens no other block, so the line stays the paragraph's -
// all but a lone `-`, which opens an empty list item there, as goldmark's own handling leaves it
// when the table takes every line. Where text stands before the table, that handling makes the
// text a heading after the table, which is marked (underlinedTextAttr) for the refusal walk.
// That handling closes the paragraph, trimming the whitespace every line opens with, before the
// table transformer reads it, so the lines are kept as read (readLinesAttr) for markBlockRows.
type underlineAfterTable struct{ parser.BlockParser }

func (p underlineAfterTable) Open(parent ast.Node, reader gmtext.Reader, pc parser.Context) (ast.Node, parser.State) {
	paragraph, ok := pc.LastOpenedBlock().Node.(*ast.Paragraph)
	if !ok || paragraph.Parent() != parent {
		return p.BlockParser.Open(parent, reader, pc)
	}
	table, textBefore := scanForTable(paragraph, reader.Source())
	if !table {
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
		if textBefore {
			node.SetAttribute(underlinedTextAttr, true)
		}
	}
	return node, state
}

// readLinesAttr holds, on a paragraph goldmark closes for a setext underline, its lines as read,
// before that close trims them (underlineAfterTable).
var readLinesAttr = []byte("pmdoc-read-lines")

// underlinedTextAttr marks the heading goldmark makes, after a table, of the text standing before
// the table in its paragraph, under a lone `-` the browser editor's parser reads as an empty list
// item (underlineAfterTable).
var underlinedTextAttr = []byte("pmdoc-underlined-text")

// tableScanAttr holds, on an open paragraph, what underlineAfterTable has read of its lines for a
// table (tableScan).
var tableScanAttr = []byte("pmdoc-table-scan")

// tableScan is what findTableRows has read of an open paragraph's lines. Goldmark tries every line
// opening with `-` or `=` under an open paragraph as a setext underline, and an open paragraph only
// gains lines, so each is read once for a delimiter row however many lines after it goldmark tries.
type tableScan struct {
	rows  tableRows
	found bool
	next  int
}

// scanForTable is formsTable for the open paragraph underlineAfterTable asks about, resuming the
// read where the last one on paragraph stopped (tableScan). formsTable does not keep it, since the
// paragraph interruptsOpenBlock asks about can be closed, its lines trimmed after it was read.
func scanForTable(paragraph *ast.Paragraph, source []byte) (table, textBefore bool) {
	value, _ := paragraph.Attribute(tableScanAttr)
	scan, _ := value.(*tableScan)
	if scan == nil {
		scan = &tableScan{next: 1}
		paragraph.SetAttribute(tableScanAttr, scan)
	}
	if scan.next >= 0 {
		scan.rows, scan.found, scan.next = findTableRows(paragraph.Lines(), source, scan.next)
	}
	return scan.found, scan.found && scan.rows.header > 0
}

// formsTable reports whether goldmark's table transformer reads a table in paragraph's lines
// (findTableRows), and whether text stands before that table, which the transformer leaves a
// paragraph of its own. It reads the lines up to the first delimiter row: the ones after it change
// neither.
func formsTable(paragraph *ast.Paragraph, source []byte) (table, textBefore bool) {
	rows, found, _ := findTableRows(paragraph.Lines(), source, 1)
	return found, found && rows.header > 0
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
