package store

import (
	"context"
	"testing"
)

// deliveryRunCreatedAtVersion is the migration that adds delivery_runs.created_at and the index
// over a run's creation time (LEGION-730).
const deliveryRunCreatedAtVersion = 108

// TestMigrationAddsRunCreatedAt runs the shipped migration files, never a constant copy of them:
// a database at the version before, holding one stored run, migrates to this one and reads the
// run back with created_at null and nothing else rewritten, with the creation-time index in place
// and the head-commit index gone, and the change log records nothing for a migration that rewrote
// no row.
func TestMigrationAddsRunCreatedAt(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunCreatedAtVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryRunCreatedAtVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, url, head_branch, event)
		values ('acme/widgets', 1, 'deploy', 'abc', '2026-10-07T11:40:00Z', '2026-10-07T12:01:00Z', 'https://github.com/acme/widgets/actions/runs/1', 'main', 'push')
	`); err != nil {
		t.Fatalf("seed a run stored before the migration: %v", err)
	}
	var changesBefore int
	if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes`).Scan(&changesBefore); err != nil {
		t.Fatalf("count delivery_changes: %v", err)
	}

	if err := store.migrate(ctx, migrationsThrough(t, deliveryRunCreatedAtVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryRunCreatedAtVersion, err)
	}
	var createdAt *string
	var startedAt string
	if err := store.Pool.QueryRow(ctx, `select created_at::text, started_at::text from delivery_runs where run_id = 1`).Scan(&createdAt, &startedAt); err != nil {
		t.Fatalf("read the run back: %v", err)
	}
	if createdAt != nil {
		t.Errorf("created_at = %v, want null on a run stored before the migration", *createdAt)
	}
	for index, want := range map[string]int{"delivery_runs_kind_created": 1, "delivery_runs_kind_started": 0} {
		var got int
		if err := store.Pool.QueryRow(ctx, `select count(*) from pg_indexes where tablename = 'delivery_runs' and indexname = $1`, index).Scan(&got); err != nil {
			t.Fatalf("read pg_indexes: %v", err)
		}
		if got != want {
			t.Errorf("%s indexes = %d, want %d", index, got, want)
		}
	}
	var changesAfter int
	if err := store.Pool.QueryRow(ctx, `select count(*) from delivery_changes`).Scan(&changesAfter); err != nil {
		t.Fatalf("count delivery_changes: %v", err)
	}
	if changesAfter != changesBefore {
		t.Errorf("delivery_changes rows = %d after the migration, want %d: it rewrites no row", changesAfter, changesBefore)
	}
}
