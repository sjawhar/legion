package workflow

import (
	"context"
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
	gate      = "pr-checks-result"
	besideIt  = []string{"dev-apply / dev-chain-tripwire", "dev-apply / staging-e2e / staging-e2e", "review"}
	allTheRun = []record.AttemptRun{{Name: "dev-apply / dev-chain-tripwire", ID: 2}, {Name: "dev-apply / staging-e2e / staging-e2e", ID: 3}, {Name: gate, ID: 1}, {Name: "review", ID: 4}}
)

// apply applies facts in order, each its own event, and fails the test on an error or refusal.
func apply(t *testing.T, pool *pgxpool.Pool, engine *Engine, facts ...intake.Fact) {
	t.Helper()
	for _, fact := range facts {
		if result, err := intake.ApplyFact(context.Background(), pool, "test", t.Name()+randomSuffix(t), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("apply %T = %+v, %v", fact, result.Refusal, err)
		}
	}
}

// startTask is the task the implementer was last started with, "" when it was not started.
func startTask(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var task string
	if err := pool.QueryRow(context.Background(), `select coalesce(max(payload->>'task'), '') from outbox
		where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'`).Scan(&task); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	return task
}

// Only a check the base branch requires makes CI red at a head, under the rule READY refuses by: a
// red beside a passing required gate sends nothing back from testing or reviewing, a required
// check that failed does and names only itself, and a required check a settled head reports no
// result for is red too, named as such since it has no run to open.
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
		{"reds beside the required gate, in testing", phase.Testing, "testing", []string{gate}, besideIt, allTheRun, ""},
		{"reds beside the required gate, in reviewing", phase.Reviewing, "needs_review", []string{gate}, besideIt, allTheRun, ""},
		{"the required gate failed beside them", phase.Testing, "testing", []string{gate}, append([]string{gate}, besideIt...), allTheRun,
			"CI is red at head: pr-checks-result"},
		{"a required check the settled head reports no result for", phase.Testing, "testing", []string{"lint", gate}, []string{}, []record.AttemptRun{{Name: gate, ID: 1}},
			"CI is red at head: lint (no result)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: tc.required})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
			verdict := "green"
			if len(tc.failing) > 0 {
				verdict = "red"
			}
			apply(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: "head", CheckRuns: tc.runs, Generation: 1, Snapshot: "settled", Verdict: verdict, Failing: tc.failing})
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
			if task := startTask(t, pool); !strings.Contains(task, tc.red) || !strings.HasSuffix(reason, tc.red) {
				t.Fatalf("task %q and notice %q; want both to end %q, naming no check the base branch does not require", task, reason, tc.red)
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
		Branch: "legion/LEGION-208", HeadSHA: "handoff", CheckedHead: "code", Verdict: "red", Failing: besideIt, CheckRuns: allTheRun,
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
		{"on the boot read of the required set", nil, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{gate}}, phase.Retro},
		{"on the next fact, the set already read", []string{gate}, comment, phase.Retro},
		{"a read that requires one of the reds", nil, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{gate, "review"}}, phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedStuckRound(t, pool, tc.recorded)
			apply(t, pool, testEngine(config.DesignGateRootIssues, nil), tc.next)
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
			verdict := "green"
			if len(tc.failing) > 0 {
				verdict = "red"
			}
			apply(t, pool, engine, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "settled", Verdict: verdict, Failing: tc.failing})
			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("with the required set unread the issue is in %s, want it left in %s", got, tc.from)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("with the required set unread the architect was told %v, want nothing", got)
			}
			apply(t, pool, engine, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: tc.required})
			if got := issuePhase(t, pool); got != tc.decidedTo {
				t.Fatalf("once the required set is read the issue is in %s, want %s", got, tc.decidedTo)
			}
		})
	}
}
