package pmdoc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMoveInsideItself reports a move whose anchor lies inside the moved block.
var ErrMoveInsideItself = errors.New("pmdoc: move anchor lies inside the moved block")

// findBlock returns the path and range of the block carrying blockID.
func findBlock(doc *Node, blockID string) ([]int, Range, bool) {
	var found []int
	var r Range
	walk(doc, func(node *Node, path []int, pos, end int) bool {
		if node.Type == "doc" || isInlineNodeType(node.Type) || node.Attrs[BlockIDAttr] != blockID {
			return true
		}
		found = append([]int(nil), path...)
		r = Range{From: pos, To: end}
		return false
	})
	return found, r, found != nil
}

func wantDocument(doc *Node, operation string) error {
	if doc == nil || doc.Type != "doc" {
		return fmt.Errorf("%w: %s wants a document", ErrSchema, operation)
	}
	return doc.Validate()
}

// BlockRange returns the ProseMirror range covering the block carrying blockID.
func BlockRange(doc *Node, blockID string) (Range, error) {
	if err := wantDocument(doc, "BlockRange"); err != nil {
		return Range{}, err
	}
	_, r, ok := findBlock(doc, blockID)
	if !ok {
		return Range{}, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	return r, nil
}

// BlockDescendantIDs returns the target block id followed by every block id
// that deleting it would remove as a descendant.
func BlockDescendantIDs(doc *Node, blockID string) ([]string, error) {
	if err := wantDocument(doc, "BlockDescendantIDs"); err != nil {
		return nil, err
	}
	path, _, ok := findBlock(doc, blockID)
	if !ok {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	var ids []string
	var collect func(*Node)
	collect = func(node *Node) {
		if !isInlineNodeType(node.Type) {
			if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
				ids = append(ids, id)
			}
		}
		for _, child := range node.Children {
			collect(child)
		}
	}
	collect(nodeAtPath(doc, path))
	return ids, nil
}

// TableDescendantIDs reports every non-inline descendant block id for each
// table. Table ids themselves are excluded because their direct references are
// counted separately.
func TableDescendantIDs(doc *Node) (map[string][]string, error) {
	if err := wantDocument(doc, "TableDescendantIDs"); err != nil {
		return nil, err
	}
	tables := make(map[string][]string)
	var descendants func(*Node, *[]string, map[string]struct{})
	descendants = func(node *Node, ids *[]string, seen map[string]struct{}) {
		if !isInlineNodeType(node.Type) {
			if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
				if _, duplicate := seen[id]; !duplicate {
					seen[id] = struct{}{}
					*ids = append(*ids, id)
				}
			}
		}
		for _, child := range node.Children {
			descendants(child, ids, seen)
		}
	}
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if node.Type != "table" {
			return true
		}
		tableID, _ := node.Attrs[BlockIDAttr].(string)
		if tableID == "" {
			return true
		}
		ids := make([]string, 0)
		seen := make(map[string]struct{})
		for _, child := range node.Children {
			descendants(child, &ids, seen)
		}
		tables[tableID] = ids
		return true
	})
	return tables, nil
}

