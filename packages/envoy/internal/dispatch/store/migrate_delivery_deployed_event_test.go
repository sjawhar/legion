package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// deliveryDeployedEventVersion is the migration that adds the once-per-pull-request marker of the
// issue.deployed event and stamps the pull requests already shipped when it arrives (LEGION-781).
const deliveryDeployedEventVersion = 114

// TestMigrationStampsThePullRequestsAlreadyShipped: a merged pull request whose shipping run the
// timeline already knows reads back stamped, so the first deployed pass writes no event for it.
// One merged after the only successful run was created, one whose only later run's production job
// failed, one not merged, and one outside the deploy repository keep null and get their event when
// a shipping run arrives.
func TestMigrationStampsThePullRequestsAlreadyShipped(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, deliveryDeployedEventVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", deliveryDeployedEventVersion-1, err)
	}
	merged := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// One statement per Exec: pgx's extended protocol refuses several statements in one
	// parameterised Exec (SQLSTATE 42601), and each statement here binds the merge instant.
	for _, seed := range []string{
		`insert into delivery_settings (deploy_repo, deploy_workflow_path, production_job_names, production_apply_step_name, pr_checks_workflow_path, updated_by)
		values ('acme/widgets', '.github/workflows/deploy.yml', array['production / production-apply'], 'Run ./.github/actions/apply', '.github/workflows/pr-checks.yml', '{"kind":"system","id":"test"}')`,
		`insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, partial) values
		  ('acme/widgets', 1, 'shipped by run 100', 'https://github.com/acme/widgets/pull/1', 'octocat', $1::timestamptz, $1::timestamptz, false),
		  ('acme/widgets', 2, 'merged after run 100 was created', 'https://github.com/acme/widgets/pull/2', 'octocat', $1::timestamptz, $1::timestamptz + interval '3 hours', false),
		  ('acme/widgets', 3, 'only run 200 follows, whose production job failed', 'https://github.com/acme/widgets/pull/3', 'octocat', $1::timestamptz, $1::timestamptz + interval '90 minutes', false),
		  ('acme/widgets', 4, 'not merged', 'https://github.com/acme/widgets/pull/4', 'octocat', $1::timestamptz, null, false),
		  ('acme/gadgets', 5, 'another repository', 'https://github.com/acme/gadgets/pull/5', 'octocat', $1::timestamptz, $1::timestamptz, false)`,
		`insert into delivery_runs (repo, run_id, kind, head_sha, head_commit_at, started_at, created_at, url, head_branch, event) values
		  ('acme/widgets', 100, 'deploy', 'abc', $1::timestamptz, $1::timestamptz + interval '1 hour', $1::timestamptz + interval '1 hour', 'https://github.com/acme/widgets/actions/runs/100', 'main', 'push'),
		  ('acme/widgets', 200, 'deploy', 'def', $1::timestamptz, $1::timestamptz + interval '2 hours', $1::timestamptz + interval '2 hours', 'https://github.com/acme/widgets/actions/runs/200', 'main', 'push'),
		  ('acme/gadgets', 300, 'deploy', 'ghi', $1::timestamptz, $1::timestamptz + interval '1 hour', $1::timestamptz + interval '1 hour', 'https://github.com/acme/gadgets/actions/runs/300', 'main', 'push')`,
		`insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion, apply_step, apply_step_conclusion) values
		  ('acme/widgets', 100, 'production / production-apply', $1::timestamptz + interval '1 hour', $1::timestamptz + interval '70 minutes', 'success', 'Run ./.github/actions/apply', 'success'),
		  ('acme/widgets', 200, 'production / production-apply', $1::timestamptz + interval '2 hours', $1::timestamptz + interval '130 minutes', 'failure', 'Run ./.github/actions/apply', 'failure'),
		  ('acme/gadgets', 300, 'production / production-apply', $1::timestamptz + interval '1 hour', $1::timestamptz + interval '70 minutes', 'success', null, null)`,
	} {
		var args []any
		if strings.Contains(seed, "$1") {
			args = append(args, merged)
		}
		if _, err := store.Pool.Exec(ctx, seed, args...); err != nil {
			t.Fatalf("seed the timeline before the migration: %v", err)
		}
	}
	if err := store.migrate(ctx, migrationsThrough(t, deliveryDeployedEventVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", deliveryDeployedEventVersion, err)
	}
	for _, tc := range []struct {
		repo    string
		number  int
		stamped bool
	}{
		{"acme/widgets", 1, true},
		{"acme/widgets", 2, false},
		{"acme/widgets", 3, false},
		{"acme/widgets", 4, false},
		{"acme/gadgets", 5, false},
	} {
		var stamped bool
		if err := store.Pool.QueryRow(ctx, `select deployed_event_at is not null from delivery_pull_requests where repo = $1 and number = $2`,
			tc.repo, tc.number).Scan(&stamped); err != nil {
			t.Fatalf("read %s#%d: %v", tc.repo, tc.number, err)
		}
		if stamped != tc.stamped {
			t.Errorf("%s: deployed_event_at set = %t, want %t", fmt.Sprintf("%s#%d", tc.repo, tc.number), stamped, tc.stamped)
		}
	}
}
