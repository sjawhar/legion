package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The live shape a round got stuck on: the repository's one required gate passed, and two
// workflow_dispatch lanes and an advisory review check failed beside it.
var (
	requiredGate      = "pr-checks-result"
	redsBesideTheGate = []string{"dev-apply / dev-chain-tripwire", "dev-apply / staging-e2e / staging-e2e", "review"}
	runsBesideTheGate = []record.AttemptRun{{Name: "dev-apply / dev-chain-tripwire", ID: 2}, {Name: "dev-apply / staging-e2e / staging-e2e", ID: 3}, {Name: requiredGate, ID: 1}, {Name: "review", ID: 4}}
)

// applyRefusingNothing applies each fact as its own event, and fails the test on a refusal.
func applyRefusingNothing(t *testing.T, pool *pgxpool.Pool, engine *Engine, facts ...intake.Fact) {
	t.Helper()
	apply := applyFacts(t, pool, engine)
	for _, fact := range facts {
		if result := apply(t.Name()+randomSuffix(t), fact); result.Refusal != nil {
			t.Fatalf("apply %T = %+v", fact, result.Refusal)
		}
	}
}

// implementerTask is the task the implementer was last started with, "" when it was not started.
func implementerTask(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	return startTask(t, pool, claim.RoleImplementer)
}

// implementerPass is the implementer's count of the issue's moves back into an earlier phase.
func implementerPass(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var pass int
	if err := pool.QueryRow(context.Background(), "select pass from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&pass); err != nil {
		t.Fatalf("read the implementer's pass: %v", err)
	}
	return pass
}

// implementerRounds is the implementer's count of the review rounds a review ended by requesting
// changes, which review_round_cap bounds.
func implementerRounds(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var rounds int
	if err := pool.QueryRow(context.Background(), "select rounds from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&rounds); err != nil {
		t.Fatalf("read the implementer's rounds: %v", err)
	}
	return rounds
}

// Only a check the base branch requires makes CI red at a head: a red beside a passing required
// gate sends nothing back from testing or reviewing, and a required check that failed does and
// names only itself.
func TestOnlyARedTheBaseBranchRequiresSendsTheWorkBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from     phase.Phase
		status   string
		required []string
		failing  []string
		runs     []record.AttemptRun
		// red is what the implementer's task and the checks-red notice say; "" sends nothing back.
		red string
	}{
		{"reds beside the required gate, in testing", phase.Testing, "testing", []string{requiredGate}, redsBesideTheGate, runsBesideTheGate, ""},
		{"reds beside the required gate, in reviewing", phase.Reviewing, "needs_review", []string{requiredGate}, redsBesideTheGate, runsBesideTheGate, ""},
		{"the required gate failed beside them", phase.Testing, "testing", []string{requiredGate}, append([]string{requiredGate}, redsBesideTheGate...), runsBesideTheGate,
			"CI is red at head: pr-checks-result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: tc.required})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: "head", CheckRuns: tc.runs, Generation: 1, Snapshot: "settled", Failing: tc.failing})
			if tc.red == "" {
				if got := issuePhase(t, pool); got != tc.from {
					t.Fatalf("the issue is in %s, want it left in %s", got, tc.from)
				}
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
					t.Fatalf("notices %v, want no checks-red", got)
				}
				assertOutboxCount(t, pool, "supervise", 0)
				return
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("the issue is in %s, want implementing", got)
			}
			var reason string
			if err := pool.QueryRow(context.Background(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason); err != nil {
				t.Fatalf("read the architect's checks-red notice: %v", err)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, tc.red) || !strings.HasSuffix(reason, tc.red) {
				t.Fatalf("task %q and notice %q; want both to end %q, naming no check the base branch does not require", task, reason, tc.red)
			}
		})
	}
}

