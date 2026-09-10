package api

import (
	"net/http"
	"strings"
	"testing"
)

type repoProjectResponse struct {
	Repo    string `json:"repo"`
	Project string `json:"project"`
}

func TestRepoProjectSettingsUpsertListAndDelete(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice")
	if mapped.Code != http.StatusOK {
		t.Fatalf("upsert repository project: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	if got := decodeBody[repoProjectResponse](t, mapped); got != (repoProjectResponse{Repo: "owner/repo", Project: "CORE"}) {
		t.Fatalf("upserted mapping: got %#v", got)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list repository projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	if got := decodeBody[[]repoProjectResponse](t, listed); len(got) != 1 || got[0] != (repoProjectResponse{Repo: "owner/repo", Project: "CORE"}) {
		t.Fatalf("listed mappings: got %#v", got)
	}

	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/owner/repo", nil, "alice")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete repository project: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	listed = dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice")
	if got := decodeBody[[]repoProjectResponse](t, listed); len(got) != 0 {
		t.Fatalf("mappings after delete: got %#v", got)
	}

	forbidden := agentRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "agent-token")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("agent upsert: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}

	missingProject := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/missing", map[string]string{
		"project": "MISSING",
	}, "alice")
	if missingProject.Code != http.StatusNotFound {
		t.Fatalf("upsert missing project: status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}

	for _, request := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/v1/settings/repo-projects"},
		{http.MethodDelete, "/api/v1/settings/repo-projects/owner/repo"},
	} {
		response := agentRequest(t, handler, request.method, request.target, nil, "agent-token")
		if response.Code != http.StatusForbidden {
			t.Fatalf("agent %s: status=%d body=%s", request.target, response.Code, response.Body.String())
		}
	}
}

func TestRepoProjectMappingCanonicalizesRepositoryVariants(t *testing.T) {
	handler, _ := newTestHandlerWithDefaultProject(t)
	for _, project := range []map[string]string{
		{"key": "CORE", "name": "Core"},
		{"key": "DEFAULT", "name": "Default"},
	} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %q: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}
	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/Owner/Repo.git", map[string]string{"project": "CORE"}, "alice")
	if mapped.Code != http.StatusOK || decodeBody[repoProjectResponse](t, mapped).Repo != "owner/repo" {
		t.Fatalf("map canonical repository: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	for number, ref := range []string{"Owner/Repo#1", "owner/repo.git#2"} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{"external": ref}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create external issue %q: status=%d body=%s", ref, response.Code, response.Body.String())
		}
		if issue := decodeBody[struct {
			Project string `json:"project"`
		}](t, response); issue.Project != "CORE" {
			t.Fatalf("external issue %d project: got %q, want CORE", number, issue.Project)
		}
	}
	deleted := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/OWNER/REPO.git", nil, "alice")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete canonical repository: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if got := canonicalRepo(" Owner ", " Repo.git "); got != "owner/repo" {
		t.Fatalf("canonical repository whitespace: got %q", got)
	}
}

func TestExternalIssueUsesPersistedRepoProject(t *testing.T) {
	handler, _ := newTestHandlerWithDefaultProject(t)
	for _, project := range []map[string]string{
		{"key": "CORE", "name": "Core"},
		{"key": "DEFAULT", "name": "Default"},
	} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %q: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}

	mapped := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/mapped/repository", map[string]string{
		"project": "CORE",
	}, "alice")
	if mapped.Code != http.StatusOK {
		t.Fatalf("upsert repository project: status=%d body=%s", mapped.Code, mapped.Body.String())
	}
	mismatch := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "mapped/repository#1",
		"project":  "DEFAULT",
	}, "alice")
	if mismatch.Code != http.StatusBadRequest || !strings.Contains(mismatch.Body.String(), `"code":"EXTERNAL_PROJECT_MISMATCH"`) {
		t.Fatalf("reject mismatched external project: status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "mapped/repository#1",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create mapped external issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key    string   `json:"key"`
		Labels []string `json:"labels"`
	}](t, created)
	if issue.Key != "CORE-1" || len(issue.Labels) != 0 {
		t.Fatalf("persisted mapping external issue: got %#v", issue)
	}
}
