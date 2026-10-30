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
// continuation lines and off a header line holding one pipe alone (lazyTableRows), and without its
// AST transformer, whose work parseSource does itself (unescapeTablePipes).
type lazyAwareTable struct{}

// tableTransformer is goldmark's table paragraph transformer, which lazyTableRows runs where
// findTableRows reads a table the budget pays for.
var tableTransformer = extension.NewTableParagraphTransformer()

// unescapeTablePipes takes the backslash out of every escaped pipe in a table cell's code span, as
// goldmark's table AST transformer does (extension/table.go:299-342 at v1.8.6). pmdoc does not
// register that transformer: goldmark runs it inside its parse, before parseSource checks the
// nesting bounds, and it recurses through every cell holding a backtick before an escaped pipe and
// checks each code span text it meets against every escaped pipe in the document, time quadratic
// in a table of them. In a cell every `|` is one an odd run of backslashes escapes, since any other
// ends the cell (splitAtEvenEscapedPipes), and a code span opens with a backtick, which goldmark's
// row reader needs before an escaped pipe to record it (parseRow): so the escaped pipes it records
// inside a code span's text are exactly the `\|` that text holds. Each text is split around the
// backslash of each one, which it drops, as goldmark splits it; in a cell split off a goldmark cell,
// which goldmark's transformer never walks, too, as the browser editor's parser drops it. Run once
// the bounds hold, it walks the blocks and each cell once.
func unescapeTablePipes(root ast.Node, source []byte) {
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		switch {
		case !entering:
			return ast.WalkContinue, nil
		case node.Kind() == extensionast.KindTableCell:
			_ = ast.Walk(node, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
				if entering && node.Kind() == ast.KindCodeSpan {
					unescapeCodeSpanPipes(node, source)
					return ast.WalkSkipChildren, nil
				}
				return ast.WalkContinue, nil
			})
			return ast.WalkSkipChildren, nil
		case node.Type() == ast.TypeInline:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
}

// unescapeCodeSpanPipes splits each text of span around the backslash of every `\|` it holds,
// dropping the backslash, with the nodes goldmark's transformer makes.
func unescapeCodeSpanPipes(span ast.Node, source []byte) {
	for child := span.FirstChild(); child != nil; {
		next := child.NextSibling()
		if text, ok := child.(*ast.Text); ok {
			rest := text
			for index := text.Segment.Start + 1; index < text.Segment.Stop; index++ {
				if source[index] != '|' || source[index-1] != '\\' {
					continue
				}
				before := ast.NewRawTextSegment(rest.Segment.WithStop(index - 1))
				after := ast.NewRawTextSegment(rest.Segment.WithStart(index))
				span.InsertAfter(span, rest, before)
				span.InsertAfter(span, before, after)
				span.RemoveChild(span, rest)
				rest = after
			}
		}
		child = next
	}
}

// maxTablePaddingCells is how many empty cells the tables in the markdown one caller write sends
// - a document, the operations of an edit batch, an accepted suggestion - may have added to their
// short rows, and an accept or a reject to the rows of the tables its splice cut (PadTables).
const maxTablePaddingCells = 10_000

// ErrTablePadding is the refusal (ErrSchema) of markdown whose tables' short rows would be padded
// past their budget's limit, before the cells are allocated.
var ErrTablePadding = fmt.Errorf("%w: table padding", ErrSchema)

var (
	tablePaddingErrorKey = parser.NewContextKey()
	writeBudgetKey       = parser.NewContextKey()
)

type tablePadding struct {
	written int
	implied int
}

func (p tablePadding) cells() int {
	return p.implied - p.written
}

// WriteBudget is what one caller write's markdown may cost: the elements it makes (elementCount),
// and the empty cells reading or padding its tables may add to their short rows. A caller write
// takes one (NewWriteBudget) and spends it on every parse of the markdown it sends and every table
// it pads, so the elements and cells it costs are bounded however many operations carry them.
// Reading back a rendering spends a budget of its own (readBackBudget), which counts no elements.
type WriteBudget struct {
	cells    int
	tables   int
	limit    int
	scope    string
	elements elementCount
}

