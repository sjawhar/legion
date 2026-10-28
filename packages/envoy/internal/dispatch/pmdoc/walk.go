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
func walk(doc *Node, visit func(n *Node, path []int, pos, end int) bool) {
	if doc == nil || visit == nil {
		return
	}
	walkNode(doc, nil, 0, visit)
}

func walkNode(node *Node, path []int, pos int, visit func(n *Node, path []int, pos, end int) bool) bool {
	end := pos + nodeSize(node)
	if !visit(node, path, pos, end) {
		return false
	}
	if isOpaqueNodeType(node.Type) {
		return true
	}

	childPos := pos + boundaryTokens(node.Type)/2
	for index, child := range node.Children {
		if !walkNode(child, append(path, index), childPos, visit) {
			return false
		}
		childPos += nodeSize(child)
	}
	return true
}

func nodeSize(node *Node) int {
	if node == nil {
		return 0
	}
	if isOpaqueNodeType(node.Type) {
		return opaqueNodeSize(node.Type, node.Text)
	}
	size := boundaryTokens(node.Type)
	for _, child := range node.Children {
		size += nodeSize(child)
	}
	return size
}

// isOpaqueNodeType reports whether a node of nodeType is one walk visits without entering its
// children, which take no position: a text run or a leaf.
func isOpaqueNodeType(nodeType string) bool {
	return nodeType == "text" || isLeafNodeType(nodeType)
}

// opaqueNodeSize is the size of an opaque node (isOpaqueNodeType) of nodeType holding text: a text
// run is its text's UTF-16 length, a leaf one position.
func opaqueNodeSize(nodeType, text string) int {
	if nodeType == "text" {
		return len16(text)
	}
	return 1
}

// boundaryTokens is the positions a node of nodeType that is not opaque adds around its children:
// an opening and a closing token, so its children start one past its own position, except for the
// document, which has neither.
func boundaryTokens(nodeType string) int {
	if nodeType == "doc" {
		return 0
	}
	return 2
}

func isLeafNodeType(nodeType string) bool {
	switch nodeType {
	case "hr", "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}
