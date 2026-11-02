package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A rework round ends with that round's own implementer handoff. The implementer's pushes during
// the round only synchronize the pull request: before this held, round 1's handoff commit stayed
// on the implementer's row, so the first push of every later round moved the issue to testing and
// started the tester on the old commit. Its handoff record is cleared with its commit when the
// round starts, so neither reads as the new round's completion; the tester's task then names the
// round's record, and the commit the implementer reported with it.
func TestReworkRoundAdvancesOnlyOnThatRoundsHandoff(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-1", CheckedHead: "head-1", Failing: []string{}, Required: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "round-0", HandoffRecord: "implement-0", HandoffVersion: 1, LastHandoff: "implement-0"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1", HandoffVersion: 1, LastHandoff: "review-1"})
	engine := testEngine(config.DesignGateRootIssues, nil)

	apply := func(eventID string, fact intake.Fact) {
		t.Helper()
		result, err := intake.ApplyFact(ctx, pool, "fact", eventID, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		if result.Refusal != nil {
			t.Fatalf("ApplyFact %s refused: %+v", eventID, *result.Refusal)
		}
	}
	apply("changes-requested", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head-1", HeadSHA: "head-1", Author: testReviewApp})
	assertPhase(t, pool, phase.Implementing)

	apply("rework-push", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"})
	assertPhase(t, pool, phase.Implementing)

	if _, err := pool.Exec(ctx, "delete from outbox"); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}
	apply("round-1-handoff", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "round-1", HandoffRecord: "implement-1", HandoffVersion: 2})
	assertPhase(t, pool, phase.Testing)
	var task string
	if err := pool.QueryRow(ctx, "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start'").Scan(&task); err != nil {
		t.Fatalf("read the tester's start task: %v", err)
	}
	for _, want := range []string{"Handoff: implement version 2 (dispatch handoff-read --issue LEGION-208 --phase implement --version 2).", "Handoff commit: round-1."} {
		if !strings.Contains(task, want) {
			t.Fatalf("tester task %q, want it to carry %q", task, want)
		}
	}
}

// phase.Handoff's writer of each phase with a handoff is the role the workflow starts on it
// (RoleFor), so the route reads a record from that role's completion alone, and the task line
// names each role's word (handoffWord).
func TestTheHandoffWriterOfEachPhaseIsTheRoleTheWorkflowStartsOnIt(t *testing.T) {
	handoffs := 0
	for _, p := range phase.All {
		word, writer, ok := phase.Handoff(p)
		if !ok {
			continue
		}
		handoffs++
		if writer != RoleFor(p) || handoffWord(writer) != word {
			t.Errorf("phase %s: phase.Handoff names %q written by %q; RoleFor names %q, and handoffWord(%q) is %q", p, word, writer, RoleFor(p), writer, handoffWord(writer))
		}
	}
	if handoffs != 4 {
		t.Errorf("phase.Handoff names %d phases with a handoff, want planning, implementing, testing and reviewing", handoffs)
	}
}

// A completion the workflow does not act on is refused, not answered as though it applied: a role
// reporting a phase it no longer owns, a merger's completion without READY, and a file-backed
// phase's completion that names no handoff record.
func TestIgnoredCompletionsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current phase.Phase
		fact    intake.HandoffComplete
		code    string
	}{
		{
			name: "role no longer owns the phase", current: phase.Testing,
			fact: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "late", HandoffRecord: "implement-late", HandoffVersion: 3},
			code: "HANDOFF_NOT_CURRENT_PHASE",
		},
		{
			name: "merger without READY", current: phase.Merging,
			fact: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", Commit: "merge"},
			code: "READY_REQUIRED",
		},
		{
			name: "tester naming no handoff record", current: phase.Testing,
			fact: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Verdict: "pass"},
			code: "HANDOFF_RECORD_REQUIRED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.current, Generation: 1, Status: fixtureStatus(tc.current), Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 1, ApprovedVersion: new(1)})
			result, err := intake.ApplyFact(t.Context(), pool, "api", tc.name, tc.fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != tc.code {
				t.Fatalf("refusal = %+v, want 409 %s", result.Refusal, tc.code)
			}
			assertPhase(t, pool, tc.current)
			assertOutboxKinds(t, pool, []string{})
			var rows int
			if err := pool.QueryRow(t.Context(), "select count(*) from phases where issue = 'LEGION-208' and (handoff_commit <> '' or handoff_record <> '')").Scan(&rows); err != nil {
				t.Fatalf("count recorded handoffs: %v", err)
			}
			if rows != 0 {
				t.Fatalf("a refused completion recorded %d handoff rows", rows)
			}
		})
	}
}

