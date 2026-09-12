package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type approvalRead struct {
	Approval *struct {
		State         string  `json:"state"`
		LatestVersion int     `json:"latest_version"`
		Version       *int    `json:"version"`
		Reason        *string `json:"reason"`
		AskID         *string `json:"ask_id"`
		By            *struct {
			ID string `json:"id"`
		} `json:"by"`
	} `json:"approval"`
}

func readApproval(t *testing.T, handler http.Handler, artifactID string) approvalRead {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read artifact: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[approvalRead](t, response)
}

func TestApprovalRequestOpensAnAskWhoseAnswerPinsAReviewToTheDocumentVersion(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Approve me", "A spec")
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval == nil || got.Approval.State != "draft" || got.Approval.LatestVersion != 1 {
		t.Fatalf("fresh document approval = %#v, want draft at version 1", got.Approval)
	}

	// The agent requests approval: a fixed-option ask lands in the Inbox.
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	request := decodeBody[struct {
		Ask struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
			Approval struct {
				ArtifactID string `json:"artifact_id"`
				Version    int    `json:"version"`
			} `json:"approval"`
		} `json:"ask"`
		Version int `json:"version"`
	}](t, requested)
	if request.Ask.Kind != "approval" || len(request.Ask.Options) != 2 || request.Ask.Options[0].Label != "Approve" || request.Ask.Options[1].Label != "Request changes" || request.Ask.Approval.ArtifactID != issue.PrimaryArtifactID || request.Version != 1 {
		t.Fatalf("approval ask = %#v", request)
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "awaiting" || got.Approval.AskID == nil || *got.Approval.AskID != request.Ask.ID {
		t.Fatalf("approval after request = %#v, want awaiting with the ask id", got.Approval)
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if !strings.Contains(inbox.Body.String(), `"kind":"approval"`) {
		t.Fatalf("inbox = %s, want the approval ask", inbox.Body.String())
	}
	// A second request while one is open returns the same ask.
	again := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if again.Code != http.StatusOK || !strings.Contains(again.Body.String(), request.Ask.ID) {
		t.Fatalf("repeat request: status=%d body=%s", again.Code, again.Body.String())
	}
	// Its wording cannot be edited.
	if edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+request.Ask.ID, map[string]any{"question": "Other", "actor": sessionActor()}); edited.Code != http.StatusConflict {
		t.Fatalf("edit approval ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	// Agents cannot answer it.
	if agent := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+request.Ask.ID+"/answer", map[string]any{"selected": []string{"Approve"}, "actor": sessionActor()}); agent.Code < 400 {
		t.Fatalf("agent answered an approval ask: status=%d", agent.Code)
	}
	// Request changes without a reason is refused; with one it is a review.
	if noReason := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+request.Ask.ID+"/answer", map[string]any{"selected": []string{"Request changes"}}, "alice"); noReason.Code != http.StatusBadRequest {
		t.Fatalf("request changes without reason: status=%d body=%s", noReason.Code, noReason.Body.String())
	}
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+request.Ask.ID+"/answer", map[string]any{"selected": []string{"Approve"}}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("approve: status=%d body=%s", answered.Code, answered.Body.String())
	}
	got := readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != 1 || got.Approval.By == nil || got.Approval.By.ID != "alice" || got.Approval.AskID == nil || *got.Approval.AskID != request.Ask.ID {
		t.Fatalf("approval after answer = %#v, want approved at v1 by alice via the ask", got.Approval)
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	var events []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(log.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var approvedPayload map[string]any
	for _, event := range events {
		if event.Type == "artifact.approved" {
			if err := json.Unmarshal(event.Payload, &approvedPayload); err != nil {
				t.Fatalf("decode artifact.approved: %v", err)
			}
		}
	}
	if approvedPayload == nil || approvedPayload["artifact_id"] != issue.PrimaryArtifactID || approvedPayload["version"] != float64(1) || approvedPayload["ask_id"] != request.Ask.ID || approvedPayload["name"] != "spec.md" {
		t.Fatalf("artifact.approved payload = %#v", approvedPayload)
	}

	// A request while the current version is approved returns the approval and opens nothing.
	satisfied := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if satisfied.Code != http.StatusOK || !strings.Contains(satisfied.Body.String(), `"ask":null`) || !strings.Contains(satisfied.Body.String(), `"state":"approved"`) {
		t.Fatalf("request on an approved document: status=%d body=%s", satisfied.Code, satisfied.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "approved" {
		t.Fatalf("approval after a satisfied request = %#v, want still approved", got.Approval)
	}

	// A new version makes the approval stale; nothing is emitted for that.
	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "A revised spec", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("revise document: %v", err)
	}
	if _, err := documentService.NamedVersion(context.Background(), issue.PrimaryArtifactID, "revised", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("settle revised version: %v", err)
	}
	got = readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval.State != "stale" || got.Approval.LatestVersion != 2 || *got.Approval.Version != 1 {
		t.Fatalf("approval after edit = %#v, want stale (approved v1, latest v2)", got.Approval)
	}

	// Re-approving from the header, with no request open, pins the new version and names no ask.
	header := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "approved"}, "alice")
	if header.Code != http.StatusCreated {
		t.Fatalf("header approve: status=%d body=%s", header.Code, header.Body.String())
	}
	got = readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval.State != "approved" || *got.Approval.Version != 2 || got.Approval.AskID != nil {
		t.Fatalf("approval after header approve = %#v, want approved v2 with no ask", got.Approval)
	}
	reviews := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", nil, "alice")
	var history []struct {
		Version int    `json:"version"`
		State   string `json:"state"`
	}
	if err := json.NewDecoder(reviews.Body).Decode(&history); err != nil {
		t.Fatalf("decode reviews: %v", err)
	}
	if len(history) != 2 || history[0].Version != 2 || history[1].Version != 1 {
		t.Fatalf("review history = %#v, want v2 then v1", history)
	}
}

func TestHeaderChangesRequestedAnswersTheOpenApprovalAskAndNeedsAReason(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Needs work", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID

	if missing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "changes_requested"}, "alice"); missing.Code != http.StatusBadRequest {
		t.Fatalf("changes requested without reason: status=%d body=%s", missing.Code, missing.Body.String())
	}
	if agent := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "approved", "actor": sessionActor()}); agent.Code < 400 {
		t.Fatalf("agent wrote a review: status=%d", agent.Code)
	}
	review := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "changes_requested", "reason": "Name the rollback path."}, "alice")
	if review.Code != http.StatusCreated {
		t.Fatalf("request changes: status=%d body=%s", review.Code, review.Body.String())
	}
	got := readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval.State != "changes_requested" || got.Approval.Reason == nil || *got.Approval.Reason != "Name the rollback path." || got.Approval.AskID == nil || *got.Approval.AskID != askID {
		t.Fatalf("approval = %#v, want changes_requested with the reason, via the open ask", got.Approval)
	}
	ask := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if !strings.Contains(ask.Body.String(), `"state":"answered"`) || !strings.Contains(ask.Body.String(), `"selected":["Request changes"]`) {
		t.Fatalf("approval ask after header review = %s, want answered with Request changes", ask.Body.String())
	}
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if !strings.Contains(log.Body.String(), `"type":"artifact.changes_requested"`) || !strings.Contains(log.Body.String(), `"reason":"Name the rollback path."`) {
		t.Fatalf("events = %s, want artifact.changes_requested with the reason", log.Body.String())
	}
}
