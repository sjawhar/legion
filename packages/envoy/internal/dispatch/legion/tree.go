package legion

// tree.go holds the one rule for an issue's tree root (dispatch://INFRA-1077). The Legion view and
// the four metrics both place an issue in a tree — the view to tell a root from a child and to
// count a root's unstarted children, the metrics to scope a filing and a root's wait on its
// children — and one rule stated twice is two rules: a board that puts an issue in one tree while
// the metrics put it in another is a view and a card that disagree about the same issue.

// TreeNode is what the walk reads of one issue: the tree root the daemon named for it, the parent
// Dispatch records, and whether the walk may climb through it from a child below.
//
// Tree is the daemon's own `tree` for the issue, which names its top issue outright. It is a fact
// of the project and not of any window, so a population answers it for every issue the daemon
// named one for, not for the issues one report happens to read. It is empty where no read carried
// one, which is every daemon before dispatch://INFRA-980.
//
// Legion is one rule, the same for every reader: an issue is a Legion issue when the daemon
// reports it — it is in the daemon's state, or Dispatch observed a phase of it — or when it
// carries Legion phase work, a worker event or a handoff (dispatch://INFRA-1077's spec, "The top
// issue of a tree"). A population answers it for the issues it holds and false for the rest; it
// is not the rule that picks which finished issues a metric counts, which needs phase work. A
// chain stops below the first issue that is not a Legion issue, which is what keeps a tree from
// climbing into the tracking issues above it.
//
// Both halves are facts of the project and not of any window, so both readers read them
// project-wide: the daemon's reports from the whole of what the sampler recorded, and the phase
// work from one read of every handoff and every Legion role session's event (PhaseWorkKeys).
// A window narrows which issues a metric measures, never which issues are Legion's.
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
// lookup answers what the population holds for a key, and false for a key it does not hold.
func TreeRoot(key string, lookup func(key string) (TreeNode, bool)) string {
	chain := TreeChain(key, lookup)
	return chain[len(chain)-1]
}

// TreeChain is the walk TreeRoot makes, kept: the issue itself, then every issue the walk climbs
// through, ending at the tree root, which is why TreeRoot is its last entry. A reader that must
// place something on the nearest issue above another — the Legion view counts an unstarted child
// on the nearest issue of its tree that carries a card — reads the chain rather than stating the
// climb a second time.
//
// The walk keeps the keys it has visited, so a parent chain that loops and a `tree` that points
// back into the chain both stop rather than running to a depth cap.
func TreeChain(key string, lookup func(key string) (TreeNode, bool)) []string {
	chain := []string{key}
	seen := map[string]bool{}
	for {
		current := chain[len(chain)-1]
		if seen[current] {
			return chain
		}
		seen[current] = true
		node, known := lookup(current)
		if !known {
			return chain
		}
		if node.Tree != "" {
			if node.Tree == current {
				return chain
			}
			chain = append(chain, node.Tree)
			if seen[node.Tree] {
				return chain
			}
			continue
		}
		if node.Parent == "" {
			return chain
		}
		if above, known := lookup(node.Parent); !known || !above.Legion {
			return chain
		}
		chain = append(chain, node.Parent)
	}
}
