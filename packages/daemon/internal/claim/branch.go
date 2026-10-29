package claim

import "strings"

// issueBranchPrefix begins the GitHub branch of every issue Legion works.
const issueBranchPrefix = "legion/"

// IssueBranch is the GitHub branch issue is worked on, legion/<KEY>: the jj bookmark of its
// workspace, the branch the daemon creates for it, and the head branch of its pull request.
func IssueBranch(issue string) string { return issueBranchPrefix + issue }

// BranchIssue is the issue whose branch (IssueBranch) branch is, and whether it is one: a branch
// that is not legion/ followed by an issue key is no issue's, and no pull request Legion records is
// on it, so a GitHub event about it changes nothing the daemon keeps.
func BranchIssue(branch string) (string, bool) {
	issue, ok := strings.CutPrefix(branch, issueBranchPrefix)
	if !ok || !IsIssueKey(issue) {
		return "", false
	}
	return issue, true
}
