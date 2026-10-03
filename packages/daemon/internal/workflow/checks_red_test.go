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
// implementing, where the implementer is already at work. Nor does a red on a head a handoff-only
// push reached, whose code is the head it replaced: in reviewing that is the reviewer's own
// handoff head, whose settled verdict the reviewer waits for before it decides, so the round is
// left open, its reviewer running, and the red is the round's to decide. And a round that has
// already decided is ended by its decision before the red rule is asked: a request for changes
// sends the implementer the reviewer's body and counts the round, and no checks-red is told. The
// same holds for the code head's own red carried to the handoff head that replaced it
// (classify.SettlementFor): the round decides it, and nothing is sent back.
func TestARedVerdictInTestingOrReviewingSendsTheTreeBackToImplementing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		from        phase.Phase
		status      string
		planned     bool
		handoffHead bool
		// decided seeds the reviewer's row with a request for changes: both halves of the round in.
		decided bool
		want    phase.Phase
		// settles is the commit the red settlement names: the head, or the code head a handoff-only
		// push replaced.
		settles string
	}{
		{"in testing", phase.Testing, "testing", false, false, false, phase.Implementing, ""},
		{"in reviewing", phase.Reviewing, "needs_review", false, false, false, phase.Implementing, ""},
		{"a planned red in testing", phase.Testing, "testing", true, false, false, phase.Testing, ""},
		{"in implementing", phase.Implementing, "in_progress", false, false, false, phase.Implementing, ""},
		{"on the tester's handoff-only head", phase.Testing, "testing", false, true, false, phase.Testing, ""},
		{"on the reviewer's handoff-only head, its round half in", phase.Reviewing, "needs_review", false, true, false, phase.Reviewing, ""},
		{"on a code head whose round has decided", phase.Reviewing, "needs_review", false, false, true, phase.Implementing, ""},
		{"on the code head, carried to the tester's handoff-only head", phase.Testing, "testing", false, true, false, phase.Testing, "code"},
		{"on the code head, carried to the reviewer's handoff-only head", phase.Reviewing, "needs_review", false, true, false, phase.Reviewing, "code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", PlannedRed: tc.planned, Required: []string{"python-cli-tests / test (pytest)"}}
			if tc.handoffHead {
				pr.Pushes = []record.ClassifiedPush{{SHA: "head", Before: "code", HandoffOnly: true}}
			}
			seedPR(t, pool, pr)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.Reviewing {
				reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "head", Summary: "reviewed"}
				if tc.decided {
					reviewer.Decision = &record.ReviewDecision{State: "changes_requested", Body: "rename the widget", Head: "head"}
				}
				seedPhase(t, pool, reviewer)
			}
			engine := testEngine(config.DesignGateRootIssues, nil)
			settles := tc.settles
			if settles == "" {
				settles = "head"
			}
			if result, err := intake.ApplyFact(context.Background(), pool, "github", "red", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: settles, CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: "red-head", Verdict: "red",
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
				assertOutboxCount(t, pool, "supervise", 0)
				if tc.settles != "" {
					var verdict, checked string
					if err := pool.QueryRow(context.Background(), "select verdict, checked_head from pull_requests where number = 42").Scan(&verdict, &checked); err != nil {
						t.Fatalf("read the pull request's verdict: %v", err)
					}
					if verdict != "red" || checked != tc.settles {
						t.Fatalf("verdict %q of %q, want the carried red of %q", verdict, checked, tc.settles)
					}
				}
				return
			}
			var task string
			if err := pool.QueryRow(context.Background(), "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'").Scan(&task); err != nil {
				t.Fatalf("read the implementer's start: %v", err)
			}
			want := "CI is red at head: python-cli-tests / test (pytest)"
			if tc.decided {
				var rounds int
				if err := pool.QueryRow(context.Background(), "select rounds from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&rounds); err != nil {
					t.Fatalf("read the implementer's rounds: %v", err)
				}
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") || !strings.Contains(task, "rename the widget") || strings.Contains(task, want) || rounds != 1 {
					t.Fatalf("task %q, notices %v, rounds %d; want the reviewer's body, no checks-red and the round counted", task, got, rounds)
				}
				return
			}
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
