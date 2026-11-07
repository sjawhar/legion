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

// What a tree's liveness is read from moves with this release: from the slot its root holds to
// its lifecycle row. A pre-release store can hold a running tree whose row was never written, so
// the migration carries the fact across; without it the first admission pass finds a root of no
// live tree at a running status and takes it out of the workflow altogether, architect at work
// and all (admit's unslotted and releaseUnslotted).
//
// The two halves are one rule: the fact is carried where it is missing, and a row that already
// stands is left exactly as it is, since its epoch is the one the tree's claims carry.
func TestTheMigrationCarriesEachRunningTreesLivenessAndKeepsTheRowsItFinds(t *testing.T) {
	ctx := context.Background()
	st := preReleaseStore(t)
	pool := st.Pool()
	records := record.NewStore()
	fresh, known, parked := bootProject+"-1", bootProject+"-3", bootProject+"-2"
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// A root whose architect is writing the spec, with a child a person parked, and no
		// lifecycle row of its own.
		if err := records.PutIssue(ctx, tx, bootIssue(fresh, fresh, "in_progress", phase.Admitted)); err != nil {
			return err
		}
		if err := records.PutIssue(ctx, tx, bootIssue(parked, fresh, "backlog", phase.Done)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into design_gates (issue, artifact_id, latest_version, approved_version)
			values ($1, 'spec', 1, null)`, fresh); err != nil {
			return err
		}
		// A second running tree, whose lifecycle row an earlier epoch already wrote.
		if err := records.PutIssue(ctx, tx, bootIssue(known, known, "in_progress", phase.Implementing)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into tree_lifecycles (project, tree, epoch, authority)
			values ('legion', $1, 7, 'workflow')`, known); err != nil {
			return err
		}
		for index, root := range []string{fresh, known} {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: root, Index: index, AdmittedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the pre-release records: %v", err)
	}

	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the pre-release store onto the release: %v", err)
	}
	epochs := map[string]int64{}
	rows, err := pool.Query(ctx, `select tree, epoch, authority from tree_lifecycles order by tree`)
	if err != nil {
		t.Fatalf("read the tree lifecycles: %v", err)
	}
	for rows.Next() {
		var tree, authority string
		var epoch int64
		if err := rows.Scan(&tree, &epoch, &authority); err != nil {
			t.Fatalf("scan a lifecycle: %v", err)
		}
		if authority != "workflow" {
			t.Fatalf("the lifecycle of %s stands under %q, want the workflow's own", tree, authority)
		}
		epochs[tree] = epoch
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the lifecycles: %v", err)
	}
	if epochs[fresh] != 1 {
		t.Fatalf("the lifecycle of %s = %d, want epoch 1: its tree runs and the record did not say so", fresh, epochs[fresh])
	}
	if epochs[known] != 7 {
		t.Fatalf("the lifecycle of %s = %d, want the epoch 7 it already stood at: the tree's claims carry that one", known, epochs[known])
	}

	// The first boot then leaves both trees exactly where they were working.
	handlers := bootHandlers(t, records, 4)
	if _, err := intake.ApplyFact(ctx, pool, "daemon", "boot-1", intake.DaemonBooted{Boot: 1}, handlers...); err != nil {
		t.Fatalf("apply the first boot on the release: %v", err)
	}
	if holders := slotHolders(t, pool); len(holders) != 2 || holders[0] != fresh || holders[1] != known {
		t.Fatalf("the slots after the first boot = %v, want both running roots", holders)
	}
	for key, want := range map[string]phase.Phase{fresh: phase.Admitted, known: phase.Implementing} {
		var stood record.Issue
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			issue, err := records.Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			stood = *issue
			return nil
		}); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if stood.Phase != want {
			t.Fatalf("%s stands at %s after the first boot, want %s: a running tree is not taken out of the workflow by a deploy", key, stood.Phase, want)
		}
		if stood.Status != "in_progress" {
			t.Fatalf("%s reads %q after the first boot, want in_progress", key, stood.Status)
		}
	}
}
