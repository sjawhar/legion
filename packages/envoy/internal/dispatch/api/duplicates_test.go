package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createIssueRequest(t *testing.T, handler http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
}

func createDuplicateTestProject(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": key, "name": key + " project",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create project %q: status=%d body=%s", key, response.Code, response.Body.String())
	}
}

func requireCreatedIssue(t *testing.T, response *httptest.ResponseRecorder) model.Issue {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

func TestCreateIssueRejectsNearDuplicateTitleWithCandidates(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	second := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search for issues and documents",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("near-duplicate issue: status=%d body=%s", second.Code, second.Body.String())
	}
	body := decodeBody[struct {
		Code       string                     `json:"code"`
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, second)
	if body.Code != "POSSIBLE_DUPLICATE" {
		t.Fatalf("duplicate code = %q, want POSSIBLE_DUPLICATE", body.Code)
	}
	if len(body.Candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", len(body.Candidates), body.Candidates)
	}
	candidate := body.Candidates[0]
	if candidate.Key != first.Key || candidate.Title != "Global search across issues and documents" ||
		candidate.Status != "triage" || candidate.SharedTerms != 4 ||
		candidate.Snippet != "<mark>Global</mark> <mark>search</mark> across <mark>issues</mark> and <mark>documents</mark>" ||
		candidate.Href != "/issues/"+first.Key {
		t.Fatalf("candidate = %#v", candidate)
	}

	third := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Unrelated",
	}))
	if third.Key != "TEST-2" {
		t.Fatalf("issue created after refusal = %q, want TEST-2", third.Key)
	}
}

func TestCreateIssueForceBypassesTheGate(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	forced := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search for issues and documents", "force": true,
	}))
	if forced.Key != "TEST-2" {
		t.Fatalf("forced issue key = %q, want TEST-2", forced.Key)
	}
}

func TestCreateIssueGateIgnoresParentTitleTerms(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	parent := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Dispatch global search",
	}))
	childA := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key, "title": "Dispatch global search: server",
	}))
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key, "title": "Dispatch global search: SPA",
	}))
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "parent": parent.Key, "title": "Dispatch global search: server",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate child: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	body := decodeBody[struct {
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, duplicate)
	if len(body.Candidates) != 1 || body.Candidates[0].Key != childA.Key {
		t.Fatalf("duplicate child candidates = %#v, want %q", body.Candidates, childA.Key)
	}
}

func TestCreateIssueGateIsScopedToProjectAndSkipsExternal(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")
	createDuplicateTestProject(t, handler, "OTHER")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	other := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "OTHER", "title": "Global search across issues and documents",
	}))
	if other.Key != "OTHER-1" {
		t.Fatalf("cross-project issue key = %q, want OTHER-1", other.Key)
	}
	mapping := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/owner/repo", map[string]string{
		"project": "TEST",
	}, "alice")
	if mapping.Code != http.StatusOK {
		t.Fatalf("map external repository: status=%d body=%s", mapping.Code, mapping.Body.String())
	}
	external := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"external": "owner/repo#41", "title": "Global search across issues and documents",
	}))
	if external.Key != "TEST-2" {
		t.Fatalf("external issue key = %q, want TEST-2", external.Key)
	}
}

func TestCreateIssueGateIncludesClosedIssues(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	first := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe calibration notes",
	}))
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+first.Key, map[string]string{
		"status": "done",
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Astrolabe",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate closed issue: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	body := decodeBody[struct {
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, duplicate)
	if len(body.Candidates) != 1 || body.Candidates[0].Key != first.Key || body.Candidates[0].Status != "done" {
		t.Fatalf("closed duplicate candidates = %#v", body.Candidates)
	}
}

func TestCreateIssueGateRule(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "TEST")

	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search across issues and documents",
	}))
	threeShared := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Global search issues title",
	})
	if threeShared.Code != http.StatusConflict {
		t.Fatalf("three shared terms issue: status=%d body=%s", threeShared.Code, threeShared.Body.String())
	}
	oneShared := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Search palette loses focus on phone",
	})
	if oneShared.Code != http.StatusCreated {
		t.Fatalf("one shared term issue: status=%d body=%s", oneShared.Code, oneShared.Body.String())
	}
	requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Red-teamer hiring test on the platform: candidate attack authoring and live results",
	}))
	fourShared := createIssueRequest(t, handler, map[string]any{
		"project": "TEST", "title": "Chief of staff: red-teamer ops, onboarding, weekly check-ins, contractor comms, platform access",
	})
	if fourShared.Code != http.StatusCreated {
		t.Fatalf("four shared terms in long title: status=%d body=%s", fourShared.Code, fourShared.Body.String())
	}
}

func TestCreateIssueOrdersDuplicateCandidates(t *testing.T) {
	handler := newTestHandler(t)
	createDuplicateTestProject(t, handler, "ORDER")

	older := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents alpha", "force": true,
	}))
	newer := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents beta", "force": true,
	}))
	highest := requireCreatedIssue(t, createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents alpha beta", "force": true,
	}))
	duplicate := createIssueRequest(t, handler, map[string]any{
		"project": "ORDER", "title": "Global search issues documents alpha beta gamma",
	})
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("ordered candidates: status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	body := decodeBody[struct {
		Candidates []model.DuplicateCandidate `json:"candidates"`
	}](t, duplicate)
	if len(body.Candidates) != 3 || body.Candidates[0].Key != highest.Key || body.Candidates[1].Key != newer.Key || body.Candidates[2].Key != older.Key {
		t.Fatalf("candidate ordering = %#v, want %q, %q, then %q", body.Candidates, highest.Key, newer.Key, older.Key)
	}
}
