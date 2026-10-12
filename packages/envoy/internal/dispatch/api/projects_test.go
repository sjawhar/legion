package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
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

// An agent session creates a project with its bearer, as it creates an issue: the project is
// listed like any other, and its project.created event names the session that created it.
func TestASessionBearerCreatesAProject(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)

	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "AGENT", "name": "Agent project", "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("a session creates a project: status=%d body=%s", created.Code, created.Body.String())
	}
	project := decodeBody[projectResponse](t, created)
	if project.Key != "AGENT" || project.Name != "Agent project" || project.CreatedAt == "" {
		t.Fatalf("created project = %#v", project)
	}

	var actor []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select actor from events where type = 'project.created' and project_key = 'AGENT'
	`).Scan(&actor); err != nil {
		t.Fatalf("read the project.created event: %v", err)
	}
	var by model.Actor
	if err := json.Unmarshal(actor, &by); err != nil {
		t.Fatalf("decode the project.created actor %s: %v", actor, err)
	}
	if by.Kind != "session" || by.ID != sessionActor()["id"] {
		t.Fatalf("project.created actor = %s, want the creating session", actor)
	}

	listed := sessionRequest(t, handler, http.MethodGet, "/api/v1/projects", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list projects: status=%d body=%s", listed.Code, listed.Body.String())
	}
	projects := decodeBody[[]projectResponse](t, listed)
	if len(projects) != 1 || projects[0].Key != "AGENT" || projects[0].Name != "Agent project" {
		t.Fatalf("listed projects = %#v, want the session's AGENT", projects)
	}
}

// A key that already exists is refused whoever asks: a session's bearer and a signed-in person's
// cookie each get 409 PROJECT_EXISTS, and the project keeps the name it was created with.
func TestCreatingATakenProjectKeyIsRefusedForABearerAndACookie(t *testing.T) {
	handler, _, _, withCookie := newCookieServer(t)

	if first := sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": "TAKEN", "name": "First", "actor": sessionActor(),
	}); first.Code != http.StatusCreated {
		t.Fatalf("create TAKEN: status=%d body=%s", first.Code, first.Body.String())
	}
	for _, attempt := range []struct {
		caller   string
		response *httptest.ResponseRecorder
	}{
		{"bearer", sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
			"key": "TAKEN", "name": "Again", "actor": sessionActor(),
		})},
		{"cookie", withCookie(http.MethodPost, "/api/v1/projects", map[string]string{"key": "TAKEN", "name": "Again"})},
	} {
		if attempt.response.Code != http.StatusConflict || !strings.Contains(attempt.response.Body.String(), `"code":"PROJECT_EXISTS"`) {
			t.Fatalf("a %s creating a taken key: status=%d body=%s", attempt.caller, attempt.response.Code, attempt.response.Body.String())
		}
	}

	listed := sessionRequest(t, handler, http.MethodGet, "/api/v1/projects", nil)
	projects := decodeBody[[]projectResponse](t, listed)
	if len(projects) != 1 || projects[0].Name != "First" {
		t.Fatalf("listed projects = %#v, want TAKEN still named First", projects)
	}
}

// Every route that takes a project key refuses a bad one with the same description of the rule,
// so an agent learns the rule once, whichever route it called. The rule is contracts'
// PROJECT_KEY_RULE, generated into Go, which the dispatch command's own refusal gives too.
func TestEveryRouteRefusesABadProjectKeyInOneWording(t *testing.T) {
	handler := newTestHandler(t)
	rule := contracts.ProjectKeyRule
	for _, probe := range []struct {
		name, method, target, code string
		body                       any
	}{
		{"create project", http.MethodPost, "/api/v1/projects", "INVALID_PROJECT", map[string]string{"key": "bad", "name": "Bad"}},
		{"create issue", http.MethodPost, "/api/v1/issues", "INVALID_ISSUE", map[string]string{"project": "bad", "title": "Title"}},
		{"map a repository", http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", "INVALID_REPO_PROJECT", map[string]string{"project": "bad"}},
		{"search", http.MethodGet, "/api/v1/search?q=ok&project=bad", "INVALID_PROJECT", nil},
	} {
		response := dispatchRequest(t, handler, probe.method, probe.target, probe.body, "alice")
		body := response.Body.String()
		if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+probe.code+`"`) || !strings.Contains(body, rule) {
			t.Errorf("%s with key \"bad\": status=%d body=%s, want 400 %s describing the rule %q", probe.name, response.Code, body, probe.code, rule)
		}
	}
}

