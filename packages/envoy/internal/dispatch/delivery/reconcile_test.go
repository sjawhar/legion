package delivery

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
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

// TestReconcileClearsLastErrorOnceItSucceedsAgain proves the freshness row recovers: a prior
// failing pass's named error must not linger once a later pass actually succeeds --
// RecordReconcileSuccess's own "last_error = null" is exercised here through the real reconcile
// path, not just the bare SQL statement.
func TestReconcileClearsLastErrorOnceItSucceedsAgain(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	if err := RecordReconcileError(ctx, pool, "the installation lacks Actions: read on acme/widgets"); err != nil {
		t.Fatalf("seed a prior failure: %v", err)
	}

	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
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

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError != nil {
		t.Fatalf("settings.LastError = %v, want nil once a pass has actually succeeded", *after.LastError)
	}
	if after.LastReconcileAt == nil {
		t.Fatal("settings.LastReconcileAt is nil, want it set by the successful pass")
	}
}

// TestReconcileMarksA404PullRequestUnfetchableAndSkipsItUntilItRecovers proves S3: a partial row
// whose completing fetch answers a permanent 404 is marked unfetchable (not retried every pass
// forever), and a later successful fetch of the same pull request clears it again.
func TestReconcileMarksA404PullRequestUnfetchableAndSkipsItUntilItRecovers(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 70, Title: "feat: gone", URL: "https://github.com/acme/widgets/pull/70",
		Author: "octocat", CreatedAt: &merged, Partial: true,
	}); err != nil {
		t.Fatalf("seed partial pull request: %v", err)
	}

	requests := 0
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/70", func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
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
	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 70))
	if err != nil {
		t.Fatalf("scan after first pass: %v", err)
	}
	if pr.UnfetchableAt == nil {
		t.Fatal("pr.UnfetchableAt is nil after a 404, want it set")
	}
	if pr.UnfetchableReason == nil || !strings.Contains(*pr.UnfetchableReason, "not found or gone") {
		t.Errorf("pr.UnfetchableReason = %v, want it to name the 404", pr.UnfetchableReason)
	}
	if requests != 1 {
		t.Fatalf("requests to GET the pull request = %d, want 1", requests)
	}

	// A second pass must not retry it: ListPartialPullRequests excludes a marked row.
	reconcile.runOnce(ctx)
	if requests != 1 {
		t.Fatalf("requests after a second pass = %d, want still 1 (an unfetchable row is never retried)", requests)
	}

	// A later successful fetch of the same pull request (a live webhook retry, here simulated
	// directly through the shared upsert) clears it.
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 70, Title: "feat: gone", URL: "https://github.com/acme/widgets/pull/70",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Partial: false,
	}); err != nil {
		t.Fatalf("simulate a later successful fetch: %v", err)
	}
	recovered, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where repo = $1 and number = $2`, "acme/widgets", 70))
	if err != nil {
		t.Fatalf("scan after recovery: %v", err)
	}
	if recovered.UnfetchableAt != nil || recovered.UnfetchableReason != nil {
		t.Errorf("recovered.UnfetchableAt = %v, UnfetchableReason = %v, want both nil after a later successful write", recovered.UnfetchableAt, recovered.UnfetchableReason)
	}
}

// TestReconcileWorkflowFailureStopsThePassFromReportingSuccess proves S1: a per-run failure (here,
// the jobs listing for a run the runs listing itself found successfully) must not be swallowed as
// a healthy pass -- last_reconcile_at must not advance past a window this pass left incompletely
// fetched.
func TestReconcileWorkflowFailureStopsThePassFromReportingSuccess(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	before, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings before reconcile: %v", err)
	}

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
	// The run listing succeeds, but its jobs listing fails: a run reconcile already knows about,
	// upserted, yet never finished.
	fake.handle("GET /repos/acme/widgets/actions/runs/900/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
	})
	client := fake.newTestClient()

	reconcile := NewReconcile(pool, client)
	reconcile.runOnce(ctx)

	run, err := ScanRun(pool.QueryRow(ctx, `select `+RunColumns+` from delivery_runs where repo = $1 and run_id = $2`, "acme/widgets", int64(900)))
	if err != nil {
		t.Fatalf("the run itself must still be upserted even though its jobs failed: %v", err)
	}
	if run.RunID != 900 {
		t.Fatalf("run = %+v", run)
	}

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError == nil {
		t.Fatal("settings.LastError is nil, want the jobs-listing failure recorded by name")
	}
	if !strings.Contains(*after.LastError, "900") {
		t.Errorf("settings.LastError = %q, want it to name run 900", *after.LastError)
	}
	if !reflect.DeepEqual(after.LastReconcileAt, before.LastReconcileAt) {
		t.Fatalf("settings.LastReconcileAt changed from %v to %v on a pass with a real per-run failure", before.LastReconcileAt, after.LastReconcileAt)
	}
}

// TestReconcileMarksA404RunJobsUnfetchableAndSkipsItUntilItRecovers proves Deep's finding: unlike
// TestReconcileWorkflowFailureStopsThePassFromReportingSuccess's transient 500 (which must keep
// failing the pass), a run whose jobs listing answers a permanent 404 must not fail the pass at
// all -- it is marked unfetchable instead, the same treatment S3 gives a permanently 404ing pull
// request -- and a later pass must not even re-call the jobs endpoint for it. reconcileRun itself
// never retries a run it has marked unfetchable (there is no "the window reopened" signal to
// retry on, unlike a partial pull request's own completing fetch); what clears the mark is an
// independent successful write of its jobs -- here, standing in for intake.go's own live-webhook
// UpsertRunJobs call, which already runs through the same shared upsert.
func TestReconcileMarksA404RunJobsUnfetchableAndSkipsItUntilItRecovers(t *testing.T) {
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
					"id": 901, "head_sha": "deadbeef", "html_url": "https://github.com/acme/widgets/actions/runs/901",
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
	var jobsCalls atomic.Int32
	fake.handle("GET /repos/acme/widgets/actions/runs/901/jobs", func(w http.ResponseWriter, r *http.Request) {
		jobsCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	client := fake.newTestClient()
	reconcile := NewReconcile(pool, client)

	reconcile.runOnce(ctx)
	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after first pass: %v", err)
	}
	if after.LastError != nil {
		t.Fatalf("settings.LastError = %q, want nil: a permanently 404ing run's jobs must not fail the whole pass", *after.LastError)
	}
	if after.LastReconcileAt == nil {
		t.Fatal("settings.LastReconcileAt is nil, want it advanced: the pass must be reported healthy")
	}
	if jobsCalls.Load() != 1 {
		t.Fatalf("jobs endpoint called %d times, want 1", jobsCalls.Load())
	}
	unfetchable, err := RunJobsUnfetchable(ctx, pool, "acme/widgets", 901)
	if err != nil {
		t.Fatalf("RunJobsUnfetchable after first pass: %v", err)
	}
	if !unfetchable {
		t.Fatal("RunJobsUnfetchable = false after a permanent 404, want true")
	}

	// A second pass must skip the jobs listing entirely: the run is already marked unfetchable.
	reconcile.runOnce(ctx)
	if jobsCalls.Load() != 1 {
		t.Fatalf("jobs endpoint called %d times after a second pass, want still 1 (skipped once marked unfetchable)", jobsCalls.Load())
	}

	// An independent successful write of this run's jobs (a live webhook retry's own
	// UpsertRunJobs call, intake.go's own write path) clears the mark.
	if err := UpsertRunJobs(ctx, pool, "acme/widgets", 901, []DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 901, Name: "build", StartedAt: ptrTime("2024-01-01T02:00:00Z"), CompletedAt: ptrTime("2024-01-01T02:05:00Z")},
	}); err != nil {
		t.Fatalf("UpsertRunJobs (simulating a live webhook retry): %v", err)
	}
	unfetchable, err = RunJobsUnfetchable(ctx, pool, "acme/widgets", 901)
	if err != nil {
		t.Fatalf("RunJobsUnfetchable after recovery: %v", err)
	}
	if unfetchable {
		t.Fatal("RunJobsUnfetchable = true after a successful UpsertRunJobs, want false")
	}
	jobs, err := ListRunJobs(ctx, pool, "acme/widgets", 901)
	if err != nil {
		t.Fatalf("ListRunJobs after recovery: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "build" {
		t.Fatalf("jobs = %+v, want one job named build once the listing recovers", jobs)
	}
}

// TestReconcilePartialPullRequestsStopsOnRateLimitInsteadOfRetryingAtFullConcurrency proves the
// errgroup.WithContext + SetLimit(8) rate-limit stop end to end: among several partial rows, the
// one answering a 403 secondary-rate-limit response must stop every further completion from
// starting (never more than the ones already in flight when it happened), and the pass must be
// reported failed, not successful.
func TestReconcilePartialPullRequestsStopsOnRateLimitInsteadOfRetryingAtFullConcurrency(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	const total = 20
	for i := 1; i <= total; i++ {
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: "acme/widgets", Number: i, Title: "feat: x", URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", i),
			Author: "octocat", CreatedAt: &merged, Partial: true,
		}); err != nil {
			t.Fatalf("seed partial pull request #%d: %v", i, err)
		}
	}

	var attempts atomic.Int64
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
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

	got := attempts.Load()
	if got < 1 {
		t.Fatal("expected at least the first batch of completions to have been attempted")
	}
	if got > reconcilePartialConcurrency {
		t.Errorf("GET pulls attempts = %d, want at most reconcilePartialConcurrency (%d): the rate-limit stop must not let more than one wave start after the first limit response", got, reconcilePartialConcurrency)
	}
	if got >= total {
		t.Errorf("GET pulls attempts = %d out of %d partial rows, want it to stop well short of retrying every row at full concurrency", got, total)
	}

	after, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("GetSettings after reconcile: %v", err)
	}
	if after.LastError == nil {
		t.Fatal("settings.LastError is nil, want the rate limit recorded and the pass reported failed")
	}
}

// TestReconcileWorkflowStopsAtTheFirstRateLimitAndCapsLastError proves the per-run rate-limit
// stop end to end: among three completed runs in one window, the second's jobs listing
// answering a rate limit must stop the third's jobs listing from ever being requested (the
// un-fixed loop would send every remaining run's request after the first limit), the returned
// error must still satisfy errors.As for a *githubapp.RateLimitError (not stringified away into
// the per-run aggregate), and fail()'s own maxLastErrorLength bound still caps what reaches
// delivery_settings.last_error. Removing reconcileWorkflow's unconditional per-run stop on a
// rate limit (reverting to "log and keep going") makes this red: the third run's jobs endpoint
// would be called, defeating the "no further GitHub request" assertion below.
func TestReconcileWorkflowStopsAtTheFirstRateLimitAndCapsLastError(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 3,
			"workflow_runs": []map[string]any{
				{
					"id": 910, "head_sha": "cafef00d", "html_url": "https://github.com/acme/widgets/actions/runs/910",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:55:00Z"},
					"pull_requests": []any{},
				},
				{
					"id": 911, "head_sha": "deadbeef", "html_url": "https://github.com/acme/widgets/actions/runs/911",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T03:00:00Z", "created_at": "2024-01-01T03:00:00Z",
					"updated_at":    "2024-01-01T03:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T02:55:00Z"},
					"pull_requests": []any{},
				},
				{
					"id": 912, "head_sha": "f00dcafe", "html_url": "https://github.com/acme/widgets/actions/runs/912",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T04:00:00Z", "created_at": "2024-01-01T04:00:00Z",
					"updated_at":    "2024-01-01T04:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T03:55:00Z"},
					"pull_requests": []any{},
				},
			},
		})
	})
	var firstJobsCalls, secondJobsCalls, thirdJobsCalls atomic.Int32
	fake.handle("GET /repos/acme/widgets/actions/runs/910/jobs", func(w http.ResponseWriter, r *http.Request) {
		firstJobsCalls.Add(1)
		mustEncode(t, w, map[string]any{"jobs": []any{}})
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/911/jobs", func(w http.ResponseWriter, r *http.Request) {
		secondJobsCalls.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})
	fake.handle("GET /repos/acme/widgets/actions/runs/912/jobs", func(w http.ResponseWriter, r *http.Request) {
		thirdJobsCalls.Add(1)
		mustEncode(t, w, map[string]any{"jobs": []any{}})
	})
	client := fake.newTestClient()
	reconcile := NewReconcile(pool, client)

	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	err := reconcile.reconcileWorkflow(ctx, "acme", "widgets", "acme/widgets", ".github/workflows/deploy.yml", DeliveryRunKindDeploy, until)
	if err == nil {
		t.Fatal("reconcileWorkflow: err = nil, want the second run's rate limit to fail the pass")
	}
	var limited *githubapp.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("errors.As(err, &limited) = false, want true (the typed rate limit must survive the per-run aggregation); err = %v", err)
	}

	if firstJobsCalls.Load() != 1 {
		t.Fatalf("first run's jobs endpoint called %d times, want 1", firstJobsCalls.Load())
	}
	if secondJobsCalls.Load() != 1 {
		t.Fatalf("second (rate-limited) run's jobs endpoint called %d times, want 1", secondJobsCalls.Load())
	}
	if thirdJobsCalls.Load() != 0 {
		t.Fatalf("third run's jobs endpoint called %d times, want 0: the rate limit must stop every further request", thirdJobsCalls.Load())
	}

	reconcile.fail(ctx, err)
	settings, getErr := GetSettings(ctx, pool)
	if getErr != nil {
		t.Fatalf("GetSettings after fail: %v", getErr)
	}
	if settings.LastError == nil {
		t.Fatal("settings.LastError is nil, want the rate limit recorded")
	}
	if len(*settings.LastError) > maxLastErrorLength {
		t.Fatalf("len(settings.LastError) = %d, want capped at maxLastErrorLength (%d)", len(*settings.LastError), maxLastErrorLength)
	}
}
