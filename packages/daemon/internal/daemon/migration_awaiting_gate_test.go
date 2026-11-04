package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The other half of the backfill, and the one the release ships against: a root whose first spec
// is with a person. Before this release such a root stands in awaiting_person, and may be held
// from it, which is neither admitted nor done — and its tree is its own key, so a member test
// that reads every row whose tree is this gate's issue reads the root itself and marks the gate
// as having opened, though no version was ever approved.
//
// What that costs: a changes request answers the root, slotFor reads !EverOpened and gives it no
// slot, record.AtWork is false for it too, so the release of slots takes back the one it holds
// and the boot's catch-up passes over it — the architect rewrites the spec with no slot and
// outside the cap. On a deployment that is every root waiting on its first approval at the
// deploy.
//
// So the member test leaves the root's own row out, and only a member below it says the gate
// opened.
func TestTheMigrationLeavesTheGateOfARootWaitingOnItsFirstApprovalUnopened(t *testing.T) {
	ctx := context.Background()
	st := preReleaseStore(t)
	pool := st.Pool()
	records := record.NewStore()
	waiting := bootProject + "-1"
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// The root as the pre-release daemon left it: version 1 of its spec with a person, the
		// wait recorded, and the slot it holds while that wait lasts.
		root := bootIssue(waiting, waiting, "in_progress", phase.AwaitingPerson)
		root.Awaiting = &record.Awaiting{GateVersion: 1, Artifact: "spec",
			Since: record.EventMark{Seq: 3, At: time.Now().UTC()}}
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
			values ($1, 'spec', 1, null)`, waiting); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: waiting, Index: 0, AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("seed the pre-release records: %v", err)
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the pre-release store onto the release: %v", err)
	}

	var everOpened bool
	if err := pool.QueryRow(ctx, `select ever_opened from design_gates where issue = $1`, waiting).Scan(&everOpened); err != nil {
		t.Fatalf("read the gate of %s: %v", waiting, err)
	}
	if everOpened {
		t.Fatalf("the migration marked the gate of %s as having opened, though no version of its spec was ever approved: the root's own row is no member of the tree below it", waiting)
	}
}

// A member below the root in a phase the workflow reaches only after the gate opened still says
// the gate opened: the clause the fix narrows keeps the case it exists for.
func TestTheMigrationReadsAMemberInAPhaseAsAGateThatOpened(t *testing.T) {
	ctx := context.Background()
	st := preReleaseStore(t)
	pool := st.Pool()
	records := record.NewStore()
	root, child := bootProject+"-3", bootProject+"-4"
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, bootIssue(root, root, "in_progress", phase.Admitted)); err != nil {
			return err
		}
		if err := records.PutIssue(ctx, tx, bootIssue(child, root, "in_progress", phase.Implementing)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
			values ($1, 'spec', 2, null)`, root)
		return err
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
	if !everOpened {
		t.Fatalf("the migration left the gate of %s unopened, though %s below it is implementing: only the gate's opening starts a member's phase", root, child)
	}
}
