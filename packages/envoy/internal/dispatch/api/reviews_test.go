package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
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

// An approval request remains one conversation across document versions. A version move keeps its
// ask, carries its existing summary forward, and pauses the card for the requesting agent. Handing
// the revision back updates the same card and makes the human its next actor again.
func TestApprovalRequestFollowsDocumentVersions(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Moving approval", "A spec")
	type approvalAsk struct {
		ID       string  `json:"id"`
		State    string  `json:"state"`
		Question string  `json:"question"`
		EditedAt *string `json:"edited_at"`
		Approval struct {
			Version          int `json:"version"`
			RequestedVersion int `json:"requested_version"`
		} `json:"approval"`
	}
	type requestResponse struct {
		Ask     approvalAsk `json:"ask"`
		Version int         `json:"version"`
	}
	var askID string

	request := func(summary string) *httptest.ResponseRecorder {
		return sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
			"actor": sessionActor(), "summary": summary,
		})
	}
	readAsk := func() approvalAsk {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read approval ask: status=%d body=%s", response.Code, response.Body.String())
		}
		return decodeBody[struct {
			Ask approvalAsk `json:"ask"`
		}](t, response).Ask
	}
	askEvents := func() map[string]int {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read events: status=%d body=%s", response.Code, response.Body.String())
		}
		var events []struct {
			Type    string `json:"type"`
			Payload struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
			t.Fatalf("decode events: %v", err)
		}
		counts := map[string]int{}
		for _, event := range events {
			if event.Payload.ID == askID {
				counts[event.Type]++
			}
		}
		return counts
	}

	first := request("Caps retries at three attempts.")
	if first.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", first.Code, first.Body.String())
	}
	opened := decodeBody[requestResponse](t, first).Ask
	askID = opened.ID
	if opened.Approval.Version != 1 || opened.Approval.RequestedVersion != 1 {
		t.Fatalf("opened approval = %#v, want requested version 1", opened)
	}

	if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "A revised spec", model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
		t.Fatalf("revise document: %v", err)
	}
	if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
	}
	moved := readAsk()
	if moved.ID != askID || moved.State != "open" || moved.Approval.Version != 2 || moved.Approval.RequestedVersion != 1 || moved.Question != "Approve spec.md (version 2)? Caps retries at three attempts." {
		t.Fatalf("moved approval = %#v, want the original ask open at version 2 and requested at version 1", moved)
	}
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"id":"`+askID+`"`) || !strings.Contains(inbox.Body.String(), `"waiting_on":"agent"`) {
		t.Fatalf("inbox after version move: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	if counts := askEvents(); counts["ask.opened"] != 1 || counts["ask.edited"] != 1 || counts["ask.resolved"] != 0 {
		t.Fatalf("ask events after move = %#v, want one open, one edit, and no resolution", counts)
	}

	returnedSummary := "Adds the rollback budget."
	returned := request(returnedSummary)
	if returned.Code != http.StatusOK {
		t.Fatalf("hand approval back: status=%d body=%s", returned.Code, returned.Body.String())
	}
	handedBack := decodeBody[requestResponse](t, returned).Ask
	if handedBack.ID != askID || handedBack.Approval.Version != 2 || handedBack.Approval.RequestedVersion != 2 || handedBack.Question != "Approve spec.md (version 2)? "+returnedSummary {
		t.Fatalf("handed-back approval = %#v, want the same ask at the requested version with its new summary", handedBack)
	}
	inbox = dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox?project=TEST", nil, "alice")
	if inbox.Code != http.StatusOK || !strings.Contains(inbox.Body.String(), `"id":"`+askID+`"`) || !strings.Contains(inbox.Body.String(), `"waiting_on":"human"`) {
		t.Fatalf("inbox after hand-back: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	if counts := askEvents(); counts["ask.opened"] != 1 || counts["ask.edited"] != 2 || counts["ask.resolved"] != 0 {
		t.Fatalf("ask events after hand-back = %#v, want one open, two edits, and no resolution", counts)
	}

	repeat := request(returnedSummary)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat hand-back: status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	if repeated := decodeBody[requestResponse](t, repeat).Ask; repeated.ID != askID || repeated.Question != handedBack.Question {
		t.Fatalf("repeat approval request = %#v, want the same unchanged ask", repeated)
	}
	if counts := askEvents(); counts["ask.opened"] != 1 || counts["ask.edited"] != 2 || counts["ask.resolved"] != 0 {
		t.Fatalf("ask events after repeat = %#v, want no additional event", counts)
	}

	approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"Approve"}, "expected_edited_at": handedBack.EditedAt,
	}, "alice")
	if approved.Code != http.StatusOK {
		t.Fatalf("approve moved request: status=%d body=%s", approved.Code, approved.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval == nil || got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.AskID == nil || *got.Approval.AskID != askID {
		t.Fatalf("approval after moved request = %#v, want v2 approved through %s", got.Approval, askID)
	}
}

func TestApprovalHandBackWaitsOnTheHuman(t *testing.T) {
	for _, flow := range []struct {
		name     string
		move     bool
		progress bool
	}{
		{name: "after a human comment and version move", move: true},
		{name: "after a human comment at the same version"},
		{name: "after a human comment and agent progress before a version move", move: true, progress: true},
	} {
		t.Run(flow.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "Hand-back turn", "A spec")
			request := func(summary string) *httptest.ResponseRecorder {
				return sessionRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
					map[string]any{"actor": sessionActor(), "summary": summary})
			}
			first := request("Names the existing proposal.")
			if first.Code != http.StatusCreated {
				t.Fatalf("request approval: status=%d body=%s", first.Code, first.Body.String())
			}
			askID := decodeBody[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, first).Ask.ID
			human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
				map[string]any{"ask_id": askID, "body": "Please clarify the rollout."}, "alice")
			if human.Code != http.StatusCreated {
				t.Fatalf("human comment: status=%d body=%s", human.Code, human.Body.String())
			}
			if flow.progress {
				progress := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
					map[string]any{
						"actor": sessionActor(), "ask_id": askID, "body": "Checking the rollout.",
						"turn": "agent",
					})
				if progress.Code != http.StatusCreated {
					t.Fatalf("agent progress: status=%d body=%s", progress.Code, progress.Body.String())
				}
			}
			if flow.move {
				if _, err := documentService.ReplaceText(
					context.Background(), issue.PrimaryArtifactID, "A revised spec",
					model.Actor{Kind: "session", ID: sessionActor()["id"].(string)},
				); err != nil {
					t.Fatalf("revise document: %v", err)
				}
				if named := dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
					map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
					t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
				}
			}
			handedBack := request("Clarifies the rollout.")
			if handedBack.Code != http.StatusOK {
				t.Fatalf("hand approval back: status=%d body=%s", handedBack.Code, handedBack.Body.String())
			}
			for _, read := range []struct {
				name string
				ask  turnAskRow
			}{
				{name: "ask detail", ask: readAskDetail(t, handler, askID)},
				{name: "Inbox", ask: readInboxRow(t, handler, askID)},
			} {
				if read.ask.WaitingOn != "human" {
					t.Fatalf("%s waiting_on = %q, want human", read.name, read.ask.WaitingOn)
				}
			}
			issueAsks := sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=open", nil)
			if issueAsks.Code != http.StatusOK {
				t.Fatalf("list issue asks: status=%d body=%s", issueAsks.Code, issueAsks.Body.String())
			}
			if asks := decodeBody[[]turnAskRow](t, issueAsks); len(asks) != 1 || asks[0].WaitingOn != "human" {
				t.Fatalf("issue asks = %#v, want the handed-back ask waiting on human", asks)
			}
			openAsks := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/open?project=TEST", nil)
			if openAsks.Code != http.StatusOK {
				t.Fatalf("list open asks: status=%d body=%s", openAsks.Code, openAsks.Body.String())
			}
			if response := decodeBody[struct {
				Asks []turnAskRow `json:"asks"`
			}](t, openAsks); len(response.Asks) != 1 || response.Asks[0].WaitingOn != "human" {
				t.Fatalf("open asks = %#v, want the handed-back ask waiting on human", response)
			}
		})
	}
}

func TestCommentOnMovedApprovalAskReportsItsDerivedWaitingOn(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Moved approval comment", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests",
		map[string]any{"actor": sessionActor(), "summary": "Names the initial proposal."})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	if _, err := documentService.ReplaceText(
		context.Background(), issue.PrimaryArtifactID, "A revised spec",
		model.Actor{Kind: "session", ID: sessionActor()["id"].(string)},
	); err != nil {
		t.Fatalf("revise document: %v", err)
	}
	if named := dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions",
		map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
		t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
	}
	comment := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"actor": sessionActor(), "ask_id": askID, "body": "The revision is ready."})
	if comment.Code != http.StatusCreated {
		t.Fatalf("comment on moved approval: status=%d body=%s", comment.Code, comment.Body.String())
	}
	if response := decodeBody[map[string]any](t, comment); response["waiting_on"] != "agent" {
		t.Fatalf("comment waiting_on = %#v, want agent", response["waiting_on"])
	}
	if event := latestCommentCreatedPayload(t, handler, issue.Key); event["ask_waiting_on"] != "agent" {
		t.Fatalf("comment.created ask_waiting_on = %#v, want agent", event["ask_waiting_on"])
	}
	if detail := readAskDetail(t, handler, askID); detail.WaitingOn != "agent" {
		t.Fatalf("ask detail waiting_on = %q, want agent", detail.WaitingOn)
	}
	if inbox := readInboxRow(t, handler, askID); inbox.WaitingOn != "agent" {
		t.Fatalf("Inbox waiting_on = %q, want agent", inbox.WaitingOn)
	}
}

// A human can still use the document header while the card has moved to a newer version but has
// not been handed back. The action answers that same open card and pins its review to the version
// the human saw, rather than resolving the card and losing its thread.
func TestHeaderReviewAnswersMovedApprovalAskBeforeTheAgentHandsItBack(t *testing.T) {
	for _, review := range []struct {
		name   string
		state  string
		reason string
		event  string
	}{
		{name: "approve", state: "approved", event: "artifact.approved"},
		{name: "request changes", state: "changes_requested", reason: "Name the rollback path.", event: "artifact.changes_requested"},
	} {
		t.Run(review.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "Header review", "A spec")
			requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
				"actor": sessionActor(), "summary": "Caps retries at three attempts.",
			})
			if requested.Code != http.StatusCreated {
				t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
			}
			askID := decodeBody[struct {
				Ask struct {
					ID string `json:"id"`
				} `json:"ask"`
			}](t, requested).Ask.ID
			if _, err := documentService.ReplaceText(context.Background(), issue.PrimaryArtifactID, "A revised spec", model.Actor{Kind: "session", ID: "session-0123456789abcdef"}); err != nil {
				t.Fatalf("revise document: %v", err)
			}
			if named := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
				t.Fatalf("name revised version: status=%d body=%s", named.Code, named.Body.String())
			}

			header := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{
				"state": review.state, "reason": review.reason,
			}, "alice")
			if header.Code != http.StatusCreated {
				t.Fatalf("review from header: status=%d body=%s", header.Code, header.Body.String())
			}
			ask := decodeBody[struct {
				Ask struct {
					State string `json:"state"`
				} `json:"ask"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
			if ask.State != "answered" {
				t.Fatalf("header review left moved ask %#v, want it answered", ask)
			}
			if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval == nil || got.Approval.State != review.state || got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.AskID == nil || *got.Approval.AskID != askID {
				t.Fatalf("header review approval = %#v, want %s at v2 through %s", got.Approval, review.state, askID)
			}
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
			var events []struct {
				Type    string `json:"type"`
				Payload struct {
					AskID *string `json:"ask_id"`
				} `json:"payload"`
			}
			if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
				t.Fatalf("decode events: %v", err)
			}
			var reviewAskID *string
			resolved := 0
			for _, event := range events {
				if event.Type == review.event {
					reviewAskID = event.Payload.AskID
				}
				if event.Type == "ask.resolved" {
					resolved++
				}
			}
			if reviewAskID == nil || *reviewAskID != askID || resolved != 0 {
				t.Fatalf("header review events = %#v, want %s through %s and no retraction", events, review.event, askID)
			}
		})
	}
}

