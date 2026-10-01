package pmdoc

import (
	"fmt"
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
// block. Header is the header row's cell text at Column, "" where the header row has no cell
// there. Cells is each cell of the row as its opening words, by column; nil for the table block.
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
	path := BlockPath{ID: blockID, Path: make([]BlockPathEntry, 0, len(indexes))}
	var table *Node
	node := doc
	for _, index := range indexes {
		parent := node
		node = node.Children[index]
		id, _ := node.Attrs[BlockIDAttr].(string)
		path.Path = append(path.Path, BlockPathEntry{Type: node.Type, ID: id, Index: index})
		switch {
		case node.Type == "table":
			table = node
			path.Table = &TablePosition{ID: id}
		case table != nil && parent == table:
			row := index
			path.Table.Row = &row
			path.Table.Cells = make([]string, 0, len(node.Children))
			for _, cell := range node.Children {
				path.Table.Cells = append(path.Table.Cells, openingWords(tableCellText(cell)))
			}
		case table != nil && (parent.Type == "table_row" || parent.Type == "table_header_row"):
			column := index
			header := ""
			if headerRow := table.Children[0]; column < len(headerRow.Children) {
				header = tableCellText(headerRow.Children[column])
			}
			path.Table.Column, path.Table.Header = &column, &header
		}
	}
	path.Type = node.Type
	return path, nil
}

// tableCellText is a table cell's text on one line, as the renderer writes a cell (hard breaks
// as spaces).
func tableCellText(cell *Node) string {
	return strings.Join(strings.Fields(textContent(cell)), " ")
}
