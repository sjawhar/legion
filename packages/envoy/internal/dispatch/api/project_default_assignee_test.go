package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type projectDefaultAssigneeRow struct {
	Project  string  `json:"project"`
	Assignee *string `json:"assignee"`
}

func createDefaultAssigneeProject(t *testing.T, handler http.Handler, key, name string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": name,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func putDefaultAssignee(t *testing.T, handler http.Handler, project string, assignee any) projectDefaultAssigneeRow {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-assignee",
		map[string]any{"assignee": assignee}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("put default assignee %v: status=%d body=%s", assignee, response.Code, response.Body.String())
	}
	return decodeBody[projectDefaultAssigneeRow](t, response)
}

// The PUT sets, changes and clears a project's default, answering {project, assignee} each time.
func TestPutProjectDefaultAssigneeSetsChangesAndClears(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	set := putDefaultAssignee(t, handler, "CORE", " Bob ")
	if set.Project != "CORE" || set.Assignee == nil || *set.Assignee != "bob" {
		t.Fatalf("set = %#v, want CORE/bob", set)
	}
	changed := putDefaultAssignee(t, handler, "CORE", "Alice")
	if changed.Assignee == nil || *changed.Assignee != "alice" {
		t.Fatalf("changed = %#v, want alice", changed)
	}
	cleared := putDefaultAssignee(t, handler, "CORE", nil)
	if cleared.Project != "CORE" || cleared.Assignee != nil {
		t.Fatalf("cleared = %#v, want CORE/null", cleared)
	}
	var stored *string
	if err := database.Pool.QueryRow(context.Background(),
		`select default_assignee from projects where key = 'CORE'`).Scan(&stored); err != nil {
		t.Fatalf("read stored default: %v", err)
	}
	if stored != nil {
		t.Fatalf("stored default = %q, want null", *stored)
	}
}

// An unknown project is a 404, and a login nobody has signed in as is refused before anything is
// stored.
func TestPutProjectDefaultAssigneeRefusesUnknownProjectAndLogin(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	missing := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/NOPE/default-assignee",
		map[string]any{"assignee": "bob"}, "alice")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown project: status=%d body=%s", missing.Code, missing.Body.String())
	}
	unlisted := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{"assignee": "mallory"}, "alice")
	if unlisted.Code != http.StatusBadRequest || !strings.Contains(unlisted.Body.String(), `"code":"ASSIGNEE_NOT_ALLOWED"`) {
		t.Fatalf("unlisted login: status=%d body=%s", unlisted.Code, unlisted.Body.String())
	}
	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	if strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("a refused put stored something: %s", list.Body.String())
	}
}

// The list route answers every project that names a default, by key, and an empty list — never
// null — when none does.
func TestListProjectDefaultAssigneesOrdersByProjectKey(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "OPS", "Ops")
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	createDefaultAssigneeProject(t, handler, "WEB", "Web")

	empty := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	if empty.Code != http.StatusOK || strings.TrimSpace(empty.Body.String()) != "[]" {
		t.Fatalf("list with no default: status=%d body=%s", empty.Code, empty.Body.String())
	}

	putDefaultAssignee(t, handler, "OPS", "bob")
	putDefaultAssignee(t, handler, "CORE", "alice")
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list defaults: status=%d body=%s", response.Code, response.Body.String())
	}
	defaults := decodeBody[[]projectDefaultAssigneeRow](t, response)
	if len(defaults) != 2 ||
		defaults[0].Project != "CORE" || defaults[0].Assignee == nil || *defaults[0].Assignee != "alice" ||
		defaults[1].Project != "OPS" || defaults[1].Assignee == nil || *defaults[1].Assignee != "bob" {
		t.Fatalf("defaults = %#v, want CORE/alice then OPS/bob", defaults)
	}
}

// Both routes are human-only: a project's default names a person, and no agent session sets or
// reads one.
func TestProjectDefaultAssigneeRoutesRefuseAgentSessions(t *testing.T) {
	handler := newTestHandler(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")

	listed := agentRequest(t, handler, http.MethodGet, "/api/v1/settings/project-default-assignees", nil, "agent-token")
	if listed.Code != http.StatusForbidden || !strings.Contains(listed.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("agent lists defaults: status=%d body=%s", listed.Code, listed.Body.String())
	}
	put := agentRequest(t, handler, http.MethodPut, "/api/v1/projects/CORE/default-assignee",
		map[string]any{"assignee": "bob", "actor": sessionActor()}, "agent-token")
	if put.Code != http.StatusForbidden || !strings.Contains(put.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("agent sets default: status=%d body=%s", put.Code, put.Body.String())
	}
}

// The PUT appends one quiet event on the project: settings, retained and streamed, waking nobody.
func TestPutProjectDefaultAssigneeAppendsAQuietEvent(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createDefaultAssigneeProject(t, handler, "CORE", "Core")
	putDefaultAssignee(t, handler, "CORE", "bob")

	var project, assignee string
	var notify bool
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(project_key, ''), payload->>'assignee', notify from events
		where type = 'settings.project_default_assignee.updated' order by id desc limit 1
	`).Scan(&project, &assignee, &notify); err != nil {
		t.Fatalf("read default assignee event: %v", err)
	}
	if project != "CORE" || assignee != "bob" || notify {
		t.Fatalf("event = project %q assignee %q notify %t, want CORE/bob and quiet", project, assignee, notify)
	}
}
