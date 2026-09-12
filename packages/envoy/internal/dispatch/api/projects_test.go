package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

func TestProjectAndSettingsMutationsAppendLiveEvents(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("map repository: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "State owner",
	}, "alice")
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, created)
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+issue.Key+"/state", map[string]any{
		"pinned": true,
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("write user state: status=%d body=%s", response.Code, response.Body.String())
	}

	rows, err := database.Pool.Query(t.Context(), `
		select type, coalesce(project_key, ''), coalesce(issue_key, ''), notify
		from events
		where type in ('project.created', 'settings.repo_project.updated', 'user_state.updated')
		order by id
	`)
	if err != nil {
		t.Fatalf("read settings events: %v", err)
	}
	defer rows.Close()
	type persistedEvent struct {
		Type    string
		Project string
		Issue   string
		Notify  bool
	}
	var got []persistedEvent
	for rows.Next() {
		var event persistedEvent
		if err := rows.Scan(&event.Type, &event.Project, &event.Issue, &event.Notify); err != nil {
			t.Fatalf("scan settings event: %v", err)
		}
		got = append(got, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate settings events: %v", err)
	}
	want := []persistedEvent{
		{Type: "project.created", Project: "CORE"},
		{Type: "settings.repo_project.updated", Project: "CORE"},
		{Type: "user_state.updated", Project: "CORE"},
	}
	if len(got) != len(want) {
		t.Fatalf("settings events = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("settings event %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}

func TestListProjectsCountsOpenAsksOnIssuesAndDocuments(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Open issue",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create open issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	openIssue := decodeBody[model.Issue](t, issueResponse)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+openIssue.Key+"/asks", map[string]any{
		"question": "Issue ask", "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create issue ask: status=%d body=%s", response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	openDocumentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Document ask", "actor": sessionActor(),
	})
	if openDocumentAsk.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", openDocumentAsk.Code, openDocumentAsk.Body.String())
	}
	answeredDocumentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Answered document ask", "actor": sessionActor(),
	})
	if answeredDocumentAsk.Code != http.StatusCreated {
		t.Fatalf("create answered document ask: status=%d body=%s", answeredDocumentAsk.Code, answeredDocumentAsk.Body.String())
	}
	answered := decodeBody[model.Ask](t, answeredDocumentAsk)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered.ID+"/answer", map[string]string{"text": "Done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer document ask: status=%d body=%s", response.Code, response.Body.String())
	}
	closedResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Closed issue",
	}, "alice")
	if closedResponse.Code != http.StatusCreated {
		t.Fatalf("create closed issue: status=%d body=%s", closedResponse.Code, closedResponse.Body.String())
	}
	closedIssue := decodeBody[model.Issue](t, closedResponse)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+closedIssue.Key+"/asks", map[string]any{
		"question": "Closed issue ask", "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("create closed issue ask: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closedIssue.Key, map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	projects := decodeBody[[]model.Project](t, listed)
	if len(projects) != 1 || projects[0].Key != "CORE" || projects[0].OpenAsks != 2 {
		t.Fatalf("listed projects = %#v, want CORE with two open asks", projects)
	}
}
