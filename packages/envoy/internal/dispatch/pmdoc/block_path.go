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
// the cell is written in once the colspans before it and the rowspans reaching down from the rows
// above take their places (tableGrid), "" where no header cell covers that column. Cells is each
// cell of the row as its opening words, by child index; nil for the table block.
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
	cells := table.Children[row].Children
	position.Row, position.Cells = &row, make([]string, len(cells))
	for index, cell := range cells {
		position.Cells[index] = openingWords(oneLineText(cell))
	}
	if len(below) == 1 {
		return position
	}
	column := below[1]
	header := columnHeader(table, row, column)
	position.Column, position.Header = &column, &header
	return position
}

// columnHeader is the text of the header cell drawn above the cell at row and column, both child
// indexes: the header cell covering the column the renderer writes that cell in (tableGrid), or ""
// where none covers it. Each grid is laid out on a budget of its own, as if its table were the
// document's only one, and only through the anchored row, since no row below moves a cell above.
func columnHeader(table *Node, row, column int) string {
	rows := (&renderer{spanBudget: maxSpanCells}).tableGrid(&Node{Type: table.Type, Children: table.Children[:row+1]})
	at := slices.Index(rows[row], table.Children[row].Children[column])
	// The header row is the first, so no rowspan reaches it, and laid out alone it is not widened
	// to a longer row: each header cell stands at its own first column, followed by the columns
	// its colspan adds, and nothing else.
	headers := table.Children[0].Children
	grid := (&renderer{spanBudget: maxSpanCells}).tableGrid(&Node{Type: table.Type, Children: table.Children[:1]})[0]
	if at >= len(grid) {
		return ""
	}
	var covering *Node
	next := 0
	for _, cell := range grid[:at+1] {
		if next < len(headers) && cell == headers[next] {
			covering, next = cell, next+1
		}
	}
	return oneLineText(covering)
}

// oneLineText is node's text on one line, as the renderer writes a table cell (hard breaks as
// spaces).
func oneLineText(node *Node) string {
	return strings.Join(strings.Fields(textContent(node)), " ")
}
