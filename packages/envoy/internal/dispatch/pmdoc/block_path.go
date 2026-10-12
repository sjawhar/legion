package pmdoc

import (
	"fmt"
	"slices"
	"strings"
)

// BlockPath is where a block stands in its document: Path is every node from the document's
// top-level block down to the block itself, and Table places a block that is a table or stands
// in one.
type BlockPath struct {
	ID    string
	Type  string
	Path  []BlockPathEntry
	Table *TablePosition
}

// BlockPathEntry is one node on a block's path: its type, its block id (empty for a node the live
// document has not stamped yet) and its index among its parent's children.
type BlockPathEntry struct {
	Type  string
	ID    string
	Index int
}

// TablePosition places a block in the table holding it. Row is the row's index among the table's
// rows, 0 the header row, and Column the cell's index in its row - the indexes DeleteTableRow and
// DeleteTableColumn take, so a cell spanning columns (a pasted HTML table's colspan) counts once.
// Row is nil for the table block itself; Column and Header are nil for the table and for a row
// block. Header is the text of the header cell drawn above the cell: the one covering the column
// the renderer writes the cell in once the colspans before it and the rowspans reaching down from
// the rows above take their places, with the whole table laid out as the renderer writes it
// (tableGrid), "" where no header cell covers that column. Cells is each cell of the row as its
// opening words, by child index; nil for the table block.
type TablePosition struct {
	ID     string
	Row    *int
	Column *int
	Header *string
	Cells  []string
}