// NewWriteBudget is the budget of one caller write, MaxDocumentElements elements and
// maxTablePaddingCells cells.
func NewWriteBudget() *WriteBudget {
	return &WriteBudget{limit: maxTablePaddingCells, scope: "this write", elements: newElementCount(MaxDocumentElements)}
}

// readBackBudget is the budget of one parse of a rendering. The renderer writes a row short
// where its table's spans are left unwritten (renderSpanless) or past the cells its spans may add
// (maxSpanCells), or where the tree holds a short row, so the cells its parse pads are the tree's
// own, not a caller's: a table the browser editor pads whose spans the renderer writes whole pads
// at most maxSpanCells cells in any read-back. A rendering's elements are its tree's, so they are
// not counted.
func readBackBudget() *WriteBudget {
	return &WriteBudget{limit: maxSpanCells, scope: "this read-back", elements: newElementCount(0)}
}

// quoteBudget is the budget of a quote read as markdown (renderedMarkdownQuote), which
// matches by text: the cells a short row is padded with hold none, so a quote pads none. A quote is
// a caller's markdown as much as a write's is - the `find` of an edit, the quote of a comment or an
// ask - so it makes at most MaxDocumentElements elements; past them it is no markdown at all, and
// the lookup matches it by its text alone.
func quoteBudget() *WriteBudget {
	return &WriteBudget{limit: 0, scope: "a quote", elements: newElementCount(MaxDocumentElements)}
}

func (b *WriteBudget) add(padding tablePadding) error {
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
	if err := pc.Get(writeBudgetKey).(*WriteBudget).add(padding); err != nil {
		pc.Set(tablePaddingErrorKey, err)
		return true
	}
	return false
}

func (lazyAwareTable) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithParagraphTransformers(util.Prioritized(lazyTableRows{table: tableTransformer}, 200)))
}

// lazyTableRows runs goldmark's table transformer only where the browser editor's parser reads a
// table too (findTableRows): its delimiter row on no lazy continuation line and not four columns
// or more past its container, under a header row holding as many cells as it. In GFM, and in that
// parser, a line that continues a paragraph in a list item, a quote or a footnote definition
// without that container's prefix only continues the paragraph; goldmark's transformer reads a
// paragraph's lines as rows wherever they came from, so `- a\n|-|` would become a table in the
// list item. That parser reads a header line of a lone `|` as text, so `|\n-|` is the paragraph
// `| -|`, where goldmark reads a table of one empty cell: no cell opens on such a line
// (headerCells), so it is a header of none and no table forms. The transformer runs on the lines
// of the table that parser reads, the lines before them left a paragraph of their own where
// goldmark would read no table in them at all (textBeforeTable). Where goldmark would read a
// table, the budget pays for its padded cells before goldmark's transformer builds them.
type lazyTableRows struct{ table parser.ParagraphTransformer }

func (t lazyTableRows) Transform(node *ast.Paragraph, reader gmtext.Reader, pc parser.Context) {
	source := reader.Source()
	rows, found, _ := findTableRows(paragraphTableLines(node, source, pc), 1)
	if !found {
		return
	}
	padding := rows.padding(node.Lines(), source)
	if recordTablePadding(pc, padding) {
		return
	}
	if count := countedElements(pc); count != nil && rows.charge(count, node.Lines()) {
		return
	}
	t.transform(node, rows, reader, pc)
}

// tableRows is the table the browser editor's parser reads in a paragraph's lines (findTableRows):
// the line its header row is, and the width of its delimiter row.
type tableRows struct{ header, width int }

// tableLines is a paragraph's lines as the browser editor's parser read them: each as it was read,
// before a close for a setext underline took its indentation off (lineRecordingParagraph,
// underlineAfterTable), and where the lazy continuation lines among them start (lazyLinesAttr).
// That parser reads a delimiter row off the line as it is written, after the containers' prefix:
// the indentation such a close takes off is indentation a delimiter row may not have.
type tableLines struct {
	lines  *gmtext.Segments
	read   map[int]gmtext.Segment
	lazy   []int
	source []byte
}

