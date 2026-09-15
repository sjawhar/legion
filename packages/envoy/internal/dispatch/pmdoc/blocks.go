package pmdoc

import (
	"errors"
	"fmt"
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
			rest := parent.Children[1:]
			switch {
			case textblockPath[len(textblockPath)-1] != 0:
				// A later paragraph of the item: only that paragraph goes.
			case len(rest) == 0:
				removePath = parentPath
			case len(rest) == 1 && (rest[0].Type == "bullet_list" || rest[0].Type == "ordered_list"):
				removePath = parentPath
				hoistNested = true
			default:
				id, _ := parent.Attrs[BlockIDAttr].(string)
				return nil, false, fmt.Errorf(`%w: %w; delete {block:%q} removes the item with its content`, ErrSchema, ErrListItemContent, id)
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
