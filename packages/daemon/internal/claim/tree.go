package claim

// IsTreeRoot is whether the issue is its tree's root. A tree is named for the issue it grew from,
// so the root is the one issue whose key is the tree's, whichever role is on it.
func IsTreeRoot(issue, tree string) bool { return issue == tree }

// IsTreeArchitect is whether a claim is the architect of its tree's own issue: the tree's root
// claim, one per tree, which ends only when the tree closes. An architect on a child issue is a
// sub-architect, and a worker on the root issue is not the root claim.
func IsTreeArchitect(role Role, issue, tree string) bool {
	return role == RoleArchitect && IsTreeRoot(issue, tree)
}