// paragraphTableLines is paragraph's lines with what the parse recorded of them: the lines as they
// were read (untrimmedLinesKey) and the lazy ones among them (lazyLinesAttr).
func paragraphTableLines(paragraph *ast.Paragraph, source []byte, pc parser.Context) tableLines {
	read, _ := pc.Get(untrimmedLinesKey).(map[int]gmtext.Segment)
	lazy, _ := paragraph.Attribute(lazyLinesAttr)
	starts, _ := lazy.([]int)
	return tableLines{lines: paragraph.Lines(), read: read, lazy: starts, source: source}
}

func (l tableLines) len() int { return l.lines.Len() }

// at is line index as it was read, its indentation expanded where it holds a tab (tabExpandedLine):
// a line goldmark trimmed for a setext underline holds its indentation again, which is how
// goldmark's transformer reads the lines of every paragraph it has not closed.
func (l tableLines) at(index int) gmtext.Segment {
	return tabExpandedLine(l.asRead(index), l.source)
}

// asRead is line index as lineRecordingParagraph read it, before the paragraph trimmed its
// leading whitespace, or the line itself where the parse recorded none under its start. The parse
// keys each line it records by where the trimmed line starts (recordLine, lines.go), while the
// line of a paragraph goldmark has not closed starts where its containers left it, untrimmed, so
// the lookup misses on every line of such a paragraph. What it misses is that same untrimmed
// segment, recorded under the trimmed start, so the line itself is the answer there, and a closed
// paragraph's trimmed lines find what they were read as.
func (l tableLines) asRead(index int) gmtext.Segment {
	line := l.lines.At(index)
	if read, ok := l.read[line.Start]; ok {
		return read
	}
	return line
}

// isLazy reports whether line index is a lazy continuation line, one no container around the
// paragraph continues (lazyLinesAttr). lineRecordingParagraph.Continue appends each lazy line's
// start as it reads it, so the starts rise and a search over them costs a logarithm rather than
// the walk a scan of every candidate delimiter row would pay for every lazy line before it.
func (l tableLines) isLazy(index int) bool {
	if len(l.lazy) == 0 {
		return false
	}
	_, lazy := slices.BinarySearch(l.lazy, lineStart(l.source, l.lines.At(index).Start))
	return lazy
}

// findTableRows reads lines as the browser editor's parser reads them
// (micromark-extension-gfm-table's tokenizeTable): it tries a header row and the delimiter row
// under it at every line, and the first pair that holds makes a table of the lines from its
// header on. A pair holds where the second line reads as a delimiter row (tableDelimiterWidth)
// and the first holds as many cells as that row has columns (headerCells) - GFM's rule, which
// goldmark holds a header wider than the delimiter row to and a narrower one, which it pads out
// to the delimiter row's width, not to.
// That parser reads no delimiter row on a lazy continuation line, and no row of a table four
// columns or more past its container: a header row there is indented code to it, which a
// paragraph's line only continues the paragraph, and goldmark reads a delimiter row there where a
// close for a setext underline took the line's indentation off (underlineAfterTable), which is
// why the lines are read as they were written (tableLines). A pair that fails on any of these
// leaves its lines to the pair starting at the next line, and the lines of every failed pair are
// the text before the table (textBeforeTable).
// It reads lines for a pair from from on; next is where a read of lines added after these
// resumes, or -1 once a table is found.
func findTableRows(lines tableLines, from int) (rows tableRows, found bool, next int) {
	for delimiter := max(from, 1); delimiter < lines.len(); delimiter++ {
		width, ok := tableDelimiterWidth(lines.at(delimiter), lines.source)
		if !ok || lines.isLazy(delimiter) {
			continue
		}
		header := lines.at(delimiter - 1)
		if indent, _ := util.IndentWidth(header.Value(lines.source), 0); indent > 3 {
			continue
		}
		if headerCells(header, lines.source) != width {
			continue
		}
		return tableRows{header: delimiter - 1, width: width}, true, -1
	}
	return tableRows{}, false, lines.len()
}

