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

// An approval request's question carries the requester's summary of what the version proposes,
// within the ask cap. A request after the document has moved on retracts the ask naming the older
// version and opens one at the latest.
func TestApprovalRequestSummaryAndARepeatAfterANewVersion(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
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

	// A repeat after a new version retracts that ask and opens one at the new version. This
	// server's version writes retract the ask themselves, so the version here is one an older
	// server wrote.
	writeVersionAsAnOlderServer(t, database, issue.PrimaryArtifactID)
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

// writeVersionAsAnOlderServer writes version 2 of the document straight into the table, as a
// server that predates the version writer's retraction wrote every version: an approval ask
// naming version 1 stays open, which no version this server writes leaves.
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

// An answer to an approval ask pins its review to the document's latest settled version, so an ask
// naming an older version is not answered: its question never named what the review would
// approve. This server retracts such an ask when it writes the version; one an older server left
// open is refused. The human reviews from the document header, or waits for a new request; the
// header's review retracts the old ask rather than citing it.
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
		if refused.Code != http.StatusConflict || !strings.Contains(body, `"code":"APPROVAL_ASK_STALE"`) || !strings.Contains(body, "version 1") || !strings.Contains(body, "version 2") || !strings.Contains(body, "document header") {
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

	// Approving from the document header reviews the version it shows. The ask naming version 1 is
	// retracted, with a reason naming version 2, and the review cites no ask.
	if header := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", map[string]any{"state": "approved"}, "alice"); header.Code != http.StatusCreated {
		t.Fatalf("header approve: status=%d body=%s", header.Code, header.Body.String())
	}
	if got := readApproval(t, handler, issue.PrimaryArtifactID); got.Approval.State != "approved" || got.Approval.Version == nil || *got.Approval.Version != 2 || got.Approval.AskID != nil {
		t.Fatalf("approval after the header approve = %#v, want approved at version 2 citing no ask", got.Approval)
	}
	retracted := decodeBody[struct {
		Ask struct {
			State      string `json:"state"`
			Resolution *struct {
				Kind   string `json:"kind"`
				Reason string `json:"reason"`
				Actor  struct {
					ID string `json:"id"`
				} `json:"actor"`
			} `json:"resolution"`
		} `json:"ask"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")).Ask
	if retracted.State != "resolved" || retracted.Resolution == nil || retracted.Resolution.Kind != "retracted" || !strings.Contains(retracted.Resolution.Reason, "version 2") || retracted.Resolution.Actor.ID != "alice" {
		t.Fatalf("the ask naming version 1 after the header approve = %#v, want retracted by alice with a reason naming version 2", retracted)
	}
}

// A new version of a document retracts an approval ask naming an older one in the same
// transaction, with a reason naming the new version, since no answer could review that ask any
// more; its followers learn from the ask.resolved that approval has to be requested again. Every
// path that writes a version does it, and a write that versions nothing leaves the ask open.
//
// The retraction is in the name of the version's writer only when the version credits exactly
// one. A version credits every writer whose change it carries: each peer connected when a browser
// edit arrives, since the room cannot tell which of them sent it, and every author an agent's
// write joins in one settlement window. Naming the first of several could name the asking
// session, and the outbox sends no event to its own actor, so the session would never hear that
// it has to request approval again; settlement retracts it instead, in its own name. The outbox
// runs over every case to show where the ask.resolved goes: a sole writer that is the asking
// session gets no copy of its own retraction.
func TestANewVersionRetractsTheApprovalAskNamingAnOlderOne(t *testing.T) {
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
				Identity: headerIdentity(database),
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
			State      string `json:"state"`
			Resolution *struct {
				Kind   string      `json:"kind"`
				Reason string      `json:"reason"`
				Actor  model.Actor `json:"actor"`
			} `json:"resolution"`
		} `json:"ask"`
	}

	for _, write := range []struct {
		name   string
		settle time.Duration
		write  func(t *testing.T, doc *document)
		// authors is how many writers version 2 credits, retractor who the ask's retraction is
		// recorded as, and askerHears whether the outbox sends its ask.resolved to the asking
		// session's own topic.
		authors    int
		retractor  model.Actor
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
			// retraction is settlement's, and it reaches subscribers once settlement commits.
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
						t.Fatal("the document service closed the subscription before settlement published the retraction")
					}
					if payload, isAsk := event.Payload.(model.AskEventPayload); isAsk && event.Type == "ask.resolved" && payload.ID == doc.askID {
						return
					}
				case <-deadline:
					t.Fatalf("settlement published no ask.resolved for %s", doc.askID)
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
			// The retraction commits in the transaction that writes the version.
			retracted := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask
			resolution := retracted.Resolution
			if retracted.State != "resolved" || resolution == nil || resolution.Kind != "retracted" || !resolution.Actor.SameAs(write.retractor) || !strings.Contains(resolution.Reason, "version 2") {
				t.Fatalf("the ask naming version 1 after %s is %s with resolution %+v, want retracted by %+v with a reason naming version 2", write.name, retracted.State, resolution, write.retractor)
			}
			if strings.HasPrefix(resolution.Reason, docs.SettlementRetractionReason) {
				t.Fatalf("retraction reason %q is one only settlement's removal of a block writes", resolution.Reason)
			}
			if got := readApproval(t, doc.handler, doc.artifactID); got.Approval.State != "draft" || got.Approval.LatestVersion != 2 || got.Approval.AskID != nil {
				t.Fatalf("approval after %s = %#v, want draft at version 2 with no ask open", write.name, got.Approval)
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
			resolved := 0
			for _, event := range logged {
				if event.Type == "ask.resolved" && event.Payload.ID == doc.askID {
					resolved++
					if !event.Actor.SameAs(write.retractor) {
						t.Fatalf("ask.resolved actor = %#v, want %#v", event.Actor, write.retractor)
					}
				}
			}
			if resolved != 1 {
				t.Fatalf("ask.resolved events for %s = %d, want 1", doc.askID, resolved)
			}

			destinations := askResolvedDestinations(t, doc.database, doc.documentService, doc.askID)
			if heard := slices.Contains(destinations, contracts.AgentSubject(asker.ID)); heard != write.askerHears {
				t.Fatalf("the outbox published the retraction to %v; the asking session's topic among them = %t, want %t", destinations, heard, write.askerHears)
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
		// reload; the retraction is then its own.
		doc := open(t, time.Hour)
		ctx := context.Background()
		tx, err := doc.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		retractions, err := docs.RetractStaleApprovalAsks(ctx, tx, events.NewBroker(), doc.artifactID, model.Version{Number: 2})
		if err != nil {
			t.Fatalf("retract with no known writer: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if len(retractions) != 1 || !retractions[0].Actor.SameAs(docs.SettlementActor) {
			t.Fatalf("retractions = %#v, want one ask.resolved by %#v", retractions, docs.SettlementActor)
		}
		retracted := decodeBody[askRead](t, dispatchRequest(t, doc.handler, http.MethodGet, "/api/v1/asks/"+doc.askID, nil, "alice")).Ask
		if retracted.State != "resolved" || retracted.Resolution == nil || !retracted.Resolution.Actor.SameAs(docs.SettlementActor) {
			t.Fatalf("the ask after a version no writer is credited with = %#v, want retracted by %s", retracted, docs.SettlementActor.ID)
		}
	})
}

// askResolvedDestinations runs the outbox over every committed event and returns the topics it
// published askID's ask.resolved to.
func askResolvedDestinations(t *testing.T, database *store.Store, documentService docs.API, askID string) []string {
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
			where type = 'ask.resolved' and payload->>'id' = $1 and published_at is not null
		`, askID).Scan(&destinations)
		if err == nil {
			return destinations
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("read the retraction's published destinations: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox never published the ask.resolved for %s", askID)
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
