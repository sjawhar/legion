package pmdoc

import "fmt"

// Splice returns a copy of doc with r replaced by with using Proof's
// ProseMirror-compatible range-fitting rules.
func Splice(doc *Node, r Range, with *Node) (*Node, error) {
	if err := validateSplice(doc, r, with); err != nil {
		return nil, err
	}
	selection, err := selectRange(doc, r)
	if err != nil {
		return nil, err
	}
	if out, ok, err := spliceInlineSelection(doc, selection, r, with); err != nil || ok {
		return out, err
	}
	if out, ok, err := joinAcrossBoundary(doc, selection, r, with); err != nil || ok {
		return out, err
	}
	return ascend(doc, selection, r, with)
}

func validateSplice(doc *Node, r Range, with *Node) error {
	if doc == nil || with == nil || doc.Type != "doc" || with.Type != "doc" || r.From >= r.To {
		return fmt.Errorf("%w: invalid splice", ErrSchema)
	}
	if err := doc.Validate(); err != nil {
		return err
	}
	return with.Validate()
}

type spliceSelection struct {
	first      nodeLocation
	last       nodeLocation
	inline     *nodeLocation
	parentPath []int
}

// selectRange maps the range's textblock endpoints through the canonical
// position walker and records their minimal shared parent.
func selectRange(doc *Node, r Range) (spliceSelection, error) {
	var textblocks []nodeLocation
	var inline *nodeLocation
	walk(doc, func(node *Node, path []int, pos, end int) bool {
		if !isTextblock(node.Type) || r.From >= end-1 || r.To <= pos+1 {
			return true
		}
		location := nodeLocation{path: append([]int(nil), path...), pos: pos, end: end}
		textblocks = append(textblocks, location)
		if r.From >= pos+1 && r.To <= end-1 {
			inline = &location
		}
		return true
	})
	if len(textblocks) == 0 {
		return spliceSelection{}, ErrTargetNotFound
	}
	first, last := textblocks[0], textblocks[len(textblocks)-1]
	parentPath := commonParentPath(first.path, last.path)
	if samePath(first.path, last.path) {
		parentPath = parentPath[:len(parentPath)-1]
	}
	return spliceSelection{first: first, last: last, inline: inline, parentPath: parentPath}, nil
}