// A settlement can come before a required check is decided: the listener settles a commit once
// every check it has seen is terminal and the commit is quiet, which can be before an aggregator
// job with needs: is queued, or just after concurrency cancelled a run of the same commit. So a
// required check the head's own settlement names cancelled, or does not report, leaves the head
// undecided: it sends nothing back in any phase and tells nobody, and the head's next settlement
// decides it - failed, the work goes back naming it. The round reads none of it: a reviewer's
// approval of the head ends the round with the check still undecided, and a head with no verdict
// is sent nowhere; whether such a head merges is GitHub's to decide, under the base branch's own
// rules.
func TestARequiredCheckCancelledOrMissingLeavesTheHeadUndecided(t *testing.T) {
	gateRun := func(id int64) record.AttemptRun { return record.AttemptRun{Name: requiredGate, ID: id} }
	missing := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}}, Failing: []string{}}
	cancelled := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{gateRun(1)}, Failing: []string{}, Cancelled: []string{requiredGate}}
	failed := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}, gateRun(2)}, Failing: []string{requiredGate}}
	approval := []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", Author: testReviewApp, Body: "looks right"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "approved", Commit: "review-1"},
	}
	for _, tc := range []struct {
		name   string
		from   phase.Phase
		status string
		first  intake.PullRequestChecks
		// then is what follows the undecided settlements: the head's next settlement, or the
		// reviewer's approval and completion.
		then []intake.Fact
		want phase.Phase
	}{
		{"missing, then failed, in testing", phase.Testing, "testing", missing, []intake.Fact{failed}, phase.Implementing},
		{"cancelled, then failed, in testing", phase.Testing, "testing", cancelled, []intake.Fact{failed}, phase.Implementing},
		{"missing, then failed, in reviewing", phase.Reviewing, "needs_review", missing, []intake.Fact{failed}, phase.Implementing},
		{"missing, then the reviewer approves the head", phase.Reviewing, "needs_review", missing, approval, phase.Accepting},
		{"cancelled, then the reviewer approves the head", phase.Reviewing, "needs_review", cancelled, approval, phase.Accepting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{requiredGate}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
			settle := func(fact intake.PullRequestChecks, generation int64) intake.Fact {
				fact.Repo, fact.Number, fact.HeadSHA, fact.Generation = "sjawhar/legion", 42, "head", generation
				fact.Snapshot = fmt.Sprintf("settled-%d", generation)
				return fact
			}
			// The same undecided settlement twice: the second is newer (its generation) and leaves
			// the head as undecided as the first.
			applyRefusingNothing(t, pool, engine, settle(tc.first, 1), settle(tc.first, 2))
			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("after the undecided settlements the issue is in %s, want it left in %s", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("after the undecided settlements the architect was told %v, want nothing", got)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			for _, fact := range tc.then {
				if checks, ok := fact.(intake.PullRequestChecks); ok {
					fact = settle(checks, 3)
				}
				applyRefusingNothing(t, pool, engine, fact)
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after what followed the issue is in %s, want %s", got, tc.want)
			}
			if task := implementerTask(t, pool); tc.want == phase.Implementing && !strings.Contains(task, "CI is red at head: pr-checks-result") {
				t.Fatalf("the implementer's task %q does not name the failed required check", task)
			}
			if got := reviewStuckNotices(t, pool); tc.want == phase.Retro && len(got) != 0 {
				t.Fatalf("the approval of an undecided head told the architect %+v, want nothing: the round reads no verdict", got)
			}
		})
	}
}

// A required set that was never read - every pull request recorded since the daemon stopped
// reading rulesets (dispatch://INFRA-714's R1) - decides nothing: a red settlement sends nothing
// back and tells nobody, since nothing says the red check is required, so the head is neither
// green nor red. A recorded set is what decides it, in whatever phase the settlement finds the
// issue.
func TestAnUnreadRequiredSetLeavesTheHeadUndecided(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from     phase.Phase
		status   string
		required []string
		// want is the phase the red settlement leaves the issue in, and verdict the CI verdict that
		// stands for its head.
		want    phase.Phase
		verdict string
	}{
		{"a red settlement in testing, with the set never read", phase.Testing, "testing", nil, phase.Testing, ""},
		{"a red settlement in testing, on a base that requires the check", phase.Testing, "testing", []string{"ci"}, phase.Implementing, "red"},
		{"a red settlement in reviewing, on a base that requires nothing", phase.Reviewing, "needs_review", []string{}, phase.Reviewing, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: tc.required})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
			applyRefusingNothing(t, pool, engine, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "settled", Failing: []string{"ci"}})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the red settlement the issue is in %s, want %s", got, tc.want)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") != (tc.want == phase.Implementing) {
				t.Fatalf("after the red settlement the architect was told %v; want checks-red exactly when the work went back", got)
			}
			if verdict := pullRequestAt(t, pool).verdict; verdict != tc.verdict {
				t.Fatalf("the head's verdict is %q, want %q", verdict, tc.verdict)
			}
		})
	}
}