// DeleteBlock returns a copy of doc without the block carrying blockID. A list,
// list item, or blockquote the removal empties goes with it and an emptied
// document keeps one empty paragraph; any other container the removal leaves
// outside its content rule rejects the delete with that rule.
func DeleteBlock(doc *Node, blockID string) (*Node, error) {
	if err := wantDocument(doc, "DeleteBlock"); err != nil {
		return nil, err
	}
	path, _, ok := findBlock(doc, blockID)
	if !ok {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	out := cloneNode(doc)
	removeAtPath(out, path)
	if len(out.Children) == 0 {
		out.Children = []*Node{{Type: "paragraph"}}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// TableIndexError describes a row or column index that cannot be applied to a
// table. Its dimensions preserve ragged rows rather than concealing them.
type TableIndexError struct {
	Axis    string
	Index   string
	Rows    int
	Widths  []int
	Problem string
}

func (e *TableIndexError) Error() string {
	dimensions := fmt.Sprintf("table has %d rows", e.Rows)
	if len(e.Widths) > 0 {
		columns := e.Widths[0]
		rectangular := true
		for _, width := range e.Widths[1:] {
			if width != columns {
				rectangular = false
				break
			}
		}
		if rectangular {
			columnName := "columns"
			if columns == 1 {
				columnName = "column"
			}
			dimensions += fmt.Sprintf(" and %d %s", columns, columnName)
		} else {
			dimensions += fmt.Sprintf(" with row widths %v", e.Widths)
		}
	}
	if e.Problem != "" {
		return fmt.Sprintf("pmdoc: %s; %s", e.Problem, dimensions)
	}
	return fmt.Sprintf("pmdoc: %s index %s is out of range; %s", e.Axis, e.Index, dimensions)
}

func tableIndexError(table *Node, axis, index, problem string) error {
	widths := make([]int, len(table.Children))
	for rowIndex, row := range table.Children {
		widths[rowIndex] = len(row.Children)
	}
	return &TableIndexError{
		Axis:    axis,
		Index:   index,
		Rows:    len(table.Children),
		Widths:  widths,
		Problem: problem,
	}
}

func editTable(doc *Node, blockID, operation string) ([]int, *Node, error) {
	if err := wantDocument(doc, operation); err != nil {
		return nil, nil, err
	}
	path, _, ok := findBlock(doc, blockID)
	if !ok {
		return nil, nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	table := nodeAtPath(doc, path)
	if table.Type != "table" {
		return nil, nil, fmt.Errorf("%w: block %q is %s, not a table", ErrSchema, blockID, table.Type)
	}
	return path, table, nil
}

// TableIndexErrorFor returns a dimension-bearing error for invalid input
// before a table row or column mutation begins.
func TableIndexErrorFor(doc *Node, blockID, axis, index, problem string) error {
	_, table, err := editTable(doc, blockID, "TableIndexErrorFor")
	if err != nil {
		return err
	}
	return tableIndexError(table, axis, index, problem)
}

// TableRowMarks returns the marks in the row a row deletion would remove.
func TableRowMarks(doc *Node, blockID string, index int) ([]MarkRef, error) {
	_, table, err := editTable(doc, blockID, "TableRowMarks")
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(table.Children) {
		return nil, tableIndexError(table, "row", strconv.Itoa(index), "")
	}
	if len(table.Children) == 2 {
		return nil, tableIndexError(table, "row", strconv.Itoa(index), fmt.Sprintf("row index %d would remove the last remaining body row", index))
	}
	return marksInNodes(table.Children[index : index+1]), nil
}

// TableColumnMarks returns the marks in the cells a column deletion would
// remove.
func TableColumnMarks(doc *Node, blockID string, index int) ([]MarkRef, error) {
	_, table, err := editTable(doc, blockID, "TableColumnMarks")
	if err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, tableIndexError(table, "column", strconv.Itoa(index), "")
	}
	cells := make([]*Node, 0, len(table.Children))
	for rowIndex, row := range table.Children {
		if index >= len(row.Children) {
			return nil, tableIndexError(table, "column", strconv.Itoa(index), fmt.Sprintf("column index %d is out of range for row %d", index, rowIndex))
		}
		if len(row.Children) == 1 {
			return nil, tableIndexError(table, "column", strconv.Itoa(index), fmt.Sprintf("column index %d would remove the last remaining column from row %d", index, rowIndex))
		}
		cells = append(cells, row.Children[index])
	}
	return marksInNodes(cells), nil
}

func marksInNodes(nodes []*Node) []MarkRef {
	seen := make(map[MarkRef]struct{})
	var marks []MarkRef
	var visit func(*Node)
	visit = func(node *Node) {
		if node.Type == "text" {
			for _, mark := range node.Marks {
				if !strings.HasPrefix(mark.Type, "proof") && mark.Type != "dispatchAsk" {
					continue
				}
				id, _ := mark.Attrs["id"].(string)
				if id == "" {
					continue
				}
				ref := MarkRef{Type: mark.Type, ID: id}
				if _, duplicate := seen[ref]; !duplicate {
					seen[ref] = struct{}{}
					marks = append(marks, ref)
				}
			}
		}
		for _, child := range node.Children {
			visit(child)
		}
	}
	for _, node := range nodes {
		visit(node)
	}
	return marks
}

// DeleteTableRow returns a copy of doc with the zero-based row removed from
// the table carrying blockID. Row zero is the header; deleting it promotes the
// first body row to the header so the table remains valid. A table's last body
// row is refused so the table block survives.
func DeleteTableRow(doc *Node, blockID string, index int) (*Node, error) {
	path, table, err := editTable(doc, blockID, "DeleteTableRow")
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(table.Children) {
		return nil, tableIndexError(table, "row", strconv.Itoa(index), "")
	}
	if len(table.Children) == 2 {
		return nil, tableIndexError(table, "row", strconv.Itoa(index), fmt.Sprintf("row index %d would remove the last remaining body row", index))
	}

	out := cloneNode(doc)
	outTable := nodeAtPath(out, path)
	children := make([]*Node, 0, len(outTable.Children)-1)
	if index == 0 {
		header := outTable.Children[1]
		header.Type = "table_header_row"
		for _, cell := range header.Children {
			cell.Type = "table_header"
		}
		children = append(children, header)
		children = append(children, outTable.Children[2:]...)
	} else {
		children = append(children, outTable.Children[:index]...)
		children = append(children, outTable.Children[index+1:]...)
	}
	outTable.Children = children
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteTableColumn returns a copy of doc without the zero-based column from
// the table carrying blockID. The column must exist in every row; this keeps a
// ragged table intact rather than silently deleting cells from only some rows.
// Removing a row's only cell is refused so the table block survives.
func DeleteTableColumn(doc *Node, blockID string, index int) (*Node, error) {
	path, table, err := editTable(doc, blockID, "DeleteTableColumn")
	if err != nil {
		return nil, err
	}
	if index < 0 {
		return nil, tableIndexError(table, "column", strconv.Itoa(index), "")
	}
	for rowIndex, row := range table.Children {
		if index >= len(row.Children) {
			return nil, tableIndexError(table, "column", strconv.Itoa(index), fmt.Sprintf("column index %d is out of range for row %d", index, rowIndex))
		}
		if len(row.Children) == 1 {
			return nil, tableIndexError(table, "column", strconv.Itoa(index), fmt.Sprintf("column index %d would remove the last remaining column from row %d", index, rowIndex))
		}
	}

	out := cloneNode(doc)
	outTable := nodeAtPath(out, path)
	for _, row := range outTable.Children {
		row.Children = append(row.Children[:index:index], row.Children[index+1:]...)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// removeAtPath removes the node at path and every list, list item, or
// blockquote that removal empties. Other containers keep their emptied shape
// for Validate to report.
func removeAtPath(root *Node, path []int) {
	for len(path) > 0 {
		parent := nodeAtPath(root, path[:len(path)-1])
		index := path[len(path)-1]
		parent.Children = append(parent.Children[:index:index], parent.Children[index+1:]...)
		if len(parent.Children) > 0 {
			return
		}
		switch parent.Type {
		case "bullet_list", "ordered_list", "list_item", "blockquote":
			path = path[:len(path)-1]
		default:
			return
		}
	}
}

// ErrListItemContent reports a whole-text delete of a list item that holds
// content besides its text and one nested list.
var ErrListItemContent = errors.New("pmdoc: list item has content besides its text")

// DeleteTextblock applies a delete of r when r covers a textblock's entire
// text: the textblock goes, and with it the list, list item, or blockquote it
// empties. A list item whose text goes and whose only other content is one
// nested list hoists that list's items into its place, the way an outliner
// does; an item with any other content is ErrListItemContent, naming the
// block delete that removes the item with its content. It reports
// removed=false when r covers part of a textblock's text or the textblock is a
// table cell's, whose content rule keeps the cell.
func DeleteTextblock(doc *Node, r Range) (*Node, bool, error) {
	if err := wantDocument(doc, "DeleteTextblock"); err != nil {
		return nil, false, err
	}
	var textblockPath []int
	walk(doc, func(node *Node, path []int, pos, end int) bool {
		if !isTextblock(node.Type) || r.From != pos+1 || r.To != end-1 {
			return true
		}
		textblockPath = append([]int(nil), path...)
		return false
	})
	if textblockPath == nil {
		return nil, false, nil
	}
	removePath := textblockPath
	hoistNested := false
	if len(textblockPath) > 1 {
		parentPath := textblockPath[:len(textblockPath)-1]
		parent := nodeAtPath(doc, parentPath)
		switch parent.Type {
		case "table_cell", "table_header":
			return nil, false, nil
		case "list_item":
			var err error
			removePath, hoistNested, err = listItemRemoval(parent, textblockPath)
			if err != nil {
				return nil, false, err
			}
		}
	}
	out := cloneNode(doc)
	if hoistNested {
		list := nodeAtPath(out, removePath[:len(removePath)-1])
		index := removePath[len(removePath)-1]
		hoisted := list.Children[index].Children[1].Children
		children := make([]*Node, 0, len(list.Children)+len(hoisted)-1)
		children = append(children, list.Children[:index]...)
		children = append(children, hoisted...)
		children = append(children, list.Children[index+1:]...)
		list.Children = children
	} else {
		removeAtPath(out, removePath)
	}
	if len(out.Children) == 0 {
		out.Children = []*Node{{Type: "paragraph"}}
	}
	if err := out.Validate(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// listItemRemoval decides what deleting the textblock at textblockPath, a child of the list
// item parent, removes: a later paragraph of the item goes alone; the item's text goes with
// the item when nothing else is left, hoisting a sole nested list's items into its place;
// any other remaining content is ErrListItemContent.
func listItemRemoval(parent *Node, textblockPath []int) (removePath []int, hoistNested bool, err error) {
	parentPath := textblockPath[:len(textblockPath)-1]
	rest := parent.Children[1:]
	switch {
	case textblockPath[len(textblockPath)-1] != 0:
		return textblockPath, false, nil
	case len(rest) == 0:
		return parentPath, false, nil
	case len(rest) == 1 && (rest[0].Type == "bullet_list" || rest[0].Type == "ordered_list"):
		return parentPath, true, nil
	default:
		id, _ := parent.Attrs[BlockIDAttr].(string)
		return nil, false, fmt.Errorf(`%w: %w; delete {block:%q} removes the item with its content`, ErrSchema, ErrListItemContent, id)
	}
}

// MoveBlock returns a copy of doc with the block carrying blockID relocated to
// the document-level boundary before or after the top-level block containing
// target, the boundary a block insert uses. An empty target at the document
// start or end selects that edge. A target inside the moved block is
// ErrMoveInsideItself; a block that cannot stand at document level is ErrSchema.
func MoveBlock(doc *Node, blockID string, target Range, after bool) (*Node, error) {
	if err := wantDocument(doc, "MoveBlock"); err != nil {
		return nil, err
	}
	path, moved, ok := findBlock(doc, blockID)
	if !ok {
		return nil, fmt.Errorf("%w: block %q", ErrTargetNotFound, blockID)
	}
	if target.From < target.To && target.From >= moved.From && target.To <= moved.To {
		return nil, fmt.Errorf("%w: block %q", ErrMoveInsideItself, blockID)
	}
	if node := nodeAtPath(doc, path); !isBlockNodeType(node.Type) {
		return nil, fmt.Errorf("%w: %s %q cannot stand at document level; move its enclosing block", ErrSchema, node.Type, blockID)
	}
	anchorIndex := -1
	position := 0
	for index, block := range doc.Children {
		end := position + nodeSize(block)
		if target.From >= position && target.To <= end {
			anchorIndex = index
			break
		}
		position = end
	}
	if anchorIndex < 0 {
		return nil, ErrTargetNotFound
	}
	out := cloneNode(doc)
	if len(path) == 1 && path[0] == anchorIndex {
		return out, nil
	}
	anchor := out.Children[anchorIndex]
	block := nodeAtPath(out, path)
	removeAtPath(out, path)
	insertIndex := -1
	for index, child := range out.Children {
		if child == anchor {
			insertIndex = index
			break
		}
	}
	if insertIndex < 0 {
		return nil, fmt.Errorf("%w: the anchor block was emptied by moving block %q out of it", ErrSchema, blockID)
	}
	if after {
		insertIndex++
	}
	children := make([]*Node, 0, len(out.Children)+1)
	children = append(children, out.Children[:insertIndex]...)
	children = append(children, block)
	children = append(children, out.Children[insertIndex:]...)
	out.Children = children
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}
