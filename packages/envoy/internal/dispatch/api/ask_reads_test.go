package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type askIdentity struct {
	ID            string `json:"id"`
	OpenedEventID int64  `json:"opened_event_id"`
}

func TestAskReadsAttachCanonicalOpenedEventID(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask read identity", "A spec")
	openResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Keep open?",
		"options":  []map[string]string{{"label": "Yes"}},
		"actor":    sessionActor(),
	})
	if openResponse.Code != http.StatusCreated {
		t.Fatalf("create open ask: status=%d body=%s", openResponse.Code, openResponse.Body.String())
	}
	openAsk := decodeBody[askIdentity](t, openResponse)
	legacyResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Answer for fallback?",
		"options":  []map[string]string{{"label": "Yes"}},
		"actor":    sessionActor(),
	})
	if legacyResponse.Code != http.StatusCreated {
		t.Fatalf("create fallback ask: status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}
	legacyAsk := decodeBody[askIdentity](t, legacyResponse)
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+legacyAsk.ID+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer fallback ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	rows := decodeBody[[]struct {
		ID      int64 `json:"id"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
		Type string `json:"type"`
	}](t, events)
	var opened, answeredEvent int64
	for _, row := range rows {
		if row.Payload.ID != legacyAsk.ID {
			continue
		}
		if row.Type == "ask.opened" {
			opened = row.ID
		}
		if row.Type == "ask.answered" {
			answeredEvent = row.ID
		}
	}
	if opened == 0 || answeredEvent == 0 {
		t.Fatalf("missing fallback events: opened=%d answered=%d", opened, answeredEvent)
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from events where id = $1`, opened); err != nil {
		t.Fatalf("delete legacy opened event: %v", err)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+legacyAsk.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("get ask: status=%d body=%s", read.Code, read.Body.String())
	}
	readAsk := decodeBody[struct {
		Ask askIdentity `json:"ask"`
	}](t, read).Ask
	if readAsk.OpenedEventID != answeredEvent {
		t.Fatalf("get ask opened_event_id=%d, want legacy fallback %d", readAsk.OpenedEventID, answeredEvent)
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list asks: status=%d body=%s", listed.Code, listed.Body.String())
	}
	listedLegacy := false
	for _, ask := range decodeBody[[]askIdentity](t, listed) {
		if ask.ID == legacyAsk.ID {
			listedLegacy = true
			if ask.OpenedEventID != answeredEvent {
				t.Fatalf("listed ask opened_event_id=%d, want legacy fallback %d", ask.OpenedEventID, answeredEvent)
			}
		}
	}
	if !listedLegacy {
		t.Fatal("fallback ask missing from issue ask list")
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("get issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	detailOpen := false
	for _, ask := range decodeBody[struct {
		OpenAsks []askIdentity `json:"open_asks"`
	}](t, detail).OpenAsks {
		if ask.ID == openAsk.ID {
			detailOpen = true
			if ask.OpenedEventID != openAsk.OpenedEventID {
				t.Fatalf("issue open ask opened_event_id=%d, want %d", ask.OpenedEventID, openAsk.OpenedEventID)
			}
		}
	}
	if !detailOpen {
		t.Fatal("open ask missing from issue detail")
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("list inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	inboxOpen := false
	for _, ask := range decodeBody[[]askIdentity](t, inbox) {
		if ask.ID == openAsk.ID {
			inboxOpen = true
			if ask.OpenedEventID != openAsk.OpenedEventID {
				t.Fatalf("inbox ask opened_event_id=%d, want %d", ask.OpenedEventID, openAsk.OpenedEventID)
			}
		}
	}
	if !inboxOpen {
		t.Fatal("open ask missing from inbox")
	}
}

type openAsksRead struct {
	SessionID      string `json:"session_id"`
	AsOf           string `json:"as_of"`
	OpenedSince    bool   `json:"opened_since"`
	Count          int    `json:"count"`
	WaitingOnHuman int    `json:"waiting_on_human"`
	WaitingOnAgent int    `json:"waiting_on_agent"`
	Asks           []struct {
		ID           string `json:"id"`
		Question     string `json:"question"`
		HumanReplied bool   `json:"human_replied"`
		Priority     *int   `json:"priority"`
		WaitingOn    string `json:"waiting_on"`
		Owner        struct {
			Issue *struct {
				Key   string `json:"key"`
				Title string `json:"title"`
			} `json:"issue,omitempty"`
			Document *struct {
				Project string `json:"project"`
				Slug    string `json:"slug"`
				Name    string `json:"name"`
			} `json:"document,omitempty"`
		} `json:"owner"`
		LastReply *model.AskLastReply `json:"last_reply"`
	} `json:"asks"`
}

func listOpenAsks(t *testing.T, handler http.Handler, sessionID string, since string) openAsksRead {
	t.Helper()
	target := "/api/v1/asks/open?author_session=" + url.QueryEscape(sessionID)
	if since != "" {
		target += "&since=" + url.QueryEscape(since)
	}
	response := sessionRequest(t, handler, http.MethodGet, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list open asks: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[openAsksRead](t, response)
}

func TestListOpenAsksForSession(t *testing.T) {
	handler, _ := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Open-ask owner", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	const ownerSession = "session-open-asks"
	baseline := time.Now().UTC().Format(time.RFC3339Nano)

	createAsk := func(target, question, sessionID string) model.Ask {
		t.Helper()
		response := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
			"question": question,
			"actor":    map[string]any{"kind": "session", "id": sessionID},
		})
		if response.Code != http.StatusCreated {
			t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
		}
		return decodeBody[model.Ask](t, response)
	}

	if empty := listOpenAsks(t, handler, "session-with-no-asks", ""); empty.Count != 0 || len(empty.Asks) != 0 {
		t.Fatalf("empty open asks = %#v, want no asks", empty)
	}

	issueAsk := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Issue question", ownerSession)
	priorityIssueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "TEST", "title": "Priority owner", "priority": 0,
	}, "alice")
	if priorityIssueResponse.Code != http.StatusCreated {
		t.Fatalf("create priority issue: status=%d body=%s", priorityIssueResponse.Code, priorityIssueResponse.Body.String())
	}
	priorityIssue := decodeBody[model.Issue](t, priorityIssueResponse)
	priorityAsk := createAsk("/api/v1/issues/"+priorityIssue.Key+"/asks", "Priority question", ownerSession)
	documentAsk := createAsk("/api/v1/artifacts/"+document.ID+"/asks", "Document question", ownerSession)
	createAsk("/api/v1/issues/"+issue.Key+"/asks", "Other session", "session-other")
	answered := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Answered question", ownerSession)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered.ID+"/answer", map[string]any{
		"text": "Done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", response.Code, response.Body.String())
	}
	retracted := createAsk("/api/v1/issues/"+issue.Key+"/asks", "Retracted question", ownerSession)
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/asks/"+retracted.ID+"/resolve", map[string]any{
		"kind": "retracted", "reason": "No longer needed",
		"actor": map[string]any{"kind": "session", "id": ownerSession},
	}); response.Code != http.StatusOK {
		t.Fatalf("retract ask: status=%d body=%s", response.Code, response.Body.String())
	}

	humanReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Could you clarify?", "ask_id": issueAsk.ID,
	}, "alice")
	if humanReply.Code != http.StatusCreated {
		t.Fatalf("human reply: status=%d body=%s", humanReply.Code, humanReply.Body.String())
	}
	agentReply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Here is the clarification.", "ask_id": issueAsk.ID,
		"actor": map[string]any{"kind": "session", "id": ownerSession},
	})
	if agentReply.Code != http.StatusCreated {
		t.Fatalf("agent reply: status=%d body=%s", agentReply.Code, agentReply.Body.String())
	}
	documentReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body": "What is the rollout?", "ask_id": documentAsk.ID,
	}, "alice")
	if documentReply.Code != http.StatusCreated {
		t.Fatalf("document human reply: status=%d body=%s", documentReply.Code, documentReply.Body.String())
	}

	first := listOpenAsks(t, handler, ownerSession, baseline)
	if first.SessionID != ownerSession || first.AsOf == "" || !first.OpenedSince {
		t.Fatalf("open asks metadata = %#v, want session/as_of/opened_since", first)
	}
	if first.Count != 3 || first.WaitingOnHuman != 2 || first.WaitingOnAgent != 1 || len(first.Asks) != 3 {
		t.Fatalf("open ask counts = %#v, want three split between human and agent", first)
	}
	if first.Asks[0].ID != priorityAsk.ID || first.Asks[0].Priority == nil || *first.Asks[0].Priority != 0 {
		t.Fatalf("open asks first row = %#v, want the later P0 ask", first.Asks[0])
	}
	var gotIssue, gotDocument, gotPriority bool
	for _, ask := range first.Asks {
		switch ask.ID {
		case issueAsk.ID:
			gotIssue = ask.Question == "Issue question" &&
				ask.Owner.Issue != nil && ask.Owner.Issue.Key == issue.Key && ask.Owner.Issue.Title == "Open-ask owner" &&
				ask.Owner.Document == nil && ask.HumanReplied && ask.WaitingOn == "human" &&
				ask.LastReply != nil && ask.LastReply.Author.Kind == "session" && ask.LastReply.Author.ID == ownerSession
		case documentAsk.ID:
			gotDocument = ask.Question == "Document question" && ask.Owner.Issue == nil &&
				ask.Owner.Document != nil && ask.Owner.Document.Project == "TEST" && ask.Owner.Document.Slug == "runbook" &&
				ask.Owner.Document.Name == "Runbook" && ask.HumanReplied && ask.WaitingOn == "agent" &&
				ask.LastReply != nil && ask.LastReply.Author.Kind == "user" && ask.LastReply.Author.ID == "alice"
		case priorityAsk.ID:
			gotPriority = ask.Question == "Priority question" && ask.Priority != nil && *ask.Priority == 0 &&
				ask.Owner.Issue != nil && ask.Owner.Issue.Key == priorityIssue.Key && ask.WaitingOn == "human"
		}
	}
	if !gotIssue || !gotDocument || !gotPriority {
		t.Fatalf("open ask ownership/replies = %#v", first.Asks)
	}

	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "done",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", response.Code, response.Body.String())
	}
	closed := listOpenAsks(t, handler, ownerSession, "")
	if closed.Count != 2 || len(closed.Asks) != 2 || closed.Asks[0].ID != priorityAsk.ID || closed.Asks[1].ID != documentAsk.ID {
		t.Fatalf("closed issue open asks = %#v, want priority and document asks", closed)
	}
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{
		"status": "todo",
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("reopen issue: status=%d body=%s", response.Code, response.Body.String())
	}
	reopened := listOpenAsks(t, handler, ownerSession, "")
	if reopened.Count != 3 {
		t.Fatalf("reopened issue open asks = %#v, want all active asks", reopened)
	}
}

