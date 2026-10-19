// Ordinary, always-visible unit tests for the Stage 4a cache checks' pure logic that
// live_cache_pure_test.go defines with no e2e build tag.
package sandbox

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// cache-lost-volume pushes cache-b's branch onto the run's repository, which every Stage 4a run
// shares, so two runs at once must push different branches: cache-b's issue, which names the
// branch, is its own run's. Each run's key is one the claim token takes, and the same run always
// gets the same key, so the run's checks and its teardown agree on the branch.
func TestEachRunsCacheIssueNamesABranchOfItsOwn(t *testing.T) {
	runs := []string{"s4a-20261019144300-4108", "s4a-20261019144300-4109", "s4a-20261019145627-1308"}
	branches := map[string]string{}
	for _, project := range runs {
		issue := cacheRunIssue(project)
		if !claim.IsIssueKey(issue) {
			t.Fatalf("cacheRunIssue(%q) = %q, not an issue key", project, issue)
		}
		if _, err := claim.NewToken("s4a20261019144300", issue, claim.RoleArchitect); err != nil {
			t.Fatalf("cacheRunIssue(%q) = %q, which a claim token refuses: %v", project, issue, err)
		}
		if again := cacheRunIssue(project); again != issue {
			t.Fatalf("cacheRunIssue(%q) is %q, then %q", project, issue, again)
		}
		branch := workspace.Bookmark(issue)
		if other, taken := branches[branch]; taken {
			t.Fatalf("runs %s and %s both push %s", other, project, branch)
		}
		branches[branch] = project
	}
}
