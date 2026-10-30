package store

import (
	"context"
	"testing"
)

// 0064 records GitHub's merge state for a pull request's head. A pull request written before it
// has no merge state to carry: the row keeps the tri-state mergeability decoded from the same
// GitHub answer, and that boolean says nothing about whether GitHub holds the head on a check or
// a review the repository requires. The column takes the empty value rather than a state the
// migration would have to invent, and empty reads as "never read", which the daemon's next read
// of the pull request fills.
func TestAPullRequestRecordedBefore0064HasNoMergeState(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 63)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'retro', 1, 'retro', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 0, 0, '[]',
		 1, '', '', false, 0, 'open', '', '[]', '', 'MERGEABLE')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var state string
	if err := store.Pool().QueryRow(ctx,
		`select merge_state from pull_requests where issue = 'LEGION-208'`).Scan(&state); err != nil {
		t.Fatalf("read the pull request after 0064: %v", err)
	}
	if state != "" {
		t.Fatalf("merge state after 0064 = %q, want empty for a pull request read before it", state)
	}
}
