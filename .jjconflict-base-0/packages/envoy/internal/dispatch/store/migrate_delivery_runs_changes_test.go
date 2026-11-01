package store

import (
	"context"
	"testing"
)

// deliveryRunsChangesVersion is the migration that adds delivery_runs.jobs_complete and the
// table's change trigger (LEGION-690).
const deliveryRunsChangesVersion = 102

// TestARunWrittenAfterTheMigrationStartsIncomplete runs the shipped migration files: a run stored
// before the migration reads jobs_complete true, since intake and the reconcile read jobs only for
// a concluded run, and a run inserted after it reads false.
func TestARunWrittenAfterTheMigrationStartsIncomplete(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunsChangesVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryRunsChangesVersion-1, err)
	}
	insert := `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, url)
		values ('acme/widgets', $1, 'deploy', 'abc', now(), now(), 'https://github.com/acme/widgets/actions/runs/1')`
	if _, err := store.Pool.Exec(ctx, insert, 1); err != nil {
		t.Fatalf("seed a run stored before the migration: %v", err)
	}

	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunsChangesVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryRunsChangesVersion, err)
	}
	if _, err := store.Pool.Exec(ctx, insert, 2); err != nil {
		t.Fatalf("insert a run after the migration: %v", err)
	}
	for runID, want := range map[int]bool{1: true, 2: false} {
		var complete bool
		if err := store.Pool.QueryRow(ctx, `select jobs_complete from delivery_runs where run_id = $1`, runID).Scan(&complete); err != nil {
			t.Fatalf("read run %d: %v", runID, err)
		}
		if complete != want {
			t.Errorf("run %d jobs_complete = %t, want %t", runID, complete, want)
		}
	}
}