func TestListOpenAsksRejectsInvalidSelectors(t *testing.T) {
	handler := newTestHandler(t)
	for _, target := range []string{
		"/api/v1/asks/open",
		"/api/v1/asks/open?author_session=%20%20",
		"/api/v1/asks/open?author_session=session&since=not-a-timestamp",
	} {
		response := sessionRequest(t, handler, http.MethodGet, target, nil)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", target, response.Code, response.Body.String())
		}
	}
}

func TestTargetedReadsRejectNonUUIDIDsWithA400(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Short ids", "A spec")
	for _, test := range []struct {
		target string
		code   string
		text   string
	}{
		{target: "/api/v1/asks/7430fab3", code: "ASK_ID_INPUT", text: "ask id must be a full uuid"},
		{target: "/api/v1/comments/7430fab3", code: "COMMENT_ID_INPUT", text: "comment id must be a full uuid"},
		{target: "/api/v1/issues/" + issue.Key + "/messages/7430fab3", code: "MESSAGE_ID_INPUT", text: "message id must be a full uuid"},
	} {
		response := sessionRequest(t, handler, http.MethodGet, test.target, nil)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) || !strings.Contains(response.Body.String(), test.text) {
			t.Errorf("%s: status=%d body=%s, want 400 %s", test.target, response.Code, response.Body.String(), test.code)
		}
	}
}

