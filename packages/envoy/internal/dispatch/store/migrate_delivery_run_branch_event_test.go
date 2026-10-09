package store

import (
	"context"
	"testing"
)

// deliveryRunBranchEventVersion is the migration that adds delivery_runs.head_branch and event,
// their started-at index, and delivery_reconcile_progress.began_at (LEGION-567 slice 2).
const deliveryRunBranchEventVersion = 85

// TestMigrationAddsBranchAndEventColumns runs the shipped migration files, never a constant copy
// of them: a database at the version before, holding one stored run, migrates to this one and
// reads the run back with both new columns null, the started-at index in place and the progress
// table's began_at column added.
func TestMigrationAddsBranchAndEventColumns(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunBranchEventVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryRunBranchEventVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, url)
		values ('acme/widgets', 1, 'deploy', 'abc', now(), now(), 'https://github.com/acme/widgets/actions/runs/1')
	`); err != nil {
		t.Fatalf("seed a run stored before the migration: %v", err)
	}

	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunBranchEventVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryRunBranchEventVersion, err)
	}
	var headBranch, event *string
	if err := store.Pool.QueryRow(ctx, `select head_branch, event from delivery_runs where run_id = 1`).Scan(&headBranch, &event); err != nil {
		t.Fatalf("read the run back: %v", err)
	}
	if headBranch != nil || event != nil {
		t.Errorf("head_branch, event = %v, %v, want both null on a run stored before the migration", headBranch, event)
	}
	var indexes int
	if err := store.Pool.QueryRow(ctx, `select count(*) from pg_indexes where tablename = 'delivery_runs' and indexname = 'delivery_runs_kind_started_at'`).Scan(&indexes); err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}
	if indexes != 1 {
		t.Errorf("delivery_runs_kind_started_at indexes = %d, want 1", indexes)
	}
	var beganAt int
	if err := store.Pool.QueryRow(ctx, `
		select count(*) from information_schema.columns
		where table_name = 'delivery_reconcile_progress' and column_name = 'began_at' and is_nullable = 'YES'
	`).Scan(&beganAt); err != nil {
		t.Fatalf("read information_schema: %v", err)
	}
	if beganAt != 1 {
		t.Errorf("delivery_reconcile_progress.began_at nullable columns = %d, want 1", beganAt)
	}
}
