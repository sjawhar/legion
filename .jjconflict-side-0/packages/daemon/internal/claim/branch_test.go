package claim

import "testing"

// An issue's branch and the issue of a branch are one rule read in both directions: every branch
// IssueBranch builds names its issue again, and a branch of any other shape is no issue's, so the
// daemon neither records nor acts on a pull request or a push there.
func TestIssueBranchAndBranchIssueAreOneRule(t *testing.T) {
	for _, issue := range []string{"ACME-1", "ACME-208", "A1-9000"} {
		branch := IssueBranch(issue)
		if branch != "legion/"+issue {
			t.Fatalf("IssueBranch(%q) = %q, want legion/%s", issue, branch, issue)
		}
		if got, ok := BranchIssue(branch); !ok || got != issue {
			t.Fatalf("BranchIssue(%q) = %q, %t; want %q, true", branch, got, ok, issue)
		}
	}
	for _, branch := range []string{
		"main",
		"legion",
		"legion/",
		"legion/main",
		"legion/acme-1",
		"legion/ACME-1/x",
		"legion/ACME-",
		"legion/-1",
		"release/legion/ACME-1",
		"",
	} {
		if issue, ok := BranchIssue(branch); ok {
			t.Fatalf("BranchIssue(%q) = %q, true; want no issue", branch, issue)
		}
	}
}