func assertPhase(t *testing.T, pool *pgxpool.Pool, want phase.Phase) {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = 'LEGION-208'").Scan(&got); err != nil {
		t.Fatalf("read phase: %v", err)
	}
	if got != string(want) {
		t.Fatalf("phase = %q, want %q", got, want)
	}
}

// The reviewer usually approves the head it was just shown, before CI settles on it: every new
// head clears the verdict. The approval ends the round all the same, and the green that settles
// afterwards finds the issue in accepting and moves nothing.
func TestAnApprovalBeforeTheChecksSettleAdvancesWithoutThem(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}, Required: []string{"ci"}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1", HandoffVersion: 1})
	engine := testEngine(config.DesignGateRootIssues, nil)
	if _, err := intake.ApplyFact(ctx, pool, "github", "approved", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", HeadSHA: "head", Author: testReviewApp}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact approval: %v", err)
	}
	assertPhase(t, pool, phase.Accepting)
	if _, err := intake.ApplyFact(ctx, pool, "github", "checks-green", intake.PullRequestChecks{
		Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "green-1", Failing: []string{},
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact checks: %v", err)
	}
	assertPhase(t, pool, phase.Accepting)
}

// The fix that exhausted the count can pass: a green settlement on it is no blocked pull request,
// and only a red one publishes pr-blocked, once for the count.
func TestAnExhaustedCountPublishesPRBlockedOnlyOnARedSettlement(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}, FixAttempts: 3, HeadCounted: "head", Required: []string{"ci"}})
	engine := testEngine(config.DesignGateRootIssues, nil)
	settle := func(eventID string, generation int64, failing []string) {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "github", eventID, intake.PullRequestChecks{
			Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: generation}}, Generation: generation, Snapshot: eventID, Failing: failing,
		}, engine, admissionStub{}); err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
	}

	settle("checks-green", 1, []string{})
	if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 0 {
		t.Fatalf("notices after a green settlement at an exhausted count = %v, want none", kinds)
	}
	settle("checks-red", 2, []string{"ci"})
	if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 1 || kinds[0] != "pr-blocked" {
		t.Fatalf("notices after a red settlement at the exhausted count = %v, want one pr-blocked", kinds)
	}
}

// A file-backed phase's completion names the Dispatch handoff record the role wrote for it. When a
// role names, in a later phase, the record it named before, it wrote no handoff since its previous
// phase ended: the completion is refused, whatever commit it reports. A new record is a new
// handoff even at the commit the role reported before.
func TestAHandoffMustBeNewSinceTheRolesPreviousPhase(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})
	engine := testEngine(config.DesignGateRootIssues, nil)
	complete := func(eventID string, fact intake.HandoffComplete) *intake.Refusal {
		t.Helper()
		result, err := intake.ApplyFact(ctx, pool, "api", eventID, fact, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		return result.Refusal
	}
	if refusal := complete("implement-0", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "head", HandoffRecord: "implement-record-0", HandoffVersion: 1}); refusal != nil {
		t.Fatalf("round 0 refused: %+v", *refusal)
	}
	assertPhase(t, pool, phase.Testing)
	if refusal := complete("test-0", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Verdict: "fail", HandoffRecord: "test-record-0", HandoffVersion: 1}); refusal != nil {
		t.Fatalf("tester refused: %+v", *refusal)
	}
	assertPhase(t, pool, phase.Implementing)

	refusal := complete("implement-1-stale", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "head-2", HandoffRecord: "implement-record-0", HandoffVersion: 1})
	if refusal == nil || refusal.Status != 409 || refusal.Code != "HANDOFF_NOT_NEW" || !strings.Contains(refusal.Message, "handoff record implement-record-0") {
		t.Fatalf("round 1 naming round 0's record = %+v, want 409 HANDOFF_NOT_NEW naming the record", refusal)
	}
	assertPhase(t, pool, phase.Implementing)
	if refusal := complete("implement-1", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "head", HandoffRecord: "implement-record-1", HandoffVersion: 2}); refusal != nil {
		t.Fatalf("round 1 with its own record, at round 0's commit, refused: %+v", *refusal)
	}
	assertPhase(t, pool, phase.Testing)
}

