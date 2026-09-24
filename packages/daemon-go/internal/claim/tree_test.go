package claim

import "testing"

// Where a claim sits in its tree, as the predicates' callers meet it: the tree's own issue, and
// children whose keys resemble the tree's closely enough that a prefix or suffix comparison, or
// one that ignored the project, would call them the root.
var treePlacements = []struct {
	name  string
	issue string
	tree  string
	root  bool
}{
	{name: "the tree's own issue", issue: "LEGION-208", tree: "LEGION-208", root: true},
	{name: "a child issue", issue: "LEGION-231", tree: "LEGION-208", root: false},
	{name: "a child whose key extends the tree's", issue: "LEGION-2081", tree: "LEGION-208", root: false},
	{name: "a child whose key the tree's extends", issue: "LEGION-20", tree: "LEGION-208", root: false},
	{name: "the tree's number in another project", issue: "WIDGETS-208", tree: "LEGION-208", root: false},
}

var everyRole = []Role{RoleArchitect, RolePlanner, RoleImplementer, RoleTester, RoleReviewer, RoleMerger}

// The root is a fact about the issue, whichever role asks: a planner on the tree's own issue is
// on the root issue, and an architect on a child is not.
func TestTheTreeRootIsTheIssueTheTreeIsNamedFor(t *testing.T) {
	for _, place := range treePlacements {
		if got := IsTreeRoot(place.issue, place.tree); got != place.root {
			t.Errorf("%s: IsTreeRoot(%q, %q) = %t, want %t", place.name, place.issue, place.tree, got, place.root)
		}
	}
}

// The tree architect is exactly one claim per tree: the architect on the tree's own issue. A
// sub-architect on a child is not it, and neither is any worker on the root issue.
func TestOnlyTheArchitectOfTheRootIssueIsTheTreeArchitect(t *testing.T) {
	for _, place := range treePlacements {
		for _, role := range everyRole {
			want := role == RoleArchitect && place.root
			if got := IsTreeArchitect(role, place.issue, place.tree); got != want {
				t.Errorf("%s as %s: IsTreeArchitect = %t, want %t", place.name, role, got, want)
			}
		}
	}
}
