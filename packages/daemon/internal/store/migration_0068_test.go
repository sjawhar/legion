package store

import (
	"context"
	"testing"
)

// 0068 records the base branch a pull request's head merges into, and the index the base push's
// handler looks the open pull requests of one base up by. A pull request written before it has no
// base in the row to derive one from: the column takes the empty value, which reads as "not
// recorded yet" and matches no base push, and the daemon's next read of the pull request fills it.
func TestAPullRequestRecordedBefore0068HasNoBase(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 67)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'root', 'reviewing', 1, 'needs_review', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests (issue, repo, number, branch, head_sha, head_updated_at,
		failing, cancelled, fix_attempts, blocked_attempts, check_runs, generation, snapshot, head_counted, planned_red,
		review_seen, state, checked_head, required_workflows, required_workflows_head, mergeability, merge_state)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'h4', now(), '[]', '[]', 0, 0, '[]',
		 1, '', '', false, 0, 'open', '', '[]', '', 'MERGEABLE', 'clean')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var base string
	if err := store.Pool().QueryRow(ctx,
		`select base from pull_requests where issue = 'LEGION-208'`).Scan(&base); err != nil {
		t.Fatalf("read the pull request after 0068: %v", err)
	}
	if base != "" {
		t.Fatalf("base after 0068 = %q, want empty for a pull request recorded before it", base)
	}
	var indexed bool
	if err := store.Pool().QueryRow(ctx,
		`select exists (select 1 from pg_indexes where tablename = 'pull_requests' and indexname = 'pull_requests_open_on_base')`).
		Scan(&indexed); err != nil {
		t.Fatalf("read the indexes of pull_requests after 0068: %v", err)
	}
	if !indexed {
		t.Fatal("0068 left no pull_requests_open_on_base index; the base push's handler reads the open pull requests of a base by it")
	}
}
