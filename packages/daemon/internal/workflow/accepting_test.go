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

const acceptingRequired = "ci"

// seedAccepting seeds LEGION-208 in accepting on an open pull request at head, its implementer's,
// reviewer's and merger's phase rows as the review round left them: the reviewer's round completed
// and approved at head.
func seedAccepting(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Accepting, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
		CheckedHead: "head", Required: []string{acceptingRequired}, CheckRuns: []record.AttemptRun{{Name: acceptingRequired, ID: 1}}, Failing: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "implement-1"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "review-1", Summary: "reviewed",
		Decision: &record.ReviewDecision{State: "approved", Head: "head"}})
}

// The reviewer's approval moves the issue to accepting and nothing else: the board already reads
// needs_review, so no status is written; the tree's architect is already live, so no worker is
// started and no claim is minted (a start row is what mints one); the reviewer's phase-finished is
// the architect's cue to accept.
func TestAnApprovalMovesTheIssueToAcceptingAndStartsNobody(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "review-1", Summary: "looks right"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", CheckedHead: "head", Failing: []string{}, Required: []string{}})

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head", HeadSHA: "head", Author: testReviewApp})

	assertPhase(t, pool, phase.Accepting)
	var status string
	if err := pool.QueryRow(context.Background(), "select status from issues where key = 'LEGION-208'").Scan(&status); err != nil || status != "needs_review" {
		t.Fatalf("status = %q, %v; want needs_review kept", status, err)
	}
	assertOutboxKinds(t, pool, []string{"notice"})
	var kind, role, from, summary string
	if err := pool.QueryRow(context.Background(), "select payload->>'kind', payload->>'role', payload->>'phase', payload->>'summary' from outbox where kind = 'notice'").Scan(&kind, &role, &from, &summary); err != nil {
		t.Fatalf("read the notice: %v", err)
	}
	if kind != "phase-finished" || role != string(claim.RoleReviewer) || from != string(phase.Reviewing) || summary != "looks right" {
		t.Fatalf("notice = %s %s %s %q, want the reviewer's phase-finished for reviewing with its summary", kind, role, from, summary)
	}
}

// sign_off is the architect's acceptance, taken in accepting alone: elsewhere it changes nothing
// and says so.
func TestSignOffIsRefusedOutsideAccepting(t *testing.T) {
	for _, at := range []phase.Phase{phase.Reviewing, phase.AwaitingMerge} {
		t.Run(string(at), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: at, Generation: 1, Status: fixtureStatus(at), Rank: "U"})
			result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: "LEGION-208"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "SIGNOFF_OUTSIDE_ACCEPTING" {
				t.Fatalf("sign-off in %s = %+v, %v; want 409 SIGNOFF_OUTSIDE_ACCEPTING", at, result.Refusal, err)
			}
			if want := "LEGION-208 is in phase " + string(at) + ", not accepting"; !strings.Contains(result.Refusal.Message, want) {
				t.Fatalf("refusal %q does not say %q", result.Refusal.Message, want)
			}
			assertPhase(t, pool, at)
			assertOutboxKinds(t, pool, []string{})
		})
	}
}

// The architect's sign_off in accepting moves the issue to retro with the status retro and starts
// the implementer there, its task naming the phase, the review's handoff and the pull request; the
// architect that just acted is told nothing.
func TestSignOffInAcceptingStartsRetro(t *testing.T) {
	pool := migratedPool(t)
	seedAccepting(t, pool)
	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.SignOff{Issue: "LEGION-208"})
	assertPhase(t, pool, phase.Retro)
	var status string
	if err := pool.QueryRow(context.Background(), "select status from issues where key = 'LEGION-208'").Scan(&status); err != nil || status != "retro" {
		t.Fatalf("status = %q, %v; want retro", status, err)
	}
	assertOutboxKinds(t, pool, []string{"dispatch_status", "supervise"})
	var written string
	if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&written); err != nil || written != "retro" {
		t.Fatalf("status write = %q, %v; want retro", written, err)
	}
	task := startTask(t, pool, claim.RoleImplementer)
	for _, want := range []string{"Phase: retro.", "Handoff commit: review-1.", "Pull request: #42."} {
		if !strings.Contains(task, want) {
			t.Fatalf("the implementer's retro task %q does not carry %q", task, want)
		}
	}
	if rounds := implementerRounds(t, pool); rounds != 0 {
		t.Fatalf("acceptance counted the implementer %d rounds, want none", rounds)
	}
}

// The architect accepts with sign_off: its handoff_complete in accepting is refused and records
// nothing, before the phase gate that RoleFor(accepting) would let it through.
func TestTheArchitectsCompletionInAcceptingIsRefused(t *testing.T) {
	pool := migratedPool(t)
	seedAccepting(t, pool)
	result, err := intake.ApplyFact(context.Background(), pool, "api", "architect-complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleArchitect, Claim: "architect-claim", Summary: "accepted", Commit: "head"},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{})
	if err != nil || result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "ACCEPTANCE_IS_SIGN_OFF" {
		t.Fatalf("the architect's completion = %+v, %v; want 409 ACCEPTANCE_IS_SIGN_OFF", result.Refusal, err)
	}
	assertPhase(t, pool, phase.Accepting)
	assertOutboxKinds(t, pool, []string{})
	var rows int
	if err := pool.QueryRow(context.Background(), "select count(*) from phases where issue = 'LEGION-208' and role = 'architect'").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("architect phase rows = %d, %v; want none recorded", rows, err)
	}
}

