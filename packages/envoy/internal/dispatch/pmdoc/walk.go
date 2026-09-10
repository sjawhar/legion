package pmdoc

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
	if node.Type == "text" || isLeafNodeType(node.Type) {
		return true
	}

	childPos := pos
	if node.Type != "doc" {
		childPos++
	}
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
	if node.Type == "text" {
		return len16(node.Text)
	}
	if isLeafNodeType(node.Type) {
		return 1
	}
	size := 0
	if node.Type != "doc" {
		size = 2
	}
	for _, child := range node.Children {
		size += nodeSize(child)
	}
	return size
}

func isLeafNodeType(nodeType string) bool {
	switch nodeType {
	case "hr", "hardbreak", "image", "html", "footnote_reference":
		return true
	default:
		return false
	}
}
