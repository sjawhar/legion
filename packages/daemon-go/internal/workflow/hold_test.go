package workflow

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A tree's architect is who every notice of its tree reaches, so its own claim failing — its
// launches or prompts ran out — would reach nobody unless the daemon says so: the failure is a
// worker-died notice naming the architect and the phase the root is in, written for the architect
// that owns the issue and for the controller. Nothing is held: a phase is its worker's, and the root's planner here
// keeps its phase.
func TestATreeArchitectsFailedClaimIsNoticedAndHoldsNoPhase(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(ctx, pool, "supervise", "architect-failed", intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleArchitect}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the architect's failed claim: %v", err)
	}
	var gotPhase phase.Phase
	var heldFrom *string
	if err := pool.QueryRow(ctx, "select phase, held_from from issues where key = $1", "LEGION-208").Scan(&gotPhase, &heldFrom); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if gotPhase != phase.Planning || heldFrom != nil {
		t.Fatalf("root phase = %s held from %v, want planning and not held", gotPhase, heldFrom)
	}
	died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Planning}
	if got, want := noticeRows(t, pool), []record.OutboxPayload{died, record.ControllerNotice(died)}; !slices.Equal(got, want) {
		t.Fatalf("notice rows = %+v, want %+v, to the issue and then to the controller", got, want)
	}
}

// An escalation is recorded on the held issue as well as noticed to the architect that owns the
// issue and to the controller, so a controller that starts after it finds it in the state it reads at boot
// (issues.<KEY>.holdReason); the retry that ends the hold clears it.
func TestAnEscalationIsRecordedOnTheHoldForAControllerThatStartsLater(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Planning}, Generation: 1, Status: "in_progress", Rank: "U"})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.EscalateDecision}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Held || reason != "escalated" {
		t.Fatalf("after the escalation the state reads phase %s hold reason %q, want held and escalated", got, reason)
	}
	escalated := record.Notice{Kind: "held", Phase: phase.Planning, Reason: "escalated"}
	if got, want := noticeRows(t, pool), []record.OutboxPayload{escalated, record.ControllerNotice(escalated)}; !slices.Equal(got, want) {
		t.Fatalf("notice rows = %+v, want %+v, to the issue and then to the controller", got, want)
	}
	if _, err := intake.ApplyFact(ctx, pool, "architect", "retry", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Planning || reason != "" {
		t.Fatalf("after the retry the state reads phase %s hold reason %q, want planning and none", got, reason)
	}
}

// A held root moved out of the workflow ends its hold, and an escalation with it: its tree lingers
// with the root's phase done and no hold reason, so the state never shows a closed root escalated.
func TestAClosedRootEndsItsHoldAndItsEscalation(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Held, Hold: &record.Hold{From: phase.Planning}, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.EscalateDecision}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("move the root to backlog: %v", err)
	}
	var held bool
	if err := pool.QueryRow(ctx, "select held_from is not null from issues where key = $1", "LEGION-208").Scan(&held); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-208"); got != phase.Done || reason != "" || held {
		t.Fatalf("after the backlog move the state reads phase %s hold reason %q, held from a phase %t; want done, none, and not held", got, reason, held)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
}

