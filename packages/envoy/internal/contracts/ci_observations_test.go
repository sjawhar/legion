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
			"completed_at":  "2026-09-07T03:00:00Z",
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
	if observation.ObservedAt != "2026-09-07T03:00:00Z" {
		t.Errorf("check run observation timestamp = %q", observation.ObservedAt)
	}
}

func TestGithubCIObservationsUsesCheckRunStartedAtBeforeCompletion(t *testing.T) {
	body := map[string]any{
		"repository": map[string]any{
			"name":  "example-repo",
			"owner": map[string]any{"login": "example-org"},
		},
		"check_run": map[string]any{
			"id":            float64(987654321),
			"name":          "unit-tests",
			"status":        "in_progress",
			"head_sha":      "abcdef1234567",
			"started_at":    "2026-09-07T02:00:00Z",
			"pull_requests": []any{map[string]any{"number": float64(42)}},
		},
	}

	observations := GithubCIObservations("check_run", body)
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	if observations[0].ObservedAt != "2026-09-07T02:00:00Z" {
		t.Errorf("check run started timestamp = %q", observations[0].ObservedAt)
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
			"updated_at":    "2026-09-07T03:01:00Z",
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
	if observation.ObservedAt != "2026-09-07T03:01:00Z" {
		t.Errorf("check suite observation timestamp = %q", observation.ObservedAt)
	}
}