// The tree architect's send-back from accepting to implementing or testing is a backward move it
// requests: that phase's role is started with the reason, and the implementer is counted a review
// round, as a reviewer's request for changes counts one. The move writes no architect phase row.
func TestTheArchitectsBackwardMoveFromAcceptingStartsThePhaseAndCountsARound(t *testing.T) {
	for _, tc := range []struct {
		to     phase.Phase
		role   claim.Role
		status string
	}{
		{phase.Implementing, claim.RoleImplementer, "in_progress"},
		{phase.Testing, claim.RoleTester, "testing"},
	} {
		t.Run(string(tc.to), func(t *testing.T) {
			pool := migratedPool(t)
			seedAccepting(t, pool)
			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
				intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleArchitect, To: tc.to, Reason: "the evidence does not cover the migration"})
			assertPhase(t, pool, tc.to)
			var status string
			if err := pool.QueryRow(context.Background(), "select status from issues where key = 'LEGION-208'").Scan(&status); err != nil || status != tc.status {
				t.Fatalf("status = %q, %v; want %s", status, err, tc.status)
			}
			task := startTask(t, pool, tc.role)
			for _, want := range []string{"Phase: " + string(tc.to) + ".", "Reason: the evidence does not cover the migration.", "Pull request: #42."} {
				if !strings.Contains(task, want) {
					t.Fatalf("the %s's task %q does not carry %q", tc.role, task, want)
				}
			}
			if started := superviseRequests(t, pool, "start"); len(started) != 1 || started["LEGION-208/"+string(tc.role)] != 1 {
				t.Fatalf("starts = %v, want the %s once and nobody else", started, tc.role)
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("implementer rounds = %d, want the send-back counted as one", rounds)
			}
			var rows int
			if err := pool.QueryRow(context.Background(), "select count(*) from phases where issue = 'LEGION-208' and role = 'architect'").Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("architect phase rows = %d, %v; want none", rows, err)
			}
		})
	}
}

// Accepting is the architect's phase: a planner's backward move from it is refused, moving and
// counting nothing.
func TestAPlannersBackwardMoveFromAcceptingIsRefused(t *testing.T) {
	pool := migratedPool(t)
	seedAccepting(t, pool)
	result, err := intake.ApplyFact(context.Background(), pool, "api", "planner-backward",
		intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RolePlanner, To: phase.Planning, Reason: "replan"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
	if err != nil || refusalCode(result) != "BACKWARD_REFUSED" {
		t.Fatalf("the planner's move = %+v, %v; want BACKWARD_REFUSED", result.Refusal, err)
	}
	assertPhase(t, pool, phase.Accepting)
	if rounds := implementerRounds(t, pool); rounds != 0 {
		t.Fatalf("implementer rounds = %d, want none", rounds)
	}
	assertOutboxKinds(t, pool, []string{})
}

// CI red in accepting sends the work back to implementing as it does in review. The architect holds
// no workspace turn, so the implementer's start waits on nobody (an empty quiesce).
func TestChecksRedInAcceptingSendsTheWorkBackWaitingOnNobody(t *testing.T) {
	pool := migratedPool(t)
	seedAccepting(t, pool)
	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: acceptingRequired, ID: 2}}, Generation: 2, Snapshot: "red-2", Failing: []string{acceptingRequired}})
	assertPhase(t, pool, phase.Implementing)
	task, quiesce := implementerStart(t, pool)
	if quiesce != "" || !strings.Contains(task, "Phase: implementing.") {
		t.Fatalf("the implementer's start %q waits for %q, want the implementing task waiting on nobody", task, quiesce)
	}
	reason, from := checksRedNotice(t, pool)
	if from != phase.Accepting || !strings.Contains(reason, "CI is red at head") {
		t.Fatalf("checks-red notice %q in %s, want the red told in accepting", reason, from)
	}
	if rounds := implementerRounds(t, pool); rounds != 1 {
		t.Fatalf("implementer rounds = %d, want the red counted as one", rounds)
	}
}

// A backward move onto accepting from a later phase writes needs_review, the status the board shows
// for the phase, and starts nobody.
func TestABackwardMoveOntoAcceptingWritesNeedsReview(t *testing.T) {
	for _, from := range []phase.Phase{phase.Retro, phase.Merging} {
		t.Run(string(from), func(t *testing.T) {
			pool := migratedPool(t)
			role := RoleFor(from)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: from, Generation: 1, Status: "retro", Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: claim.Token(string(role) + "-claim")})
			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
				intake.BackwardMove{Issue: "LEGION-208", Requester: role, To: phase.Accepting, Reason: "the architect should look again"})
			assertPhase(t, pool, phase.Accepting)
			var status, written string
			if err := pool.QueryRow(context.Background(), "select status from issues where key = 'LEGION-208'").Scan(&status); err != nil || status != "needs_review" {
				t.Fatalf("status = %q, %v; want needs_review", status, err)
			}
			if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&written); err != nil || written != "needs_review" {
				t.Fatalf("status write = %q, %v; want needs_review", written, err)
			}
			if started := superviseRequests(t, pool, "start"); len(started) != 0 {
				t.Fatalf("the move onto accepting started %v, want nobody", started)
			}
		})
	}
}

