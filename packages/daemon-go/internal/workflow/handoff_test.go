package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A rework round ends with that round's own implementer handoff. The implementer's pushes during
// the round only synchronize the pull request: before this held, round 1's handoff commit stayed
// on the implementer's row, so the first push of every later round moved the issue to testing and
// started the tester on the old commit.
func TestReworkRoundAdvancesOnlyOnThatRoundsHandoff(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-1", Verdict: "green", Failing: []string{}, FailingStatuses: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "round-0"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	engine := testEngine()

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
	apply("changes-requested", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head-1", HeadSHA: "head-1"})
	assertPhase(t, pool, phase.Implementing)

	apply("rework-push", intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-2"})
	assertPhase(t, pool, phase.Implementing)

	if _, err := pool.Exec(ctx, "delete from outbox"); err != nil {
		t.Fatalf("clear outbox: %v", err)
	}
	apply("round-1-handoff", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "round-1"})
	assertPhase(t, pool, phase.Testing)
	var task string
	if err := pool.QueryRow(ctx, "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start'").Scan(&task); err != nil {
		t.Fatalf("read the tester's start task: %v", err)
	}
	if !strings.Contains(task, "Handoff commit: round-1.") {
		t.Fatalf("tester task %q, want round 1's handoff commit", task)
	}
}

// A completion the workflow does not act on is refused, not answered as though it applied: a role
// reporting a phase it no longer owns, and a merger's completion without READY.
func TestIgnoredCompletionsAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current phase.Phase
		fact    intake.HandoffComplete
		code    string
	}{
		{
			name: "role no longer owns the phase", current: phase.Testing,
			fact: intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "late"},
			code: "HANDOFF_NOT_CURRENT_PHASE",
		},
		{
			name: "merger without READY", current: phase.Merging,
			fact: intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", Commit: "merge"},
			code: "READY_REQUIRED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.current, Generation: 1, Status: fixtureStatus(tc.current), Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 1, ApprovedVersion: new(1)})
			result, err := intake.ApplyFact(t.Context(), pool, "api", tc.name, tc.fact, testEngine(), admissionStub{})
			if err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != tc.code {
				t.Fatalf("refusal = %+v, want 409 %s", result.Refusal, tc.code)
			}
			assertPhase(t, pool, tc.current)
			assertOutboxKinds(t, pool, []string{})
			var rows int
			if err := pool.QueryRow(t.Context(), "select count(*) from phases where issue = 'LEGION-208' and handoff_commit <> ''").Scan(&rows); err != nil {
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

// The architect's sign-off closes the issue only after the implementer's production check was
// recorded; a sign-off before it, or outside production_check, changes nothing and says so.
func TestSignOffWaitsForTheRecordedProductionCheck(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	engine := testEngine()
	signOff := func(eventID string) *intake.Refusal {
		t.Helper()
		result, err := intake.ApplyFact(ctx, pool, "api", eventID, intake.SignOff{Issue: "LEGION-208"}, engine, admissionStub{})
		if err != nil {
			t.Fatalf("ApplyFact %s: %v", eventID, err)
		}
		return result.Refusal
	}
	if refusal := signOff("signoff-while-implementing"); refusal == nil || refusal.Status != 409 || refusal.Code != "SIGNOFF_OUTSIDE_PRODUCTION_CHECK" {
		t.Fatalf("sign-off while implementing = %+v, want 409 SIGNOFF_OUTSIDE_PRODUCTION_CHECK", refusal)
	}
	assertPhase(t, pool, phase.Implementing)

	// The retro's handoff is on the implementer's row when the merge starts the production check.
	seedRecord(t, pool, func(tx pgx.Tx) error {
		store := record.NewStore()
		if err := store.PutIssue(ctx, tx, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"}); err != nil {
			return err
		}
		return store.PutPhase(ctx, tx, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "retro"})
	})
	seedPR(t, pool, record.PullRequest{Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}, FailingStatuses: []string{}})
	if _, err := intake.ApplyFact(ctx, pool, "github", "merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact merged: %v", err)
	}
	assertPhase(t, pool, phase.ProductionCheck)
	if refusal := signOff("signoff-before-check"); refusal == nil || refusal.Status != 409 || refusal.Code != "PRODUCTION_CHECK_NOT_RECORDED" {
		t.Fatalf("sign-off before the production check = %+v, want 409 PRODUCTION_CHECK_NOT_RECORDED", refusal)
	}
	assertPhase(t, pool, phase.ProductionCheck)

	if _, err := intake.ApplyFact(ctx, pool, "api", "production-check", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "serves", Commit: "retro"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact production check: %v", err)
	}
	if refusal := signOff("signoff-after-check"); refusal != nil {
		t.Fatalf("sign-off after the production check refused: %+v", *refusal)
	}
	assertPhase(t, pool, phase.Done)
}
