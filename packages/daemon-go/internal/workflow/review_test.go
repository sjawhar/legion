package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A review ends when the reviewer completes it, not when GitHub reports it: the reviewer's handoff
// is part of its phase's contract, and a transition fired on the review event left it nowhere to
// land — the issue had already moved on, so the reviewer's completion was refused as a phase it no
// longer runs and its review.json never recorded. GitHub's decision and the reviewer's completion
// can arrive in either order; whichever lands second moves the issue.
func TestAReviewEndsWhenTheReviewerCompletesIt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		decision      string
		verdict       string
		completeFirst bool
		want          phase.Phase
	}{
		{name: "changes requested, then the reviewer completes", decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then changes are requested", decision: "changes_requested", completeFirst: true, want: phase.Implementing},
		{name: "approved on a green head, then the reviewer completes", decision: "approved", verdict: "green", want: phase.Retro},
		{name: "the reviewer completes, then an approval on a green head", decision: "approved", verdict: "green", completeFirst: true, want: phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, tc.verdict)
			engine := testEngine()
			review := func() {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, pool, "github", "review", intake.PullRequestReview{
					Repo: "sjawhar/legion", Number: 42, State: tc.decision, CommitID: "head", Body: "the review"}, engine); err != nil {
					t.Fatalf("apply the review: %v", err)
				}
			}
			complete := func() {
				t.Helper()
				result, err := intake.ApplyFact(ctx, pool, "api", "reviewer-handoff", intake.HandoffComplete{Generation: 1,
					Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("the reviewer's completion = %+v, %v; want it recorded", result.Refusal, err)
				}
			}
			if tc.completeFirst {
				complete()
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after the completion alone the issue is in %s, want reviewing until GitHub reports the decision", got)
				}
				review()
			} else {
				review()
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after the review alone the issue is in %s, want reviewing until the reviewer completes", got)
				}
				complete()
			}

			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			var commit string
			if err := pool.QueryRow(ctx, "select handoff_commit from phases where issue = $1 and role = $2",
				"LEGION-208", "reviewer").Scan(&commit); err != nil {
				t.Fatal(err)
			}
			if commit != "review-1" {
				t.Fatalf("the reviewer's handoff commit = %q, want its completion's review-1", commit)
			}
		})
	}
}

// An approval does not end the review while the head's checks have not come back green: the
// reviewer's completion waits for them as the approval always did, and their settling moves it.
func TestAnApprovalWaitsForGreenChecksAfterTheReviewerCompletes(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "")
	engine := testEngine()
	if _, err := intake.ApplyFact(ctx, pool, "github", "review", intake.PullRequestReview{
		Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head"}, engine); err != nil {
		t.Fatalf("apply the review: %v", err)
	}
	if result, err := intake.ApplyFact(ctx, pool, "api", "reviewer-handoff", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("the reviewer's completion = %+v, %v", result.Refusal, err)
	}
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("with the checks unsettled the issue is in %s, want reviewing", got)
	}
}

func seedReview(t *testing.T, pool *pgxpool.Pool, verdict string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Verdict: verdict})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
}

func issuePhase(t *testing.T, pool *pgxpool.Pool) phase.Phase {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = $1", "LEGION-208").Scan(&got); err != nil {
		t.Fatal(err)
	}
	return phase.Phase(got)
}

// An approval is of the head it approved. The reviewer's own handoff push after it changes only
// .legion/, so the approval stands for that head once its checks are green; any other push while
// the review is open — or one whose paths are not known — is code the reviewer never approved, and
// the reviewer's completion then ends nothing.
func TestAnApprovalStandsOnlyForHeadsThatChangeNothingButTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths string
		want  phase.Phase
	}{
		{name: "the reviewer's handoff push", paths: ".legion/review.json", want: phase.Retro},
		{name: "a push that changes code", paths: ".legion/review.json\nsrc/widget.go", want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine()
			if _, err := intake.ApplyFact(ctx, pool, "github", "review", intake.PullRequestReview{
				Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head"}, engine); err != nil {
				t.Fatalf("apply the review: %v", err)
			}
			paths, truncated := tc.paths, "false"
			if _, err := intake.ApplyFact(ctx, pool, "github", "push", intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208",
				After: "head-2", ChangedPaths: &paths, Truncated: &truncated, Pusher: "legion-reviewer[bot]"}, engine); err != nil {
				t.Fatalf("apply the push: %v", err)
			}
			if _, err := intake.ApplyFact(ctx, pool, "github", "checks-green", intake.PullRequestChecks{
				Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-2", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}},
				Generation: 2, Snapshot: "green-2", Verdict: "green", Failing: []string{}}, engine); err != nil {
				t.Fatalf("apply the checks: %v", err)
			}
			if result, err := intake.ApplyFact(ctx, pool, "api", "reviewer-handoff", intake.HandoffComplete{Generation: 1,
				Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}, engine); err != nil || result.Refusal != nil {
				t.Fatalf("the reviewer's completion = %+v, %v", result.Refusal, err)
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
		})
	}
}