// toSecondReviewRound takes LEGION-208, seeded in its first review round (seedReviewOf at head),
// through that round by record — its reviewer's request for changes and completion — and through
// implementing and testing, each completed by record, into its second review round, whose reviewer
// has not completed. The implementer's push to head-2 arrives with its synchronize, so no head run
// holds a red at head-2. The reviewer's row then holds no handoff record, and keeps the first
// round's as the record it last named.
func toSecondReviewRound(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
	t.Helper()
	applyRefusingNothing(t, pool, engine,
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head", Author: testReviewApp, Body: "rename the widget"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1})
	assertPhase(t, pool, phase.Implementing)
	applyRefusingNothing(t, pool, engine,
		pushedFromTo("head", "head-2", "legion-implementer[bot]"),
		intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "renamed", Commit: "head-2", HandoffRecord: "implement-2", HandoffVersion: 2})
	assertPhase(t, pool, phase.Testing)
	applyRefusingNothing(t, pool, engine,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested", Verdict: "pass", HandoffRecord: "test-2", HandoffVersion: 1})
	assertPhase(t, pool, phase.Reviewing)
	reviewer := phaseRowOf(t, pool, claim.RoleReviewer)
	if reviewer.Completed() || reviewer.HandoffVersion != 0 || reviewer.LastHandoff != "review-1" {
		t.Fatalf("the round-2 reviewer's row = %+v, want no handoff and round 1's record kept as the last named", reviewer)
	}
}

// phaseRowOf is LEGION-208's row of role.
func phaseRowOf(t *testing.T, pool *pgxpool.Pool, role claim.Role) record.PhaseRow {
	t.Helper()
	var row record.PhaseRow
	seedRecord(t, pool, func(tx pgx.Tx) error {
		rows, err := record.NewStore().Phases(t.Context(), tx, "LEGION-208")
		for _, r := range rows {
			if r.Role == role {
				row = r
			}
		}
		return err
	})
	return row
}

// A reviewer whose first round completed by record starts its second round not completed: the
// record its first completion named is cleared with the round. So GitHub's approval of the new head
// alone does not end round 2, as it would were the first round's record read as this round's
// completion; the reviewer's own completion, naming a new record, is the round's other half.
func TestASecondReviewRoundIsNotCompletedByTheFirstRoundsRecord(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "head", "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	toSecondReviewRound(t, pool, engine)
	applyRefusingNothing(t, pool, engine,
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head-2", HeadSHA: "head-2", Author: testReviewApp, Body: "ship it"})
	assertPhase(t, pool, phase.Reviewing)
	result, err := intake.ApplyFact(t.Context(), pool, "api", "review-2-stale", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
		Claim: "review-claim", Summary: "reviewed", HandoffRecord: "review-1", HandoffVersion: 1}, engine, admissionStub{})
	if err != nil || result.Refusal == nil || result.Refusal.Code != "HANDOFF_NOT_NEW" {
		t.Fatalf("round 2's completion naming round 1's record = %+v, %v; want HANDOFF_NOT_NEW", result.Refusal, err)
	}
	assertPhase(t, pool, phase.Reviewing)
	applyRefusingNothing(t, pool, engine,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", HandoffRecord: "review-2", HandoffVersion: 2})
	assertPhase(t, pool, phase.Accepting)
}

// A tester whose first run completed by record is started again not completed: the record is
// cleared with the run, and the record it last named is kept, so its next completion must name a
// new one.
func TestARerunTesterIsNotCompletedByItsLastRunsRecord(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{"ci"}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffRecord: "implement-1", HandoffVersion: 1, LastHandoff: "implement-1"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim"})
	engine := testEngine(config.DesignGateRootIssues, nil)
	applyRefusingNothing(t, pool, engine,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "two scenarios fail", Verdict: "fail", HandoffRecord: "test-1", HandoffVersion: 1})
	assertPhase(t, pool, phase.Implementing)
	applyRefusingNothing(t, pool, engine,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "fixed", Commit: "head", HandoffRecord: "implement-2", HandoffVersion: 2})
	assertPhase(t, pool, phase.Testing)
	if tester := phaseRowOf(t, pool, claim.RoleTester); tester.Completed() || tester.LastHandoff != "test-1" {
		t.Fatalf("the re-run tester's row = %+v, want no handoff and its last run's record kept as the last named", tester)
	}
}

