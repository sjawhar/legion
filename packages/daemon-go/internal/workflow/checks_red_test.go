package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A red CI verdict settled on the head while the issue is in testing or reviewing is a regression
// the implementer has to fix: nothing else moves the tree, since the tester's pass and the
// reviewer's decision are both of code CI has now found broken. The issue goes back to
// implementing, the implementer's task names the failing checks, and the architect is told with
// the same checks. Its next push is then a counted fix attempt, as a red verdict makes any new head.
// A red the review App planned (its failing tests) sends nothing back, and neither does a red in
// implementing, where the implementer is already at work.
func TestARedVerdictInTestingOrReviewingSendsTheTreeBackToImplementing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		from    phase.Phase
		status  string
		planned bool
		want    phase.Phase
	}{
		{"in testing", phase.Testing, "testing", false, phase.Implementing},
		{"in reviewing", phase.Reviewing, "needs_review", false, phase.Implementing},
		{"a planned red in testing", phase.Testing, "testing", true, phase.Testing},
		{"in implementing", phase.Implementing, "in_progress", false, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", PlannedRed: tc.planned})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			if result, err := intake.ApplyFact(context.Background(), pool, "github", "red", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: "red-head", Verdict: "red",
				Failing: []string{"python-cli-tests / test (pytest)"}}, engine); err != nil || result.Refusal != nil {
				t.Fatalf("apply the red verdict = %+v, %v", result.Refusal, err)
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the red verdict the issue is in %s, want %s", got, tc.want)
			}
			if tc.want == tc.from {
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
					t.Fatalf("notices %v, want no checks-red", got)
				}
				return
			}
			var task string
			if err := pool.QueryRow(context.Background(), "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'").Scan(&task); err != nil {
				t.Fatalf("read the implementer's start: %v", err)
			}
			want := "CI is red at head: python-cli-tests / test (pytest)"
			var reason string
			if err := pool.QueryRow(context.Background(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason); err != nil {
				t.Fatalf("read the architect's checks-red notice: %v", err)
			}
			if !strings.Contains(task, want) || !strings.Contains(reason, want) {
				t.Fatalf("task %q and notice %q; want both to say %q", task, reason, want)
			}
		})
	}
}
