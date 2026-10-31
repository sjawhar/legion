package pmdoc

import (
	"fmt"
	"slices"
	"strings"
)

func (r *renderer) table(table *Node, prefix string) {
	if len(table.Children) == 0 || table.Children[0].Type != "table_header_row" {
		r.err = fmt.Errorf("%w: table requires a header row", ErrSchema)
		return
	}
	grid, _ := r.tableGrid(table)
	r.tableRow(table.Children[0], grid[0], true, prefix)
	r.writeSyntax("\n" + prefix + "| ")
	for i, cell := range grid[0] {
		if i > 0 {
			r.writeSyntax(" | ")
		}
		r.writeSyntax(tableAlignment(cell.Attrs["alignment"]))
	}
	r.writeSyntax(" |")
	for index, row := range table.Children[1:] {
		// A row with no cells is written as nothing: a table with only such rows reads back
		// holding one, as the browser editor's parser reads a table with no body row.
		if len(row.Children) == 0 {
			continue
		}
		r.writeSyntax("\n" + prefix)
		r.tableRow(row, grid[index+1], false, prefix)
	}
}

// tableRow writes row as cells, its cells in the columns tableGrid places them in. A cell is one
// line, so a hard break in it is written as a space, as the browser editor writes it: written as a
// break, it would end the row, and `<br>` reads back as inline HTML the editor shows as that text.
// A `|` in a cell's code after an odd run of backslashes, which no markdown of a cell writes, is
// written as text after the code (codePipesOutOfCode).
func (r *renderer) tableRow(row *Node, cells []*Node, header bool, prefix string) {
	want := "table_cell"
	rowType := "table_row"
	if header {
		want = "table_header"
		rowType = "table_header_row"
	}
	if row.Type != rowType {
		r.err = fmt.Errorf("%w: unexpected table row %q", ErrSchema, row.Type)
		return
	}
	r.writeSyntax("| ")
	for i, cell := range cells {
		if cell.Type != want {
			r.err = fmt.Errorf("%w: table row contains %q", ErrSchema, cell.Type)
			return
		}
		if i > 0 {
			r.writeSyntax(" | ")
		}
		if len(cell.Children) != 1 || cell.Children[0].Type != "paragraph" {
			r.err = fmt.Errorf("%w: table cell requires one paragraph", ErrSchema)
			return
		}
		written, _ := codePipesOutOfCode(hardBreaksAsSpaces(cell.Children[0].Children))
		r.tableCellInline(written, prefix)
	}
	r.writeSyntax(" |")
}

// codePipesOutOfCode is nodes, a table cell's inline content, with each `|` its code holds after an
// odd run of backslashes taken out of the code: the code before it ends there, and the `|` is text
// under the code's other marks, so every character stays and that one loses its code mark. It also
// returns the text of each code span that held one, once a span and in order, or none when none
// does and nodes are returned as they are. A code span is the code-marked text nodes written in
// one: one after another under the same written marks (writtenMarks), as writeInlineRun keeps a
// span open across them, so the backslashes before a `|` can end the node before it.
//
// A table row is split into cells before inline parsing, so in a cell a `|` is written `\|`, in
// code too, where both parsers take the backslash out; one after an even run of backslashes ends
// the cell (escapedByBackslashes); and a backslash escapes nothing in code. So code holding `\|`
// has no spelling in a cell: written `\\|`, the cell ends between the backslashes and the `|`.
// Text outside code takes the escapes it needs (needsInlineEscape), so the `|` taken out is
// written `\|` after the code's closing backtick, which both parsers read back as the `|`.
func codePipesOutOfCode(nodes []*Node) ([]*Node, []string) {
	if !slices.ContainsFunc(nodes, func(node *Node) bool {
		if strings.IndexByte(node.Text, '|') < 0 {
			return false
		}
		_, code := codeSpanMarks(node)
		return code
	}) {
		return nodes, nil
	}
	var out []*Node
	var held []string
	// run is how many backslashes the code span written so far ends with, from opens, the index of
	// its first node, under marks; opens is -1 outside code. heldOpens is opens of the last span
	// held reports.
	run, opens, heldOpens := 0, -1, -1
	var marks []Mark
	for index, node := range nodes {
		written, code := codeSpanMarks(node)
		if !code {
			run, opens = 0, -1
			if out != nil {
				out = append(out, node)
			}
			continue
		}
		if opens < 0 || !marksEqual(written, marks) {
			run, opens, marks = 0, index, written
		}
		from := 0
		for at := range len(node.Text) {
			char := node.Text[at]
			if char == '|' && run%2 == 1 {
				if out == nil {
					out = append(make([]*Node, 0, len(nodes)+2), nodes[:index]...)
				}
				if heldOpens != opens {
					held, heldOpens = append(held, codeSpanText(nodes, opens, marks)), opens
				}
				if at > from {
					out = append(out, &Node{Type: "text", Text: node.Text[from:at], Marks: node.Marks})
				}
				out = append(out, &Node{Type: "text", Text: "|", Marks: withoutMark(slices.Clone(node.Marks), "inlineCode")})
				from = at + 1
			}
			if char == '\\' {
				run++
			} else {
				run = 0
			}
		}
		switch {
		case out == nil:
		case from == 0:
			out = append(out, node)
		case from < len(node.Text):
			out = append(out, &Node{Type: "text", Text: node.Text[from:], Marks: node.Marks})
		}
	}
	if out == nil {
		return nodes, nil
	}
	return out, held
}

