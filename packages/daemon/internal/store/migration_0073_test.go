package store

import (
	"context"
	"testing"
)

// 0073 records the commit a push left a pull request's base branch at. No release before it
// recorded a push onto a base at all, so a pull request written before it has none to carry: the
// column takes the empty value, which reads as "no push onto the base recorded", and the daemon
// reads that pull request again at its issue's next event, its next handoff or the next start.
func TestAPullRequestRecordedBefore0073HasNoMovedBase(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 72)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state, base)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 0, 0, '[]',
		 1, '', '', false, 0, 'open', '', '[]', '', 'MERGEABLE', 'clean', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var moved string
	if err := store.Pool().QueryRow(ctx,
		`select base_moved from pull_requests where issue = 'LEGION-208'`).Scan(&moved); err != nil {
		t.Fatalf("read the pull request after 0073: %v", err)
	}
	if moved != "" {
		t.Fatalf("base_moved after 0073 = %q, want empty for a pull request recorded before it", moved)
	}
}
