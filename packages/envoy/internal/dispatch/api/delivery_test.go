package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestGetDeliveryTimelineWithoutSettingsIs404(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404 DELIVERY_NOT_CONFIGURED", response.Code, response.Body.String())
	}
}

func TestGetDeliveryTimelineRejectsMalformedWindow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if _, err := delivery.PutSettings(t.Context(), database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	for _, target := range []string{
		"/api/v1/delivery/timeline?from=not-a-date",
		"/api/v1/delivery/timeline?to=not-a-date",
		"/api/v1/delivery/timeline?from=2024-01-02T00:00:00Z&to=2024-01-01T00:00:00Z",
		"/api/v1/delivery/timeline?from=2020-01-01T00:00:00Z&to=2024-01-01T00:00:00Z",
	} {
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, body = %s, want 400 INVALID_QUERY", target, response.Code, response.Body.String())
		}
	}
}

func TestGetDeliveryTimelineComputesDeployedStatusAndFacets(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	pool := database.Pool
	ctx := t.Context()

	if _, err := delivery.PutSettings(ctx, pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	deployedAt := merged.Add(2 * time.Hour)
	waitingMerged := merged.Add(time.Hour)

	// A PR a deploy ships (deployed), and one merged after the only deploy run (waiting), both in
	// the deploy repository, plus one merged in a different repository the configured deploy
	// pipeline never tracks (not_tracked). acme/widgets#1 also names a P0 issue, for the priority
	// facet case below.
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ('ACME', 'Acme') on conflict do nothing`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, priority, created_by, rank) values ('ACME-1', 'ACME', 1, 'x', 'todo', 0, '{"kind":"system","id":"test"}', 'U')`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	issueKey := "ACME-1"
	for _, pr := range []delivery.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 1, Title: "feat: shipped", URL: "https://github.com/acme/widgets/pull/1", Author: "octocat", MergedAt: &merged, CreatedAt: &merged, IssueKey: &issueKey},
		{Repo: "acme/widgets", Number: 2, Title: "feat: waiting", URL: "https://github.com/acme/widgets/pull/2", Author: "octocat", MergedAt: &waitingMerged, CreatedAt: &waitingMerged},
		{Repo: "acme/other", Number: 3, Title: "feat: elsewhere", URL: "https://github.com/acme/other/pull/3", Author: "octocat", MergedAt: &merged, CreatedAt: &merged},
	} {
		if err := delivery.UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}

	runSuccess := delivery.DeliveryRunConclusionSuccess
	jobSuccess := delivery.DeliveryJobConclusionSuccess
	if err := delivery.UpsertRun(ctx, pool, delivery.DeliveryRun{
		Repo: "acme/widgets", RunID: 100, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "deadbeef",
		HeadCommitAt: merged, StartedAt: deployedAt, CompletedAt: &deployedAt, Conclusion: &runSuccess,
		URL: "https://github.com/acme/widgets/actions/runs/100",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, pool, "acme/widgets", 100, []delivery.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 100, Name: "widgets-release / widgets-release", StartedAt: &deployedAt, CompletedAt: &deployedAt, Conclusion: &jobSuccess},
	}); err != nil {
		t.Fatalf("seed run job: %v", err)
	}

	from := merged.Add(-time.Hour).Format(time.RFC3339)
	to := merged.Add(24 * time.Hour).Format(time.RFC3339)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, response.Body.String())
	}
	if len(body.PRs) != 3 {
		t.Fatalf("len(prs) = %d, want 3; body=%s", len(body.PRs), response.Body.String())
	}
	status := map[string]delivery.DeployedStatus{}
	for _, pr := range body.PRs {
		status[pr.ID] = pr.DeployedStatus
	}
	if status["acme/widgets#1"] != delivery.DeployedStatusDeployed {
		t.Errorf("acme/widgets#1 deployed_status = %q, want deployed", status["acme/widgets#1"])
	}
	if status["acme/widgets#2"] != delivery.DeployedStatusWaiting {
		t.Errorf("acme/widgets#2 deployed_status = %q, want waiting", status["acme/widgets#2"])
	}
	if status["acme/other#3"] != delivery.DeployedStatusNotTracked {
		t.Errorf("acme/other#3 deployed_status = %q, want not_tracked", status["acme/other#3"])
	}
	if len(body.Runs) != 1 || len(body.Runs[0].PRs) != 1 || body.Runs[0].PRs[0] != "acme/widgets#1" {
		t.Fatalf("runs = %+v, want one run shipping acme/widgets#1", body.Runs)
	}

	// deployed= facet narrows to exactly the deployed PR.
	deployedOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&deployed=deployed", nil, "alice")
	var deployedBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(deployedOnly.Body.Bytes(), &deployedBody); err != nil {
		t.Fatalf("decode deployed-only response: %v", err)
	}
	if len(deployedBody.PRs) != 1 || deployedBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("deployed=deployed filter: prs = %+v, want only acme/widgets#1", deployedBody.PRs)
	}

	// priority=P0, exactly as the UI's own placeholder ("Type a priority (e.g. P0).") tells a
	// user to type, must match the P0 issue's PR -- not silently match nothing.
	priorityP0 := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&priority=P0", nil, "alice")
	var priorityBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(priorityP0.Body.Bytes(), &priorityBody); err != nil {
		t.Fatalf("decode priority=P0 response: %v", err)
	}
	if len(priorityBody.PRs) != 1 || priorityBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("priority=P0 filter: prs = %+v, want only acme/widgets#1 (the P0 issue's PR)", priorityBody.PRs)
	}
	// The bare digit form still works too.
	priorityDigit := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&priority=0", nil, "alice")
	var priorityDigitBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(priorityDigit.Body.Bytes(), &priorityDigitBody); err != nil {
		t.Fatalf("decode priority=0 response: %v", err)
	}
	if len(priorityDigitBody.PRs) != 1 || priorityDigitBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("priority=0 filter: prs = %+v, want only acme/widgets#1", priorityDigitBody.PRs)
	}

	// repo= facet narrows to the other repository alone; the one deploy run shipped nothing under
	// this filter, so its prs[] must serialize as `[]`, never `null` (the common case for an
	// ordinary redeploy that shipped no in-window population PR).
	otherOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&repo=acme/other", nil, "alice")
	if strings.Contains(otherOnly.Body.String(), `"prs":null`) {
		t.Fatalf("repo=acme/other filter: response body contains \"prs\":null, want \"prs\":[]: %s", otherOnly.Body.String())
	}
	var otherBody delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(otherOnly.Body.Bytes(), &otherBody); err != nil {
		t.Fatalf("decode repo-filtered response: %v", err)
	}
	if len(otherBody.PRs) != 1 || otherBody.PRs[0].ID != "acme/other#3" {
		t.Fatalf("repo=acme/other filter: prs = %+v, want only acme/other#3", otherBody.PRs)
	}
	if len(otherBody.Runs) != 1 || otherBody.Runs[0].PRs == nil || len(otherBody.Runs[0].PRs) != 0 {
		t.Fatalf("repo=acme/other filter: runs[0].prs = %+v, want a non-nil empty slice (the shipped PR is excluded by the active repo facet)", otherBody.Runs)
	}
}

// TestGetDeliveryTimelineSurfacesReconcileErrorOnFreshness proves a recorded reconcile failure
// (delivery_settings.last_error) reaches the API's freshness object by name, not just the
// database -- a stale-but-healthy pass and a failing one must be distinguishable on the wire.
func TestGetDeliveryTimelineSurfacesReconcileErrorOnFreshness(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := t.Context()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	if err := delivery.RecordReconcileError(ctx, database.Pool, "the installation lacks Actions: read on acme/widgets"); err != nil {
		t.Fatalf("seed reconcile error: %v", err)
	}

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body delivery.DeliveryTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Freshness.LastError == nil || *body.Freshness.LastError != "the installation lacks Actions: read on acme/widgets" {
		t.Fatalf("freshness.last_error = %v, want the recorded permission failure by name", body.Freshness.LastError)
	}
}
