// Ordinary, always-visible unit tests for the Stage 4a cache checks' pure logic that
// live_cache_pure_test.go defines with no e2e build tag.
package sandbox

import (
	"slices"
	"testing"
	"time"

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

// A run killed past its test timeout skips its teardown and leaves its cache-b branch behind.
// Each run sweeps such leftovers first: a branch of cache-b's shape whose commit is the harness's
// own and older than any run lives (staleRunBranchAge). A run still in flight has a younger
// commit, so no live run's branch is taken, and nothing the harness did not write is either.
func TestASweepTakesOnlyTheHarnessesOwnBranchesOlderThanAnyRun(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)
	old, young := now.Add(-staleRunBranchAge-time.Minute), now.Add(-time.Hour)
	own := func(name string) string { return runCommitMessage(name) }
	branches := []runBranch{
		{"legion/S4A-1861345169", own("legion/S4A-1861345169"), old},
		{"legion/S4A-9", own("legion/S4A-9"), old},
		{"legion/S4A-2050000000", own("legion/S4A-2050000000"), young},
		{"legion/S4A-1111111111", "a person's commit", old},
		{"legion/S4A-1222222222-extra", own("legion/S4A-1222222222-extra"), old},
		{"legion/LEGSMOKE-3", own("legion/LEGSMOKE-3"), old},
	}
	got := staleRunBranches(branches, now)
	if want := []string{"legion/S4A-1861345169", "legion/S4A-9"}; !slices.Equal(got, want) {
		t.Fatalf("staleRunBranches = %q, want %q", got, want)
	}
}