// BlockPathOf is the path of the block carrying blockID, or ErrTargetNotFound when no block does.
func BlockPathOf(doc *Node, blockID string) (BlockPath, error) {
	if err := wantDocument(doc, "BlockPathOf"); err != nil {
		return BlockPath{}, err
	}
	indexes, _, ok := findBlock(doc, blockID)
	if !ok {
		return BlockPath{}, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	path := BlockPath{ID: blockID, Path: make([]BlockPathEntry, len(indexes))}
	node := doc
	for depth, index := range indexes {
		node = node.Children[index]
		id, _ := node.Attrs[BlockIDAttr].(string)
		path.Path[depth] = BlockPathEntry{Type: node.Type, ID: id, Index: index}
		if node.Type == "table" {
			path.Table = tablePosition(node, id, indexes[depth+1:])
		}
	}
	path.Type = node.Type
	return path, nil
}

// tablePosition places the block at below, the child indexes under table (a row, then a cell).
func tablePosition(table *Node, id string, below []int) *TablePosition {
	position := &TablePosition{ID: id}
	if len(below) == 0 {
		return position
	}
	row := below[0]
	position.Row, position.Cells = &row, cellOpenings(table.Children[row])
	if len(below) == 1 {
		return position
	}
	column := below[1]
	layout := layoutTable(table)
	header := ""
	if h := layout.cellHeaders(table, row)[column]; h >= 0 {
		header = layout.headers[h]
	}
	position.Column, position.Header = &column, &header
	return position
}

// cellOpenings is each cell of row as its opening words, by child index.
func cellOpenings(row *Node) []string {
	cells := make([]string, len(row.Children))
	for index, cell := range row.Children {
		cells[index] = openingWords(oneLineText(cell))
	}
	return cells
}

// tableLayout is a table laid out whole, as the renderer writes it (tableGrid), on a span budget of
// its own, as if it were the document's only table: grid is each row as written, cell by cell, and
// covering is, for each column the header row writes before it is widened to the widest row, the
// index among the header row's children of the header cell covering it. The header row is the
// first, so no rowspan reaches it: each header cell stands at its own first column, followed by the
// columns its colspan adds.
type tableLayout struct {
	grid     [][]*Node
	covering []int
	headers  []string
}

func layoutTable(table *Node) tableLayout {
	grid, headerCells := (&renderer{spanBudget: maxSpanCells}).tableGrid(table)
	headerRow := table.Children[0].Children
	layout := tableLayout{grid: grid, covering: make([]int, headerCells), headers: make([]string, len(headerRow))}
	for index, cell := range headerRow {
		layout.headers[index] = oneLineText(cell)
	}
	next := 0
	for column, cell := range grid[0][:headerCells] {
		if next < len(headerRow) && cell == headerRow[next] {
			next++
		}
		layout.covering[column] = next - 1
	}
	return layout
}

// cellHeaders is, for each cell of the table's row at rowIndex in child order, the index among the
// header row's cells of the header cell drawn above it, or -1 where none is: the one covering the
// column the renderer writes the cell in. Every cell stands in its row's grid in child order, so
// one pass over the row places them all.
func (layout tableLayout) cellHeaders(table *Node, rowIndex int) []int {
	row := table.Children[rowIndex].Children
	heads := make([]int, len(row))
	written := layout.grid[rowIndex]
	at := 0
	for column, cell := range row {
		for written[at] != cell {
			at++
		}
		heads[column] = -1
		if at < len(layout.covering) {
			heads[column] = layout.covering[at]
		}
	}
	return heads
}

// TableRow is one row of a table as TableRows lists it: its block id ("" for a row the live
// document has not stamped yet), its index among the table's rows (0 the header row, the index
// DeleteTableRow takes) and each of its cells, by child index.
type TableRow struct {
	ID    string
	Index int
	Cells []TableCell
}

// TableCell is one cell of a table row as TableRows lists it: its block id ("" for a cell the live
// document has not stamped yet), its Row and Column (its index in its row, the index
// DeleteTableColumn takes, so a cell spanning columns counts once), the opening words of the header
// cell drawn above it ("" where none is) and its own opening words. A header is listed by its
// opening words, as the header row lists it, so a long header repeated under every row of a long
// table adds no more than they do to each.
type TableCell struct {
	ID     string
	Row    int
	Column int
	Header string
	Text   string
}

// TableRows is every row of every table carrying a block id, keyed by the table's id, each row and
// each cell placed as BlockPathOf places it, with each table laid out once (tableLayout) and no
// block's path built. Where two tables carry one id, the first in document order is listed under
// it, as BlockPathOf finds it.
func TableRows(doc *Node) (map[string][]TableRow, error) {
	if err := wantDocument(doc, "TableRows"); err != nil {
		return nil, err
	}
	rows := map[string][]TableRow{}
	Walk(doc, func(node *Node) bool {
		if node.Type != "table" {
			return true
		}
		layout := layoutTable(node)
		cells, heads := make([][]string, len(node.Children)), make([][]int, len(node.Children))
		for rowIndex, row := range node.Children {
			cells[rowIndex], heads[rowIndex] = cellOpenings(row), layout.cellHeaders(node, rowIndex)
		}
		id, _ := node.Attrs[BlockIDAttr].(string)
		listRows(rows, node, id, cells, heads)
		return true
	})
	return rows, nil
}

// BlockPathsAndTableRows is the path of every block carrying a block id, keyed by that id, each as
// BlockPathOf finds it (where two blocks carry one id, the first in document order), and TableRows,
// from one walk. It lays each table out once (tableLayout) for both, so its cost is linear in the
// document's size: every path of a row shares one read-only Cells slice, every block in a cell
// shares one read-only TablePosition, their Row, Column and Header point into storage the table's
// paths share, and a listed cell's opening words and header are the strings its path holds.
func BlockPathsAndTableRows(doc *Node) (map[string]BlockPath, map[string][]TableRow, error) {
	if err := wantDocument(doc, "BlockPathsAndTableRows"); err != nil {
		return nil, nil, err
	}
	walker := blockPathWalker{paths: map[string]BlockPath{}, rows: map[string][]TableRow{}}
	walker.children(doc, nil)
	return walker.paths, walker.rows, nil
}

// listRows adds table's rows to rows under id, unless id is empty or an earlier table carries it
// (BlockPathOf finds the first): cells is each row's cells' opening words, and heads each cell's
// header (cellHeaders), listed by the header row's opening words.
func listRows(rows map[string][]TableRow, table *Node, id string, cells [][]string, heads [][]int) {
	if id == "" {
		return
	}
	if _, earlier := rows[id]; earlier {
		return
	}
	listed := make([]TableRow, len(table.Children))
	for rowIndex, row := range table.Children {
		rowID, _ := row.Attrs[BlockIDAttr].(string)
		listed[rowIndex] = TableRow{ID: rowID, Index: rowIndex, Cells: make([]TableCell, len(row.Children))}
		for column, cell := range row.Children {
			cellID, _ := cell.Attrs[BlockIDAttr].(string)
			header := ""
			if h := heads[rowIndex][column]; h >= 0 {
				header = cells[0][h]
			}
			listed[rowIndex].Cells[column] = TableCell{ID: cellID, Row: rowIndex, Column: column, Header: header, Text: cells[rowIndex][column]}
		}
	}
	rows[id] = listed
}

// blockPathWalker collects BlockPathsAndTableRows' paths and rows. stack is the path of the node
// being visited, and table the position every block below it has in the table holding it, nil
// outside a table.
type blockPathWalker struct {
	paths map[string]BlockPath
	rows  map[string][]TableRow
	stack []BlockPathEntry
}

func (w *blockPathWalker) children(parent *Node, table *TablePosition) {
	for index, child := range parent.Children {
		if isInlineNodeType(child.Type) {
			continue
		}
		id, hasID := child.Attrs[BlockIDAttr].(string)
		w.stack = append(w.stack, BlockPathEntry{Type: child.Type, ID: id, Index: index})
		if child.Type == "table" {
			w.table(child, id, hasID)
		} else {
			w.record(child, id, hasID, table)
			w.children(child, table)
		}
		w.stack = w.stack[:len(w.stack)-1]
	}
}

// record keeps the path of node, which carries id when hasID, unless an earlier block carries id.
func (w *blockPathWalker) record(node *Node, id string, hasID bool, table *TablePosition) {
	if !hasID {
		return
	}
	if _, taken := w.paths[id]; taken {
		return
	}
	w.paths[id] = BlockPath{ID: id, Type: node.Type, Path: slices.Clone(w.stack), Table: table}
}

// table records table and every block in it, and lists its rows, laying the table out once.
func (w *blockPathWalker) table(table *Node, id string, hasID bool) {
	w.record(table, id, hasID, &TablePosition{ID: id})
	layout := layoutTable(table)
	// ordinals[i] is i, so every Row and Column points into one slice rather than an int each.
	widest := len(table.Children)
	for _, row := range table.Children {
		widest = max(widest, len(row.Children))
	}
	ordinals := make([]int, widest)
	for index := range ordinals {
		ordinals[index] = index
	}
	cells, heads := make([][]string, len(table.Children)), make([][]int, len(table.Children))
	var noHeader string
	for rowIndex, row := range table.Children {
		rowID, rowHasID := row.Attrs[BlockIDAttr].(string)
		w.stack = append(w.stack, BlockPathEntry{Type: row.Type, ID: rowID, Index: rowIndex})
		cells[rowIndex], heads[rowIndex] = cellOpenings(row), layout.cellHeaders(table, rowIndex)
		w.record(row, rowID, rowHasID, &TablePosition{ID: id, Row: &ordinals[rowIndex], Cells: cells[rowIndex]})
		for column, cell := range row.Children {
			header := &noHeader
			if h := heads[rowIndex][column]; h >= 0 {
				header = &layout.headers[h]
			}
			position := &TablePosition{ID: id, Row: &ordinals[rowIndex], Column: &ordinals[column], Header: header, Cells: cells[rowIndex]}
			cellID, cellHasID := cell.Attrs[BlockIDAttr].(string)
			w.stack = append(w.stack, BlockPathEntry{Type: cell.Type, ID: cellID, Index: column})
			w.record(cell, cellID, cellHasID, position)
			w.children(cell, position)
			w.stack = w.stack[:len(w.stack)-1]
		}
		w.stack = w.stack[:len(w.stack)-1]
	}
	listRows(w.rows, table, id, cells, heads)
}

// oneLineText is node's text on one line, as the renderer writes a table cell (hard breaks as
// spaces).
func oneLineText(node *Node) string {
	return strings.Join(strings.Fields(TextContent(node)), " ")
}
