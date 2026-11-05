package store

import (
	"context"
	"testing"
)

// 0061 adds the head run and drops the marks a late push took a count back by. A pull request
// recorded before it, its count standing, keeps its count and its mark and has no run open, so a
// late push for its head replays nothing and takes nothing back.
func TestAPullRequestRecordedBefore0061HasNoHeadRunOpen(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 60)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'testing', 1, 'testing', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests
		(issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts, blocked_attempts, check_runs,
		 generation, snapshot, pushes, head_counted, head_counted_before, head_counted_from, head_arrived_from, planned_red,
		 review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 3, 0, '[]',
		 0, '', '[]', 'h4', 'h2', 'h3', 'h3', false, 0, 'open', '', '[]', '', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var attempts int
	var counted, moves string
	var open bool
	if err := store.Pool().QueryRow(ctx, `select fix_attempts, head_counted, head_run is not null, head_moves::text from pull_requests where issue = 'LEGION-208'`).
		Scan(&attempts, &counted, &open, &moves); err != nil {
		t.Fatalf("read the pull request after 0061: %v", err)
	}
	if attempts != 3 || counted != "h4" || open || moves != "[]" {
		t.Fatalf("fix_attempts=%d head_counted=%q head run open %v head_moves %s after 0061, want 3, \"h4\", none and []", attempts, counted, open, moves)
	}
	var dropped int
	if err := store.Pool().QueryRow(ctx, `select count(*) from information_schema.columns where table_name = 'pull_requests'
		and column_name in ('head_counted_before', 'head_counted_from', 'head_arrived_from')`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Fatalf("%d of the marks 0061 drops remain", dropped)
	}
}