func spliceInlineSelection(doc *Node, selection spliceSelection, r Range, with *Node) (*Node, bool, error) {
	if selection.inline == nil || !inlineDocument(with) {
		return nil, false, nil
	}
	out := cloneNode(doc)
	block := nodeAtPath(out, selection.inline.path)
	if err := spliceInline(block, r, selection.inline.pos, with.Children[0].Children); err != nil {
		return nil, false, err
	}
	if err := out.Validate(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// ascend retries each ancestor until its content expression can contain the
// preserved range fragments and fitted replacement.
func ascend(doc *Node, selection spliceSelection, r Range, with *Node) (*Node, error) {
	openingItem := enclosingListItem(doc, selection.first.path)
	for path := selection.parentPath; ; path = path[:len(path)-1] {
		candidate, ok, err := fitContent(nodeAtPath(doc, path), selection, path, r, with, openingItem)
		if err != nil {
			return nil, err
		}
		if ok {
			return replaceFittedContent(doc, path, candidate)
		}
		if len(path) == 0 {
			return nil, fmt.Errorf("%w: replacement does not fit document", ErrSchema)
		}
	}
}

// sliceBoundaries preserves the open and close fragments at this fit depth.
func sliceBoundaries(parent *Node, selection spliceSelection, path []int, r Range) (*Node, *Node, error) {
	before, err := beforeRange(parent, selection.first.path[len(path):], selection.first.pos, r.From)
	if err != nil {
		return nil, nil, err
	}
	after, err := afterRange(parent, selection.last.path[len(path):], selection.last.pos, r.To)
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// fitContent applies the target parent's permitted Proof content shape.
func fitContent(parent *Node, selection spliceSelection, path []int, r Range, with, openingItem *Node) (*Node, bool, error) {
	before, after, err := sliceBoundaries(parent, selection, path, r)
	if err != nil {
		return nil, false, err
	}
	replacement, fits := fitReplacement(parent, with, openingItem, preservedParagraph(before, parent))
	if !fits {
		return nil, false, nil
	}
	candidate := cloneNode(parent)
	candidate.Children = combineFragments(before, replacement, after)
	if candidate.Type == "doc" && len(candidate.Children) == 0 {
		candidate.Children = []*Node{{Type: "paragraph"}}
	}
	if err := candidate.Validate(); err != nil {
		return nil, false, nil
	}
	return candidate, true, nil
}

func combineFragments(before *Node, replacement []*Node, after *Node) []*Node {
	var children []*Node
	if before = normalizeFragment(before); before != nil {
		children = append(children, before.Children...)
	}
	children = append(children, replacement...)
	if after = normalizeFragment(after); after != nil {
		children = append(children, after.Children...)
	}
	return children
}

func replaceFittedContent(doc *Node, path []int, candidate *Node) (*Node, error) {
	if len(path) == 0 {
		return candidate, nil
	}
	out := cloneNode(doc)
	replaceNodeAtPath(out, path, candidate)
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

type nodeLocation struct {
	path []int
	pos  int
	end  int
}

func isTextblock(nodeType string) bool {
	switch nodeType {
	case "paragraph", "heading", "code_block":
		return true
	default:
		return false
	}
}

func commonParentPath(left, right []int) []int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	index := 0
	for index < limit && left[index] == right[index] {
		index++
	}
	return append([]int(nil), left[:index]...)
}

func samePath(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func nodeAtPath(node *Node, path []int) *Node {
	for _, index := range path {
		node = node.Children[index]
	}
	return node
}

func enclosingListItem(doc *Node, path []int) *Node {
	for depth := len(path); depth > 0; depth-- {
		node := nodeAtPath(doc, path[:depth])
		if node.Type == "list_item" {
			return node
		}
	}
	return nil
}

func replaceNodeAtPath(root *Node, path []int, replacement *Node) {
	parent := nodeAtPath(root, path[:len(path)-1])
	parent.Children[path[len(path)-1]] = replacement
}

// joinAcrossBoundary applies ProseMirror's close rule when an inline range
// spans distinct textblocks.
func joinAcrossBoundary(doc *Node, selection spliceSelection, r Range, with *Node) (*Node, bool, error) {
	if !inlineDocument(with) || samePath(selection.first.path, selection.last.path) {
		return nil, false, nil
	}
	leftPath := selection.first.path[len(selection.parentPath):]
	rightPath := selection.last.path[len(selection.parentPath):]
	if len(leftPath) == 0 || len(rightPath) == 0 {
		return nil, false, nil
	}
	mergedTextblock, ok, err := buildMergedTextblock(doc, selection, r, with)
	if err != nil || !ok {
		return nil, ok, err
	}
	return joinSiblingsAtBoundary(doc, selection.parentPath, leftPath, rightPath, mergedTextblock)
}

// buildMergedTextblock forms the ProseMirror close node: the open (left)
// textblock's type and pre-cut content, the replacement, and the close
// (right) textblock's post-cut content. Joining requires matching types or
// equal attrs, mirroring Fragment.append's compatibility check.
func buildMergedTextblock(doc *Node, selection spliceSelection, r Range, with *Node) (*Node, bool, error) {
	left := nodeAtPath(doc, selection.first.path)
	right := nodeAtPath(doc, selection.last.path)
	if left.Type == right.Type && !attrsEqual(left.Attrs, right.Attrs) {
		return nil, false, nil
	}
	prefix, err := inlineRange(left, selection.first.pos, selection.first.pos+1, r.From)
	if err != nil {
		return nil, false, err
	}
	suffix, err := inlineRange(right, selection.last.pos, r.To, selection.last.end-1)
	if err != nil {
		return nil, false, err
	}
	merged := cloneNode(left)
	merged.Children = nil
	appendInline(&merged.Children, prefix)
	appendInline(&merged.Children, with.Children[0].Children)
	appendInline(&merged.Children, suffix)
	return merged, true, nil
}

// joinSiblingsAtBoundary replaces the shared parent's open/close child range
// with the merged node, followed by the close side's unconsumed remaining
// children re-emitted as siblings (ProseMirror never drops content past the
// close boundary that the selection did not cover).
func joinSiblingsAtBoundary(doc *Node, parentPath, leftPath, rightPath []int, mergedTextblock *Node) (*Node, bool, error) {
	parent := nodeAtPath(doc, parentPath)
	leftIndex, rightIndex := leftPath[0], rightPath[0]
	merged, ok := closeOpenSide(parent.Children[leftIndex], leftPath[1:], mergedTextblock)
	if !ok {
		return nil, false, nil
	}
	closeRemainder := closeSideRemainder(parent.Children[rightIndex], rightPath[1:])
	out := cloneNode(doc)
	outParent := nodeAtPath(out, parentPath)
	children := append([]*Node{}, outParent.Children[:leftIndex]...)
	children = append(children, merged)
	if closeRemainder != nil {
		children = append(children, closeRemainder)
	}
	children = append(children, outParent.Children[rightIndex+1:]...)
	outParent.Children = children
	if err := out.Validate(); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func closeOpenSide(node *Node, path []int, replacement *Node) (*Node, bool) {
	if len(path) == 0 {
		return cloneNode(replacement), true
	}
	index := path[0]
	if index >= len(node.Children) {
		return nil, false
	}
	child, ok := closeOpenSide(node.Children[index], path[1:], replacement)
	if !ok {
		return nil, false
	}
	out := cloneNode(node)
	out.Children = out.Children[:index]
	out.Children = append(out.Children, child)
	return out, true
}

func closeSideRemainder(node *Node, path []int) *Node {
	if len(path) == 0 {
		return nil
	}
	index := path[0]
	if index >= len(node.Children) {
		return nil
	}
	child := closeSideRemainder(node.Children[index], path[1:])
	out := cloneNode(node)
	out.Children = out.Children[:0]
	if child != nil {
		out.Children = append(out.Children, child)
	}
	for _, sibling := range node.Children[index+1:] {
		out.Children = append(out.Children, cloneNode(sibling))
	}
	return normalizeFragment(out)
}

func beforeRange(node *Node, path []int, textblockStart, cut int) (*Node, error) {
	if len(path) == 0 {
		if cut == textblockStart {
			return nil, nil
		}
		children, err := inlineRange(node, textblockStart, textblockStart+1, cut)
		if err != nil || len(children) == 0 {
			return nil, err
		}
		out := cloneNode(node)
		out.Children = children
		return out, nil
	}
	index := path[0]
	out := cloneNode(node)
	out.Children = out.Children[:index]
	child, err := beforeRange(node.Children[index], path[1:], textblockStart, cut)
	if err != nil {
		return nil, err
	}
	if child != nil {
		out.Children = append(out.Children, child)
	}
	if len(out.Children) == 0 {
		return nil, nil
	}
	return out, nil
}

func afterRange(node *Node, path []int, textblockStart, cut int) (*Node, error) {
	if len(path) == 0 {
		if cut == textblockStart+nodeSize(node) {
			return nil, nil
		}
		children, err := inlineRange(node, textblockStart, cut, textblockStart+nodeSize(node)-1)
		if err != nil || len(children) == 0 {
			return nil, err
		}
		out := cloneNode(node)
		out.Children = children
		return out, nil
	}
	index := path[0]
	out := cloneNode(node)
	out.Children = out.Children[index+1:]
	child, err := afterRange(node.Children[index], path[1:], textblockStart, cut)
	if err != nil {
		return nil, err
	}
	if child != nil {
		out.Children = append([]*Node{child}, out.Children...)
	}
	if len(out.Children) == 0 {
		return nil, nil
	}
	return out, nil
}

func emptyDocument(doc *Node) bool {
	return len(doc.Children) == 1 && doc.Children[0].Type == "paragraph" && len(doc.Children[0].Children) == 0
}

func fitReplacement(parent, with, openingListItem *Node, hasPrefixParagraph bool) ([]*Node, bool) {
	var source []*Node
	if !emptyDocument(with) {
		source = make([]*Node, 0, len(with.Children))
		for _, node := range with.Children {
			source = append(source, cloneNode(node))
		}
	}
	switch parent.Type {
	case "doc", "blockquote", "footnote_definition":
		return source, true
	case "bullet_list", "ordered_list":
		if len(source) == 1 && source[0].Type == parent.Type {
			return source[0].Children, true
		}
		items := make([]*Node, 0, len(source))
		for _, node := range source {
			if node.Type == "list_item" {
				items = append(items, node)
				continue
			}
			item := &Node{Type: "list_item", Attrs: listItemAttrs(openingListItem)}
			if node.Type == "paragraph" {
				item.Children = []*Node{node}
			} else {
				item.Children = []*Node{{Type: "paragraph"}, node}
			}
			items = append(items, item)
		}
		return items, true
	case "list_item":
		if len(source) == 0 || source[0].Type == "paragraph" || hasPrefixParagraph {
			return source, true
		}
		return append([]*Node{{Type: "paragraph"}}, source...), true
	default:
		return nil, false
	}
}

func listItemAttrs(item *Node) Attrs {
	if item != nil {
		return cloneAttrs(item.Attrs)
	}
	return Attrs{"label": "•", "listType": "bullet", "checked": nil, "spread": false}
}

func preservedParagraph(fragment, parent *Node) bool {
	return parent.Type == "list_item" && fragment != nil && len(fragment.Children) > 0 && fragment.Children[0].Type == "paragraph"
}

func normalizeFragment(node *Node) *Node {
	if node == nil {
		return nil
	}
	children := node.Children[:0]
	for _, child := range node.Children {
		if child = normalizeFragment(child); child != nil {
			children = append(children, child)
		}
	}
	node.Children = children
	switch node.Type {
	case "blockquote", "bullet_list", "ordered_list", "footnote_definition":
		if len(node.Children) == 0 {
			return nil
		}
	case "list_item":
		if len(node.Children) == 0 {
			return nil
		}
		if node.Children[0].Type != "paragraph" {
			node.Children = append([]*Node{{Type: "paragraph"}}, node.Children...)
		}
	case "table":
		if len(node.Children) == 0 {
			return nil
		}
		if node.Children[0].Type != "table_header_row" {
			node.Children = append([]*Node{{Type: "table_header_row"}}, node.Children...)
		}
	case "table_row":
		if len(node.Children) == 0 {
			return nil
		}
	case "table_cell", "table_header":
		if len(node.Children) == 0 {
			return nil
		}
	}
	return node
}

func inlineRange(block *Node, blockStart, from, to int) ([]*Node, error) {
	if from < blockStart+1 || to > blockStart+nodeSize(block)-1 || from > to {
		return nil, fmt.Errorf("%w: inline range is outside %s", ErrSchema, block.Type)
	}
	var children []*Node
	position := blockStart + 1
	for _, child := range block.Children {
		end := position + nodeSize(child)
		if to <= position {
			break
		}
		if from >= end {
			position = end
			continue
		}
		if child.Type == "text" {
			start := max(from, position) - position
			stop := min(to, end) - position
			appendText(&children, slice16(child.Text, start, stop), child.Marks)
		} else {
			if from > position || to < end {
				return nil, fmt.Errorf("%w: splice partially selects inline leaf", ErrSchema)
			}
			appendInline(&children, []*Node{cloneNode(child)})
		}
		position = end
	}
	return children, nil
}

func inlineDocument(doc *Node) bool {
	if len(doc.Children) != 1 || doc.Children[0].Type != "paragraph" {
		return false
	}
	for _, child := range doc.Children[0].Children {
		if child.Type != "text" && !isLeafNodeType(child.Type) {
			return false
		}
	}
	return true
}

func spliceInline(block *Node, r Range, blockStart int, replacement []*Node) error {
	position := blockStart + 1
	children := make([]*Node, 0, len(block.Children)+len(replacement))
	inserted := false
	for _, child := range block.Children {
		size := nodeSize(child)
		end := position + size
		if r.To <= position || r.From >= end {
			appendInline(&children, []*Node{cloneNode(child)})
			position = end
			continue
		}
		if child.Type == "text" {
			if r.From > position {
				appendText(&children, slice16(child.Text, 0, r.From-position), child.Marks)
			}
			if !inserted {
				for _, node := range replacement {
					appendInline(&children, []*Node{cloneNode(node)})
				}
				inserted = true
			}
			if r.To < end {
				appendText(&children, slice16(child.Text, r.To-position, size), child.Marks)
			}
		} else {
			if r.From > position || r.To < end {
				return fmt.Errorf("%w: splice partially selects inline leaf", ErrSchema)
			}
			if !inserted {
				for _, node := range replacement {
					appendInline(&children, []*Node{cloneNode(node)})
				}
				inserted = true
			}
		}
		position = end
	}
	if !inserted {
		return ErrTargetNotFound
	}
	block.Children = children
	return nil
}

func cloneNode(node *Node) *Node {
	if node == nil {
		return nil
	}
	out := &Node{Type: node.Type, Attrs: cloneAttrs(node.Attrs), Text: node.Text, Marks: cloneMarks(node.Marks)}
	for _, child := range node.Children {
		out.Children = append(out.Children, cloneNode(child))
	}
	return out
}

func cloneAttrs(attrs Attrs) Attrs {
	if len(attrs) == 0 {
		return nil
	}
	out := make(Attrs, len(attrs))
	for key, value := range attrs {
		out[key] = value
	}
	return out
}

func cloneMarks(marks []Mark) []Mark {
	if len(marks) == 0 {
		return nil
	}
	out := make([]Mark, len(marks))
	for index, mark := range marks {
		out[index] = Mark{Type: mark.Type, Attrs: cloneAttrs(mark.Attrs)}
	}
	return out
}
