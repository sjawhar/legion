package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lastIssueEventPayload reads the payload of the issue's newest event.
func lastIssueEventPayload(t *testing.T, database *store.Store, key string) map[string]any {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), `
		select payload from events where issue_key = $1 order by id desc limit 1
	`, key).Scan(&raw); err != nil {
		t.Fatalf("read %s's newest event: %v", key, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode %s's newest event: %v", key, err)
	}
	return payload
}

// An issue's repository: its own when set, else its nearest ancestor's, else the project's default,
// else none; every list row, the detail and every issue event carry both the own field and the
// resolved one. A repository not mapped to the project is refused by name.
func TestIssueRepoResolvesThroughParentsThenTheProjectDefault(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/gadgets", map[string]any{"project": "CORE"}, "alice")

	root := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Root"}, "alice"))
	if root.Repo != nil || root.ResolvedRepo == nil || *root.ResolvedRepo != "acme/widgets" {
		t.Fatalf("root repo=%v resolved=%v, want none and the project default acme/widgets", root.Repo, root.ResolvedRepo)
	}
	created := lastIssueEventPayload(t, database, root.Key) // the issue.created the create wrote
	if created["repo"] != nil || created["resolved_repo"] != "acme/widgets" {
		t.Fatalf("issue.created payload repo=%v resolved_repo=%v, want null and acme/widgets", created["repo"], created["resolved_repo"])
	}
	mid := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Mid", "parent": root.Key, "repo": "acme/gadgets", "force": true}, "alice"))
	if mid.Repo == nil || *mid.Repo != "acme/gadgets" {
		t.Fatalf("mid repo=%v, want its own acme/gadgets", mid.Repo)
	}
	leaf := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Leaf", "parent": mid.Key, "force": true}, "alice"))
	if leaf.Repo != nil || leaf.ResolvedRepo == nil || *leaf.ResolvedRepo != "acme/gadgets" {
		t.Fatalf("leaf repo=%v resolved=%v, want none and the parent's acme/gadgets", leaf.Repo, leaf.ResolvedRepo)
	}
	listed := decodeBody[[]model.IssueSummary](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice"))
	resolved := map[string]string{}
	own := map[string]string{}
	for _, issue := range listed {
		if issue.ResolvedRepo != nil {
			resolved[issue.Key] = *issue.ResolvedRepo
		}
		if issue.Repo != nil {
			own[issue.Key] = *issue.Repo
		}
	}
	if resolved[root.Key] != "acme/widgets" || resolved[mid.Key] != "acme/gadgets" || resolved[leaf.Key] != "acme/gadgets" {
		t.Fatalf("listed resolved repos = %v", resolved)
	}
	if len(own) != 1 || own[mid.Key] != "acme/gadgets" {
		t.Fatalf("listed own repos = %v, want only %s's acme/gadgets", own, mid.Key)
	}
	patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{"repo": "Acme/Widgets.git"}, "alice")
	if patched.Code != http.StatusOK || decodeBody[model.Issue](t, patched).Repo == nil || *decodeBody[model.Issue](t, patched).Repo != "acme/widgets" {
		t.Fatalf("set the leaf's own repo: status=%d body=%s, want it canonical", patched.Code, patched.Body.String())
	}
	detail := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+leaf.Key, nil, "alice"))
	if detail.Repo == nil || *detail.Repo != "acme/widgets" || detail.ResolvedRepo == nil || *detail.ResolvedRepo != "acme/widgets" {
		t.Fatalf("leaf detail repo=%v resolved=%v, want its own acme/widgets", detail.Repo, detail.ResolvedRepo)
	}
	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{"repo": nil}, "alice")
	if cleared.Code != http.StatusOK || decodeBody[model.Issue](t, cleared).Repo != nil {
		t.Fatalf("clear the leaf's own repo: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	payload := lastIssueEventPayload(t, database, leaf.Key) // the issue.updated the clear wrote
	if payload["repo"] != nil || payload["resolved_repo"] != "acme/gadgets" {
		t.Fatalf("issue.updated payload repo=%v resolved_repo=%v, want null and acme/gadgets", payload["repo"], payload["resolved_repo"])
	}
	for _, bad := range []struct{ repo, code string }{
		{"acme/unmapped", "REPO_NOT_MAPPED"},
		{"not-a-repo", "INVALID_ISSUE"},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+leaf.Key, map[string]any{"repo": bad.repo}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), bad.code) {
			t.Fatalf("patch repo %q: status=%d body=%s, want 400 %s", bad.repo, response.Code, response.Body.String(), bad.code)
		}
		response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Bad " + bad.repo, "repo": bad.repo, "force": true}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), bad.code) {
			t.Fatalf("create with repo %q: status=%d body=%s, want 400 %s", bad.repo, response.Code, response.Body.String(), bad.code)
		}
	}
}

// A project with no default and an issue chain naming none resolves to none: null on list, detail and
// events, never an error.
func TestIssueRepoResolvesToNoneWithoutADefault(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE"}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Alone"}, "alice"))
	if issue.Repo != nil || issue.ResolvedRepo != nil {
		t.Fatalf("repo=%v resolved=%v, want both null", issue.Repo, issue.ResolvedRepo)
	}
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if !strings.Contains(listed.Body.String(), `"repo":null`) || !strings.Contains(listed.Body.String(), `"resolved_repo":null`) {
		t.Fatalf("listed issue = %s, want repo and resolved_repo both null", listed.Body.String())
	}
	payload := lastIssueEventPayload(t, database, issue.Key)
	if value, ok := payload["resolved_repo"]; !ok || value != nil {
		t.Fatalf("issue.created payload resolved_repo=%v (present %t), want null", value, ok)
	}
}

// The claim and its release are issue events too, and carry the repository as the others do.
func TestIssueRepoRidesTheClaimEvents(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE", "default": true}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Claimed"}, "alice"))
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if response := dispatchRequest(t, handler, method, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s claim: status=%d body=%s", method, response.Code, response.Body.String())
		}
		payload := lastIssueEventPayload(t, database, issue.Key)
		if value, ok := payload["repo"]; !ok || value != nil || payload["resolved_repo"] != "acme/widgets" {
			t.Fatalf("%s claim event payload = %v, want repo null and resolved_repo acme/widgets", method, payload)
		}
	}
}

// A closed issue takes no repository write, as it takes no other field but its rank and components.
func TestIssueRepoIsRefusedOnAClosedIssue(t *testing.T) {
	handler := newTestHandler(t)
	dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CORE", "name": "Core"}, "alice")
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/acme/widgets", map[string]any{"project": "CORE"}, "alice")
	issue := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CORE", "title": "Done"}, "alice"))
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"repo": "acme/widgets"}, "alice")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "ISSUE_CLOSED") {
		t.Fatalf("set repo on a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED", response.Code, response.Body.String())
	}
}