// padding is the cells the table in lines holds as goldmark's transformer writes it, its cells
// split as rowCells splits them (splitAtEvenEscapedPipes), and the cells it holds once padded:
// every row from the header on, the delimiter row aside, holds as many cells as the delimiter row,
// the cells written past that dropped and the ones short of it padded (parseRow).
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

// charge charges count the cells goldmark's transformer makes of the table in lines once padded,
// each row's at the line it is written on, so a row the server wrote ahead of the caller's
// markdown is charged nothing (parseCount.charge, below parseCount.server). It reports whether the
// count passed the guard.
func (r tableRows) charge(count *parseCount, lines *gmtext.Segments) bool {
	for row := r.header; row < lines.Len(); row++ {
		if row != r.header+1 && count.charge(r.width, lines.At(row).Start) {
			return true
		}
	}
	return false
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
	width := 0
	rowCells(segment, source, func(int, []byte) { width++ })
	return width
}

// headerCells is how many cells the browser editor's parser reads in a table's header row (size in
// micromark-extension-gfm-table's tokenizeTable): a cell opens at the row's first character where
// that is no `|`, and after every `|` ending one where anything but a space or a tab follows
// before the line ends. A `|` an odd run of backslashes escapes ends no cell
// (escapedByBackslashes), as in rowCells. Goldmark counts one cell fewer in a row that ends with
// two pipes, `a||`, whose last `|` it drops as the row's closing one and whose empty cell before
// it it writes nothing of; it pads such a row out to its table's width, so the cells it writes
// where a table forms are the ones that parser reads.
func headerCells(segment gmtext.Segment, source []byte) int {
	segment = segment.TrimLeftSpace(source)
	segment = segment.TrimRightSpace(source)
	line := segment.Value(source)
	cells, open := 0, len(line) > 0 && line[0] != '|'
	for index, char := range line {
		if char == ' ' || char == '\t' {
			continue
		}
		if open {
			cells++
			open = false
		}
		if char == '|' && !escapedByBackslashes(line, index) {
			open = true
		}
	}
	return cells
}

// rowCells calls visit with each cell the browser editor's parser splits a table line into: its
// first `|` and its last dropped, whatever stands before either, as goldmark's transformer drops
// them (parseRow), and a cell ending at each other `|` that no odd run of backslashes stands
// before (escapedByBackslashes). Goldmark ends a cell only at a `|` with no backslash right before
// it; splitAtEvenEscapedPipes splits its cells where the two differ.
func rowCells(segment gmtext.Segment, source []byte, visit func(index int, value []byte)) {
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
	for index := 0; position < limit; index++ {
		closure := position
		for closure < limit && (line[closure] != '|' || escapedByBackslashes(line, closure)) {
			closure++
		}
		visit(index, line[position:closure])
		position = closure + 1
	}
}

// transform is goldmark's table transformer, reading the paragraph's lines with their indentation
// expanded where it holds a tab (tabExpandedLines); a paragraph that stays one keeps its lines. A
// table that takes the paragraph's first line takes the paragraph's place, starting where its text
// does and after the blank line before it if there was one; goldmark gives it neither. The browser
// list spacing reads both, the blank line through blankAfterAttr, since goldmark's looseness, which
// reads HasBlankPreviousLines, has never seen it. The lines before the table's header row that
// goldmark would not leave of its own accord are left a paragraph first (textBeforeTable).
func (t lazyTableRows) transform(node *ast.Paragraph, rows tableRows, reader gmtext.Reader, pc parser.Context) {
	textBeforeTable(node, rows, reader.Source())
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
	var table *extensionast.Table
	rowStarts, rowSegments := starts, segments
	if node.Parent() != nil {
		kept := node.Lines().Len()
		if kept == len(starts) {
			node.SetLines(lines)
			return
		}
		next, ok := node.NextSibling().(*extensionast.Table)
		if !ok {
			return
		}
		table, rowStarts, rowSegments = next, starts[kept:], segments[kept:]
	} else {
		place := parent.FirstChild()
		if previous != nil {
			place = previous.NextSibling()
		}
		placed, ok := place.(*extensionast.Table)
		if !ok {
			return
		}
		placed.SetPos(start)
		placed.SetAttribute(blankAfterAttr, blank)
		table = placed
	}
	markRows(table, rowStarts, lazyStarts, rowSegments, reader.Source())
}

