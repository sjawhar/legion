package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// listReviews is every review of a document, newest first, as the review list route serves them.
func listReviews(t *testing.T, handler http.Handler, artifactID string) []model.ArtifactReview {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/reviews", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list reviews of %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	return decodeBody[[]model.ArtifactReview](t, response)
}

// approvalRequestAsk is what the approval request route answers, with the documents its ask names.
type approvalRequestAsk struct {
	Ask *struct {
		ID       string `json:"id"`
		Question string `json:"question"`
		Approval struct {
			ArtifactID string                      `json:"artifact_id"`
			Version    int                         `json:"version"`
			Documents  []model.AskApprovalDocument `json:"documents"`
		} `json:"approval"`
	} `json:"ask"`
	Version int `json:"version"`
}

// One request covers a spec and its issue's Verification document: one Inbox card names both at
// their current versions, and the human's one answer records an approved review on each.
func TestApprovalRequestWithTheVerificationDocumentReviewsBoth(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VER", "Approve both", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	verification := uploadDocument(t, handler, owner, "verification.md", "# Verification\n\nOne.\n")

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "summary": "A live sync replaces the export.", "include_verification": true})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval of both: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[approvalRequestAsk](t, requested)
	if request.Ask == nil || len(request.Ask.Approval.Documents) != 2 {
		t.Fatalf("approval ask = %#v, want one ask naming two documents", request.Ask)
	}
	want := "Approve spec.md (version 1) and verification.md (version 1)? A live sync replaces the export."
	if request.Ask.Question != want {
		t.Fatalf("question = %q, want %q", request.Ask.Question, want)
	}
	if request.Ask.Approval.Documents[0].ArtifactID != issue.PrimaryArtifactID ||
		request.Ask.Approval.Documents[1].ArtifactID != verification.Artifact.ID {
		t.Fatalf("documents = %#v, want the spec then the Verification document", request.Ask.Approval.Documents)
	}
	// Either document finds the request, and each read names both.
	for _, artifactID := range []string{issue.PrimaryArtifactID, verification.Artifact.ID} {
		read := readApproval(t, handler, artifactID)
		if read.Approval == nil || read.Approval.State != "awaiting" || read.Approval.AskID == nil || *read.Approval.AskID != request.Ask.ID {
			t.Fatalf("approval of %s = %#v, want awaiting on the one request", artifactID, read.Approval)
		}
	}

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+request.Ask.ID+"/answer",
		map[string]any{"selected": []string{"Approve"}}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("approve both: status=%d body=%s", answered.Code, answered.Body.String())
	}
	for _, artifactID := range []string{issue.PrimaryArtifactID, verification.Artifact.ID} {
		reviews := listReviews(t, handler, artifactID)
		if len(reviews) != 1 || reviews[0].State != "approved" || reviews[0].Version != 1 || reviews[0].Kind != "person" {
			t.Fatalf("reviews of %s = %#v, want one person's approval of version 1", artifactID, reviews)
		}
		if read := readApproval(t, handler, artifactID); read.Approval.State != "approved" {
			t.Fatalf("approval of %s after the answer = %#v, want approved", artifactID, read.Approval)
		}
	}
	approvals := 0
	for _, event := range issueEvents(t, handler, issue.Key) {
		if event.Type == "artifact.approved" {
			approvals++
		}
	}
	if approvals != 2 {
		t.Fatalf("artifact.approved events = %d, want one per document", approvals)
	}
}

// Without the flag the request names the spec alone and writes nothing on the Verification
// document, as it always has.
func TestApprovalRequestWithoutTheFlagLeavesTheVerificationDocumentAlone(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VER", "Approve the spec", "A spec\n")
	verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[approvalRequestAsk](t, requested)
	if request.Ask == nil || len(request.Ask.Approval.Documents) != 1 || request.Ask.Question != "Approve spec.md (version 1)?" {
		t.Fatalf("approval ask = %#v, want the spec alone", request.Ask)
	}
	if read := readApproval(t, handler, verification.Artifact.ID); read.Approval == nil || read.Approval.State != "draft" {
		t.Fatalf("Verification document while the spec's request is open = %#v, want draft", read.Approval)
	}

	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+request.Ask.ID+"/answer",
		map[string]any{"selected": []string{"Approve"}}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("approve the spec: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if reviews := listReviews(t, handler, verification.Artifact.ID); len(reviews) != 0 {
		t.Fatalf("reviews of the Verification document = %#v, want none", reviews)
	}
}

