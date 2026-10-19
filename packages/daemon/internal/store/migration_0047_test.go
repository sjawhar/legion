package store

import (
	"context"
	"strings"
	"testing"
)

// The architect accepts before the merge and the merge closes the issue, so production_check is no
// phase. An issue the old daemon left there, or held from there, has its pull request merged and
// stands at awaiting_merge after 0047, where the new daemon's boot closes it; the phase constraints
// then admit accepting and refuse production_check.
func TestTheOldPostMergePhaseMigratesToAwaitingMergeAndTheConstraintsSwap(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 46)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, held_from)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'checking root', 'production_check', 1, 'retro', 'A', 1, null),
		       ('LEGION-209', 'LEGION-208', 'LEGION', 'held child', 'held', 1, 'retro', 'B', 1, 'production_check'),
		       ('LEGION-210', 'LEGION-208', 'LEGION', 'reviewing child', 'reviewing', 1, 'needs_review', 'C', 1, null)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, want := range []struct{ key, phase, heldFrom string }{
		{"LEGION-208", "awaiting_merge", ""},
		{"LEGION-209", "held", "awaiting_merge"},
		{"LEGION-210", "reviewing", ""},
	} {
		var phase string
		var heldFrom *string
		if err := store.Pool().QueryRow(ctx, `select phase, held_from from issues where key = $1`, want.key).Scan(&phase, &heldFrom); err != nil {
			t.Fatalf("read %s: %v", want.key, err)
		}
		got := ""
		if heldFrom != nil {
			got = *heldFrom
		}
		if phase != want.phase || got != want.heldFrom {
			t.Fatalf("%s after 0047 = phase %q held_from %q, want %q and %q", want.key, phase, got, want.phase, want.heldFrom)
		}
	}
	if _, err := store.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-211', 'LEGION-208', 'LEGION', 'accepted child', 'accepting', 1, 'needs_review', 'D', 1)`); err != nil {
		t.Fatalf("insert an issue in accepting: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `update issues set phase = 'held', held_from = 'accepting' where key = 'LEGION-211'`); err != nil {
		t.Fatalf("hold an issue from accepting: %v", err)
	}
	for _, refused := range []string{
		`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
			values ('LEGION-212', 'LEGION-208', 'LEGION', 'old phase', 'production_check', 1, 'retro', 'E', 1)`,
		`update issues set held_from = 'production_check' where key = 'LEGION-211'`,
	} {
		_, err := store.Pool().Exec(ctx, refused)
		if err == nil || !strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("production_check after 0047 = %v, want the check constraint to refuse it", err)
		}
	}
}

// 0047 also gives the pull request its merge commit, empty for one merged before the column
// existed, and the production check its table: a row per closed issue the constraint admits with
// a known outcome and refuses with any other, and the outbox admits the check's three kinds.
func TestTheMergeCommitAndTheProductionCheckArriveWith0047(t *testing.T) {
	store := emptyStore(t)
	ctx := context.Background()
	migrateThrough(t, store, 46)
	if _, err := store.Pool().Exec(ctx, `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'merged root', 'awaiting_merge', 1, 'retro', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx, `insert into pull_requests
		(issue, repo, number, branch, head_sha, head_updated_at, failing, cancelled, fix_attempts, blocked_attempts, check_runs,
		 generation, snapshot, pushes, head_counted, head_counted_before, planned_red, review_seen, state, checked_head,
		 required_workflows, required_workflows_head, mergeability)
		values ('LEGION-208', 'sjawhar/legion', 1208, 'legion/LEGION-208', 'b1c2d3', now(), '[]', '[]', 0, 0, '[]',
		 1, '', '[]', '', '', false, 0, 'merged', '', '[]', '', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var mergeSHA string
	if err := store.Pool().QueryRow(ctx, `select merge_sha from pull_requests where issue = 'LEGION-208'`).Scan(&mergeSHA); err != nil {
		t.Fatalf("read merge_sha: %v", err)
	}
	if mergeSHA != "" {
		t.Fatalf("merge_sha of a pull request merged before 0047 = %q, want empty", mergeSHA)
	}
	if _, err := store.Pool().Exec(ctx, `insert into production_checks (issue, project, tree, repo, pr_number, merge_sha, opened_at, bound_at)
		values ('LEGION-208', 'LEGION', 'LEGION-208', 'sjawhar/legion', 1208, 'd4e5f6', now(), now() + interval '336 hours')`); err != nil {
		t.Fatalf("insert a production check: %v", err)
	}
	var looks int
	var lookOpen, last bool
	var outcome *string
	if err := store.Pool().QueryRow(ctx, `select looks, look_open, last, outcome from production_checks where issue = 'LEGION-208'`).Scan(&looks, &lookOpen, &last, &outcome); err != nil {
		t.Fatalf("read the production check: %v", err)
	}
	if looks != 0 || lookOpen || last || outcome != nil {
		t.Fatalf("a fresh production check = looks %d, look_open %v, last %v, outcome %v; want no look, none open, not last, no outcome", looks, lookOpen, last, outcome)
	}
	if _, err := store.Pool().Exec(ctx, `update production_checks set closed_at = now(), outcome = 'not_run' where issue = 'LEGION-208'`); err != nil {
		t.Fatalf("close the production check as not_run: %v", err)
	}
	_, err := store.Pool().Exec(ctx, `update production_checks set outcome = 'shrug' where issue = 'LEGION-208'`)
	if err == nil || !strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("an unknown outcome = %v, want the check constraint to refuse it", err)
	}
	for _, kind := range []string{"check_compare", "check_look", "check_bound"} {
		if _, err := store.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, 'LEGION-208', '{}', 0, now(), '')`, kind); err != nil {
			t.Fatalf("enqueue a %s row after 0047: %v", kind, err)
		}
	}
}
