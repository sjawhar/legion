package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
)

func TestGetDeliverySettingsWithoutAnyIsNull(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/delivery", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.TrimSpace(response.Body.String()) != "null" {
		t.Fatalf("body = %s, want null", response.Body.String())
	}
}

func TestPutDeliverySettingsRejectsIncompleteInput(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo": "acme/widgets",
	}, "alice")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400 DELIVERY_SETTINGS_INPUT", response.Code, response.Body.String())
	}
}

// TestPutDeliverySettingsWritesSettings proves putDeliverySettings actually persists the row
// (no event is appended: delivery_settings has no project/issue/artifact to own one -- see the
// handler's own doc comment).
func TestPutDeliverySettingsWritesSettings(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"acme/widgets": "read"}, commit: "deadbeef"}
	handler, database := newArchitectureSourceServer(t, fake)

	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo":             "acme/widgets",
		"deploy_workflow_path":    ".github/workflows/deploy.yml",
		"production_job_name":     "widgets-release / widgets-release",
		"pr_checks_workflow_path": ".github/workflows/pr-checks.yml",
		"population_authors":      []string{"octocat"},
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var settings delivery.DeliverySettings
	if err := json.Unmarshal(response.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if settings.DeployRepo != "acme/widgets" {
		t.Errorf("settings.DeployRepo = %q, want acme/widgets", settings.DeployRepo)
	}

	stored, err := delivery.GetSettings(t.Context(), database.Pool)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if stored.DeployRepo != "acme/widgets" {
		t.Errorf("stored settings.DeployRepo = %q, want acme/widgets", stored.DeployRepo)
	}
}

func TestPutDeliverySettingsRefusesWithoutGitHubAccess(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{}}
	handler, _ := newArchitectureSourceServer(t, fake)

	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo":             "acme/widgets",
		"deploy_workflow_path":    ".github/workflows/deploy.yml",
		"production_job_name":     "widgets-release / widgets-release",
		"pr_checks_workflow_path": ".github/workflows/pr-checks.yml",
		"population_authors":      []string{"octocat"},
	}, "alice")
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409 DELIVERY_SETTINGS_ACCESS", response.Code, response.Body.String())
	}
}

// TestPutDeliverySettingsRoundTripsThePipelineJobNames proves the three optional job names the
// Pipeline page follows are stored as sent and read back through the GET.
func TestPutDeliverySettingsRoundTripsThePipelineJobNames(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"acme/widgets": "read"}, commit: "deadbeef"}
	handler, _ := newArchitectureSourceServer(t, fake)

	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo":               "acme/widgets",
		"deploy_workflow_path":      ".github/workflows/deploy.yml",
		"production_job_name":       "widgets-release / widgets-release",
		"pr_checks_workflow_path":   ".github/workflows/pr-checks.yml",
		"population_authors":        []string{"octocat"},
		"staging_e2e_job_name":      " staging-e2e / run ",
		"staging_apply_job_name":    "staging-release / apply",
		"pr_checks_rollup_job_name": "checks-complete",
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/delivery", nil, "alice")
	var got map[string]any
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v (%s)", err, read.Body.String())
	}
	want := map[string]string{
		"staging_e2e_job_name":      "staging-e2e / run",
		"staging_apply_job_name":    "staging-release / apply",
		"pr_checks_rollup_job_name": "checks-complete",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("GET %s = %#v, want %q", key, got[key], value)
		}
	}
}

// TestPutDeliverySettingsWithoutThePipelineJobNamesStoresNull proves the three job names are
// optional: a PUT that leaves them out, or sends one empty, stores null, and the GET answers null.
func TestPutDeliverySettingsWithoutThePipelineJobNamesStoresNull(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"acme/widgets": "read"}, commit: "deadbeef"}
	handler, database := newArchitectureSourceServer(t, fake)

	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo":             "acme/widgets",
		"deploy_workflow_path":    ".github/workflows/deploy.yml",
		"production_job_name":     "widgets-release / widgets-release",
		"pr_checks_workflow_path": ".github/workflows/pr-checks.yml",
		"population_authors":      []string{"octocat"},
		"staging_apply_job_name":  "  ",
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/delivery", nil, "alice")
	var got map[string]any
	if err := json.Unmarshal(read.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v (%s)", err, read.Body.String())
	}
	for _, key := range []string{"staging_e2e_job_name", "staging_apply_job_name", "pr_checks_rollup_job_name"} {
		value, present := got[key]
		if !present || value != nil {
			t.Errorf("GET %s = %#v (present %v), want null", key, value, present)
		}
	}
	var stored *string
	if err := database.Pool.QueryRow(t.Context(), `
		select staging_apply_job_name from delivery_settings where singleton
	`).Scan(&stored); err != nil {
		t.Fatalf("read staging_apply_job_name: %v", err)
	}
	if stored != nil {
		t.Fatalf("stored staging_apply_job_name = %q, want null", *stored)
	}
}

func TestAgentPutsDeliverySettingsWithItsActor(t *testing.T) {
	fake := &fakeGitHubApp{installations: map[string]string{"acme/widgets": "read"}, commit: "deadbeef"}
	handler, database := newArchitectureSourceServer(t, fake)

	response := agentRequest(t, handler, http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo":             "acme/widgets",
		"deploy_workflow_path":    ".github/workflows/deploy.yml",
		"production_job_name":     "widgets-release / widgets-release",
		"pr_checks_workflow_path": ".github/workflows/pr-checks.yml",
		"population_authors":      []string{"octocat"},
		"actor":                   map[string]string{"kind": "session", "id": "agent-delivery-put"},
	}, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("agent put delivery settings: status=%d body=%s", response.Code, response.Body.String())
	}
	var actorID string
	if err := database.Pool.QueryRow(t.Context(), `
		select updated_by->>'id' from delivery_settings where singleton
	`).Scan(&actorID); err != nil {
		t.Fatalf("read delivery settings actor: %v", err)
	}
	if actorID != "agent-delivery-put" {
		t.Fatalf("delivery settings actor = %q, want agent-delivery-put", actorID)
	}
}
