package delivery

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestReconcileCatchesAMissedPullRequest proves the plan's "a missed event appears by the next
// five-minute reconcile": a merged PR intake's NATS consumer never saw (simulating a dropped
// delivery -- no envelope is published anywhere in this test) is found and stored by reconcile's
// own merged-PR search, complete, through the same upsert intake uses.
func TestReconcileCatchesAMissedPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		item := searchIssueItemJSON(50, "feat: a missed merge", "octocat", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
		item["labels"] = []any{map[string]any{"name": "non-task"}}
		mustEncode(t, w, map[string]any{
			"total_count": 1,
			"items":       []map[string]any{item},
		})
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
	if err != nil || got.LastReconcileAt == nil {
		t.Errorf("settings.LastReconcileAt not recorded after a reconcile pass: %+v, %v", got, err)
	}
	_ = settings
}

// TestReconcileCatchesAMissedDeployRun proves the same for a deploy run intake never saw: it is
// found by the deploy workflow's run listing and upserted with its jobs.
func TestReconcileCatchesAMissedDeployRun(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"total_count": 0, "items": []any{}})
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
				{"name": "production-apply / production-apply", "started_at": "2024-01-01T02:00:00Z", "completed_at": "2024-01-01T02:10:00Z", "conclusion": "success"},
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
	if run.Kind != model.DeliveryRunKindDeploy || run.Conclusion == nil || *run.Conclusion != model.DeliveryConclusionSuccess {
		t.Fatalf("reconciled run = %+v", run)
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 900)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "production-apply / production-apply" {
		t.Fatalf("reconciled run jobs = %+v, %v", jobs, err)
	}
}
