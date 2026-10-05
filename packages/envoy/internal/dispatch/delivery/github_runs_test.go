package delivery

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListWorkflowRunsMapsRunShapes(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2Fdeploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 3,
			"workflow_runs": []map[string]any{
				{
					// A run with exactly one associated pull request.
					"id": 1, "head_sha": "sha1", "html_url": "https://github.com/acme/widgets/actions/runs/1",
					"status": "completed", "conclusion": "success",
					"run_started_at": "2024-01-01T00:00:00Z", "created_at": "2024-01-01T00:00:00Z",
					"updated_at":    "2024-01-01T00:10:00Z",
					"head_commit":   map[string]any{"timestamp": "2023-12-31T23:55:00Z"},
					"pull_requests": []map[string]any{{"number": 7}},
				},
				{
					// A run with zero associated pull requests (e.g. a push run).
					"id": 2, "head_sha": "sha2", "html_url": "https://github.com/acme/widgets/actions/runs/2",
					"status": "completed", "conclusion": "failure",
					"run_started_at": "2024-01-01T01:00:00Z", "created_at": "2024-01-01T01:00:00Z",
					"updated_at":    "2024-01-01T01:05:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T00:59:00Z"},
					"pull_requests": []map[string]any{},
				},
				{
					// A run with more than one associated pull request, and a conclusion the
					// delivery_runs schema's check constraint does not accept.
					"id": 3, "head_sha": "sha3", "html_url": "https://github.com/acme/widgets/actions/runs/3",
					"status": "completed", "conclusion": "neutral",
					"run_started_at": "2024-01-01T02:00:00Z", "created_at": "2024-01-01T02:00:00Z",
					"updated_at":    "2024-01-01T02:05:00Z",
					"head_commit":   map[string]any{"timestamp": "2024-01-01T01:59:00Z"},
					"pull_requests": []map[string]any{{"number": 8}, {"number": 9}},
				},
			},
		})
	})
	client := fake.newTestClient()

	runs, err := ListWorkflowRuns(context.Background(), client, "acme", "widgets", ".github/workflows/deploy.yml",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("len(runs) = %d, want 3", len(runs))
	}

	one := runs[0]
	if one.RunID != 1 || one.HeadSHA != "sha1" || one.URL != "https://github.com/acme/widgets/actions/runs/1" {
		t.Fatalf("run 1 = %+v", one)
	}
	if !one.HeadCommitAt.Equal(*ptrTime("2023-12-31T23:55:00Z")) {
		t.Fatalf("run 1 HeadCommitAt = %v", one.HeadCommitAt)
	}
	if !one.StartedAt.Equal(*ptrTime("2024-01-01T00:00:00Z")) {
		t.Fatalf("run 1 StartedAt = %v", one.StartedAt)
	}
	if one.CompletedAt == nil || !one.CompletedAt.Equal(*ptrTime("2024-01-01T00:10:00Z")) {
		t.Fatalf("run 1 CompletedAt = %v", one.CompletedAt)
	}
	if one.Conclusion == nil || *one.Conclusion != "success" {
		t.Fatalf("run 1 Conclusion = %v, want success", one.Conclusion)
	}
	if one.PRNumber == nil || *one.PRNumber != 7 {
		t.Fatalf("run 1 PRNumber = %v, want 7 (exactly one associated PR)", one.PRNumber)
	}

	zero := runs[1]
	if zero.PRNumber != nil {
		t.Fatalf("run 2 PRNumber = %v, want nil (zero associated PRs)", zero.PRNumber)
	}

	many := runs[2]
	if many.PRNumber != nil {
		t.Fatalf("run 3 PRNumber = %v, want nil (more than one associated PR)", many.PRNumber)
	}
	if many.Conclusion == nil || *many.Conclusion != "neutral" {
		t.Fatalf("run 3 Conclusion = %v, want the raw GitHub value 'neutral' to pass through unfiltered", many.Conclusion)
	}
}

