package delivery

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestReconcileCatchesAMissedPullRequest proves the plan's "a missed event appears by the next
// five-minute reconcile": a merged PR intake's NATS consumer never saw (simulating a dropped
// delivery -- no envelope is published anywhere in this test) is found and stored by reconcile's
// own merged-PR search, complete, through the same upsert intake uses.
func TestReconcileCatchesAMissedPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		node := searchNodeJSON(50, "feat: a missed merge", "octocat", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
		node["labels"] = map[string]any{"nodes": []any{map[string]any{"name": "non-task"}}}
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/50", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 50, "title": "feat: a missed merge", "html_url": "https://github.com/acme/widgets/pull/50",
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "deadbeef", "additions": 5, "deletions": 1, "body": "",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/50/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "feat: a missed merge", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}},
		})
	})
	// No workflow runs in this window: the test isolates the merged-PR path.
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client)
	reconcile.runOnce(ctx)

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 50))
	if err != nil {
		t.Fatalf("scan reconciled pull request: %v", err)
	}
	if pr.Title != "feat: a missed merge" || pr.Author != "octocat" {
		t.Fatalf("reconciled pull request = %+v", pr)
	}
	if pr.Partial {
		t.Error("pr.Partial = true after reconcile's own partial-completion pass, want false")
	}
	if pr.Additions == nil || *pr.Additions != 5 {
		t.Errorf("pr.Additions = %v, want 5", pr.Additions)
	}
	if pr.FirstCommitAt == nil {
		t.Error("pr.FirstCommitAt is nil, want the completing fetch to have filled it in")
	}

	got, err := GetSettings(ctx, pool)
	if err != nil || got.LastReconcileAt == nil || got.LastError != nil {
		t.Errorf("settings freshness after a healthy reconcile pass: %+v, %v", got, err)
	}
	_ = settings
}

// TestReconcileCatchesAMissedDeployRun proves the same for a deploy run intake never saw: it is
// found by the deploy workflow's run listing and upserted with its jobs.
func TestReconcileCatchesAMissedDeployRun(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{
					"id": 900, "head_sha": "cafef00d", "html_url": "https://github.com/acme/widgets/actions/runs/900",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
					"pull_requests": []any{},
				},
			},
		})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/900/jobs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"jobs": []map[string]any{
				{"name": "widgets-release / widgets-release", "started_at": "2024-01-01T02:00:00Z", "completed_at": "2024-01-01T02:10:00Z", "conclusion": "success"},
			},
		})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client)
	reconcile.runOnce(ctx)

	run, err := ScanRun(pool.QueryRow(ctx, `select `+RunColumns+` from delivery_runs where repo = $1 and run_id = $2`, "acme/widgets", int64(900)))
	if err != nil {
		t.Fatalf("scan reconciled run: %v", err)
	}
	if run.Kind != DeliveryRunKindDeploy || run.Conclusion == nil || *run.Conclusion != DeliveryRunConclusionSuccess {
		t.Fatalf("reconciled run = %+v", run)
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 900)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "widgets-release / widgets-release" {
		t.Fatalf("reconciled run jobs = %+v, %v", jobs, err)
	}
}

// TestReconcileRecordsAMissingPermissionByNameAndDoesNotAdvanceLastReconcileAt proves LEGION-567's
// acceptance bar: a missing GitHub App permission (Actions here) is reported by name in
// delivery_settings.last_error, and last_reconcile_at is left exactly as it was -- never advanced
// as if the pass had actually done something. Blocking per the acceptance gate.
func TestReconcileRecordsAMissingPermissionByNameAndDoesNotAdvanceLastReconcileAt(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	before, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings before reconcile: %v", err)
	}

	fake := newFakeGitHub(t)
	// Contents is still granted, but Actions is missing -- a real misconfiguration shape, not a
	// transient failure.
	fake.permissions = map[string]string{"contents": "read", "pull_requests": "read"}
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client)
	reconcile.runOnce(ctx)

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError == nil || !strings.Contains(*after.LastError, "Actions") {
		t.Fatalf("settings.LastError = %v, want an error naming the missing Actions permission", after.LastError)
	}
	if !reflect.DeepEqual(after.LastReconcileAt, before.LastReconcileAt) {
		t.Fatalf("settings.LastReconcileAt changed from %v to %v on a pass that never ran a single GitHub call successfully", before.LastReconcileAt, after.LastReconcileAt)
	}
}

// TestReconcileNeverWipesACompletePullRequestsAttribution proves blocking #2 end-to-end through
// the real reconcile path (not just store.go's upsert in isolation): a PR intake already
// completed with real session/issue attribution survives a reconcile pass that re-finds the same
// PR via its merged-PR search, which carries no session data at all.
func TestReconcileNeverWipesACompletePullRequestsAttribution(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 60, Title: "feat: already complete", URL: "https://github.com/acme/widgets/pull/60",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Sessions: []string{"01a1-real-session"}, Partial: false,
	}); err != nil {
		t.Fatalf("seed complete pull request: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		// The search re-finds the same PR, exactly as a real reconcile pass's overlap window
		// would, carrying no session data (search results never do) but a real label so
		// classification succeeds and the upsert actually runs (not short-circuited by an
		// unclassified-PR error, which would prove nothing about the upsert guard).
		node := searchNodeJSON(60, "feat: already complete", "octocat", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
		node["labels"] = map[string]any{"nodes": []any{map[string]any{"name": "non-task"}}}
		mustEncode(t, w, searchResponseJSON(1, []map[string]any{node}, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fpr-checks.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client)
	reconcile.runOnce(ctx)

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 60))
	if err != nil {
		t.Fatalf("scan pull request: %v", err)
	}
	if pr.Partial {
		t.Error("pr.Partial = true after a reconcile pass that re-found an already-complete PR, want false (never regressed)")
	}
	if len(pr.Sessions) != 1 || pr.Sessions[0] != "01a1-real-session" {
		t.Fatalf("pr.Sessions = %v, want [01a1-real-session] (attribution wiped by reconcile's re-discovery)", pr.Sessions)
	}
}
