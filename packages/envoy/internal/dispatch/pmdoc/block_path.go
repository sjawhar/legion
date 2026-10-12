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
	header := layout.header(slices.Index(layout.grid[row], table.Children[row].Children[column]))
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

// header is the text of the header cell covering column at of the grid, or "" where none does.
func (layout tableLayout) header(at int) string {
	if at < 0 || at >= len(layout.covering) || layout.covering[at] < 0 {
		return ""
	}
	return layout.headers[layout.covering[at]]
}

// BlockPaths is the path of every block carrying a block id, keyed by that id, each as BlockPathOf
// finds it: where two blocks carry one id, the first in document order. It walks the document once
// and lays each table out once (tableLayout), so its cost is linear in the document's size: every
// path of a row shares one read-only Cells slice, every block in a cell shares one read-only
// TablePosition, and their Row, Column and Header point into storage the table's paths share.
func BlockPaths(doc *Node) (map[string]BlockPath, error) {
	paths, _, err := place(doc, "BlockPaths", true, false)
	return paths, err
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
// cell drawn above it ("" where none is) and its own opening words: the Row and Column BlockPathOf
// places it at, the opening words of the header cell its Header names, and the opening words its
// row's Cells names it by. A header is listed by its opening words, as the header row lists it,
// so a long header repeated under every row of a long table adds no more than they do to each.
type TableCell struct {
	ID     string
	Row    int
	Column int
	Header string
	Text   string
}

// TableRows is every row of every table carrying a block id, keyed by the table's id, each row and
// each cell placed as BlockPathOf places it, with the table laid out once (tableLayout). Where two
// tables carry one id, the first in document order is listed under it, as BlockPathOf finds it.
func TableRows(doc *Node) (map[string][]TableRow, error) {
	_, rows, err := place(doc, "TableRows", false, true)
	return rows, err
}

// BlockPathsAndTableRows is BlockPaths and TableRows from one walk, which lays each table out once
// for both: a listed cell's opening words and header are the strings its path's table position
// holds.
func BlockPathsAndTableRows(doc *Node) (map[string]BlockPath, map[string][]TableRow, error) {
	return place(doc, "BlockPathsAndTableRows", true, true)
}

// place walks doc once, collecting the paths when withPaths says so and the tables' rows when
// withRows does.
func place(doc *Node, operation string, withPaths, withRows bool) (map[string]BlockPath, map[string][]TableRow, error) {
	if err := wantDocument(doc, operation); err != nil {
		return nil, nil, err
	}
	var walker blockPathWalker
	if withPaths {
		walker.paths = map[string]BlockPath{}
	}
	if withRows {
		walker.rows = map[string][]TableRow{}
	}
	walker.children(doc, nil)
	return walker.paths, walker.rows, nil
}

// blockPathWalker collects BlockPaths' paths when paths is not nil, and TableRows' rows when rows
// is not nil. stack is the path of the node being visited, and table the position every block
// below it has in the table holding it, nil outside a table.
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

// record keeps the path of node, which carries id when hasID, unless an earlier block carries id
// or the walk collects no paths.
func (w *blockPathWalker) record(node *Node, id string, hasID bool, table *TablePosition) {
	if !hasID || w.paths == nil {
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
	var listed []TableRow
	listing := false
	if w.rows != nil && id != "" {
		_, earlier := w.rows[id]
		listing = !earlier
	}
	if listing {
		listed = make([]TableRow, len(table.Children))
	}
	var noHeader string
	// headerOpenings is the header row's Cells, each header cell's opening words, which a listed
	// cell names its header by.
	var headerOpenings []string
	for rowIndex, row := range table.Children {
		rowID, rowHasID := row.Attrs[BlockIDAttr].(string)
		w.stack = append(w.stack, BlockPathEntry{Type: row.Type, ID: rowID, Index: rowIndex})
		cells := cellOpenings(row)
		if rowIndex == 0 {
			headerOpenings = cells
		}
		w.record(row, rowID, rowHasID, &TablePosition{ID: id, Row: &ordinals[rowIndex], Cells: cells})
		if listing {
			listed[rowIndex] = TableRow{ID: rowID, Index: rowIndex, Cells: make([]TableCell, len(row.Children))}
		}
		// Each cell of the row stands in the grid in child order, so one pass over the row's grid
		// places them all.
		written := layout.grid[rowIndex]
		at := 0
		for column, cell := range row.Children {
			for written[at] != cell {
				at++
			}
			header := &noHeader
			if at < len(layout.covering) && layout.covering[at] >= 0 {
				header = &layout.headers[layout.covering[at]]
			}
			cellID, cellHasID := cell.Attrs[BlockIDAttr].(string)
			if listing {
				listedHeader := ""
				if at < len(layout.covering) && layout.covering[at] >= 0 {
					listedHeader = headerOpenings[layout.covering[at]]
				}
				listed[rowIndex].Cells[column] = TableCell{ID: cellID, Row: rowIndex, Column: column, Header: listedHeader, Text: cells[column]}
			}
			if w.paths == nil {
				continue
			}
			position := &TablePosition{ID: id, Row: &ordinals[rowIndex], Column: &ordinals[column], Header: header, Cells: cells}
			w.stack = append(w.stack, BlockPathEntry{Type: cell.Type, ID: cellID, Index: column})
			w.record(cell, cellID, cellHasID, position)
			w.children(cell, position)
			w.stack = w.stack[:len(w.stack)-1]
		}
		w.stack = w.stack[:len(w.stack)-1]
	}
	if listing {
		w.rows[id] = listed
	}
}

// oneLineText is node's text on one line, as the renderer writes a table cell (hard breaks as
// spaces).
func oneLineText(node *Node) string {
	return strings.Join(strings.Fields(TextContent(node)), " ")
}