// No path reaches accepting with another board status than needs_review: every row onto it either
// writes needs_review or writes nothing from a phase whose own board status is needs_review.
func TestEveryRowOntoAcceptingLeavesTheBoardAtNeedsReview(t *testing.T) {
	boardStatus := map[phase.Phase]string{phase.Reviewing: "needs_review"}
	rows := 0
	for _, row := range Table {
		if row.To != phase.Accepting {
			continue
		}
		rows++
		switch {
		case row.Status == "needs_review":
		case row.Status == "" && boardStatus[row.From] == "needs_review":
		default:
			t.Errorf("row %s/%s -> accepting writes status %q from a phase whose board status is %q; want needs_review on the board", row.From, row.Trigger, row.Status, boardStatus[row.From])
		}
	}
	if rows == 0 {
		t.Fatal("no row of the table reaches accepting")
	}
}

// seedAcceptingChild seeds an issue of LEGION-208's tree under parent, in the phase and status
// given, as a child the architect's acceptance has to wait for.
func seedAcceptingChild(t *testing.T, pool *pgxpool.Pool, key, parent string, at phase.Phase, status string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: at, Generation: 1, Status: status, Rank: "U"})
}

// phaseOf is the recorded phase of key, for a case that moves an issue other than LEGION-208.
func phaseOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var got string
	if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", key).Scan(&got); err != nil {
		t.Fatalf("read the phase of %s: %v", key, err)
	}
	return got
}

// An architect accepts only once the work under it is finished: a sign-off is refused while any
// issue beneath the signing one is still in the workflow, however deep, and the refusal names each
// of them so the architect knows what it is waiting for. It is taken before anything is written,
// so the issue stays in accepting and nothing is told.
func TestSignOffIsRefusedWhileAnIssueBeneathIsInWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, pool *pgxpool.Pool)
		names []string
	}{
		{
			name: "a child still running",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				seedAcceptingChild(t, pool, "LEGION-209", "LEGION-208", phase.Implementing, "in_progress")
			},
			names: []string{"LEGION-209"},
		},
		{
			name: "a grandchild still running under a closed child",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				seedAcceptingChild(t, pool, "LEGION-209", "LEGION-208", phase.Done, "done")
				seedAcceptingChild(t, pool, "LEGION-210", "LEGION-209", phase.Testing, "testing")
			},
			names: []string{"LEGION-210"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedAccepting(t, pool)
			tc.seed(t, pool)
			result, err := intake.ApplyFact(context.Background(), pool, "api", "signoff", intake.SignOff{Issue: "LEGION-208"}, testEngine(config.DesignGateRootIssues, nil), admissionStub{})
			if err != nil || result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != "SIGNOFF_CHILD_IN_WORKFLOW" {
				t.Fatalf("sign-off = %+v, %v; want 409 SIGNOFF_CHILD_IN_WORKFLOW", result.Refusal, err)
			}
			for _, want := range append([]string{"park_child"}, tc.names...) {
				if !strings.Contains(result.Refusal.Message, want) {
					t.Fatalf("refusal %q does not say %q", result.Refusal.Message, want)
				}
			}
			assertPhase(t, pool, phase.Accepting)
			assertOutboxKinds(t, pool, []string{})
		})
	}
}

// A child out of the workflow holds nothing up: once it is done, or parked in backlog, the same
// sign-off is the acceptance it always was and the issue reaches retro.
func TestSignOffIsAcceptedOnceTheChildLeftTheWorkflow(t *testing.T) {
	for _, tc := range []struct {
		at     phase.Phase
		status string
	}{
		{at: phase.Done, status: "done"},
		{at: phase.Implementing, status: "backlog"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			pool := migratedPool(t)
			seedAccepting(t, pool)
			seedAcceptingChild(t, pool, "LEGION-209", "LEGION-208", tc.at, tc.status)
			applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.SignOff{Issue: "LEGION-208"})
			assertPhase(t, pool, phase.Retro)
		})
	}
}

// The rule is read downward, never over the flat tree: a child with nothing beneath it is accepted
// while its own root is still running. Read over the tree it would wait for the root, which is
// waiting for it.
func TestSignOffOnAChildIsAcceptedWhileItsRootRuns(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	seedAcceptingChild(t, pool, "LEGION-209", "LEGION-208", phase.Accepting, "needs_review")
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "review-1", Summary: "reviewed",
		Decision: &record.ReviewDecision{State: "approved", Head: "head"}})

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil), intake.SignOff{Issue: "LEGION-209"})

	if got := phaseOf(t, pool, "LEGION-209"); got != string(phase.Retro) {
		t.Fatalf("the child is in phase %s, want retro", got)
	}
}