// A held child keeps its hold when its tree closes, escalation included, since the tree's
// re-admission leaves it held and the escalation still unanswered. While the tree lingers or is
// closed, the escalation waits on nobody, so the state the controller reads at every start shows
// the child held with no hold reason.
func TestAnEscalatedChildOfAClosedTreeKeepsItsHoldButShowsNoEscalation(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	root, from := "LEGION-208", phase.Implementing
	seedIssue(t, pool, record.Issue{Key: root, Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Held, Hold: &record.Hold{From: from}, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	if _, err := intake.ApplyFact(ctx, pool, "architect", "escalate", intake.RetryOrEscalate{Issue: "LEGION-209", Decision: intake.EscalateDecision}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("escalate the child: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Held || reason != "escalated" {
		t.Fatalf("in a running tree the state reads the child's phase %s hold reason %q, want held and escalated", got, reason)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "root-backlog", intake.DispatchIssue{Key: root, Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("move the root to backlog: %v", err)
	}
	if got, reason := projectedHold(t, pool, "LEGION-209"); got != phase.Held || reason != "" {
		t.Fatalf("with its tree closed the state reads the child's phase %s hold reason %q, want held and none", got, reason)
	}
	var heldFrom, holdReason string
	if err := pool.QueryRow(ctx, "select held_from, hold_reason from issues where key = $1", "LEGION-209").Scan(&heldFrom, &holdReason); err != nil {
		t.Fatalf("read the child's hold: %v", err)
	}
	if heldFrom != string(from) || holdReason != "escalated" {
		t.Fatalf("the child's record holds it from %q for %q, want from %s for escalated", heldFrom, holdReason, from)
	}
}

// A lingering tree holds its members where they stood (TestNoFactMovesAMemberOfALingeringTree), so a
// phase worker's failed claim there holds nothing and tells neither the architect nor the
// controller. The tree architect's own failed claim holds no phase either, and is still told to the
// issue's topic and to the controller, lingering or not: nobody inside the tree can act on it.
func TestALingeringTreesFailedClaimHoldsNothingAndItsArchitectsIsStillTold(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	root, until := "LEGION-208", time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: root, Tree: root, Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V"})

	if _, err := intake.ApplyFact(ctx, pool, "supervise", "tester-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the tester's failed claim: %v", err)
	}
	var gotPhase phase.Phase
	var held bool
	if err := pool.QueryRow(ctx, "select phase, held_from is not null from issues where key = $1", "LEGION-209").Scan(&gotPhase, &held); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if gotPhase != phase.Testing || held {
		t.Fatalf("the lingering tree's child is in %s, held %t; want testing and not held", gotPhase, held)
	}
	if got := noticeRows(t, pool); len(got) != 0 {
		t.Fatalf("notice rows after the child's failed claim = %+v, want none", got)
	}

	if _, err := intake.ApplyFact(ctx, pool, "supervise", "architect-failed", intake.ClaimFailed{Issue: root, Role: claim.RoleArchitect}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the architect's failed claim: %v", err)
	}
	died := record.Notice{Kind: "worker-died", Role: claim.RoleArchitect, Phase: phase.Done}
	if got, want := noticeRows(t, pool), []record.OutboxPayload{died, record.ControllerNotice(died)}; !slices.Equal(got, want) {
		t.Fatalf("notice rows = %+v, want %+v, to the issue and then to the controller", got, want)
	}
}

// A held member of a lingering tree keeps its hold, escalation included, when its architect retries
// it: the retry would start the held phase's worker inside a tree that has left the workflow, so it
// changes nothing, and re-admission finds the member held from the same phase for the same reason.
func TestARetryInALingeringTreeKeepsTheHoldAndItsReason(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	root, until := "LEGION-208", time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: root, Tree: root, Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: root, Parent: &root, Project: "LEGION", Title: "child", Phase: phase.Held,
		Hold: &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}, Generation: 1, Status: "in_progress", Rank: "V"})

	if _, err := intake.ApplyFact(ctx, pool, "architect", "retry", intake.RetryOrEscalate{Issue: "LEGION-209", Decision: intake.RetryDecision}, testEngine(), admissionStub{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var gotPhase phase.Phase
	var heldFrom, holdReason *string
	var starts int
	if err := pool.QueryRow(ctx, `select phase, held_from, hold_reason,
		(select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start')
		from issues where key = $1`, "LEGION-209").Scan(&gotPhase, &heldFrom, &holdReason, &starts); err != nil {
		t.Fatalf("read the child: %v", err)
	}
	if gotPhase != phase.Held || heldFrom == nil || *heldFrom != string(phase.Implementing) || holdReason == nil || *holdReason != string(record.HoldEscalated) || starts != 0 {
		t.Fatalf("after the retry the child is in %s held from %v for %v with %d starts; want held from implementing, escalated, and no start", gotPhase, heldFrom, holdReason, starts)
	}
}

// projectedHold is the issue's phase and hold reason as the state view shows them.
func projectedHold(t *testing.T, pool *pgxpool.Pool, key string) (phase.Phase, string) {
	t.Helper()
	var state api.State
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		state, err = projection.Project(t.Context(), tx, record.NewStore(), "LEGION", nil)
		return err
	}); err != nil {
		t.Fatalf("project the state: %v", err)
	}
	return state.Issues[key].Phase, state.Issues[key].HoldReason
}

// noticeRows is every notice and controller notice row's payload, oldest first.
func noticeRows(t *testing.T, pool *pgxpool.Pool) []record.OutboxPayload {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id, kind, issue, payload from outbox where kind in ($1, $2) order by id",
		string(record.OutboxKindNotice), string(record.OutboxKindControllerNotice))
	if err != nil {
		t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	payloads := []record.OutboxPayload{}
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a notice row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the notice rows: %v", err)
	}
	return payloads
}
