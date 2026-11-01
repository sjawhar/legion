package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type repoProjectResponse struct {
	Repo    string `json:"repo"`
	Project string `json:"project"`
	Default bool   `json:"default"`
}

// projectEvents reads the payloads of the project's events of one type, oldest first.
func projectEvents(t *testing.T, database *store.Store, project, eventType string) []map[string]any {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select payload from events where project_key = $1 and type = $2 order by id
	`, project, eventType)
	if err != nil {
		t.Fatalf("read %s events of %s: %v", eventType, project, err)
	}
	defer rows.Close()
	var payloads []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan %s event: %v", eventType, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s event: %v", eventType, err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s events of %s: %v", eventType, project, err)
	}
	return payloads
}

// A project's default is set with the mapping and moves with one write: setting a second mapping
// default demotes the first in the same transaction, and both rows' events say so. A mapping moved
// to another project emits settings.repo_project.updated on the old project and the new one.
func TestRepoProjectDefaultMovesAndEventsReachBothProjects(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	for _, project := range []map[string]string{{"key": "CORE", "name": "Core"}, {"key": "OTHER", "name": "Other"}} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	first := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	if first.Code != http.StatusOK || !decodeBody[repoProjectResponse](t, first).Default {
		t.Fatalf("map acme/widgets as default: status=%d body=%s", first.Code, first.Body.String())
	}
	second := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE", "default": true}, "alice")
	if second.Code != http.StatusOK {
		t.Fatalf("map acme/gadgets as default: status=%d body=%s", second.Code, second.Body.String())
	}
	listed := decodeBody[[]repoProjectResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "alice"))
	defaults := map[string]bool{}
	for _, mapping := range listed {
		defaults[mapping.Repo] = mapping.Default
	}
	if defaults["acme/widgets"] || !defaults["acme/gadgets"] {
		t.Fatalf("defaults after the move = %v, want gadgets alone", defaults)
	}
	events := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if len(events) != 3 { // widgets set, widgets demoted, gadgets set
		t.Fatalf("CORE saw %d mapping events, want 3: %v", len(events), events)
	}
	if demoted := events[1]["mapping"].(map[string]any); demoted["repo"] != "acme/widgets" || demoted["default"] != false || events[1]["deleted"] != false {
		t.Fatalf("second CORE event = %v, want acme/widgets demoted", events[1])
	}
	// A write that leaves default out keeps the mapping's default while its project is unchanged.
	if kept := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE"}, "alice"); kept.Code != http.StatusOK || !decodeBody[repoProjectResponse](t, kept).Default {
		t.Fatalf("re-map acme/gadgets without default: status=%d body=%s, want it still the default", kept.Code, kept.Body.String())
	}
	moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "OTHER"}, "alice")
	if moved.Code != http.StatusOK || decodeBody[repoProjectResponse](t, moved).Default {
		t.Fatalf("move acme/gadgets to OTHER: status=%d body=%s, want it moved and not OTHER's default", moved.Code, moved.Body.String())
	}
	if got := len(projectEvents(t, database, "OTHER", "settings.repo_project.updated")); got != 1 {
		t.Fatalf("OTHER saw %d mapping events after the move, want 1", got)
	}
	core := projectEvents(t, database, "CORE", "settings.repo_project.updated")
	if len(core) != 5 { // the three above, the re-map, and the leaving mapping
		t.Fatalf("CORE saw %d mapping events after the move, want 5: %v", len(core), core)
	}
	if leaving := core[4]["mapping"].(map[string]any); leaving["repo"] != "acme/gadgets" || leaving["project"] != "CORE" || core[4]["deleted"] != true {
		t.Fatalf("CORE's last event = %v, want acme/gadgets leaving CORE, deleted:true", core[4])
	}
}

// A mapping an issue names is in use: its delete and its move are refused with 409 naming the count.
func TestRepoProjectInUseIsNeitherDeletedNorMoved(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []map[string]string{{"key": "CORE", "name": "Core"}, {"key": "OTHER", "name": "Other"}} {
		dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice")
	}
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "One", "repo": "acme/widgets"}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	for _, attempt := range []struct {
		name   string
		method string
		body   any
	}{
		{"delete", http.MethodDelete, nil},
		{"move", http.MethodPut, map[string]any{"project": "OTHER"}},
	} {
		response := dispatchRequest(t, handler, attempt.method, "/api/v1/settings/repo-projects/acme/widgets", attempt.body, "alice")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "REPO_PROJECT_IN_USE") || !strings.Contains(response.Body.String(), "1 issue") {
			t.Fatalf("%s a mapping in use: status=%d body=%s, want 409 REPO_PROJECT_IN_USE naming 1 issue", attempt.name, response.Code, response.Body.String())
		}
	}
	// Re-mapping it to the project it already has is no move, so the issue does not refuse it.
	if same := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE"}, "alice"); same.Code != http.StatusOK {
		t.Fatalf("re-map a mapping in use to its own project: status=%d body=%s", same.Code, same.Body.String())
	}
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

	missingProject := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/missing", map[string]string{
		"project": "MISSING",
	}, "alice")
	if missingProject.Code != http.StatusNotFound {
		t.Fatalf("upsert missing project: status=%d body=%s", missingProject.Code, missingProject.Body.String())
	}
}

func TestAgentListsRepoProjectSettings(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("seed repository project mapping: status=%d body=%s", response.Code, response.Body.String())
	}

	response := agentRequest(t, handler, http.MethodGet, "/api/v1/settings/repo-projects", nil, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("agent list repository projects: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := decodeBody[[]repoProjectResponse](t, response); len(got) != 1 || got[0] != (repoProjectResponse{Repo: "owner/repo", Project: "CORE"}) {
		t.Fatalf("agent listed mappings: got %#v", got)
	}
}

func TestAgentPutsRepoProjectSettingsWithItsActor(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}

	response := agentRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]any{
		"project": "CORE",
		"actor":   map[string]string{"kind": "session", "id": "agent-repo-project-put"},
	}, "agent-token")
	if response.Code != http.StatusOK {
		t.Fatalf("agent upsert repository project: status=%d body=%s", response.Code, response.Body.String())
	}
	var actorID string
	if err := database.Pool.QueryRow(t.Context(), `
		select created_by->>'id' from repo_projects where repo = 'owner/repo'
	`).Scan(&actorID); err != nil {
		t.Fatalf("read repository project actor: %v", err)
	}
	if actorID != "agent-repo-project-put" {
		t.Fatalf("repository project actor = %q, want agent-repo-project-put", actorID)
	}
}

func TestAgentDeletesRepoProjectSettingsWithItsActor(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "CORE",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("seed repository project mapping: status=%d body=%s", response.Code, response.Body.String())
	}

	response := agentRequest(t, handler, http.MethodDelete, "/api/v1/settings/repo-projects/owner/repo", map[string]any{
		"actor": map[string]string{"kind": "session", "id": "agent-repo-project-delete"},
	}, "agent-token")
	if response.Code != http.StatusNoContent {
		t.Fatalf("agent delete repository project: status=%d body=%s", response.Code, response.Body.String())
	}
	var actorID string
	if err := database.Pool.QueryRow(t.Context(), `
		select actor->>'id'
		from events
		where type = 'settings.repo_project.updated' and payload->>'deleted' = 'true'
		order by id desc
		limit 1
	`).Scan(&actorID); err != nil {
		t.Fatalf("read repository project deletion actor: %v", err)
	}
	if actorID != "agent-repo-project-delete" {
		t.Fatalf("repository project deletion actor = %q, want agent-repo-project-delete", actorID)
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

	linked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Linked pull request",
	}, "alice")
	if linked.Code != http.StatusCreated {
		t.Fatalf("create linked issue: status=%d body=%s", linked.Code, linked.Body.String())
	}
	linkedKey := decodeBody[struct {
		Key string `json:"key"`
	}](t, linked).Key
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+linkedKey, map[string]any{
		"external_links": []map[string]string{{
			"url": "https://github.com/mapped/repository/pull/2",
		}},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("link pull request: status=%d body=%s", response.Code, response.Body.String())
	}
	mismatchedLinked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"external": "mapped/repository#2",
		"project":  "DEFAULT",
	}, "alice")
	if mismatchedLinked.Code != http.StatusBadRequest ||
		!strings.Contains(mismatchedLinked.Body.String(), `"code":"EXTERNAL_PROJECT_MISMATCH"`) {
		t.Fatalf(
			"reject mismatched linked external project: status=%d body=%s",
			mismatchedLinked.Code,
			mismatchedLinked.Body.String(),
		)
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
	if issue.Key != "CORE-2" || len(issue.Labels) != 0 {
		t.Fatalf("persisted mapping external issue: got %#v", issue)
	}
}