// textBeforeTable leaves the lines before the table's header row a paragraph of their own, where
// goldmark's transformer would leave none. That transformer reads a paragraph's lines from the
// first and gives the table up at the first line it reads as a delimiter row under a header row
// of another width (extension/table.go:168-170 at v1.8.6), where the browser editor's parser
// tries a header and delimiter row at every line and reads the lines of the pairs it refused as
// the text before the table it reads further on (findTableRows). Handing the transformer the
// lines from the header row on is what makes it read that table, and the lines before them are
// the paragraph it leaves itself where it reads the table where that parser does.
func textBeforeTable(node *ast.Paragraph, rows tableRows, source []byte) {
	lines := node.Lines()
	if rows.header == 0 || firstDelimiterRow(lines, source) == rows.header+1 {
		return
	}
	// Goldmark closes the paragraph it leaves itself once the transformer has run, and a close
	// trims every line's leading whitespace and the last line's trailing whitespace, its line
	// ending among it (parser/paragraph.go Close). Nothing closes this one, so it is trimmed here.
	text := gmtext.NewSegments()
	for index := range rows.header {
		line := lines.At(index)
		text.Append(line.TrimLeftSpace(source))
	}
	last := text.At(rows.header - 1)
	text.Set(rows.header-1, last.TrimRightSpace(source))
	before := ast.NewParagraph()
	before.SetLines(text)
	// The blank line before the paragraph is the blank line before the text, not before the table,
	// which the text now stands between (blankAfterAttr, transform).
	before.SetBlankPreviousLines(node.HasBlankPreviousLines())
	node.SetBlankPreviousLines(false)
	node.Parent().InsertBefore(node.Parent(), node, before)
	rest := gmtext.NewSegments()
	rest.AppendAll(lines.Sliced(rows.header, lines.Len()))
	node.SetLines(rest)
	if read, ok := node.Attribute(readLinesAttr); ok {
		node.SetAttribute(readLinesAttr, read.([]gmtext.Segment)[rows.header:])
	}
}

// firstDelimiterRow is the line goldmark's transformer reads as a table's delimiter row in lines,
// the first after the first (parseDelimiter), or -1 where it reads none.
func firstDelimiterRow(lines *gmtext.Segments, source []byte) int {
	for index := 1; index < lines.Len(); index++ {
		if _, ok := tableDelimiterWidth(tabExpandedLine(lines.At(index), source), source); ok {
			return index
		}
	}
	return -1
}

// markRows marks table with what its rows hold that the browser editor's parser reads otherwise
// (markLazyRows, markBlockRows, markWideRows), splits its cells where that parser ends a cell
// goldmark does not (splitAtEvenEscapedPipes), and puts back a closing pipe that parser reads as
// text (keepEscapedClosingPipes). starts and lines are the table's lines from its header row on.
func markRows(table *extensionast.Table, starts, lazy []int, lines []gmtext.Segment, source []byte) {
	markLazyRows(table, starts, lazy)
	markBlockRows(table, lines, source)
	markWideRows(table, lines, source)
	splitAtEvenEscapedPipes(table, source)
	keepEscapedClosingPipes(table, lines, source)
}

