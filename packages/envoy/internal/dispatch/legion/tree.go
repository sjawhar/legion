package legion

// tree.go holds the one rule for an issue's tree root (dispatch://INFRA-1077). The Legion view and
// the four metrics both place an issue in a tree — the view to tell a root from a child and to
// count a root's unstarted children, the metrics to scope a filing and a root's wait on its
// children — and one rule stated twice is two rules: a board that puts an issue in one tree while
// the metrics put it in another is a view and a card that disagree about the same issue.

// TreeNode is what the walk reads of one issue: the tree root the daemon named for it (empty where
// no read carried one, which is every daemon before dispatch://INFRA-980), the parent Dispatch
// records, and whether the walk may climb through it from a child below.
//
// Legion is what each population answers for itself: every issue the daemon reports is Legion's
// (the board's population holds no other), and in the metrics' population, which holds every issue
// of the project, it is an issue that carries Legion phase work. A chain stops below the first
// issue that is not Legion's, which is what keeps a tree from climbing into the tracking issues
// above it.
type TreeNode struct {
	Tree   string
	Parent string
	Legion bool
}

// TreeRoot is the issue's tree root: the `tree` the daemon named where a read carried one, and
// otherwise the topmost issue of its parent chain that is Legion's and that lookup knows. An issue
// lookup does not know, one with no parent, and one whose parent is not Legion's are each a root
// of their own. Nothing here reads a slot: a slot holder is not a root.
//
// lookup answers what the population holds for a key, and false for a key it does not hold. The
// walk keeps the keys it has visited, so a parent chain that loops and a `tree` that points back
// into the chain both stop rather than running to a depth cap.
func TreeRoot(key string, lookup func(key string) (TreeNode, bool)) string {
	seen := map[string]bool{}
	for current := key; ; {
		if seen[current] {
			return current
		}
		seen[current] = true
		node, known := lookup(current)
		if !known {
			return current
		}
		if node.Tree != "" {
			if node.Tree == current || seen[node.Tree] {
				return node.Tree
			}
			current = node.Tree
			continue
		}
		if node.Parent == "" {
			return current
		}
		if above, known := lookup(node.Parent); !known || !above.Legion {
			return current
		}
		current = node.Parent
	}
}
