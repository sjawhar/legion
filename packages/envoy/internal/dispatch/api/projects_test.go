package api

import (
	"net/http"
	"strings"
	"testing"
)

type projectResponse struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

func TestCreateProjectRejectsDuplicateKeyWithProjectExists(t *testing.T) {
	handler := newTestHandler(t)

	first := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", first.Code, first.Body.String())
	}
	created := decodeBody[projectResponse](t, first)
	if created.CreatedAt == "" {
		t.Fatalf("created project missing created_at: %#v", created)
	}

	duplicate := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core Again",
	}, "alice")
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), `"code":"PROJECT_EXISTS"`) {
		t.Fatalf("duplicate project: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
}

func TestListProjectsIncludesCreatedAt(t *testing.T) {
	handler := newTestHandler(t)

	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	projects := decodeBody[[]projectResponse](t, listed)
	if len(projects) != 1 || projects[0].Key != "CORE" || projects[0].CreatedAt == "" {
		t.Fatalf("listed projects: got %#v", projects)
	}
}