// A reopened pull request is the same pull request, and its base branch requires what it did
// before the close: the reopen keeps the required checks and the required workflows' runs as the
// daemon last read them, so the first settlement of the reopened head is judged by both. Judged
// by the checks alone, a required workflow that failed there would read green until the daemon's
// next read; judged by both, its red sends the work back from testing, naming the workflow.
func TestAReopenedPullRequestKeepsWhatItsBaseBranchRequires(t *testing.T) {
	const workflow = ".github/workflows/claude-pr-review.yml"
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", HeadUpdatedAt: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), Failing: []string{},
		Required: []string{requiredGate}, Workflows: []record.RequiredWorkflow{{Path: workflow, Result: "failure"}}, WorkflowsHead: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
			UpdatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC), Reopened: true},
		intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
			CheckRuns: []record.AttemptRun{{Name: requiredGate, ID: 1}}, Generation: 1, Snapshot: "settled", Failing: []string{}})
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the reopened head's settlement the issue is in %s, want implementing: the required workflow kept across the reopen is red", got)
	}
	if task := implementerTask(t, pool); !strings.Contains(task, "CI is red at head: "+workflow) {
		t.Fatalf("implementer task %q, want it to name the failed required workflow", task)
	}
}

// A push classified late that completes the carry of the code head's red to the READY head sends
// nothing back from awaiting_merge, as the settlement it carries would not either
// (TestARedVerdictInAwaitingMergeSendsTheTreeBackToImplementing): the red carried from the code
// head stood before the READY, and only the head's own red withdraws the READY
// (classify.RedWithdrawsReady). The CI rule the push applies in every phase before awaiting_merge
// (standingRed) does not act there, so the READY stands, nobody is told, and nothing is started.
func TestAPushClassifiedLateThatCarriesARedToTheREADYHeadWithdrawsNothing(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
	// The head has moved to the READY head, the red settlement stands recorded at the code head it
	// replaced, and the push between the two is not yet known: no verdict stands.
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "code", Required: []string{requiredGate},
		CheckRuns: []record.AttemptRun{{Name: requiredGate, ID: 1}}, Failing: []string{requiredGate}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
	applyRefusingNothing(t, pool, readyEngine("merge-queue"), pushedFromTo("code", "head", ".legion/LEGION-208/implement.json", "legion-implementer[bot]"))
	if got := pullRequestAt(t, pool); got.verdict != "red" {
		t.Fatalf("after the push the head's verdict is %q, want the code head's red carried to it", got.verdict)
	}
	if got := issuePhase(t, pool); got != phase.AwaitingMerge {
		t.Fatalf("after the late push completed the carry the issue is in %s, want awaiting_merge: a carried red withdraws no READY", got)
	}
	if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
		t.Fatalf("notices %v, want none", got)
	}
	assertOutboxCount(t, pool, "supervise", 0)
	assertOutboxCount(t, pool, "dispatch_status", 0)
	if published := mergeQueuePublishes(t, pool); len(published) != 0 {
		t.Fatalf("merge queue publishes %v, want none: the READY stands", published)
	}
	if pass := implementerPass(t, pool); pass != 0 {
		t.Fatalf("pass = %d, want none counted: nothing went back", pass)
	}
}

// startTask is the task role was last started with, "" when it was not started.
func startTask(t *testing.T, pool *pgxpool.Pool, role claim.Role) string {
	t.Helper()
	var task string
	if err := pool.QueryRow(context.Background(), `select coalesce(max(payload->>'task'), '') from outbox
		where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $1`, string(role)).Scan(&task); err != nil {
		t.Fatalf("read the %s's start: %v", role, err)
	}
	return task
}
