package workflow

import (
	"context"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The pass lists GitHub before it applies what it found, and the record can change in between, so
// the workflow tells the architect a pull request is missing only while the issue still waits for
// one at the completion the pass named: in implementing, its implementer's handoff at that commit,
// and no open or merged pull request recorded. A pull request recorded closed is not an open one;
// one recorded merged told the architect (pr-merged), and a completion moves the issue on it.
func TestAMissingPullRequestIsToldOnlyWhileTheCompletionWaitsForOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		phase    phase.Phase
		handoff  string
		recorded record.PullRequestState
		told     bool
	}{
		{"the completion waits with no pull request", phase.Implementing, "handoff", "", true},
		{"the completion waits with its pull request closed", phase.Implementing, "handoff", record.PullRequestClosed, true},
		{"an open pull request is recorded since", phase.Implementing, "handoff", record.PullRequestOpen, false},
		{"its pull request merged since", phase.Implementing, "handoff", record.PullRequestMerged, false},
		{"the implementer completed again at another commit", phase.Implementing, "later", "", false},
		{"the issue moved on", phase.Testing, "handoff", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
				Title: "root", Phase: tc.phase, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: tc.handoff})
			if tc.recorded != "" {
				seedPR(t, pool, record.PullRequest{State: tc.recorded, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "handoff", Failing: []string{}})
			}
			fact := intake.PullRequestMissing{Issue: "LEGION-208", Repo: "sjawhar/legion", Branch: "legion/LEGION-208", HandoffCommit: "handoff"}
			if _, err := intake.ApplyFact(context.Background(), pool, "github", "missing", fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the missing pull request: %v", err)
			}
			if told := slices.Contains(noticeKinds(t, pool, "LEGION-208"), "pull-request-missing"); told != tc.told {
				t.Fatalf("pull-request-missing told: %v, want %v (notices %v)", told, tc.told, noticeKinds(t, pool, "LEGION-208"))
			}
		})
	}
}