// An issue with no Verification document has nothing to name beside its spec.
func TestApprovalRequestWithTheVerificationDocumentRefusesAnIssueThatHasNone(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VER", "No verification", "A spec\n")

	refused := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "include_verification": true})
	code, _ := refusalOf(t, refused)
	if refused.Code != http.StatusConflict || code != "NO_VERIFICATION_DOCUMENT" {
		t.Fatalf("request with no Verification document: status=%d code=%s body=%s", refused.Code, code, refused.Body.String())
	}
	if read := readApproval(t, handler, issue.PrimaryArtifactID); read.Approval.State != "draft" {
		t.Fatalf("spec after the refusal = %#v, want draft with nothing opened", read.Approval)
	}
}

// include_verification asks for the issue's spec and its Verification document together, so it
// is refused on any other document. On the Verification document itself it would name that
// document twice, and the human's one answer would write two reviews and two artifact.approved
// events on it.
func TestApprovalRequestWithTheVerificationDocumentRefusesEveryDocumentButTheSpec(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VER", "Only the spec", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	verification := uploadDocument(t, handler, owner, "verification.md", "# Verification\n\nOne.\n")
	notes := uploadDocument(t, handler, owner, "notes.md", "# Notes\n\nOne.\n")

	for _, artifactID := range []string{verification.Artifact.ID, notes.Artifact.ID} {
		refused := sessionRequest(t, handler, http.MethodPost,
			"/api/v1/artifacts/"+artifactID+"/approval-requests",
			map[string]any{"actor": sessionActor(), "include_verification": true})
		code, _ := refusalOf(t, refused)
		if refused.Code != http.StatusConflict || code != "NOT_SPEC_DOCUMENT" {
			t.Fatalf("request with include_verification on %s: status=%d code=%s body=%s",
				artifactID, refused.Code, code, refused.Body.String())
		}
		if read := readApproval(t, handler, artifactID); read.Approval.State != "draft" {
			t.Fatalf("%s after the refusal = %#v, want draft with nothing opened", artifactID, read.Approval)
		}
	}

	// Without the flag the Verification document is requested on its own, as it always was.
	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+verification.Artifact.ID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval of the Verification document alone: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[approvalRequestAsk](t, requested)
	if request.Ask == nil || len(request.Ask.Approval.Documents) != 1 ||
		request.Ask.Approval.Documents[0].ArtifactID != verification.Artifact.ID {
		t.Fatalf("approval ask = %#v, want the Verification document alone", request.Ask)
	}
}

// A new settled version of either document moves the one request, as a new spec version moves a
// request on the spec alone: the card names the new version and waits on the agent again.
func TestANewVerificationVersionMovesTheRequestThatNamesBoth(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "VER", "Move both", "A spec\n")
	owner := "/api/v1/issues/" + issue.Key
	verification := uploadDocument(t, handler, owner, "verification.md", "# Verification\n\nOne.\n")

	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "include_verification": true})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval of both: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[approvalRequestAsk](t, requested).Ask.ID

	uploadDocument(t, handler, owner, "verification.md", "# Verification\n\nTwo.\n")
	moved := readAskDetail(t, handler, askID)
	want := "Approve spec.md (version 1) and verification.md (version 2)?"
	if moved.Question != want || moved.WaitingOn != "agent" {
		t.Fatalf("ask after a new Verification version = %q waiting on %q, want %q waiting on the agent", moved.Question, moved.WaitingOn, want)
	}
	if read := readApproval(t, handler, verification.Artifact.ID); read.Approval.State != "awaiting" || read.Approval.WaitingOn != "agent" {
		t.Fatalf("Verification document after its new version = %#v, want awaiting on the agent", read.Approval)
	}
}
