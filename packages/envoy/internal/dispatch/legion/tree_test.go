package legion

import "testing"

// TestTreeRootWalksWhileTheIssueAboveIsLegions: the one rule both readers of a tree use — the
// daemon's own `tree` where a read carried one, and otherwise the topmost issue of the parent
// chain that the population holds and counts as Legion's.
func TestTreeRootWalksWhileTheIssueAboveIsLegions(t *testing.T) {
	population := map[string]TreeNode{
		// A chain of four, every one Legion's: a child reaches the top.
		"ACME-1": {Legion: true},
		"ACME-2": {Parent: "ACME-1", Legion: true},
		"ACME-3": {Parent: "ACME-2", Legion: true},
		// Under a tracking issue the population holds but that carries no Legion work.
		"ACME-7": {Legion: false},
		"ACME-8": {Parent: "ACME-7", Legion: true},
		// A Legion issue over a tracking issue over a Legion issue: the walk stops below the
		// first issue above it that is not Legion's, rather than climbing over the gap to the
		// Legion issue above that.
		"ACME-10": {Legion: true},
		"ACME-11": {Parent: "ACME-10", Legion: false},
		"ACME-12": {Parent: "ACME-11", Legion: true},
		// Under an issue the population does not hold at all.
		"ACME-9": {Parent: "ACME-404", Legion: true},
		// A daemon that named the tree itself, from anywhere in the chain.
		"ACME-20": {Tree: "ACME-1", Legion: true},
	}
	lookup := func(key string) (TreeNode, bool) {
		node, known := population[key]
		return node, known
	}

	for _, want := range []struct{ key, root string }{
		{"ACME-1", "ACME-1"},
		{"ACME-3", "ACME-1"},
		{"ACME-8", "ACME-8"},
		{"ACME-12", "ACME-12"},
		{"ACME-9", "ACME-9"},
		{"ACME-20", "ACME-1"},
		{"ACME-404", "ACME-404"},
	} {
		if root := TreeRoot(want.key, lookup); root != want.root {
			t.Errorf("TreeRoot(%s) = %s, want %s", want.key, root, want.root)
		}
	}
}

// TestTreeRootStopsOnACycleRatherThanAtADepthCap: a parent chain that loops, and a `tree` that
// points back into one, each stop where the walk first meets a key it has already seen. A depth
// cap would answer differently for a chain longer than the cap, which is what put one issue in
// two trees when the view and the metrics each held their own walk.
func TestTreeRootStopsOnACycleRatherThanAtADepthCap(t *testing.T) {
	population := map[string]TreeNode{
		"ACME-1": {Parent: "ACME-2", Legion: true},
		"ACME-2": {Parent: "ACME-1", Legion: true},
		"ACME-3": {Tree: "ACME-4", Legion: true},
		"ACME-4": {Tree: "ACME-3", Legion: true},
	}
	// A chain of 64, longer than any depth cap a reader of this rule has held.
	deep := map[string]TreeNode{}
	keys := make([]string, 64)
	for index := range keys {
		keys[index] = "DEEP-" + string(rune('A'+index%26)) + string(rune('a'+index/26))
	}
	for index, key := range keys {
		node := TreeNode{Legion: true}
		if index > 0 {
			node.Parent = keys[index-1]
		}
		deep[key] = node
	}
	lookup := func(key string) (TreeNode, bool) {
		if node, known := population[key]; known {
			return node, true
		}
		node, known := deep[key]
		return node, known
	}

	if root := TreeRoot("ACME-1", lookup); root != "ACME-1" && root != "ACME-2" {
		t.Errorf("TreeRoot over a parent cycle = %s, want one of the two issues in it", root)
	}
	if root := TreeRoot("ACME-3", lookup); root != "ACME-3" && root != "ACME-4" {
		t.Errorf("TreeRoot over a tree cycle = %s, want one of the two issues in it", root)
	}
	if root := TreeRoot(keys[len(keys)-1], lookup); root != keys[0] {
		t.Errorf("TreeRoot over a chain of %d = %s, want its top %s", len(keys), root, keys[0])
	}
}