// Every ask indexed from a document block names that document on the wire, however many
// asks the issue detail lists at once.
func TestIssueDetailAttachesTheBlockArtifactToEveryBlockAsk(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: 20 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Three decisions", strings.Join([]string{
		"Context\n",
		":::ask{#ask-one urgency=\"med\" multiple=\"false\"}\nShip one?\n:::\n",
		":::ask{#ask-two urgency=\"med\" multiple=\"false\"}\nShip two?\n:::\n",
		":::ask{#ask-three urgency=\"med\" multiple=\"false\"}\nShip three?\n:::\n",
	}, "\n"))
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-one", "Ship one?")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-two", "Ship two?")
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-three", "Ship three?")

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("get issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	openAsks := decodeBody[struct {
		OpenAsks []model.Ask `json:"open_asks"`
	}](t, detail).OpenAsks
	if len(openAsks) != 3 {
		t.Fatalf("issue detail lists %d open asks, want 3: %s", len(openAsks), detail.Body.String())
	}
	for _, ask := range openAsks {
		if ask.BlockID == nil || ask.BlockArtifact == nil {
			t.Fatalf("open ask %s: block_id=%v block_artifact=%v, want both", ask.ID, ask.BlockID, ask.BlockArtifact)
		}
		want := model.AskBlockArtifact{ID: issue.PrimaryArtifactID, Slug: "spec", Primary: true}
		if *ask.BlockArtifact != want {
			t.Fatalf("open ask %s block_artifact = %#v, want %#v", *ask.BlockID, *ask.BlockArtifact, want)
		}
	}
}
