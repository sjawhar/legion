package api

import (
	"encoding/json"
	"net/http"
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
	if _, err := delivery.PutSettings(t.Context(), database.Pool, model.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "production-apply / production-apply", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	for _, target := range []string{
		"/api/v1/delivery/timeline?from=not-a-date",
		"/api/v1/delivery/timeline?to=not-a-date",
		"/api/v1/delivery/timeline?from=2024-01-02T00:00:00Z&to=2024-01-01T00:00:00Z",
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

	if _, err := delivery.PutSettings(ctx, pool, model.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "production-apply / production-apply", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	deployedAt := merged.Add(2 * time.Hour)
	waitingMerged := merged.Add(time.Hour)

	// A PR a deploy ships (deployed), and one merged after the only deploy run (waiting), both in
	// the deploy repository, plus one merged in a different repository the configured deploy
	// pipeline never tracks (not_tracked).
	for _, pr := range []model.DeliveryPullRequest{
		{Repo: "acme/widgets", Number: 1, Title: "feat: shipped", URL: "https://github.com/acme/widgets/pull/1", Author: "octocat", MergedAt: &merged, CreatedAt: &merged},
		{Repo: "acme/widgets", Number: 2, Title: "feat: waiting", URL: "https://github.com/acme/widgets/pull/2", Author: "octocat", MergedAt: &waitingMerged, CreatedAt: &waitingMerged},
		{Repo: "acme/other", Number: 3, Title: "feat: elsewhere", URL: "https://github.com/acme/other/pull/3", Author: "octocat", MergedAt: &merged, CreatedAt: &merged},
	} {
		if err := delivery.UpsertPullRequest(ctx, pool, pr); err != nil {
			t.Fatalf("seed PR %s#%d: %v", pr.Repo, pr.Number, err)
		}
	}

	success := model.DeliveryConclusionSuccess
	if err := delivery.UpsertRun(ctx, pool, model.DeliveryRun{
		Repo: "acme/widgets", RunID: 100, Kind: model.DeliveryRunKindDeploy, HeadSHA: "deadbeef",
		HeadCommitAt: merged, StartedAt: deployedAt, CompletedAt: &deployedAt, Conclusion: &success,
		URL: "https://github.com/acme/widgets/actions/runs/100",
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if err := delivery.UpsertRunJobs(ctx, pool, "acme/widgets", 100, []model.DeliveryRunJob{
		{Repo: "acme/widgets", RunID: 100, Name: "production-apply / production-apply", StartedAt: &deployedAt, CompletedAt: &deployedAt, Conclusion: &success},
	}); err != nil {
		t.Fatalf("seed run job: %v", err)
	}

	from := merged.Add(-time.Hour).Format(time.RFC3339)
	to := merged.Add(24 * time.Hour).Format(time.RFC3339)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body model.DeliveryTimelineResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, response.Body.String())
	}
	if len(body.PRs) != 3 {
		t.Fatalf("len(prs) = %d, want 3; body=%s", len(body.PRs), response.Body.String())
	}
	status := map[string]string{}
	for _, pr := range body.PRs {
		status[pr.ID] = pr.DeployedStatus
	}
	if status["acme/widgets#1"] != "deployed" {
		t.Errorf("acme/widgets#1 deployed_status = %q, want deployed", status["acme/widgets#1"])
	}
	if status["acme/widgets#2"] != "waiting" {
		t.Errorf("acme/widgets#2 deployed_status = %q, want waiting", status["acme/widgets#2"])
	}
	if status["acme/other#3"] != "not_tracked" {
		t.Errorf("acme/other#3 deployed_status = %q, want not_tracked", status["acme/other#3"])
	}
	if len(body.Runs) != 1 || len(body.Runs[0].PRs) != 1 || body.Runs[0].PRs[0] != "acme/widgets#1" {
		t.Fatalf("runs = %+v, want one run shipping acme/widgets#1", body.Runs)
	}

	// deployed= facet narrows to exactly the deployed PR.
	deployedOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&deployed=deployed", nil, "alice")
	var deployedBody model.DeliveryTimelineResponse
	if err := json.Unmarshal(deployedOnly.Body.Bytes(), &deployedBody); err != nil {
		t.Fatalf("decode deployed-only response: %v", err)
	}
	if len(deployedBody.PRs) != 1 || deployedBody.PRs[0].ID != "acme/widgets#1" {
		t.Fatalf("deployed=deployed filter: prs = %+v, want only acme/widgets#1", deployedBody.PRs)
	}

	// repo= facet narrows to the other repository alone.
	otherOnly := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline?from="+from+"&to="+to+"&repo=acme/other", nil, "alice")
	var otherBody model.DeliveryTimelineResponse
	if err := json.Unmarshal(otherOnly.Body.Bytes(), &otherBody); err != nil {
		t.Fatalf("decode repo-filtered response: %v", err)
	}
	if len(otherBody.PRs) != 1 || otherBody.PRs[0].ID != "acme/other#3" {
		t.Fatalf("repo=acme/other filter: prs = %+v, want only acme/other#3", otherBody.PRs)
	}
}
