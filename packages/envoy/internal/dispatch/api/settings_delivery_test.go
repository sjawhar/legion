package api

import (
	"context"
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

	stored, err := delivery.GetSettings(context.Background(), database.Pool)
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
