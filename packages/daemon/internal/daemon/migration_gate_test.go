package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A7's first boot, for the root the release must not take a slot from: one whose architect is
// still writing the spec. The slot such a root holds is read from its gate — record.AtWork gives
// an admitted root work only while its design gate has never opened — so the migration's
// backfill of design_gates.ever_opened decides it.
//
// The backfill reads a member of the tree standing in a phase other than admitted as proof that
// the gate opened. A parked member is one: workflow's leave records any non-root member a person
// moves to backlog, icebox, triage or done at phase.Done, whatever the gate was doing, and a
// child of a live tree is recorded before any release (the daemon records every live-tree child).
// So a tree whose architect has not yet had its spec approved, and one of whose filed children a
// person parked, reads as a tree whose gate opened.
//
// What that costs after the upgrade: the first admission pass takes the root's slot back while
// its architect works, the root is in neither part of the waiting line (its gate is not open, so
// it is no first-phase candidate, and record.ran reads ever_opened, so it is no queued root),
// and the cap under-counts a tree that is working.
func TestTheMigrationLeavesAGateThatNeverOpenedUnopened(t *testing.T) {
	ctx := context.Background()
	st := preReleaseStore(t)
	pool := st.Pool()
	records := record.NewStore()
	root, parked := bootProject+"-1", bootProject+"-2"
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// The root whose architect is writing the spec, and the child of its tree a person
		// parked before any approval, which the record keeps at phase done.
		if err := records.PutIssue(ctx, tx, bootIssue(root, root, "in_progress", phase.Admitted)); err != nil {
			return err
		}
		if err := records.PutIssue(ctx, tx, bootIssue(parked, root, "backlog", phase.Done)); err != nil {
			return err
		}
		// The gate as the architect registered it: a document and a version, with no approval.
		if _, err := tx.Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
			values ($1, 'spec', 1, null)`, root); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: 0, AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("seed the pre-release records: %v", err)
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the pre-release store onto the release: %v", err)
	}

	var everOpened bool
	if err := pool.QueryRow(ctx, `select ever_opened from design_gates where issue = $1`, root).Scan(&everOpened); err != nil {
		t.Fatalf("read the gate of %s: %v", root, err)
	}
	if everOpened {
		t.Fatalf("the migration marked the gate of %s as having opened, though no version of its spec was ever approved: a parked child is recorded at phase done whatever the gate did", root)
	}

	// And the boot that follows leaves the architect its slot.
	handlers := bootHandlers(t, records, 2)
	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot-1", intake.DaemonBooted{Boot: 1}, handlers...); err != nil {
		t.Fatalf("apply the first boot on the release: %v", err)
	}
	if holders := slotHolders(t, pool); len(holders) != 1 || holders[0] != root {
		t.Fatalf("the slots after the first boot = %v, want the root whose architect is writing the spec to keep its own", holders)
	}
}
