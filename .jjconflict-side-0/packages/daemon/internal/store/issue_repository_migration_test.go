package store

import (
	"context"
	"testing"
)

// A record written before 0047 carries no repository. Migration 0047 adds the repository and its
// default branch empty, for the next boot to fill from Dispatch and GitHub (the daemon's
// backfillRepositories), and the record store refuses to write an empty one from then on.
func TestIssuesPredating0047MigrateWithNoRepository(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 46)
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

// A record written before 0048 carries its repository in its own spelling and none of GitHub's.
// Migration 0048 adds GitHub's spelling empty, for the next boot to read from GitHub before it
// subscribes to the repository's events (the daemon's backfillRepositories): SQL cannot know it,
// since the record's spelling may be a clone's and differ from GitHub's in case.
func TestIssuesPredating0048MigrateWithNoGitHubSpelling(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 47)
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'planning', 1, 'in_progress', 'A', 1, 'acme/widgets', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var repo, github string
	if err := store.Pool().QueryRow(ctx, `select repo, github_repo from issues where key = 'LEGION-208'`).Scan(&repo, &github); err != nil {
		t.Fatal(err)
	}
	if repo != "acme/widgets" || github != "" {
		t.Fatalf("migrated record carries repo=%q github_repo=%q, want the record's own and no GitHub spelling", repo, github)
	}
}
