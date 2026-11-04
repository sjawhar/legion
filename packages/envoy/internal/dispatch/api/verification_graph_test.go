package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestVerificationGraphReadIsScopedToTheNamedIssuesDocument(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	own := createInteractionIssue(t, handler, "VGR", "Own", twoRequirements)
	other := createInteractionIssue(t, handler, "VGO", "Other project", twoRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+own.Key, "verification.md", checkServing("own", "dispatch://"+own.Key+"/spec#b-r1"))
	uploadDocument(t, handler, "/api/v1/issues/"+other.Key, "verification.md", checkServing("other", "dispatch://"+other.Key+"/spec#b-r1"))
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+own.Key+"/verification", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("verification: %d %s", response.Code, response.Body.String())
	}
	answer := decodeBody[struct {
		Issue  string              `json:"issue"`
		Checks []model.CheckBlock  `json:"checks"`
		Serves map[string][]string `json:"serves"`
	}](t, response)
	if answer.Issue != own.Key || len(answer.Checks) != 1 || answer.Checks[0].BlockID != "own" {
		t.Fatalf("cross-issue data in verification: %+v", answer)
	}
	if got := answer.Serves["own"]; len(got) != 1 || got[0] != "dispatch://"+own.Key+"/spec#b-r1" {
		t.Fatalf("serves = %v", answer.Serves)
	}
	census := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if census.Code != http.StatusOK {
		t.Fatal(census.Body.String())
	}
	report := decodeBody[verificationShadowReport](t, census)
	if report.Issues != 2 || report.Mismatches != 0 {
		t.Fatalf("census scope: %s", census.Body.String())
	}
	for _, path := range []string{"/api/v1/issues/" + own.Key + "/verification", "/api/v1/verification-shadow"} {
		if response := dispatchRequest(t, handler, http.MethodGet, path, nil, ""); response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: %d", path, response.Code)
		}
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/NOPE-1/verification", nil, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("missing issue: %d", response.Code)
	}
}

func assertVerificationShadow(t *testing.T, handler http.Handler) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("verification shadow: %d %s", response.Code, response.Body.String())
	}
	report := decodeBody[struct {
		Mismatches int `json:"mismatches"`
	}](t, response)
	if report.Mismatches != 0 {
		t.Fatalf("verification shadow mismatch: %s", response.Body.String())
	}
}

func TestVerificationGraphJSONMatchesLegacyResults(t *testing.T) {
	handler, database, _, s := newRegisteredTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VJR", "Results", twoRequirements)
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	if response := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil)); response.Code != http.StatusCreated {
		t.Fatal(response.Body.String())
	}
	old, err := s.legacyCheckResults(t.Context(), database.Pool, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.verificationGraph(t.Context(), database.Pool, issue.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, _ := json.Marshal(old)
	nextJSON, _ := json.Marshal(next.checkResultsRead)
	if !bytes.Equal(oldJSON, nextJSON) {
		t.Fatalf("old: %s\nnew: %s", oldJSON, nextJSON)
	}
	if causes := resultMismatchCauses(old, next.checkResultsRead); len(causes) != 0 {
		t.Fatalf("equal JSON has causes %v; old result %#v new result %#v", causes, old.Checks[0].Results[0], next.Checks[0].Results[0])
	}
}

func TestVerificationShadowCountsEveryDocumentAndDetectsMissingMembership(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VSH", "Shadow", twoRequirements)
	document := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", twoChecks)
	// Simulate a document whose index is owed. The census must not silently claim equality.
	if _, err := database.Pool.Exec(context.Background(), `delete from document_blocks where artifact_id = $1::uuid and block_id = 'k1'`, document.Artifact.ID); err != nil {
		t.Fatal(err)
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/verification-shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
	}
	report := decodeBody[verificationShadowReport](t, response)
	if report.Issues != 1 || report.Mismatches != 1 || report.ByCause["check_membership"] != 1 || len(report.Details) != 1 || report.Details[0].Issue != issue.Key {
		t.Fatalf("census hid missing membership: %s", response.Body.String())
	}
}
