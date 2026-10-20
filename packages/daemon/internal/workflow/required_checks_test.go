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

// implementerRounds is the implementer's count of the issue's returns to implementing.
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
// approval of the head ends the round with the check still undecided, and a head with no verdict is
// refused at READY, where the merger reads the head's checks on GitHub.
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
		{"missing, then the reviewer approves the head", phase.Reviewing, "needs_review", missing, approval, phase.Retro},
		{"cancelled, then the reviewer approves the head", phase.Reviewing, "needs_review", cancelled, approval, phase.Retro},
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

// seedRoundOnRedsBesideTheGate seeds the live shape that once stuck a round: the reviewer's round
// open at its own handoff head, whose verdict is the code head's settlement carried across the
// handoff-only push, red only on checks beside the one gate the base branch requires. required is
// the pull request's recorded required set, nil for a row recorded before the daemon read one.
func seedRoundOnRedsBesideTheGate(t *testing.T, pool *pgxpool.Pool, required []string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: "handoff", CheckedHead: "code", Failing: redsBesideTheGate, CheckRuns: runsBesideTheGate,
		Generation: 1, Snapshot: "settled", Pushes: []record.ClassifiedPush{{SHA: "handoff", Before: "code", HandoffOnly: true}}, Required: required})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
}

// A red only on checks the base branch never required is no red at all, whichever head it stands
// for: the read that records the required set - the boot read for a pull request recorded before
// any was read - judges the head by that set alone, sends nothing back and tells nobody, and the
// reviewer's approval of its handoff head then ends the round. A read that names one of those reds
// as required brings a red that stands for the head, and sends the work back to implementing from
// under the open round, naming the check and taking the phase from the reviewer still at work.
func TestARedTheBaseBranchDoesNotRequireSendsNothingBack(t *testing.T) {
	approval := []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "handoff", Author: testReviewApp, Body: "looks right"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "approved", Commit: "handoff"},
	}
	for _, tc := range []struct {
		name     string
		recorded []string
		read     intake.Fact
		want     phase.Phase
	}{
		{"the boot read of the required set", nil, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{requiredGate}}, phase.Retro},
		{"the set already read, a read that confirms it", []string{requiredGate}, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{requiredGate}}, phase.Retro},
		{"a read that requires one of the reds", nil, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{requiredGate, "review"}}, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedRoundOnRedsBesideTheGate(t, pool, tc.recorded)
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, tc.read)
			if tc.want == phase.Implementing {
				if got := issuePhase(t, pool); got != phase.Implementing {
					t.Fatalf("after the read the issue is in %s, want implementing", got)
				}
				var reason, quiesce string
				if err := pool.QueryRow(context.Background(), `select payload->>'reason', (select coalesce(payload->>'quiesce', '') from outbox
					where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer')
					from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'`).Scan(&reason, &quiesce); err != nil {
					t.Fatalf("read the architect's checks-red notice and the implementer's start: %v", err)
				}
				if reason != "CI is red at handoff: review" || claim.Role(quiesce) != claim.RoleReviewer {
					t.Fatalf("checks-red %q, the implementer's start waiting for %q; want the required review check named and the reviewer's turn waited for", reason, quiesce)
				}
				return
			}
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the read the issue is in %s, want it left in reviewing", got)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("after the read the architect was told %v, want nothing: no required check is red", got)
			}
			applyRefusingNothing(t, pool, engine, approval...)
			if got := issuePhase(t, pool); got != phase.Retro {
				t.Fatalf("after the reviewer's approval the issue is in %s, want retro", got)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "review-stuck") || containsNotice(got, "checks-red") {
				t.Fatalf("notices %v, want the round ended with no stuck or red notice", got)
			}
		})
	}
}

// A required set that was never read - GitHub refused or failed the read, or no pass has run yet -
// decides nothing: a red settlement sends nothing back, since nothing says the red check is
// required, so the head is neither green nor red. The read that records the set decides what the
// settlement comes to: a check it requires that failed sends the work back, and a base branch that
// requires no check has nothing red, so the same settlement then moves nothing.
func TestAnUnreadRequiredSetLeavesTheHeadUndecided(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from      phase.Phase
		status    string
		required  []string
		decidedTo phase.Phase
	}{
		{"a red settlement in testing, on a base that requires the check", phase.Testing, "testing", []string{"ci"}, phase.Implementing},
		{"a red settlement in reviewing, on a base that requires nothing", phase.Reviewing, "needs_review", []string{}, phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
			applyRefusingNothing(t, pool, engine, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "settled", Failing: []string{"ci"}})
			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("with the required set unread the issue is in %s, want it left in %s", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("with the required set unread the architect was told %v, want nothing", got)
			}
			applyRefusingNothing(t, pool, engine, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: tc.required})
			if got := issuePhase(t, pool); got != tc.decidedTo {
				t.Fatalf("once the required set is read the issue is in %s, want %s", got, tc.decidedTo)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") != (tc.decidedTo == phase.Implementing) {
				t.Fatalf("once the required set is read the architect was told %v; want checks-red exactly when the work went back", got)
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
// (TestARedVerdictInAwaitingMergeSendsTheTreeBackToImplementing): READY found the head's own CI
// green on GitHub, which is what GitHub merges by, and only the head's own red withdraws the READY
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
	if rounds := implementerRounds(t, pool); rounds != 0 {
		t.Fatalf("rounds = %d, want none counted: nothing went back", rounds)
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
