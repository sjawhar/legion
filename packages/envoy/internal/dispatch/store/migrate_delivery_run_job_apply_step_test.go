package store

import (
	"context"
	"testing"
)

// deliveryRunJobApplyStepVersion is the migration that adds the apply step a job was read for and
// that step's conclusion (LEGION-761).
const deliveryRunJobApplyStepVersion = 110

// TestMigrationAddsTheJobApplyStep: a job stored before the migration reads back with both new
// columns null and nothing rewritten, so the change log records nothing.
func TestMigrationAddsTheJobApplyStep(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunJobApplyStepVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryRunJobApplyStepVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, url, head_branch, event)
		values ('acme/widgets', 1, 'deploy', 'abc', '2026-10-13T11:40:00Z', '2026-10-13T12:01:00Z', 'https://github.com/acme/widgets/actions/runs/1', 'main', 'push');
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		values ('acme/widgets', 1, 'production / production-apply', '2026-10-13T12:10:00Z', '2026-10-13T12:20:00Z', 'success')
	`); err != nil {
		t.Fatalf("seed a job stored before the migration: %v", err)
	}
	var changesBefore int
	if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes`).Scan(&changesBefore); err != nil {
		t.Fatalf("count delivery_changes: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunJobApplyStepVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryRunJobApplyStepVersion, err)
	}
	var step, conclusion *string
	if err := store.Pool.QueryRow(ctx, `select apply_step, apply_step_conclusion from delivery_run_jobs where run_id = 1`).Scan(&step, &conclusion); err != nil {
		t.Fatalf("read the job back: %v", err)
	}
	if step != nil || conclusion != nil {
		t.Errorf("apply_step %v, apply_step_conclusion %v; want both null on a job stored before the migration", step, conclusion)
	}
	var changesAfter int
	if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes`).Scan(&changesAfter); err != nil {
		t.Fatalf("count delivery_changes: %v", err)
	}
	if changesAfter != changesBefore {
		t.Errorf("delivery_changes rows = %d after the migration, want %d: it rewrites no row", changesAfter, changesBefore)
	}
}