// An approval request's question carries the requester's summary of what the version proposes,
// within the ask cap. Repeating the request updates the existing row and never opens another.
func TestApprovalRequestSummaryAndARepeatAfterANewVersion(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	type approvalAsk struct {
		ID       string  `json:"id"`
		State    string  `json:"state"`
		Question string  `json:"question"`
		EditedAt *string `json:"edited_at"`
		Approval struct {
			Version          int `json:"version"`
			RequestedVersion int `json:"requested_version"`
		} `json:"approval"`
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

	// A repeat at the same version updates the one open request with its new summary.
	other := "A different summary."
	repeat := request(issue.PrimaryArtifactID, &other)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat at the same version: status=%d body=%s", repeat.Code, repeat.Body.String())
	}
	updated := decodeBody[requestResponse](t, repeat).Ask
	if updated.ID != opened.ID || updated.Question != "Approve spec.md (version 1)? "+other || updated.Approval.RequestedVersion != 1 {
		t.Fatalf("repeat at the same version = %#v, want ask %s updated with the new summary", updated, opened.ID)
	}
	if refused := request(issue.PrimaryArtifactID, &over); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("summary over the cap on a repeat at the same version: status=%d body=%s", refused.Code, refused.Body.String())
	}

	// A legacy server can leave an old version on an open request. The next hand-back adopts the
	// same row at the latest version rather than retracting it and opening another.
	writeVersionAsAnOlderServer(t, database, issue.PrimaryArtifactID)
	revised := "Adds the retry budget."
	second := request(issue.PrimaryArtifactID, &revised)
	if second.Code != http.StatusOK {
		t.Fatalf("hand back after a legacy version: status=%d body=%s", second.Code, second.Body.String())
	}
	secondResponse := decodeBody[requestResponse](t, second)
	handedBack := secondResponse.Ask
	if handedBack.ID != opened.ID || secondResponse.Version != 2 || handedBack.Approval.Version != 2 || handedBack.Approval.RequestedVersion != 2 || handedBack.Question != "Approve spec.md (version 2)? Adds the retry budget." {
		t.Fatalf("approval ask after a legacy version = %#v", handedBack)
	}
	persisted := decodeBody[requestResponse](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+opened.ID, nil, "alice")).Ask
	if persisted.State != "open" || persisted.ID != opened.ID {
		t.Fatalf("adopted approval ask = %#v, want the original open row", persisted)
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "awaiting" || got.Approval.AskID == nil || *got.Approval.AskID != opened.ID {
		t.Fatalf("approval after the hand-back = %#v, want awaiting on ask %s", got.Approval, opened.ID)
	}
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
	openedEvents, editedEvents, resolvedEvents := 0, 0, 0
	for _, event := range events {
		if event.Payload.ID != opened.ID {
			continue
		}
		switch event.Type {
		case "ask.opened":
			openedEvents++
		case "ask.edited":
			editedEvents++
		case "ask.resolved":
			resolvedEvents++
		}
	}
	if openedEvents != 1 || editedEvents != 2 || resolvedEvents != 0 {
		t.Fatalf("approval events: opened=%d edited=%d resolved=%d, want one row with two edits", openedEvents, editedEvents, resolvedEvents)
	}

	// Once the latest version is approved a request opens nothing, and its summary is still checked.
	if approved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+handedBack.ID+"/answer", map[string]any{
		"selected": []string{"Approve"}, "expected_edited_at": handedBack.EditedAt,
	}, "alice"); approved.Code != http.StatusOK {
		t.Fatalf("approve version 2: status=%d body=%s", approved.Code, approved.Body.String())
	}
	if refused := request(issue.PrimaryArtifactID, &over); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("summary over the cap on an approved document: status=%d body=%s", refused.Code, refused.Body.String())
	}
	if satisfied := request(issue.PrimaryArtifactID, &revised); satisfied.Code != http.StatusOK || !strings.Contains(satisfied.Body.String(), `"ask":null`) || !strings.Contains(satisfied.Body.String(), `"state":"approved"`) {
		t.Fatalf("request on an approved document: status=%d body=%s", satisfied.Code, satisfied.Body.String())
	}
}