// The implementer's completion never moves its issue on a pull request closed without merging: it
// is no pull request a tester can test, and the completion records the handoff and leaves the
// issue where it is, with no worker started and the status unchanged.
func TestAnImplementerHandoffOnAPullRequestClosedWithoutMergingLeavesTheIssueInImplementing(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "handoff-0", Failing: []string{}})
	engine := testEngine(config.DesignGateRootIssues, nil)
	result, err := intake.ApplyFact(ctx, pool, "api", "implement-0", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "handoff-0", HandoffRecord: "implement-record-0", HandoffVersion: 1}, engine, admissionStub{})
	if err != nil {
		t.Fatalf("ApplyFact handoff: %v", err)
	}
	if result.Refusal != nil {
		t.Fatalf("handoff refused: %+v", *result.Refusal)
	}
	assertPhase(t, pool, phase.Implementing)
	assertOutboxKinds(t, pool, []string{})
	var commit string
	if err := pool.QueryRow(ctx, "select handoff_commit from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&commit); err != nil {
		t.Fatalf("read the implementer's handoff: %v", err)
	}
	if commit != "handoff-0" {
		t.Fatalf("implementer handoff commit = %q, want handoff-0 recorded", commit)
	}
}

// A pull request merged while its issue is in implementing tells the architect (pr-merged), and
// the workflow runs on: the implementer's completion that follows moves the issue to testing on
// the merged record, where holding it for an open pull request would stall it on a branch whose
// pull request can never open again.
func TestAnImplementerHandoffAfterAMergeInImplementingMovesTheIssueToTesting(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "handoff-0", Failing: []string{}})
	engine := testEngine(config.DesignGateRootIssues, nil)
	if _, err := intake.ApplyFact(ctx, pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact merged: %v", err)
	}
	if kinds := noticeKinds(t, pool, "LEGION-208"); !slices.Equal(kinds, []record.NoticeKind{"pr-merged"}) {
		t.Fatalf("after the merge in implementing the notices are %v, want pr-merged", kinds)
	}
	assertPhase(t, pool, phase.Implementing)

	result, err := intake.ApplyFact(ctx, pool, "api", "implement-0", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "handoff-0", HandoffRecord: "implement-record-0", HandoffVersion: 1}, engine, admissionStub{})
	if err != nil {
		t.Fatalf("ApplyFact handoff: %v", err)
	}
	if result.Refusal != nil {
		t.Fatalf("handoff refused: %+v", *result.Refusal)
	}
	assertPhase(t, pool, phase.Testing)
}
