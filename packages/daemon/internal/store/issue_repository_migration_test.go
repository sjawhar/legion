package store

import (
	"context"
	"testing"
)

// A record written before 0043 carries no repository. Migration 0043 adds the repository and its
// default branch empty, for the next boot to fill from Dispatch and GitHub (the daemon's
// backfillRepositories), and the record store refuses to write an empty one from then on.
func TestIssuesPredating0043MigrateWithNoRepository(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 42)
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'planning', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var repo, branch string
	if err := store.Pool().QueryRow(ctx, `select repo, default_branch from issues where key = 'LEGION-208'`).Scan(&repo, &branch); err != nil {
		t.Fatal(err)
	}
	if repo != "" || branch != "" {
		t.Fatalf("migrated record carries repo=%q default_branch=%q, want both empty", repo, branch)
	}
	var notes int
	if err := store.Pool().QueryRow(ctx, `select count(*) from repo_notes`).Scan(&notes); err != nil || notes != 0 {
		t.Fatalf("repo_notes = %d, %v; want an empty table", notes, err)
	}
}