// splitAtEvenEscapedPipes splits a cell of table at each `|` an even run of backslashes stands
// before, two or more. Goldmark's transformer reads any `|` with a backslash right before it as the
// cell's text, where the browser editor's parser, which pairs backslashes before a pipe, ends the
// cell there (escapedByBackslashes): `| a \\| b |` is the cells `a \\` and `b`, which read `a \`
// and `b`, as rowCells splits the row. A row keeps its table's width: the cells past it are
// dropped, the empty ones goldmark padded the row with first, since they follow the written ones,
// then written ones, which markWideRows marks where they hold text, as goldmark drops them. A
// row's closing `|` is in no cell, so it closes the row after an even run as before.
func splitAtEvenEscapedPipes(table *extensionast.Table, source []byte) {
	width := len(table.Alignments)
	for row := table.FirstChild(); row != nil; row = row.NextSibling() {
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			if cell.Lines().Len() == 0 {
				continue
			}
			segment := cell.Lines().At(0)
			value := source[segment.Start:segment.Stop]
			start := segment.Start
			for index, char := range value {
				if char != '|' || escapedByBackslashes(value, index) {
					continue
				}
				cell.Lines().Set(0, tableCellSegment(start, segment.Start+index, source))
				next := extensionast.NewTableCell()
				next.SetPos(segment.Start + index + 1)
				next.Lines().Append(gmtext.Segment{})
				// Goldmark's InsertAfter of a last child counts it twice in ChildCount (it calls
				// InsertBefore with a nil sibling, which counts it, then AppendChild, which counts it
				// again), and keepEscapedClosingPipes trusts ChildCount, so a last cell is appended.
				if after := cell.NextSibling(); after != nil {
					row.InsertBefore(row, after, next)
				} else {
					row.AppendChild(row, next)
				}
				cell, start = next, segment.Start+index+1
			}
			// A cell it did not split keeps the segment goldmark trimmed it to (parseRow).
			if start != segment.Start {
				cell.Lines().Set(0, tableCellSegment(start, segment.Stop, source))
			}
		}
		column := 0
		for cell := row.FirstChild(); cell != nil; column++ {
			next := cell.NextSibling()
			if column >= width {
				row.RemoveChild(row, cell)
			}
			cell = next
		}
	}
}

