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
	if err := wantDocument(doc, "BlockPaths"); err != nil {
		return nil, err
	}
	walker := blockPathWalker{paths: map[string]BlockPath{}}
	walker.children(doc, nil)
	return walker.paths, nil
}

// blockPathWalker collects BlockPaths' paths. stack is the path of the node being visited, and
// table the position every block below it has in the table holding it, nil outside a table.
type blockPathWalker struct {
	paths map[string]BlockPath
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

// table records table and every block in it, laying the table out once.
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
	var noHeader string
	for rowIndex, row := range table.Children {
		rowID, rowHasID := row.Attrs[BlockIDAttr].(string)
		w.stack = append(w.stack, BlockPathEntry{Type: row.Type, ID: rowID, Index: rowIndex})
		cells := cellOpenings(row)
		w.record(row, rowID, rowHasID, &TablePosition{ID: id, Row: &ordinals[rowIndex], Cells: cells})
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
			position := &TablePosition{ID: id, Row: &ordinals[rowIndex], Column: &ordinals[column], Header: header, Cells: cells}
			cellID, cellHasID := cell.Attrs[BlockIDAttr].(string)
			w.stack = append(w.stack, BlockPathEntry{Type: cell.Type, ID: cellID, Index: column})
			w.record(cell, cellID, cellHasID, position)
			w.children(cell, position)
			w.stack = w.stack[:len(w.stack)-1]
		}
		w.stack = w.stack[:len(w.stack)-1]
	}
}

// oneLineText is node's text on one line, as the renderer writes a table cell (hard breaks as
// spaces).
func oneLineText(node *Node) string {
	return strings.Join(strings.Fields(TextContent(node)), " ")
}
