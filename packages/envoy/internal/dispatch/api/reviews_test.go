package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if named := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("settle revised version: status=%d body=%s", named.Code, named.Body.String())
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

// An approval request's question carries the requester's summary of what the version proposes,
// within the ask cap. A request after the document has moved on retracts the ask naming the older
// version and opens one at the latest, so no human approves a version the question never named.
func TestApprovalRequestSummaryAndARepeatAfterANewVersion(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	type approvalAsk struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Question string `json:"question"`
		Approval struct {
			Version int `json:"version"`
		} `json:"approval"`
		Resolution *struct {
			Kind   string `json:"kind"`
			Reason string `json:"reason"`
			Actor  struct {
				ID string `json:"id"`
			} `json:"actor"`
		} `json:"resolution"`
	}
	type requestResponse struct {
		Ask     approvalAsk `json:"ask"`
		Version int         `json:"version"`
	}
	request := func(artifactID string, summary *string) *httptest.ResponseRecorder {
		body := map[string]any{"actor": sessionActor()}
		if summary != nil {
			body["summary"] = *summary
		}
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", body)
	}

	// A request without a summary asks what it always has.
	plain := createInteractionIssue(t, handler, "TEST", "Unsummarised approval", "Another spec")
	unsummarised := request(plain.PrimaryArtifactID, nil)
	if unsummarised.Code != http.StatusCreated {
		t.Fatalf("request without a summary: status=%d body=%s", unsummarised.Code, unsummarised.Body.String())
	}
	if got := decodeBody[requestResponse](t, unsummarised).Ask.Question; got != "Approve spec.md (version 1)?" {
		t.Fatalf("question without a summary = %q", got)
	}

	issue := createInteractionIssue(t, handler, "SUM", "Summarised approval", "A spec")
	for _, blank := range []string{"", " \n\t "} {
		if refused := request(issue.PrimaryArtifactID, &blank); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"SUMMARY_INPUT"`) || !strings.Contains(refused.Body.String(), "summary is blank") {
			t.Fatalf("summary %q: status=%d body=%s", blank, refused.Code, refused.Body.String())
		}
	}
	// "Approve spec.md (version 1)? " leaves 771 of the 800 UTF-16 units for the summary; each
	// "é" is one unit and two bytes, and the padding around a summary is not part of it.
	over := strings.Repeat("é", 772)
	if refused := request(issue.PrimaryArtifactID, &over); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(refused.Body.String(), "summary is 1 characters over the 771-character limit (772/771)") {
		t.Fatalf("summary over the cap: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "draft" {
		t.Fatalf("approval after refused requests = %#v, want draft with nothing opened", got.Approval)
	}
	atCap := strings.Repeat("é", 771)
	padded := "  " + atCap + "\n"
	first := request(issue.PrimaryArtifactID, &padded)
	if first.Code != http.StatusCreated {
		t.Fatalf("summary at the cap: status=%d body=%s", first.Code, first.Body.String())
	}
	opened := decodeBody[requestResponse](t, first).Ask
	if opened.Question != "Approve spec.md (version 1)? "+atCap || opened.Approval.Version != 1 {
		t.Fatalf("summarised approval ask = %#v", opened)
	}

	// A repeat at the same version returns the open ask as it stands, whatever it says, though the
	// summary is still checked.
	other := "A different summary."
	repeat := request(issue.PrimaryArtifactID, &other)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat at the same version: status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	if got := decodeBody[requestResponse](t, repeat).Ask; got.ID != opened.ID || got.Question != opened.Question {
		t.Fatalf("repeat at the same version = %#v, want ask %s unchanged", got, opened.ID)
	}
	if refused := request(issue.PrimaryArtifactID, &over); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("summary over the cap on a repeat at the same version: status=%d body=%s", refused.Code, refused.Body.String())
	}

	// A repeat after a new version retracts that ask and opens one at the new version.
	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "A revised spec", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("revise document: %v", err)
	}
	if named := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("settle revised version: status=%d body=%s", named.Code, named.Body.String())
	}
	revised := "Adds the retry budget."
	second := request(issue.PrimaryArtifactID, &revised)
	if second.Code != http.StatusCreated {
		t.Fatalf("repeat after a new version: status=%d body=%s", second.Code, second.Body.String())
	}
	reopened := decodeBody[requestResponse](t, second)
	if reopened.Ask.ID == opened.ID || reopened.Version != 2 || reopened.Ask.Approval.Version != 2 || reopened.Ask.Question != "Approve spec.md (version 2)? Adds the retry budget." {
		t.Fatalf("approval ask after a new version = %#v", reopened)
	}
	retracted := decodeBody[requestResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+opened.ID, nil, "alice")).Ask
	if retracted.State != "resolved" || retracted.Resolution == nil || retracted.Resolution.Kind != "retracted" || retracted.Resolution.Actor.ID != sessionActor()["id"] || !strings.Contains(retracted.Resolution.Reason, "version 2") {
		t.Fatalf("superseded approval ask = %#v, want retracted by the requester naming version 2", retracted)
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "awaiting" || got.Approval.AskID == nil || *got.Approval.AskID != reopened.Ask.ID {
		t.Fatalf("approval after the repeat = %#v, want awaiting on ask %s", got.Approval, reopened.Ask.ID)
	}
	// Followers of the old ask learn it closed, then the new ask opens, in that order.
	log := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if log.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", log.Code, log.Body.String())
	}
	var events []struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(log.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	position := func(eventType, askID string) int {
		for index, event := range events {
			if event.Type == eventType && event.Payload.ID == askID {
				return index
			}
		}
		return -1
	}
	resolvedAt, openedAt := position("ask.resolved", opened.ID), position("ask.opened", reopened.Ask.ID)
	if resolvedAt < 0 || openedAt < 0 || resolvedAt > openedAt {
		t.Fatalf("events = %#v, want ask.resolved for %s before ask.opened for %s", events, opened.ID, reopened.Ask.ID)
	}

	// Once the latest version is approved a request opens nothing, and its summary is still checked.
	if approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+reopened.Ask.ID+"/answer", map[string]any{"selected": []string{"Approve"}}, "alice"); approved.Code != http.StatusOK {
		t.Fatalf("approve version 2: status=%d body=%s", approved.Code, approved.Body.String())
	}
	if refused := request(issue.PrimaryArtifactID, &over); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("summary over the cap on an approved document: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if satisfied := request(issue.PrimaryArtifactID, &revised); satisfied.Code != http.StatusOK || !strings.Contains(satisfied.Body.String(), `"ask":null`) || !strings.Contains(satisfied.Body.String(), `"state":"approved"`) {
		t.Fatalf("request on an approved document: status=%d body=%s", satisfied.Code, satisfied.Body.String())
	}
}

func TestEditedLegacyTableCellPipeDocumentStalesApproval(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Approved table", "| header |\n| :--- |\n| `one\\|two` |\n")
	approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]string{"state": "approved"}, "alice")
	if approved.Code != http.StatusCreated {
		t.Fatalf("approve document: status=%d body=%s", approved.Code, approved.Body.String())
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $2 where artifact_id = $1 and number = 1
	`, issue.PrimaryArtifactID, "| header |\n| :--- |\n| `one|two` |\n"); err != nil {
		t.Fatalf("seed legacy canonical markdown: %v", err)
	}
	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "| header |\n| :--- |\n| `one\\|three` |\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("edit legacy document: %v", err)
	}
	if named := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]string{"summary": "human edit"}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("record human version: status=%d body=%s", named.Code, named.Body.String())
	}
	got := readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval == nil || got.Approval.State != "stale" || got.Approval.LatestVersion != 2 || got.Approval.Version == nil || *got.Approval.Version != 1 {
		t.Fatalf("approval after editing legacy table = %#v, want stale review v1 at latest v2", got.Approval)
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

// An architect writes a spec whose decisions are ask blocks and a human approves it straight
// away. Settlement indexes those blocks seconds later, over words nobody has touched since. A
// version minted for that indexing stales an approval a human has just given, and Legion's
// design gate - open exactly while the approved version is the latest - closes with nothing in
// the event stream to explain it (LEGION-273).
func TestApprovedSpecStaysApprovedThroughItsAskBlockSettlement(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Approve the decisions", "Context\n\n"+transportAsk)
	approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "approved"}, "alice")
	if approved.Code != http.StatusCreated {
		t.Fatalf("approve the spec: status=%d body=%s", approved.Code, approved.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "approved" || got.Approval.LatestVersion != 1 {
		t.Fatalf("approval of the spec as written = %#v, want approved at version 1", got.Approval)
	}

	// The settlement has run once it has indexed the decision block.
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "decision", "Which transport?")
	got := readApproval(t, handler, issue.PrimaryArtifactID)
	if got.Approval.State != "approved" || got.Approval.LatestVersion != 1 || got.Approval.Version == nil || *got.Approval.Version != 1 {
		t.Fatalf("approval after the settlement that indexed the block = %#v, want it still approved at version 1", got.Approval)
	}
}