// writeVersionAsAnOlderServer writes version 2 straight into the table, modeling a deployment
// that predates moving approval requests with document versions.
func writeVersionAsAnOlderServer(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, max(number) + 1, 'A revised spec', '[{"kind":"user","id":"alice"}]'
		from artifact_versions where artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("write a version as an older server: %v", err)
	}
}

// A card that an older server left at a version the document no longer has is a true race: the
// human reloads it rather than approving wording that no longer describes the review. The header
// can still review the latest version and absorbs that legacy card into the review record.
func TestAnsweringAnApprovalAskThatNamesAnOlderVersionIsRefused(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Stale approval", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor":   sessionActor(),
		"summary": "Caps retries at three attempts.",
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID

	// The document moves on to version 2 on an older server, and nobody asks again.
	writeVersionAsAnOlderServer(t, database, issue.PrimaryArtifactID)
	for _, answer := range []map[string]any{
		{"selected": []string{"Approve"}},
		{"selected": []string{"Request changes"}, "text": "Keep the old budget."},
	} {
		refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", answer, "alice")
		body := refused.Body.String()
		if refused.Code != http.StatusConflict || !strings.Contains(body, `"code":"APPROVAL_ASK_STALE"`) || !strings.Contains(body, "document changed since this request was shown") || !strings.Contains(body, "reload it and answer again") {
			t.Fatalf("answer %v to the ask naming version 1: status=%d body=%s", answer["selected"], refused.Code, body)
		}
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State == "approved" || got.Approval.Version != nil {
		t.Fatalf("approval after the refused answers = %#v, want no review", got.Approval)
	}
	stillOpen := decodeBody[struct {
		Ask struct {
			State string `json:"state"`
		} `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
	if stillOpen.State != "open" {
		t.Fatalf("ask after the refused answers = %#v, want open", stillOpen)
	}

	// Approving from the document header reviews the latest version and answers the legacy card
	// after moving its metadata forward, so the review retains that request's thread.
	if header := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "approved"}, "alice"); header.Code != http.StatusCreated {
		t.Fatalf("header approve: status=%d body=%s", header.Code, header.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.AskID == nil || *got.Approval.AskID != askID {
		t.Fatalf("approval after the header approve = %#v, want approved at version 2 through %s", got.Approval, askID)
	}
	answered := decodeBody[struct {
		Ask struct {
			State string `json:"state"`
		} `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
	if answered.State != "answered" {
		t.Fatalf("the legacy ask after the header approve = %#v, want answered", answered)
	}
}

// Every path that writes a document version moves its open approval request to that version
// without creating or resolving an Inbox row. The moved request waits on its agent, and its
// ask.edited event reaches followers except the actor who made the version. A write that versions
// nothing leaves the request at the version the human was already shown.
//
// The move is attributed to the version's sole writer. A version credits every writer whose
// change it carries, so a browser edit with several peers or a settlement combining agents uses
// SettlementActor: choosing one could suppress the only delivery the requesting session receives.
func TestANewVersionMovesTheOpenApprovalAsk(t *testing.T) {
	asker := model.Actor{Kind: "session", ID: sessionActor()["id"].(string)}
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	humanPeer := http.Header{"X-Dispatch-User": []string{"bob"}}
	askerPeer := http.Header{"Authorization": []string{"Bearer agent-token"}, "X-Dispatch-Actor": []string{`{"kind":"session","id":"` + asker.ID + `"}`}}
	edit := func(t *testing.T, handler http.Handler, artifactID string, body map[string]any) {
		t.Helper()
		body["ops"] = []map[string]string{{"op": "replace", "find": "A spec", "with": "A revised spec"}}
		body["actor"] = sessionActor()
		if edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", body); edited.Code != http.StatusOK {
			t.Fatalf("edit the document: status=%d body=%s", edited.Code, edited.Body.String())
		}
	}
	type document struct {
		handler         http.Handler
		database        *store.Store
		documentService *docs.Service
		broker          *events.Broker
		issueKey        string
		artifactID      string
		askID           string
		sockets         *servedSockets
		wsURL           string
		peers           []*syncedPeer
	}
	open := func(t *testing.T, settle time.Duration) *document {
		t.Helper()
		doc := &document{broker: events.NewBroker()}
		doc.handler, doc.database = newInteractionHandler(t, func(database *store.Store) docs.API {
			doc.documentService = docs.New(docs.Deps{
				Store: database, Events: doc.broker, Settle: settle, AgentToken: "agent-token",
				Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"bob": {}}},
			})
			t.Cleanup(func() { _ = doc.documentService.Shutdown(context.Background()) })
			return doc.documentService
		})
		issue := createInteractionIssue(t, doc.handler, "TEST", "Retracted approval", "A spec")
		doc.issueKey, doc.artifactID = issue.Key, issue.PrimaryArtifactID
		requested := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/approval-requests", map[string]any{
			"actor": sessionActor(), "summary": "Caps retries at three attempts.",
		})
		if requested.Code != http.StatusCreated {
			t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
		}
		doc.askID = decodeBody[struct {
			Ask struct {
				ID string `json:"id"`
			} `json:"ask"`
		}](t, requested).Ask.ID
		doc.sockets = &servedSockets{finished: make(map[string]chan struct{})}
		server := httptest.NewServer(doc.sockets.serve(doc.documentService.ServeHTTP))
		t.Cleanup(server.Close)
		doc.wsURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + doc.artifactID
		return doc
	}
	connect := func(t *testing.T, doc *document, headers http.Header) *syncedPeer {
		t.Helper()
		peer := &syncedPeer{wsURL: doc.wsURL, sockets: doc.sockets, headers: headers, artifactID: doc.artifactID, doc: crdt.New()}
		peer.connect(t)
		t.Cleanup(peer.close)
		doc.peers = append(doc.peers, peer)
		return peer
	}
	// typeNote types a paragraph as peer and returns once the room holds it, so the peers the room
	// credits with it are the ones connected now.
	typeNote := func(t *testing.T, peer *syncedPeer, text string) {
		t.Helper()
		peer.appendParagraph(t, text)
		peer.barrier(t)
	}
	type askRead struct {
		Ask struct {
			State    string `json:"state"`
			Approval struct {
				Version          int `json:"version"`
				RequestedVersion int `json:"requested_version"`
			} `json:"approval"`
		} `json:"ask"`
	}

	for _, write := range []struct {
		name   string
		settle time.Duration
		write  func(t *testing.T, doc *document)
		// authors is how many writers version 2 credits, editor is who moves the ask, and
		// askerHears says whether the outbox sends its ask.edited to the asking session.
		authors    int
		editor     model.Actor
		askerHears bool
	}{
		{"an edit", time.Hour, func(t *testing.T, doc *document) {
			edit(t, doc.handler, doc.artifactID, map[string]any{"summary": "revise"})
		}, 1, asker, false},
		{"an edit with no summary", time.Hour, func(t *testing.T, doc *document) {
			edit(t, doc.handler, doc.artifactID, map[string]any{})
		}, 1, asker, false},
		{"a live write settlement versions", 20 * time.Millisecond, func(t *testing.T, doc *document) {
			// A live write that joins no transaction is versioned by settlement alone, so this
			// movement is settlement's and reaches followers once settlement commits.
			published, stop := doc.broker.Subscribe()
			defer stop()
			if _, err := doc.documentService.ReplaceText(context.Background(), doc.artifactID, "A revised spec", alice); err != nil {
				t.Fatalf("revise document: %v", err)
			}
			waitForArtifactVersion(t, doc.handler, doc.artifactID, 2)
			deadline := time.After(5 * time.Second)
			for {
				select {
				case event, ok := <-published:
					if !ok {
						t.Fatal("the document service closed the subscription before settlement published the moved approval")
					}
					if payload, isAsk := event.Payload.(model.AskEditEventPayload); isAsk && event.Type == "ask.edited" && payload.ID == doc.askID {
						return
					}
				case <-deadline:
					t.Fatalf("settlement published no ask.edited for %s", doc.askID)
				}
			}
		}, 1, alice, true},
		{"a named version", time.Hour, func(t *testing.T, doc *document) {
			if _, err := doc.documentService.ReplaceText(context.Background(), doc.artifactID, "A revised spec", alice); err != nil {
				t.Fatalf("revise document: %v", err)
			}
			if named := dispatchRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/versions", map[string]string{"summary": "revised"}, "alice"); named.Code != http.StatusCreated {
				t.Fatalf("name a version: status=%d body=%s", named.Code, named.Body.String())
			}
		}, 1, alice, true},
		{"an upload", time.Hour, func(t *testing.T, doc *document) {
			if uploaded := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/issues/"+doc.issueKey+"/artifacts", map[string]any{
				"actor": sessionActor(), "name": "spec.md", "content": "A revised spec",
			}); uploaded.Code != http.StatusCreated {
				t.Fatalf("upload a version: status=%d body=%s", uploaded.Code, uploaded.Body.String())
			}
		}, 1, asker, false},
		{"two connected peers where the second types", time.Hour, func(t *testing.T, doc *document) {
			connect(t, doc, askerPeer)
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
		}, 2, docs.SettlementActor, true},
		{"an agent's joined write and a human's typing in one settlement window", time.Hour, func(t *testing.T, doc *document) {
			typist := connect(t, doc, humanPeer)
			ctx := context.Background()
			tx, err := doc.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the agent's write: %v", err)
			}
			defer tx.Rollback(ctx)
			joined, ledger := doc.documentService.Join(ctx, tx)
			defer ledger.Discard()
			if _, err := doc.documentService.ReplaceText(joined, doc.artifactID, "A revised spec", asker); err != nil {
				t.Fatalf("the agent's joined write: %v", err)
			}
			if err := ledger.Commit(ctx); err != nil {
				t.Fatalf("commit the agent's joined write: %v", err)
			}
			waitForLiveText(t, doc.documentService, doc.artifactID, "A revised spec")
			typist.barrier(t)
			typeNote(t, typist, "A note from bob.")
		}, 2, docs.SettlementActor, true},
		{"a human's typing and then an agent's edit that versions both", time.Hour, func(t *testing.T, doc *document) {
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
			edit(t, doc.handler, doc.artifactID, map[string]any{})
		}, 2, docs.SettlementActor, true},
		{"the only connected peer types", time.Hour, func(t *testing.T, doc *document) {
			typeNote(t, connect(t, doc, humanPeer), "A note from bob.")
		}, 1, bob, true},
		{"the asking session alone types over its connection", time.Hour, func(t *testing.T, doc *document) {
			typeNote(t, connect(t, doc, askerPeer), "A note from the asker.")
		}, 1, asker, false},
	} {
		t.Run(write.name, func(t *testing.T) {
			doc := open(t, write.settle)
			write.write(t, doc)
			// The last peer to leave settles the room, which versions whatever the peers wrote.
			for _, peer := range doc.peers {
				peer.close()
			}
			waitForArtifactVersion(t, doc.handler, doc.artifactID, 2)

			version := decodeBody[struct {
				Authors []model.Actor `json:"authors"`
			}](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/artifacts/"+doc.artifactID+"/versions/2", nil, "alice"))
			if len(version.Authors) != write.authors {
				t.Fatalf("version 2 authors = %#v, want %d", version.Authors, write.authors)
			}
			// The move commits in the transaction that writes the version.
			moved := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask
			if moved.State != "open" || moved.Approval.Version != 2 || moved.Approval.RequestedVersion != 1 {
				t.Fatalf("the ask after %s = %#v, want the same open request at version 2 waiting for its agent", write.name, moved)
			}
			if got := readApproval(t, doc.handler, doc.artifactID); got.Approval.State != "awaiting" || got.Approval.LatestVersion != 2 || got.Approval.AskID == nil || *got.Approval.AskID != doc.askID {
				t.Fatalf("approval after %s = %#v, want awaiting on the moved ask %s", write.name, got.Approval, doc.askID)
			}
			var logged []struct {
				Type    string      `json:"type"`
				Actor   model.Actor `json:"actor"`
				Payload struct {
					ID string `json:"id"`
				} `json:"payload"`
			}
			if err := json.NewDecoder(dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/issues/"+doc.issueKey+"/events", nil, "alice").Body).Decode(&logged); err != nil {
				t.Fatalf("decode events: %v", err)
			}
			edited, resolved := 0, 0
			for _, event := range logged {
				if event.Payload.ID != doc.askID {
					continue
				}
				switch event.Type {
				case "ask.edited":
					edited++
					if !event.Actor.SameAs(write.editor) {
						t.Fatalf("ask.edited actor = %#v, want %#v", event.Actor, write.editor)
					}
				case "ask.resolved":
					resolved++
				}
			}
			if edited != 1 || resolved != 0 {
				t.Fatalf("approval events for %s: edits=%d resolved=%d, want one move and no resolution", doc.askID, edited, resolved)
			}

			destinations := askEditDestinations(t, doc.database, doc.documentService, doc.askID)
			if heard := slices.Contains(destinations, contracts.AgentSubject(asker.ID)); heard != write.askerHears {
				t.Fatalf("the outbox published the move to %v; the asking session's topic among them = %t, want %t", destinations, heard, write.askerHears)
			}
		})
	}

	t.Run("a write that versions nothing", func(t *testing.T) {
		doc := open(t, time.Hour)
		unchanged := sessionRequest(t, doc.handler, http.MethodPost, "/api/v1/artifacts/"+doc.artifactID+"/edits", map[string]any{
			"ops":     []map[string]string{{"op": "replace", "find": "A spec", "with": "A spec"}},
			"summary": "nothing", "actor": sessionActor(),
		})
		if unchanged.Code != http.StatusOK || !strings.Contains(unchanged.Body.String(), `"changed":false`) {
			t.Fatalf("an edit that changes nothing: status=%d body=%s", unchanged.Code, unchanged.Body.String())
		}
		if still := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask; still.State != "open" {
			t.Fatalf("the ask at the latest version after an edit that versions nothing = %#v, want open", still)
		}
		if got := readApproval(t, doc.handler, doc.artifactID); got.Approval.State != "awaiting" || got.Approval.LatestVersion != 1 || got.Approval.AskID == nil || *got.Approval.AskID != doc.askID {
			t.Fatalf("approval after an edit that versions nothing = %#v, want awaiting on ask %s", got.Approval, doc.askID)
		}
	})

	t.Run("a version no writer is credited with", func(t *testing.T) {
		// Settlement can version a change it credits to nobody, such as one written before a room
		// reload; it is the actor of the moved approval event.
		doc := open(t, time.Hour)
		ctx := context.Background()
		tx, err := doc.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		moved, err := docs.MoveApprovalAsks(ctx, tx, events.NewBroker(), doc.artifactID, model.Version{Number: 2}, "")
		if err != nil {
			t.Fatalf("move with no known writer: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if len(moved) != 1 || !moved[0].Actor.SameAs(docs.SettlementActor) {
			t.Fatalf("moved events = %#v, want one ask.edited by %#v", moved, docs.SettlementActor)
		}
		ask := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask
		if ask.State != "open" || ask.Approval.Version != 2 || ask.Approval.RequestedVersion != 1 {
			t.Fatalf("the ask after a version no writer is credited with = %#v, want an open moved ask", ask)
		}
	})
}

// askEditDestinations runs the outbox over every committed event and returns the topics it
// published askID's version-moving ask.edited to.
func askEditDestinations(t *testing.T, database *store.Store, documentService docs.API, askID string) []string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.Run(ctx, outbox.Deps{Store: database, Publisher: acceptingPublisher{}, Broker: events.NewBroker(), Docs: documentService})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var destinations []string
		err := database.Pool.QueryRow(context.Background(), `
			select published_destinations from events
			where type = 'ask.edited' and payload->>'id' = $1 and published_at is not null
		`, askID).Scan(&destinations)
		if err == nil {
			return destinations
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read the moved approval's published destinations: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox never published ask.edited for %s", askID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// acceptingPublisher takes every envelope the outbox publishes; the outbox records each topic on
// its event.
type acceptingPublisher struct{}

func (acceptingPublisher) Publish(contracts.Envelope) error { return nil }

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
