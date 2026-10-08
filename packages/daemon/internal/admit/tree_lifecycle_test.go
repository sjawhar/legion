package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A root waiting for a slot while its tree's cleanup is reserved is not lost and not admitted: the
// promotion leaves it waiting, writes no start, and a later tick admits it once the cleanup is
// confirmed, opening the tree's next lifecycle epoch under the normalized project the claims use.
func TestPromotionWaitsForAReservedTreeCleanupThenAdmitsTheNextEpoch(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	const tree = "LEGION-W"
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.Open(ctx, tx, "legion", tree, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatal(err)
		}
		if _, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", tree, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatal(err)
		}
	})
	seedWaiting(t, pool, tree, "A")
	tick := func(id string) {
		t.Helper()
		if _, err := intake.ApplyFact(ctx, pool, "controller", id, intake.ControllerTick{}, admission); err != nil {
			t.Fatalf("tick %s: %v", id, err)
		}
	}

	tick("tick-reserved")
	assertSlots(t, pool, nil)
	assertWaiting(t, pool, []string{tree})
	for _, e := range effects(t, pool) {
		if e.kind == record.OutboxKindSupervise {
			t.Fatalf("a start was written while the tree's cleanup was reserved: %#v", e)
		}
	}

	inTx(t, pool, func(tx pgx.Tx) {
		if err := treelifecycle.ConfirmCleanup(ctx, tx, "legion", tree, 1); err != nil {
			t.Fatal(err)
		}
	})
	tick("tick-confirmed")
	assertSlots(t, pool, []record.Slot{{Issue: tree, Index: 0, AdmittedAt: fixedNow}})
	var epoch int64
	var started bool
	if err := pool.QueryRow(ctx, `select epoch, cleanup_started from tree_lifecycles where project = 'legion' and tree = $1`, tree).Scan(&epoch, &started); err != nil {
		t.Fatal(err)
	}
	if epoch != 2 || started {
		t.Fatalf("lifecycle after admission = epoch %d, cleanup started %t; want open epoch 2", epoch, started)
	}
}
