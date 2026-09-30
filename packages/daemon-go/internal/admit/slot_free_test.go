package admit

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A slot the waiting line does not refill is the controller's to fill: it picks the next root to
// hand to Legion. Admission wakes it once per fact, on the controller topic alone, naming the
// first root whose slot it released, whether the tree finished or a human took the root out of the
// workflow, live or found at boot. A slot the waiting line refilled wakes nobody
// (TestApplyFactReleasesSlotWhenEngineCompletesPhaseAndPromotesHead pins that).
func TestAReleasedSlotNoWaitingRootTakesWakesTheController(t *testing.T) {
	slotFree := func(issue string) []effect {
		return []effect{{kind: record.OutboxKindControllerNotice, issue: issue, payload: record.ControllerNotice{Kind: record.SlotFreeNotice}}}
	}
	t.Run("a finished tree", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		seedSlotted(t, pool, "LEGION-ACTIVE", "A")

		apply(t, pool, admission, "phase-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "active", Rank: "A", HandedOver: handed}, engineStub{store: record.NewStore(), done: "LEGION-ACTIVE"})
		assertSlots(t, pool, nil)
		assertControllerNotices(t, pool, slotFree("LEGION-ACTIVE"))
	})
	t.Run("two roots a human parked, in one fact", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
		seedSlotted(t, pool, "LEGION-FIRST", "A")
		seedSlotted(t, pool, "LEGION-SECOND", "B")
		seedSlotted(t, pool, "LEGION-KEPT", "C")
		second := issue(t, pool, "LEGION-SECOND")
		second.Status = "backlog"
		putIssue(t, pool, second)

		apply(t, pool, admission, "park-first", intake.DispatchIssue{Key: "LEGION-FIRST", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "LEGION-FIRST", Rank: "A", HandedOver: handed}, engineStub{})
		assertSlots(t, pool, []record.Slot{{Issue: "LEGION-KEPT", Index: 2, AdmittedAt: fixedNow}})
		assertControllerNotices(t, pool, slotFree("LEGION-FIRST"))
	})
	t.Run("a boot that finds a parked root", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		seedSlotted(t, pool, "LEGION-ACTIVE", "A")

		reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-ACTIVE", Title: "LEGION-ACTIVE", Status: "icebox", Rank: "A", LastSeq: 2, HandedOver: true}}, 1, 1, true)
		assertSlots(t, pool, nil)
		assertControllerNotices(t, pool, slotFree("LEGION-ACTIVE"))
	})
}

// A fact that releases no slot wakes nobody, even while a slot has stood free all along: the
// controller filled the slots at its own start and at the release that emptied them, and a wake on
// every fact would be a wake on every Dispatch event of the project.
func TestAFreeSlotWakesTheControllerOnlyAtTheReleaseThatLeftItFree(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")

	apply(t, pool, admission, "rename", intake.DispatchIssue{Key: "LEGION-ACTIVE", Seq: 2, Type: "issue.updated", Status: "in_progress", Title: "renamed", Rank: "A", HandedOver: handed}, engineStub{})
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ACTIVE", Index: 0, AdmittedAt: fixedNow}})
	assertControllerNotices(t, pool, nil)
}

// assertControllerNotices compares the controller notices in the outbox, oldest first, with want;
// the other effects a fact has (a real engine's suspend of a parked tree) are not these tests'.
func assertControllerNotices(t *testing.T, pool *pgxpool.Pool, want []effect) {
	t.Helper()
	var got []effect
	for _, row := range effects(t, pool) {
		if row.kind == record.OutboxKindControllerNotice {
			got = append(got, row)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("controller notices = %#v, want %#v", got, want)
	}
}
