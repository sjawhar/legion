package contracts

import "testing"

func TestGithubCIObservationsExtractCheckRunIdentity(t *testing.T) {
	body := map[string]any{
		"repository": map[string]any{
			"name":      "example-repo",
			"owner":     map[string]any{"login": "example-org"},
			"full_name": "example-org/example-repo",
		},
		"check_run": map[string]any{
			"id":            float64(987654321),
			"name":          "unit-tests",
			"status":        "completed",
			"conclusion":    "failure",
			"head_sha":      "abcdef1234567",
			"html_url":      "https://example-host/checks/987654321",
			"pull_requests": []any{map[string]any{"number": float64(42)}},
		},
	}

	observations := GithubCIObservations("check_run", body)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observation := observations[0]
	if observation.CheckRunID != "987654321" {
		t.Errorf("check run ID = %q, want 987654321", observation.CheckRunID)
	}
	if observation.URL != "https://example-host/checks/987654321" {
		t.Errorf("check run URL = %q", observation.URL)
	}
}

func TestGithubCIObservationsExtractCheckSuite(t *testing.T) {
	body := map[string]any{
		"repository": map[string]any{
			"name":  "example-repo",
			"owner": map[string]any{"login": "example-org"},
		},
		"check_suite": map[string]any{
			"id":            float64(123456789),
			"status":        "in_progress",
			"conclusion":    nil,
			"head_sha":      "abcdef1234567890abcdef1234567890abcdef12",
			"app":           map[string]any{"id": float64(77)},
			"pull_requests": []any{map[string]any{"number": float64(42)}},
		},
	}

	observations := GithubCIObservations("check_suite", body)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	observation := observations[0]
	if observation.SuiteID != "123456789" || observation.AppID != "77" {
		t.Fatalf("suite identity = %+v", observation)
	}
	if observation.Status != "in_progress" || observation.Conclusion != "" {
		t.Fatalf("suite state = %+v", observation)
	}
}
