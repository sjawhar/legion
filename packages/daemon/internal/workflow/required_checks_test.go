package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
	var task string
	if err := pool.QueryRow(context.Background(), `select coalesce(max(payload->>'task'), '') from outbox
		where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'`).Scan(&task); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	return task
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
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
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
// required check the head's own settlement names cancelled, or does not report, moves nothing and
// tells nobody, and the head's next settlement decides it: passed, an approved round ends; failed,
// the work goes back naming it.
func TestARequiredCheckCancelledOrMissingWaitsForTheHeadsNextSettlement(t *testing.T) {
	gateRun := func(id int64) record.AttemptRun { return record.AttemptRun{Name: requiredGate, ID: id} }
	missing := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}}, Failing: []string{}}
	cancelled := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{gateRun(1)}, Failing: []string{}, Cancelled: []string{requiredGate}}
	passed := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}, gateRun(2)}, Failing: []string{}}
	failed := intake.PullRequestChecks{CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}, gateRun(2)}, Failing: []string{requiredGate}}
	for _, tc := range []struct {
		name          string
		from          phase.Phase
		status        string
		first, second intake.PullRequestChecks
		want          phase.Phase
	}{
		{"missing, then passed, under an approved round", phase.Reviewing, "needs_review", missing, passed, phase.Retro},
		{"cancelled, then passed, under an approved round", phase.Reviewing, "needs_review", cancelled, passed, phase.Retro},
		{"missing, then failed, in testing", phase.Testing, "testing", missing, failed, phase.Implementing},
		{"cancelled, then failed, in testing", phase.Testing, "testing", cancelled, failed, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{requiredGate}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"}
			if tc.from == phase.Reviewing {
				reviewer.HandoffCommit, reviewer.Summary = "head", "approved"
				reviewer.Decision = &record.ReviewDecision{State: "approved", Body: "looks right", Head: "head"}
			}
			seedPhase(t, pool, reviewer)
			settle := func(fact intake.PullRequestChecks, generation int64) {
				fact.Repo, fact.Number, fact.HeadSHA, fact.Generation = "sjawhar/legion", 42, "head", generation
				fact.Snapshot = fmt.Sprintf("settled-%d", generation)
				applyRefusingNothing(t, pool, engine, fact)
			}
			settle(tc.first, 1)
			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("after the first settlement the issue is in %s, want it left in %s", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("after the first settlement the architect was told %v, want nothing", got)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			settle(tc.second, 2)
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the next settlement the issue is in %s, want %s", got, tc.want)
			}
			if task := implementerTask(t, pool); tc.want == phase.Implementing && !strings.Contains(task, "CI is red at head: pr-checks-result") {
				t.Fatalf("the implementer's task %q does not name the failed required check", task)
			}
		})
	}
}

// seedStuckRound seeds the round the live tree got stuck in: the reviewer approved its own
// handoff head and completed, the handoff head's verdict is the code head's settlement carried
// across the handoff-only push, and that settlement is red only on checks the base branch does not
// require. required is the pull request's recorded required set, nil for a row recorded before the
// daemon read one.
func seedStuckRound(t *testing.T, pool *pgxpool.Pool, required []string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42,
		Branch: "legion/LEGION-208", HeadSHA: "handoff", CheckedHead: "code", Failing: redsBesideTheGate, CheckRuns: runsBesideTheGate,
		Generation: 1, Snapshot: "settled", Pushes: []record.ClassifiedPush{{SHA: "handoff", Before: "code", HandoffOnly: true}}, Required: required})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "handoff",
		Summary: "approved", Decision: &record.ReviewDecision{State: "approved", Body: "looks right", Head: "handoff"}})
}

// A round left stuck on reds the base branch never required ends once the daemon judges the head
// by its required set, with no new push: on the boot read that records the set for a pull request
// recorded before any was read, or on the next fact about the round once the set is recorded. A
// read that names one of those reds as required keeps the round with the reviewer, and tells the
// architect why.
func TestARoundStuckOnRedsTheBaseBranchDoesNotRequireEndsWithoutAPush(t *testing.T) {
	comment := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 9, State: "commented", CommitID: "handoff", Body: "a comment"}
	for _, tc := range []struct {
		name     string
		recorded []string
		next     intake.Fact
		want     phase.Phase
	}{
		{"on the boot read of the required set", nil, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{requiredGate}}, phase.Retro},
		{"on the next fact, the set already read", []string{requiredGate}, comment, phase.Retro},
		{"a read that requires one of the reds", nil, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{requiredGate, "review"}}, phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedStuckRound(t, pool, tc.recorded)
			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), tc.next)
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			if tc.want == phase.Retro {
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "review-stuck") || containsNotice(got, "checks-red") {
					t.Fatalf("notices %v, want the round ended with no stuck or red notice", got)
				}
				return
			}
			stuck := reviewStuckNotices(t, pool)
			if len(stuck) != 1 || stuck[0].Summary != byRequired || !strings.Contains(stuck[0].Reason, "CI is red at handoff: review") {
				t.Fatalf("review-stuck notices %+v, want one, written by the required read, naming the required review check", stuck)
			}
		})
	}
}

// A required set that was never read - GitHub refused or failed the read, or no pass has run yet -
// decides nothing: a red settlement sends nothing back and a green one ends no round, so neither is
// counted as green or red. The read that records the set decides what the settlement comes to,
// and a base branch that requires no check has nothing red.
func TestAnUnreadRequiredSetLeavesTheHeadUndecided(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from      phase.Phase
		status    string
		approved  bool
		failing   []string
		required  []string
		decidedTo phase.Phase
	}{
		{"a red settlement in testing", phase.Testing, "testing", false, []string{"ci"}, []string{"ci"}, phase.Implementing},
		{"a green settlement under an approved round", phase.Reviewing, "needs_review", true, []string{}, []string{}, phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"}
			if tc.approved {
				reviewer.HandoffCommit, reviewer.Summary = "head", "approved"
				reviewer.Decision = &record.ReviewDecision{State: "approved", Body: "looks right", Head: "head"}
			}
			seedPhase(t, pool, reviewer)
			applyRefusingNothing(t, pool, engine, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "settled", Failing: tc.failing})
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
		})
	}
}