func TestListWorkflowRunsPaginatesAcrossPages(t *testing.T) {
	fake := newFakeGitHub(t)
	var pagesSeen []string
	fake.handle("GET /repos/acme/widgets/actions/workflows/deploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pagesSeen = append(pagesSeen, page)
		switch page {
		case "1":
			mustEncode(t, w, map[string]any{"total_count": 120, "workflow_runs": repeatRunItems(100, 1)})
		case "2":
			mustEncode(t, w, map[string]any{"total_count": 120, "workflow_runs": repeatRunItems(20, 101)})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	})
	client := fake.newTestClient()

	runs, err := ListWorkflowRuns(context.Background(), client, "acme", "widgets", "deploy.yml",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(runs) != 120 {
		t.Fatalf("len(runs) = %d, want 120", len(runs))
	}
	if len(pagesSeen) != 2 || pagesSeen[0] != "1" || pagesSeen[1] != "2" {
		t.Fatalf("pages fetched = %v, want [1 2]", pagesSeen)
	}
}

func repeatRunItems(n int, startID int64) []map[string]any {
	items := make([]map[string]any, n)
	for i := range n {
		id := startID + int64(i)
		items[i] = map[string]any{
			"id": id, "head_sha": "sha", "html_url": "u", "status": "in_progress", "conclusion": nil,
			"run_started_at": "2024-01-01T00:00:00Z", "created_at": "2024-01-01T00:00:00Z",
			"updated_at": "2024-01-01T00:00:00Z", "head_commit": map[string]any{"timestamp": "2024-01-01T00:00:00Z"},
			"pull_requests": []map[string]any{},
		}
	}
	return items
}

func TestListWorkflowRunsHalvesOnOverflowWithoutGapOrOverlap(t *testing.T) {
	fake := newFakeGitHub(t)
	type window struct{ since, until string }
	var windows []window
	fake.handle("GET /repos/acme/widgets/actions/workflows/deploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		created := r.URL.Query().Get("created")
		parts := strings.SplitN(created, "..", 2)
		if len(parts) != 2 {
			t.Fatalf("created %q is not a..b", created)
		}
		windows = append(windows, window{parts[0], parts[1]})

		if created == "2024-01-01T00:00:00Z..2024-01-02T00:00:00Z" {
			mustEncode(t, w, map[string]any{"total_count": 1001, "workflow_runs": repeatRunItems(100, 1)})
			return
		}
		mustEncode(t, w, map[string]any{"total_count": 1, "workflow_runs": repeatRunItems(1, 1)})
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	runs, err := ListWorkflowRuns(context.Background(), client, "acme", "widgets", "deploy.yml", since, until)
	if err != nil {
		t.Fatalf("ListWorkflowRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2 (one per half)", len(runs))
	}
	if len(windows) != 3 {
		t.Fatalf("requests made = %d, want 3 (original + two halves)", len(windows))
	}
	// GitHub's created:A..B qualifier is inclusive on BOTH ends, so the second half must start
	// one second after the midpoint (GitHub's own query granularity), not at the midpoint itself
	// -- otherwise a run created exactly at the midpoint would be counted in both halves.
	mid := since.Add(until.Sub(since) / 2)
	first, second := windows[1], windows[2]
	if first.since != since.UTC().Format(time.RFC3339) || first.until != mid.UTC().Format(time.RFC3339) {
		t.Fatalf("first half = %+v, want [%s, %s]", first, since.UTC().Format(time.RFC3339), mid.UTC().Format(time.RFC3339))
	}
	wantSecondSince := mid.Add(time.Second).UTC().Format(time.RFC3339)
	if second.since != wantSecondSince || second.until != until.UTC().Format(time.RFC3339) {
		t.Fatalf("second half = %+v, want [%s, %s]", second, wantSecondSince, until.UTC().Format(time.RFC3339))
	}
}

func TestListWorkflowRunsUpstream404IsWrapped(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/workflows/deploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	client := fake.newTestClient()

	_, err := ListWorkflowRuns(context.Background(), client, "acme", "widgets", "deploy.yml",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("ListWorkflowRuns: err = %v, want an error naming the 404 status", err)
	}
}

func TestListWorkflowRunsUpstream500IsWrapped(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/workflows/deploy.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := fake.newTestClient()

	_, err := ListWorkflowRuns(context.Background(), client, "acme", "widgets", "deploy.yml",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("ListWorkflowRuns: err = %v, want an error naming the 500 status", err)
	}
}

func TestListWorkflowRunJobsMapsEveryField(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/42/jobs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("filter"); got != "latest" {
			t.Errorf("jobs request filter = %q, want latest", got)
		}
		mustEncode(t, w, map[string]any{
			"total_count": 2,
			"jobs": []map[string]any{
				{"name": "build", "conclusion": "success", "started_at": "2024-01-01T00:00:00Z", "completed_at": "2024-01-01T00:05:00Z"},
				{"name": "deploy", "conclusion": "skipped", "started_at": nil, "completed_at": nil},
			},
		})
	})
	client := fake.newTestClient()

	jobs, err := ListWorkflowRunJobs(context.Background(), client, "acme", "widgets", 42)
	if err != nil {
		t.Fatalf("ListWorkflowRunJobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	if jobs[0].Name != "build" || jobs[0].Conclusion == nil || *jobs[0].Conclusion != "success" {
		t.Fatalf("jobs[0] = %+v", jobs[0])
	}
	if jobs[0].StartedAt == nil || !jobs[0].StartedAt.Equal(*ptrTime("2024-01-01T00:00:00Z")) {
		t.Fatalf("jobs[0].StartedAt = %v", jobs[0].StartedAt)
	}
	if jobs[1].Name != "deploy" || jobs[1].Conclusion == nil || *jobs[1].Conclusion != "skipped" {
		t.Fatalf("jobs[1] = %+v", jobs[1])
	}
	if jobs[1].StartedAt != nil || jobs[1].CompletedAt != nil {
		t.Fatalf("jobs[1] in-progress job carries a time: %+v", jobs[1])
	}
}

func TestListWorkflowRunJobsPaginatesAcrossPages(t *testing.T) {
	fake := newFakeGitHub(t)
	var pagesSeen []string
	fake.handle("GET /repos/acme/widgets/actions/runs/7/jobs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pagesSeen = append(pagesSeen, page)
		switch page {
		case "1":
			mustEncode(t, w, map[string]any{"total_count": 110, "jobs": repeatJobItems(100)})
		case "2":
			mustEncode(t, w, map[string]any{"total_count": 110, "jobs": repeatJobItems(10)})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	})
	client := fake.newTestClient()

	jobs, err := ListWorkflowRunJobs(context.Background(), client, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("ListWorkflowRunJobs: %v", err)
	}
	if len(jobs) != 110 {
		t.Fatalf("len(jobs) = %d, want 110", len(jobs))
	}
	if len(pagesSeen) != 2 || pagesSeen[0] != "1" || pagesSeen[1] != "2" {
		t.Fatalf("pages fetched = %v, want [1 2]", pagesSeen)
	}
}

func repeatJobItems(n int) []map[string]any {
	items := make([]map[string]any, n)
	for i := range n {
		items[i] = map[string]any{"name": "job", "conclusion": "success", "started_at": "2024-01-01T00:00:00Z", "completed_at": "2024-01-01T00:01:00Z"}
	}
	return items
}

func TestListWorkflowRunJobsUpstream404IsWrapped(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/404/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	client := fake.newTestClient()

	_, err := ListWorkflowRunJobs(context.Background(), client, "acme", "widgets", 404)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("ListWorkflowRunJobs: err = %v, want an error naming the 404 status", err)
	}
}

func TestListWorkflowRunJobsUpstream500IsWrapped(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/actions/runs/500/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := fake.newTestClient()

	_, err := ListWorkflowRunJobs(context.Background(), client, "acme", "widgets", 500)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("ListWorkflowRunJobs: err = %v, want an error naming the 500 status", err)
	}
}
