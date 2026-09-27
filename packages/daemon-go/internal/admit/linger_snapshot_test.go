package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Deep review finding A / oracle: a snapshot that moves a recorded issue's status wrote it
// straight through recordObservation, with no suspend, no linger, and no linger_close — the
// engine's own leave/beginLinger never ran, so a root parked while the daemon was down kept its
// workers with no slot and no linger close, and the record's LastDispatchSeq moved to the
// snapshot's own sequence, so the real Dispatch event, once it finally arrived, was dropped by
// Engine.dispatchIssue's sequence check. applySummary now routes a status-changing snapshot
// through the engine first, exactly as the live event it stands in for would, before recording it.
func TestReleaseAppliesASnapshotThatLeavesTheWorkflowThroughTheEngineNotOnlyTheRecord(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{
		Key: "LEGION-PARK", Project: testProject, Title: "park", Tree: "LEGION-PARK", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
	})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutSlot(context.Background(), tx, record.Slot{Issue: "LEGION-PARK", Index: 0, AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("seed slot: %v", err)
		}
	})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-PARK", Title: "park", Status: "backlog", Rank: "A", HandedOver: true, LastSeq: 2},
	}, 10, 0, false)
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind parked record, want it held")
	}
	if got := issue(t, pool, "LEGION-PARK"); got.Phase != phase.Implementing || got.LingerUntil != nil {
		t.Fatalf("LEGION-PARK while held = %#v, want it untouched until release", got)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}

	got := issue(t, pool, "LEGION-PARK")
	if got.Phase != phase.Done || got.LingerUntil == nil {
		t.Fatalf("released LEGION-PARK = %#v, want phase done with a linger armed (the engine's own beginLinger), not just the record's status moved", got)
	}
	if got.Status != "backlog" {
		t.Fatalf("released LEGION-PARK status = %q, want backlog (the snapshot's own status, still recorded)", got.Status)
	}
	assertSlots(t, pool, nil)
	var sawLingerClose bool
	for _, e := range effects(t, pool) {
		if e.kind == record.OutboxKindLingerClose {
			sawLingerClose = true
		}
	}
	if !sawLingerClose {
		t.Fatal("no linger_close queued for released LEGION-PARK, want the tree's own linger deadline armed")
	}
}

// The same regression, reached through Reconcile's own immediate-apply path (the consumer already
// caught up at boot, so nothing is deferred): a recreated consumer idle from its first read
// applies every summary at once, and a status-changing one must reach the engine exactly the same
// way release's own reapplication does.
func TestReconcileAppliesASnapshotThatLeavesTheWorkflowThroughTheEngineWhenTheConsumerIsAlreadyCaughtUp(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{
		Key: "LEGION-PARK2", Project: testProject, Title: "park2", Tree: "LEGION-PARK2", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
	})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutSlot(context.Background(), tx, record.Slot{Issue: "LEGION-PARK2", Index: 0, AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("seed slot: %v", err)
		}
	})

	// idle true: caughtUp regardless of target, so Reconcile applies this summary immediately
	// rather than deferring it.
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-PARK2", Title: "park2", Status: "done", Rank: "A", HandedOver: true, LastSeq: 2},
	}, 10, 10, true)
	if admission.Held() {
		t.Fatal("Held() = true after an idle, caught-up reconcile; want nothing held")
	}

	got := issue(t, pool, "LEGION-PARK2")
	if got.Phase != phase.Done || got.LingerUntil == nil {
		t.Fatalf("reconciled LEGION-PARK2 = %#v, want phase done with a linger armed", got)
	}
	assertSlots(t, pool, nil)
	var sawLingerClose bool
	for _, e := range effects(t, pool) {
		if e.kind == record.OutboxKindLingerClose {
			sawLingerClose = true
		}
	}
	if !sawLingerClose {
		t.Fatal("no linger_close queued for the reconciled LEGION-PARK2, want the tree's own linger deadline armed")
	}
}
