package workflow

// The run fencing: a tree's generation is what says which run a worker's task and its completion
// belong to, and these are the transitions that turn a run over or refuse work from one that is
// past.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A human moving an in-flight child back to todo starts the child's next run, and the run it
// interrupted still has a worker: the implementer pane keeps the workspace and would report its
// handoff into the new run. The previous run's worker is suspended first, and that suspend is
// stamped with the generation the child now holds, since that is what the outbox fences it against.
func TestAnInFlightChildSetBackToTodoSuspendsThePreviousRunsWorker(t *testing.T) {
	// The child re-enters at the phase it was taken from, so a suspend that named that phase
	// would be dropped by SuspendApplies exactly when the phase is the one the re-entry restarts
	// — planning, under an open gate. This suspend ends a run, not a phase, and names none.
	for _, tc := range []struct {
		name  string
		phase phase.Phase
		role  claim.Role
	}{
		{name: "implementing", phase: phase.Implementing, role: claim.RoleImplementer},
		{name: "planning under an open gate", phase: phase.Planning, role: claim.RolePlanner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			approved := 1
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 3})
			seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: &approved})
			parentKey := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parentKey,
				Phase: tc.phase, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 4})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: tc.role, Claim: "child-worker", Rounds: 1})
			engine := testEngine()

			if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-back-to-todo", intake.DispatchIssue{
				Key: "LEGION-209", Seq: 5, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-208", Rank: "V",
			}, engine, admissionStub{}); err != nil {
				t.Fatalf("ApplyFact child todo: %v", err)
			}

			rows, err := pool.Query(ctx, "select payload->>'op', payload->>'role', (payload->>'generation')::bigint, coalesce(payload->>'leaves', '') from outbox where kind = 'supervise' and issue = $1 order by id", "LEGION-209")
			if err != nil {
				t.Fatalf("list the child's supervise rows: %v", err)
			}
			defer rows.Close()
			var suspends, starts int
			for rows.Next() {
				var op, role, leaves string
				var generation int64
				if err := rows.Scan(&op, &role, &generation, &leaves); err != nil {
					t.Fatalf("scan a supervise row: %v", err)
				}
				switch {
				case op == "suspend" && role == string(tc.role):
					if generation != 2 {
						t.Errorf("suspend of the previous run = generation %d, want the 2 the child now holds", generation)
					}
					if leaves != "" {
						t.Errorf("suspend of the previous run leaves %q, want none: it ends a run, not a phase", leaves)
					}
					if !SuspendApplies(phase.Phase(leaves), tc.phase) {
						t.Errorf("the suspend does not apply with the child back in %s: the interrupted worker is never stopped", tc.phase)
					}
					if starts > 0 {
						t.Error("the new run started before the previous run's worker was suspended")
					}
					suspends++
				case op == "start":
					starts++
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate the child's supervise rows: %v", err)
			}
			if suspends != 1 {
				t.Fatalf("suspends of the interrupted run = %d, want 1", suspends)
			}
		})
	}
}

// A child taken back to todo re-enters at a new generation, and its interrupted worker keeps its
// pane: the stop is an outbox row the runtime can refuse for as long as it likes. That worker
// finishes the turn it was in and reports the completion of a run that is over. Nothing else on
// the fact tells the two runs apart — the claim token is stable across runs and the session is
// kept across a suspend — so the daemon names the run from the task the worker took, and a
// completion of a generation the issue has left changes nothing.
func TestACompletionFromAnInterruptedRunIsRefused(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	approved := 1
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 3})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: &approved})
	parentKey := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parentKey,
		Phase: phase.Planning, Generation: 2, Status: "in_progress", Rank: "V", LastDispatchSeq: 6})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "child-planner"})
	engine := testEngine()

	result, err := intake.ApplyFact(ctx, pool, "api", "stale-handoff", intake.HandoffComplete{
		Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "child-planner", Summary: "the plan of the run that was interrupted",
		Commit: "plan-of-generation-1", Generation: 1,
	}, engine, admissionStub{})
	if err != nil {
		t.Fatalf("apply the stale completion: %v", err)
	}

	if result.Refusal == nil || result.Refusal.Code != "HANDOFF_STALE_GENERATION" {
		t.Fatalf("the stale completion = %+v, want a HANDOFF_STALE_GENERATION refusal", result.Refusal)
	}
	var got record.Issue
	records := record.NewStore()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		issue, err := records.Issue(ctx, tx, "LEGION-209")
		if err != nil || issue == nil {
			return err
		}
		got = *issue
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.Phase != phase.Planning {
		t.Fatalf("the child is in %s, want planning: the interrupted run's plan advanced the new one", got.Phase)
	}

	// The run the child is actually on reports the same phase, and it moves.
	if _, err := intake.ApplyFact(ctx, pool, "api", "live-handoff", intake.HandoffComplete{
		Issue: "LEGION-209", Role: claim.RolePlanner, Claim: "child-planner", Summary: "the new run's plan",
		Commit: "plan-of-generation-2", Generation: 2,
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the live completion: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		issue, err := records.Issue(ctx, tx, "LEGION-209")
		if err != nil || issue == nil {
			return err
		}
		got = *issue
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.Phase != phase.Implementing {
		t.Fatalf("after the live completion the child is in %s, want implementing", got.Phase)
	}
}
