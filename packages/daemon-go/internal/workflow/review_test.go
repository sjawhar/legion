package workflow

import (
	"context"
	"fmt"
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

// An approval is of the code the head it names carries. The reviewer approves the head it was
// shown - the tester's handoff head - and its own handoff push then replaces that head, while the
// approval event and the push arrive by different webhook paths in no promised order. So an
// approval stands for the current head when every push since the head it names changed only
// .legion/, whichever of the approval, the new head and the push's paths is processed first. A
// push that changes code, or whose changed paths are not known, starts again: an approval of any
// earlier head then approves nothing. A pull request recorded before the daemon kept that chain
// has none, so only an approval of its current head stands.
func TestAnApprovalStandsForEveryHeadThatChangesNothingButTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
		want  phase.Phase
	}{
		{name: "approval, then the reviewer's handoff push", steps: []string{"approve head", "sync", "push handoff", "green", "complete"}, want: phase.Retro},
		{name: "the reviewer's handoff push, then the approval", steps: []string{"sync", "push handoff", "green", "approve head", "complete"}, want: phase.Retro},
		{name: "the push's paths arrive after the new head and the approval", steps: []string{"sync", "approve head", "green", "complete", "push handoff"}, want: phase.Retro},
		{name: "the push's paths arrive before the new head", steps: []string{"push handoff", "approve head", "sync", "green", "complete"}, want: phase.Retro},
		{name: "a new head whose push never arrives", steps: []string{"sync", "approve head", "green", "complete"}, want: phase.Reviewing},
		{name: "a code push, then the approval of the head before it", steps: []string{"sync", "push code", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "the approval, then a code push", steps: []string{"approve head", "sync", "push code", "green", "complete"}, want: phase.Reviewing},
		{name: "a push of unknown paths, then the approval of the head before it", steps: []string{"sync", "push unknown", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "recorded before the chain: an approval of the current head", steps: []string{"approve head", "complete"}, want: phase.Retro},
		{name: "recorded before the chain: an approval of an earlier head", steps: []string{"approve older", "complete"}, want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine()
			for i, step := range tc.steps {
				var fact intake.Fact
				switch step {
				case "approve head", "approve older":
					commit := "head"
					if step == "approve older" {
						commit = "older"
					}
					fact = intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: commit}
				case "sync":
					fact = intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"}
				case "push handoff", "push code", "push unknown":
					paths, truncated := ".legion/review.json", "false"
					switch step {
					case "push code":
						paths = ".legion/review.json\nsrc/widget.go"
					case "push unknown":
						truncated = "true"
					}
					fact = intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", After: "head-2",
						ChangedPaths: &paths, Truncated: &truncated, Pusher: "legion-reviewer[bot]"}
				case "green":
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head-2",
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 2, Snapshot: "green-2", Verdict: "green", Failing: []string{}}
				case "complete":
					fact = intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
						Summary: "reviewed", Commit: "review-1"}
				}
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("%d-%s", i, step), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("%s = %+v, %v", step, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
		})
	}
}
