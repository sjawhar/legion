package store

import (
	"context"
	"testing"
)

// 0060 adds where a pull request's head moves started: the head a counted fix attempt moved from
// and the head the current head replaced. A pull request recorded before it, its count standing,
// gets an empty one of each, which no chain of pushes starts from, so a late push takes nothing
// back from it and settles its planned mark only as that head's own push.
func TestAPullRequestRecordedBefore0060NamesNoStartOfItsMoves(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 59)
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
	migrateThrough(t, store, 60)
	var counted, countedFrom, arrivedFrom string
	if err := store.Pool().QueryRow(ctx, `select head_counted, head_counted_from, head_arrived_from from pull_requests where issue = 'LEGION-208'`).
		Scan(&counted, &countedFrom, &arrivedFrom); err != nil {
		t.Fatalf("read the columns 0060 adds: %v", err)
	}
	if counted != "h4" || countedFrom != "" || arrivedFrom != "" {
		t.Fatalf("head_counted=%q head_counted_from=%q head_arrived_from=%q after 0060, want \"h4\", \"\" and \"\"", counted, countedFrom, arrivedFrom)
	}
}