// tableCellSegment is a cell's text from start to stop in source, trimmed as goldmark's transformer
// trims a cell (parseRow).
func tableCellSegment(start, stop int, source []byte) gmtext.Segment {
	segment := gmtext.NewSegment(start, stop)
	segment = segment.TrimLeftSpace(source)
	return segment.TrimRightSpace(source)
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
	table, textBefore := scanForTable(paragraph, reader.Source(), pc)
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
// gains lines, so each is read once as a header and delimiter row's pair however many lines after
// it goldmark tries.
type tableScan struct {
	rows  tableRows
	found bool
	next  int
}

// scanForTable is formsTable for the open paragraph underlineAfterTable asks about, resuming the
// read where the last one on paragraph stopped (tableScan). formsTable does not keep it, since the
// paragraph interruptsOpenBlock asks about can be closed, its lines trimmed after it was read.
func scanForTable(paragraph *ast.Paragraph, source []byte, pc parser.Context) (table, textBefore bool) {
	value, _ := paragraph.Attribute(tableScanAttr)
	scan, _ := value.(*tableScan)
	if scan == nil {
		scan = &tableScan{next: 1}
		paragraph.SetAttribute(tableScanAttr, scan)
	}
	if scan.next >= 0 {
		scan.rows, scan.found, scan.next = findTableRows(paragraphTableLines(paragraph, source, pc), scan.next)
	}
	return scan.found, scan.found && scan.rows.header > 0
}

// formsTable reports whether a table forms in paragraph's lines (findTableRows), and whether text
// stands before that table, which is left a paragraph of its own (textBeforeTable). It reads the
// lines up to the table's header and delimiter rows: the ones after them change neither.
func formsTable(paragraph *ast.Paragraph, source []byte, pc parser.Context) (table, textBefore bool) {
	rows, found, _ := findTableRows(paragraphTableLines(paragraph, source, pc), 1)
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

// wideRowAttr marks a table one of whose body rows holds text in a cell past its delimiter row,
// with the first such row (markWideRows). Goldmark drops the cells past the table's width, where
// the browser editor's parser keeps them, so the row would be stored without their text. A cell
// ends at every `|` that no odd run of backslashes stands before, inside code and links too, so a
// code span holding a bare `|`, or `\\|`, in a row that already fills its table makes one.
var wideRowAttr = []byte("pmdoc-wide-row")

// wideRow is the first body row markWideRows found holding text past its table's width: how many
// cells it holds, the table's width, and the row's opening words, which name it. It is the reason
// the refusal walk gives (blockRefusal), which also records whether the table is the document's
// first block; a bare-row insert answers that table's refusal, the rows it parses under a header
// of its own, as ErrTableWidth instead (parseTableRows).
type wideRow struct {
	cells, width int
	opening      string
	firstBlock   bool
}

func (row wideRow) Error() string {
	return fmt.Sprintf("a table row holding %d cells where its table has %d, written \"%s\": a cell ends at every | not written \\|, in code and links too, and at a | after an even run of backslashes (\\\\|), and goldmark drops the cells past the table's width, which the browser editor's parser keeps; write a | inside a cell as \\|, or give the header and delimiter rows as many cells as the row", row.cells, row.width, row.opening)
}

// markWideRows marks table (wideRowAttr) where one of its body rows - the lines past its header
// and delimiter rows, the first two of lines - holds text in a cell past the table's delimiter
// row, each row split as the browser editor's parser splits it (rowCells). A row whose cells past
// the width are all blank loses no text where goldmark drops them, and is read at the table's
// width. It is the one place a row's width is judged, for a whole document and for bare rows alike.
func markWideRows(table *extensionast.Table, lines []gmtext.Segment, source []byte) {
	width := len(table.Alignments)
	for index := 2; index < len(lines); index++ {
		line := tabExpandedLine(lines[index], source)
		cells, text := 0, false
		rowCells(line, source, func(cell int, value []byte) {
			cells++
			text = text || cell >= width && !util.IsBlank(value)
		})
		if text {
			// Trimmed as rowCells trims it, so a space outside ASCII, a cell's text, is quoted.
			written := line.TrimLeftSpace(source)
			written = written.TrimRightSpace(source)
			table.SetAttribute(wideRowAttr, wideRow{cells: cells, width: width, opening: openingWords(string(written.Value(source)))})
			return
		}
	}
}

// keepEscapedClosingPipes puts a row's closing `|` back into its last cell where an odd run of
// backslashes escapes it. Goldmark's transformer drops a row's last `|` whatever stands before
// it, so `| x | y \|` read `y \`, where the browser editor's parser, which pairs backslashes
// before a pipe, reads the pipe as the cell's text: `y |`. After an even run the pipe closes the
// row in both. lines are the table's lines from its header row on; the delimiter row, the second,
// is no row of the table.
func keepEscapedClosingPipes(table *extensionast.Table, lines []gmtext.Segment, source []byte) {
	index := 0
	for row := table.FirstChild(); row != nil; row, index = row.NextSibling(), index+1 {
		line := index
		if index > 0 {
			line++
		}
		segment := tabExpandedLine(lines[line], source)
		segment = segment.TrimRightSpace(source)
		value := segment.Value(source)
		if len(value) < 2 || value[len(value)-1] != '|' || !escapedByBackslashes(value, len(value)-1) {
			continue
		}
		// The last written cell holds the backslash, so it is never empty; a row writing past the
		// table's width (`| x | y | z \|` under two columns), which the refusal walk refuses, has
		// no such cell.
		written := tableRowWidth(segment, source)
		if written > row.ChildCount() {
			continue
		}
		cell := row.FirstChild()
		for range written - 1 {
			cell = cell.NextSibling()
		}
		last := cell.Lines().At(cell.Lines().Len() - 1)
		last.Stop = segment.Stop
		cell.Lines().Set(cell.Lines().Len()-1, last)
	}
}