// codeSpanMarks is the marks node is written under (writtenMarks) when it is code-marked text, a
// node of a code span (codePipesOutOfCode), and false when it is not.
func codeSpanMarks(node *Node) ([]Mark, bool) {
	if node.Type != "text" || !nodeHasMark(node, "inlineCode") {
		return nil, false
	}
	return writtenMarks(node, true, true), true
}

// codeSpanText is the text of the code span whose first node is nodes[opens], written under marks
// (codePipesOutOfCode).
func codeSpanText(nodes []*Node, opens int, marks []Mark) string {
	var text strings.Builder
	for _, node := range nodes[opens:] {
		if written, code := codeSpanMarks(node); !code || !marksEqual(written, marks) {
			break
		}
		text.WriteString(node.Text)
	}
	return text.String()
}

// TableCodePipes is each code span, in a cell of a table in block, a document-level block, whose
// text holds a `|` after an odd run of backslashes, which no markdown of a cell writes
// (codePipesOutOfCode), in document order: a cell holding two such spans is in it twice. A write a
// caller makes that adds one is refused with it (TableCodePipe.Error); a browser edit holding one
// is stored with the `|` written after the code, as tableRow writes it. The error is a panic
// (ErrPanic) walking the tree, this package's bug.
func TableCodePipes(block *Node) (cells []TableCodePipe, err error) {
	defer recoverPanic(&cells, &err, "finding code a table cell cannot write")
	Walk(block, func(node *Node) bool {
		if node.Type != "table" {
			return true
		}
		for rowIndex, row := range node.Children {
			for column, cell := range row.Children {
				if len(cell.Children) != 1 {
					continue
				}
				_, held := codePipesOutOfCode(cell.Children[0].Children)
				for _, code := range held {
					cells = append(cells, TableCodePipe{Table: node, Row: rowIndex, Column: column, Code: code})
				}
			}
		}
		return true
	})
	return cells, nil
}

// TableCodePipe is a code span in a table cell whose text holds a `|` after an odd run of
// backslashes (TableCodePipes): the table, the cell's row and column, and the code span's text.
// It is also the refusal (ErrSchema) of a write that adds it, naming the cell: the table's block
// id, the row's index among the table's rows (0 the header row), the cell's index in its row (the
// indexes delete_row and delete_column take), and the header cell drawn above it, and saying what
// to write instead. The header is found with the table laid out on its own (headerAbove on a full
// span budget), so it can differ from the one GET /blocks names only once earlier tables in the
// document have spent the span budget (spansBefore).
type TableCodePipe struct {
	Table       *Node
	Row, Column int
	Code        string
}

func (cell TableCodePipe) Error() string {
	table := "a table"
	if id := blockIDOf(cell.Table); id != "" {
		table = fmt.Sprintf("table %q", id)
	}
	where := fmt.Sprintf("row %d, column %d", cell.Row, cell.Column)
	if cell.Row == 0 {
		where += " (the header row)"
	} else if header, found := headerAbove(&renderer{spanBudget: maxSpanCells}, cell.Table, cell.Row, cell.Column); found {
		where += fmt.Sprintf(" (under the header \"%s\")", header)
	}
	// The code is quoted as written, so its backslashes are not doubled.
	return fmt.Sprintf("%s holds code in %s whose text has a | right after a backslash, \"%s\", which a table cell's markdown cannot write: "+
		"in a cell a | is written \\|, in code too, \\\\| ends the cell, and a backslash escapes nothing in code; "+
		"drop the backslash before the |, or end the code before the | and write the | as text", table, where, cell.Code)
}

func (TableCodePipe) Unwrap() error { return ErrSchema }

