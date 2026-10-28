package pmdoc

// Walk visits every node in document order without calculating positions. Returning false stops the
// whole traversal, not just the node's subtree: a visitor that only means to pass over a node's
// children returns true and ignores them as they are visited.
func Walk(doc *Node, visit func(*Node) bool) {
	if doc == nil || visit == nil {
		return
	}
	walkNodes(doc, visit)
}

func walkNodes(node *Node, visit func(*Node) bool) bool {
	if !visit(node) {
		return false
	}
	for _, child := range node.Children {
		if !walkNodes(child, visit) {
			return false
		}
	}
	return true
}

// walk visits every node in document order. pos and end are the node's
// half-open ProseMirror range; path indexes the node from doc.
//
// Paths are valid only for the duration of visit. Returning false stops the
// traversal.
//
// Sizes are measured before the first visit, so a visitor must not change the
// Text or Children of nodes the walk has not yet visited.
func walk(doc *Node, visit func(n *Node, path []int, pos, end int) bool) {
	if doc == nil || visit == nil {
		return
	}
	// Every subtree is measured once, before the first visit, so the walk is linear in its nodes
	// rather than in nodes times depth. walkedCount sizes the slice exactly, so it is allocated once
	// and never grown.
	sizes := make([]int, 0, walkedCount(doc))
	measure(doc, &sizes)
	walkNode(doc, nil, 0, sizes, 0, visit)
}

// walkNode visits node, the index'th node of the walk in pre-order, and its subtree. sizes holds
// every visited node's size by its pre-order index, as measure records them. It returns the index of
// the node after node's subtree, and false once a visitor has stopped the walk.
func walkNode(node *Node, path []int, pos int, sizes []int, index int, visit func(n *Node, path []int, pos, end int) bool) (int, bool) {
	end := pos + sizes[index]
	if !visit(node, path, pos, end) {
		return 0, false
	}
	index++
	if node.Type == "text" || isLeafNodeType(node.Type) {
		return index, true
	}

	childPos := pos
	if node.Type != "doc" {
		childPos++
	}
	for childIndex, child := range node.Children {
		size := sizes[index]
		var ok bool
		if index, ok = walkNode(child, append(path, childIndex), childPos, sizes, index, visit); !ok {
			return 0, false
		}
		childPos += size
	}
	return index, true
}

func nodeSize(node *Node) int {
	return measure(node, nil)
}

// measure returns node's ProseMirror size. With sizes non-nil it also appends the size of node and
// of every node the walk descends to below it, in pre-order, which is the order walk visits them:
// node's slot is reserved before its children are measured and filled once their sizes are summed.
// nodeSize and walk share this one rule, so a node's size is the same wherever it is asked for.
func measure(node *Node, sizes *[]int) int {
	slot := 0
	if sizes != nil {
		slot = len(*sizes)
		*sizes = append(*sizes, 0)
	}
	size := 0
	switch {
	case node == nil:
	case node.Type == "text":
		size = len16(node.Text)
	case isLeafNodeType(node.Type):
		size = 1
	default:
		if node.Type != "doc" {
			size = 2
		}
		for _, child := range node.Children {
			size += measure(child, sizes)
		}
	}
	if sizes != nil {
		(*sizes)[slot] = size
	}
	return size
}

// walkedCount is how many nodes walk visits from node down, node included: measure's descent,
// counted without measuring, so walk sizes its slice once. A wrong count would cost only a regrowth
// of the slice, never a wrong size.
func walkedCount(node *Node) int {
	count := 1
	if node == nil || node.Type == "text" || isLeafNodeType(node.Type) {
		return count
	}
	for _, child := range node.Children {
		count += walkedCount(child)
	}
	return count
}

func isLeafNodeType(nodeType string) bool {
	switch nodeType {
	case "hr", "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}
