package store

import (
	"context"
	"testing"
)

// 0059 adds the head a counted fix attempt moved from. A pull request recorded before it, its
// count standing, gets an empty one, which no push names, so a late push takes nothing back from it.
func TestAPullRequestCountedBefore0059NamesNoMoveItsCountCameFrom(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 58)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'testing', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests
		(issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts, blocked_attempts, check_runs,
		 generation, snapshot, pushes, head_counted, head_counted_before, planned_red, review_seen, state, checked_head,
		 required_workflows, required_workflows_head, mergeability)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 3, 0, '[]',
		 0, '', '[]', 'h4', 'h2', false, 0, 'open', '', '[]', '', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var counted, from string
	if err := store.Pool().QueryRow(ctx, `select head_counted, head_counted_from from pull_requests where issue = 'LEGION-208'`).Scan(&counted, &from); err != nil {
		t.Fatalf("read head_counted_from: %v", err)
	}
	if counted != "h4" || from != "" {
		t.Fatalf("head_counted=%q head_counted_from=%q after 0059, want \"h4\" and \"\"", counted, from)
	}
}