// tableGrid is each of table's rows as its markdown writes them, cell by cell. GFM carries no span,
// so a cell spanning columns or rows (colspan, rowspan: a pasted HTML table can hold them) is
// written in the first position it covers and each other one it covers as an empty cell of its
// alignment, as the browser editor writes a spanning cell. The header row, whose width is the
// table's to Go's parser, is as wide as the widest row, its added cells taking the alignment of the
// first cell under them, so that no cell past it is lost; headerCells is how many cells the header
// row writes before it is widened so, its own cells and the columns their colspans add. A body row
// keeps its own width, since Parse pads a short row. A table with no span and no row wider than its
// header is its own rows.
// A span comes from the live tree unchecked, so the empty cells spans add are bounded per render,
// across every table of the document (renderer.spanBudget): each column a colspan adds, each
// position a rowspan covers below and each gap filled up to one is charged, and a span past the
// budget adds no more cells. The cells the header is widened by are not charged (maxSpanCells), and
// past the grid's limit a row reaches only columns where a row holds a cell. Every cell the table
// holds is still written with its text.
func (r *renderer) tableGrid(table *Node) (grid [][]*Node, headerCells int) {
	covered := map[[2]int]*Node{}
	// lastCovered is, for each row, the last column a span from a row above covers, or -1, kept so
	// that no row scans every covered position: with that scan, 20,000 rows took 24.7 s.
	lastCovered := make([]int, len(table.Children))
	// limit bounds the header's width, and so the read-back, which pads every row to the header:
	// the cells a colspan adds stay short of limit, the wider of maxColspan and the widest row as the
	// table holds cells, so every column past it is one where a row holds a cell, and the header is
	// at most limit and the table's cells wide. The budget alone would let one row's colspans widen
	// the header by maxSpanCells columns, each read back as a cell of every row.
	limit := maxColspan
	for index, row := range table.Children {
		lastCovered[index] = -1
		limit = max(limit, len(row.Children))
	}
	grid = make([][]*Node, len(table.Children))
	width := 0
	for rowIndex, row := range table.Children {
		kind := "table_cell"
		if row.Type == "table_header_row" {
			kind = "table_header"
		}
		var cells []*Node
		// fill adds the empty cells a span from a row above covers at the next position, and past
		// it every position up to until, while the budget lasts.
		fill := func(until int) {
			for {
				spanning, spanned := covered[[2]int{rowIndex, len(cells)}]
				if !spanned && (len(cells) >= until || r.spanBudget == 0) {
					return
				}
				if !spanned {
					r.spanBudget--
				}
				cells = append(cells, emptyTableCell(kind, spanning))
			}
		}
		for _, cell := range row.Children {
			fill(0)
			column := len(cells)
			columns := max(1, min(tableSpan(cell.Attrs["colspan"]), maxColspan, limit-column))
			// A rowspan past the last row covers nothing more, and without this clamp it indexes
			// lastCovered past its end: the panic is recovered, so every render of the table fails
			// and its document gets no version.
			rows := min(tableSpan(cell.Attrs["rowspan"]), len(table.Children)-rowIndex)
			cells = append(cells, cell)
			tail := min(columns-1, r.spanBudget)
			r.spanBudget -= tail
			for range tail {
				cells = append(cells, emptyTableCell(kind, cell))
			}
			for below := 1; below < rows && r.spanBudget > 0; below++ {
				covers := min(1+tail, r.spanBudget)
				r.spanBudget -= covers
				for offset := range covers {
					covered[[2]int{rowIndex + below, column + offset}] = cell
				}
				lastCovered[rowIndex+below] = max(lastCovered[rowIndex+below], column+covers-1)
			}
		}
		fill(lastCovered[rowIndex] + 1)
		grid[rowIndex] = cells
		width = max(width, len(cells))
	}
	headerCells = len(grid[0])
	for column := headerCells; column < width; column++ {
		var under *Node
		for _, row := range grid[1:] {
			if column < len(row) {
				under = row[column]
				break
			}
		}
		grid[0] = append(grid[0], emptyTableCell("table_header", under))
	}
	return grid, headerCells
}

// tableSpan is how many columns or rows a cell's colspan or rowspan covers: at least one.
func tableSpan(value any) int {
	return max(1, int(num(value, 1)))
}

// maxColspan is the most columns a cell is written across, as HTML caps colspan, so the markdown
// holds no more of a span than the browser editor draws, where a row as wide as the span would let
// the grid's width limit reach past it.
const maxColspan = 1000

// maxSpanCells is how many empty cells the spans of a document's tables add to their rows in all
// (tableGrid). The header, widened to its table's widest row, adds one more for each column a row
// reaches past it, over a cell some row holds or a span added, so a render reached from a peer's
// update writes at most twice that many cells no one wrote, and one more for each cell a row holds
// past its header.
const maxSpanCells = 100_000

// emptyTableCell is an empty cell of kind with like's alignment, or none when like is nil.
func emptyTableCell(kind string, like *Node) *Node {
	var alignment any
	if like != nil {
		alignment = like.Attrs["alignment"]
	}
	return &Node{
		Type:     kind,
		Attrs:    Attrs{"alignment": alignment, "colspan": 1, "colwidth": nil, "rowspan": 1},
		Children: []*Node{{Type: "paragraph"}},
	}
}

// tableAlignment is the delimiter row's cell for a column's alignment. A column with none - null,
// or the "none" this parser once stored - is written `---`, which both parsers read as no
// alignment; `:---` reads as left.
func tableAlignment(value any) string {
	alignment, _ := value.(string)
	switch alignment {
	case "left":
		return ":---"
	case "center":
		return ":---:"
	case "right":
		return "---:"
	default:
		return "---"
	}
}
