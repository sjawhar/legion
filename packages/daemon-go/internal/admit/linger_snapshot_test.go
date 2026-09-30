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

// A snapshot that moves a recorded issue's status wrote it
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

// Routing a snapshot's status change through the engine only for
// record.OutOfWorkflow statuses pre-filtered a decision Engine.dispatchIssue already makes on its
// own sequence fence, which also re-enters a child set back to todo (ReenterChild). A parked
// child — recorded todo at phase done under a live tree because its own live todo event never
// arrived (the recreated-consumer case) — was left stranded: recordObservation wrote its status to
// todo with no generation bump and no re-entry. The condition is now sequence-based
// (summary.LastSeq > stored.LastDispatchSeq), not status-based, so this reaches the engine too.
func TestReconcileReentersAParkedChildSetBackToTodoInABootSummary(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	parent := "LEGION-ROOT"
	putIssue(t, pool, record.Issue{
		Key: "LEGION-CHILD", Tree: "LEGION-ROOT", Project: testProject, Title: "child", Parent: &parent,
		Phase: phase.Done, Generation: 1, Status: "done", Rank: "B", LastDispatchSeq: 1,
	})

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-CHILD", Title: "child", Status: "todo", Parent: &parent, Rank: "B", LastSeq: 5},
	}, 10, 10, true)

	got := issue(t, pool, "LEGION-CHILD")
	if got.Generation != 2 || got.Phase != phase.Admitted || got.Status != "todo" {
		t.Fatalf("reconciled parked child = %#v, want re-entered at generation 2, phase admitted, status todo — not stranded todo at phase done", got)
	}
}

// A session writes an issue's Dispatch status directly
// (sessionStatusWrite records a session's own status write, LastDispatchSeq included, but never
// changes issue.Status — the daemon reasserts its own through the outbox instead). A boot listing
// taken before that reassert lands still shows the session's own out-of-workflow write, level with
// what the daemon already recorded (same sequence): not a change to apply, since the daemon's
// reassert — not the snapshot — is the record's own truth. Before this fix, recordObservation
// wrote it anyway, releaseInactiveSlots freed the slot, and the next waiting root was admitted
// alongside the first tree, which kept running unslotted: two trees at cap 1.
func TestReconcileLeavesSlotStateUnchangedWhenALevelSnapshotEchoesAnAgentsOutOfWorkflowWrite(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	putIssue(t, pool, record.Issue{
		Key: "LEGION-AGENT", Project: testProject, Title: "agent", Tree: "LEGION-AGENT", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 5,
	})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutSlot(context.Background(), tx, record.Slot{Issue: "LEGION-AGENT", Index: 0, AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("seed slot: %v", err)
		}
	})
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	// The boot listing shows the agent's own out-of-workflow write, no newer than what the daemon
	// already recorded (its own reassert already landed at this same sequence).
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-AGENT", Title: "agent", Status: "done", Rank: "A", HandedOver: true, LastSeq: 5},
	}, 10, 10, true)

	got := issue(t, pool, "LEGION-AGENT")
	if got.Status != "in_progress" || got.Phase != phase.Implementing || got.LingerUntil != nil {
		t.Fatalf("reconciled LEGION-AGENT = %#v, want its slot state unchanged: the engine never applied this level-sequence status", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-AGENT", Index: 0, AdmittedAt: fixedNow}})
}

// putNewRoot, called from applySummary's stored==nil branch at release, never carried the
// summary's own sequence, so a freshly created root's LastDispatchSeq was always 0 regardless of
// what the summary actually showed. An older, already-superseded event reaching Apply later then
// passed applyObservation's own sequence fence (observation.Seq <= stored.LastDispatchSeq, since 0
// fences nothing) and overwrote what the newer summary had already established.
func TestAReleasedRootIgnoresAnOlderLateEvent(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-LATE", Title: "late", Status: "todo", Rank: "A", HandedOver: true, LastSeq: 5},
	}, 10, 0, false)
	if !admission.Held() {
		t.Fatal("Held() = false after seeding a behind labeled record, want it held")
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-reached", intake.DispatchConsumerPosition{AckFloorStream: 10}, engineStub{}, admission); err != nil {
		t.Fatalf("ApplyFact position-reached: %v", err)
	}
	if got := issue(t, pool, "LEGION-LATE"); !got.HandedOver || got.LastDispatchSeq != 5 {
		t.Fatalf("released LEGION-LATE = %#v, want handed over with LastDispatchSeq 5 (the summary's own)", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-LATE", Index: 0, AdmittedAt: fixedNow}})

	// An older event, already superseded by the summary that created this record, arrives late —
	// a nak's redelivery, or the outbox's own publish backoff finally catching up.
	apply(t, pool, admission, "older-late-event", intake.DispatchIssue{Key: "LEGION-LATE", Seq: 3, Type: "issue.updated", Status: "todo", Title: "late", Rank: "A", HandedOver: false}, engineStub{})
	if got := issue(t, pool, "LEGION-LATE"); !got.HandedOver {
		t.Fatalf("LEGION-LATE after an older late event = %#v, want HandedOver still true: the event is older than what created the record", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-LATE", Index: 0, AdmittedAt: fixedNow}})
}