// A bad key or an empty name is 400 INVALID_PROJECT for a session as for a person, and the
// refusal names which of the two is wrong: an agent reads it as the whole answer.
func TestAnInvalidProjectIsRefusedForABearerAndAPerson(t *testing.T) {
	handler := newTestHandler(t)
	badKey := func(key string) string {
		return fmt.Sprintf(`key \"%s\" is not a project key: %s`, key, contracts.ProjectKeyRule)
	}
	for _, input := range []struct{ name, key, projectName, says string }{
		{"lowercase key", "agent", "Agent project", badKey("agent")},
		{"one-character key", "A", "Agent project", badKey("A")},
		{"key starting with a digit", "1AGENT", "Agent project", badKey("1AGENT")},
		{"key over ten characters", "ABCDEFGHIJK", "Agent project", badKey("ABCDEFGHIJK")},
		{"empty name", "AGENT", "", "project name is required"},
		{"blank name", "AGENT", "   ", "project name is required"},
		{"a bidi override", "AGENT", "Agent \u202eproject", `name holds U+202E, a control or format character`},
		{"zero-width characters only", "AGENT", "\u200b\u200d", `name holds U+200B, a control or format character`},
		{"a control character", "AGENT", "Agent\u0007project", `name holds U+0007, a control or format character`},
	} {
		t.Run(input.name, func(t *testing.T) {
			for _, attempt := range []struct {
				caller   string
				response *httptest.ResponseRecorder
			}{
				{"bearer", sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
					"key": input.key, "name": input.projectName, "actor": sessionActor(),
				})},
				{"person", dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
					"key": input.key, "name": input.projectName,
				}, "alice")},
			} {
				body := attempt.response.Body.String()
				if attempt.response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_PROJECT"`) || !strings.Contains(body, input.says) {
					t.Fatalf("a %s creating key %q name %q: status=%d body=%s, want 400 INVALID_PROJECT saying %s", attempt.caller, input.key, input.projectName, attempt.response.Code, body, input.says)
				}
			}
		})
	}
}

// A project name is capped as an issue title is: any bearer can write one into the project list
// every dashboard reads, and no route renames a project. The cap counts UTF-16 units after the
// trim, so a name at it is created and one unit over is CAP_EXCEEDED, for a session as for a person.
func TestAProjectNameOverTheCapIsCapExceededForABearerAndAPerson(t *testing.T) {
	handler := newTestHandler(t)
	atCap := strings.Repeat("n", contracts.ProjectNameMax)
	over := strings.Repeat("😀", contracts.ProjectNameMax/2) + "n"
	for _, caller := range []struct {
		name string
		send func(body map[string]any) *httptest.ResponseRecorder
	}{
		{"bearer", func(body map[string]any) *httptest.ResponseRecorder {
			body["actor"] = sessionActor()
			return sessionRequest(t, handler, http.MethodPost, "/api/v1/projects", body)
		}},
		{"person", func(body map[string]any) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", body, "alice")
		}},
	} {
		key := map[string]string{"bearer": "CAPB", "person": "CAPP"}[caller.name]
		refused := caller.send(map[string]any{"key": key, "name": "  " + over + "  "})
		want := fmt.Sprintf("name is 1 characters over the %d-character limit (%d/%d)", contracts.ProjectNameMax, contracts.ProjectNameMax+1, contracts.ProjectNameMax)
		if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(refused.Body.String(), want) {
			t.Fatalf("a %s's name one unit over the cap: status=%d body=%s, want 400 CAP_EXCEEDED saying %q", caller.name, refused.Code, refused.Body.String(), want)
		}
		if created := caller.send(map[string]any{"key": key, "name": "  " + atCap + "  "}); created.Code != http.StatusCreated {
			t.Fatalf("a %s's name at the cap: status=%d body=%s, want 201", caller.name, created.Code, created.Body.String())
		}
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
