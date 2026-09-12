package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createThreadComment(t *testing.T, handler http.Handler, issueKey string, input map[string]any, login string) model.Comment {
	t.Helper()
	var response *httptest.ResponseRecorder
	if login == "" {
		response = sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", input)
	} else {
		response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", input, login)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Comment](t, response)
}

func readThreadComment(t *testing.T, handler http.Handler, id string) model.Comment {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read comment: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, response).Comment
}

func commentEventTypes(t *testing.T, handler http.Handler, issueKey string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", response.Code, response.Body.String())
	}
	var events []struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(response.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func countCommentRows(t *testing.T, handler http.Handler, issueKey string) int {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/comments", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list comments: status=%d body=%s", response.Code, response.Body.String())
	}
	var comments []model.Comment
	if err := json.NewDecoder(response.Body).Decode(&comments); err != nil {
		t.Fatalf("decode comments: %v", err)
	}
	return len(comments)
}

func TestReopenCommentClearsResolutionAndReprojects(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Reopen comment", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "resolve this", "anchor": map[string]any{"artifact": "spec", "quote": "before"}, "actor": sessionActor(),
	}, "")

	resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/resolve", nil, "alice")
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, root.ID); projection["resolved"] != true {
		t.Fatalf("resolved mark projection = %#v, want resolved", projection)
	}

	forbidden := sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/reopen", map[string]any{"actor": sessionActor()})
	if forbidden.Code != http.StatusForbidden || !strings.Contains(forbidden.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("session reopen: status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	reopened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/reopen", nil, "bob")
	if reopened.Code != http.StatusOK {
		t.Fatalf("reopen comment: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	stored := readThreadComment(t, handler, root.ID)
	if stored.Resolved || stored.ResolvedBy != nil || stored.ResolvedAt != nil {
		t.Fatalf("reopened comment = %#v, want unresolved without resolution metadata", stored)
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, root.ID); projection["resolved"] != false {
		t.Fatalf("reopened mark projection = %#v, want unresolved", projection)
	}
	if !strings.Contains(strings.Join(commentEventTypes(t, handler, issue.Key), ","), "comment.reopened") {
		t.Fatalf("event log is missing comment.reopened")
	}

	reply := createThreadComment(t, handler, issue.Key, map[string]any{"body": "reply", "reply_to": root.ID}, "alice")
	invalid := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+reply.ID+"/reopen", nil, "bob")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"INVALID_COMMENT"`) || !strings.Contains(invalid.Body.String(), "reopen the thread root") {
		t.Fatalf("reopen reply: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestReplyToResolvedRootReopensIt(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Reply reopens root", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "root"}, "alice")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+root.ID+"/resolve", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("resolve root: status=%d body=%s", response.Code, response.Body.String())
	}

	reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "new reply", "reply_to": root.ID,
	}, "bob")
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply to resolved root: status=%d body=%s", reply.Code, reply.Body.String())
	}
	stored := readThreadComment(t, handler, root.ID)
	if stored.Resolved || stored.ResolvedBy != nil || stored.ResolvedAt != nil {
		t.Fatalf("root after reply = %#v, want unresolved without resolution metadata", stored)
	}
	types := strings.Join(commentEventTypes(t, handler, issue.Key), ",")
	if !strings.Contains(types, "comment.created") || !strings.Contains(types, "comment.reopened") {
		t.Fatalf("reply event log = %s, want comment.created and comment.reopened", types)
	}
}

func TestReplyToNestedCommentUsesThreadRoot(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Thread root replies", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "root"}, "alice")
	reply := createThreadComment(t, handler, issue.Key, map[string]any{"body": "first reply", "reply_to": root.ID}, "bob")

	nested := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "nested reply", "reply_to": reply.ID,
	}, "alice")
	if nested.Code != http.StatusCreated {
		t.Fatalf("reply to nested comment: status=%d body=%s", nested.Code, nested.Body.String())
	}
	created := decodeBody[model.Comment](t, nested)
	if created.ReplyTo == nil || *created.ReplyTo != root.ID {
		t.Fatalf("nested reply reply_to = %#v, want root %q", created.ReplyTo, root.ID)
	}
	if created.AskID != nil {
		t.Fatalf("nested reply ask_id = %#v, want nil", created.AskID)
	}
	if rows := countCommentRows(t, handler, issue.Key); rows != 3 {
		t.Fatalf("comments after nested reply = %d, want 3", rows)
	}
}

func TestReplyToAskClarificationContinuesAskThread(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask clarification", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)
	clarification := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "What does that change?", "ask_id": ask.ID,
	}, "alice")

	reply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "It changes the delivery contract.", "reply_to": clarification.ID, "actor": sessionActor(),
	})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply to clarification: status=%d body=%s", reply.Code, reply.Body.String())
	}
	created := decodeBody[model.Comment](t, reply)
	if created.AskID == nil || *created.AskID != ask.ID {
		t.Fatalf("clarification reply ask_id = %#v, want %q", created.AskID, ask.ID)
	}
	if created.ReplyTo != nil {
		t.Fatalf("clarification reply reply_to = %#v, want nil", created.ReplyTo)
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+ask.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read ask thread: status=%d body=%s", read.Code, read.Body.String())
	}
	thread := decodeBody[struct {
		Replies []model.Comment `json:"replies"`
	}](t, read)
	if len(thread.Replies) != 2 || thread.Replies[0].ID != clarification.ID || thread.Replies[1].ID != created.ID {
		t.Fatalf("ask replies = %#v, want clarification then reply", thread.Replies)
	}
}

func TestCreateCommentSeparatesMalformedAndWrongOwnerThreadIDs(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Thread IDs", "before")
	otherResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Other owner", "spec": "before",
	}, "alice")
	if otherResponse.Code != http.StatusCreated {
		t.Fatalf("create other issue: status=%d body=%s", otherResponse.Code, otherResponse.Body.String())
	}
	other := decodeBody[struct {
		Key string `json:"key"`
	}](t, otherResponse)
	foreignComment := createThreadComment(t, handler, other.Key, map[string]any{"body": "other"}, "alice")
	foreignAskResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+other.Key+"/asks", map[string]any{
		"question": "Other ask?", "actor": sessionActor(),
	})
	if foreignAskResponse.Code != http.StatusCreated {
		t.Fatalf("create foreign ask: status=%d body=%s", foreignAskResponse.Code, foreignAskResponse.Body.String())
	}
	foreignAsk := decodeBody[model.Ask](t, foreignAskResponse)

	cases := []struct {
		name    string
		input   map[string]any
		message string
	}{
		{
			name:    "short reply_to",
			input:   map[string]any{"body": "reply", "reply_to": "aabbccdd"},
			message: "reply_to must be a full comment id",
		},
		{
			name:    "wrong-owner reply_to",
			input:   map[string]any{"body": "reply", "reply_to": foreignComment.ID},
			message: "reply_to must identify a comment on this owner",
		},
		{
			name:    "short ask_id",
			input:   map[string]any{"body": "reply", "ask_id": "aabbccdd"},
			message: "ask_id must be a full ask id",
		},
		{
			name:    "wrong-owner ask_id",
			input:   map[string]any{"body": "reply", "ask_id": foreignAsk.ID},
			message: "ask_id must identify an ask on this owner",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", testCase.input, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_COMMENT"`) || !strings.Contains(response.Body.String(), testCase.message) {
				t.Fatalf("invalid thread id: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if rows := countCommentRows(t, handler, issue.Key); rows != 0 {
		t.Fatalf("comments after rejected thread ids = %d, want 0", rows)
	}
}

func TestCreateCommentRejectsReplySuggestions(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Reply suggestions", "before")
	root := createThreadComment(t, handler, issue.Key, map[string]any{"body": "root"}, "alice")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)

	for _, testCase := range []struct {
		name  string
		input map[string]any
	}{
		{
			name: "comment reply",
			input: map[string]any{
				"body": "suggestion", "reply_to": root.ID, "suggestion": map[string]any{"replace_with": "replacement"},
			},
		},
		{
			name: "ask reply",
			input: map[string]any{
				"body": "suggestion", "ask_id": ask.ID, "suggestion": map[string]any{"replace_with": "replacement"},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", testCase.input, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_COMMENT"`) || !strings.Contains(response.Body.String(), "replies cannot carry suggestions") {
				t.Fatalf("reply suggestion: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEditCommentIsAuthorOnly(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Edit comment", "before")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "before edit", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
	}, "alice")

	edited := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]string{"body": "after edit"}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("edit comment: status=%d body=%s", edited.Code, edited.Body.String())
	}
	updated := decodeBody[model.Comment](t, edited)
	if updated.Body != "after edit" || updated.EditedAt == nil {
		t.Fatalf("edited comment = %#v, want updated body and edited_at", updated)
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID); projection["text"] != "after edit" {
		t.Fatalf("edited mark projection = %#v, want updated text", projection)
	}
	if !strings.Contains(strings.Join(commentEventTypes(t, handler, issue.Key), ","), "comment.edited") {
		t.Fatalf("event log is missing comment.edited")
	}

	other := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]string{"body": "bob edit"}, "bob")
	if other.Code != http.StatusForbidden || !strings.Contains(other.Body.String(), `"code":"NOT_AUTHOR"`) {
		t.Fatalf("edit by another user: status=%d body=%s", other.Code, other.Body.String())
	}
	bearer := sessionRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]any{
		"body": "agent edit", "actor": sessionActor(),
	})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer edit: status=%d body=%s", bearer.Code, bearer.Body.String())
	}
	overCap := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID, map[string]string{
		"body": strings.Repeat("a", maxCommentBody16+1),
	}, "alice")
	if overCap.Code != http.StatusBadRequest || !strings.Contains(overCap.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("over-cap edit: status=%d body=%s", overCap.Code, overCap.Body.String())
	}
	stored := readThreadComment(t, handler, comment.ID)
	if stored.Body != "after edit" {
		t.Fatalf("comment after rejected edits = %q, want %q", stored.Body, "after edit")
	}
}

func TestResolveRecordsWhoAndWhen(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Resolution metadata", "before")
	started := time.Now().UTC().Add(-time.Minute)

	assertResolution := func(name string, comment model.Comment, actor model.Actor) {
		t.Helper()
		if !comment.Resolved || comment.ResolvedBy == nil || *comment.ResolvedBy != actor || comment.ResolvedAt == nil {
			t.Fatalf("%s result = %#v, want resolved by %#v with a timestamp", name, comment, actor)
		}
		resolvedAt, err := time.Parse(time.RFC3339Nano, *comment.ResolvedAt)
		if err != nil || resolvedAt.Before(started) || resolvedAt.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("%s resolved_at = %q (%v), want timestamp within the last minute", name, *comment.ResolvedAt, err)
		}
	}

	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "resolve", "actor": sessionActor()}, "")
	resolved := sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", map[string]any{"actor": sessionActor()})
	if resolved.Code != http.StatusOK {
		t.Fatalf("resolve comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	assertResolution("resolve", decodeBody[model.Comment](t, resolved), model.Actor{Kind: "session", ID: "session-0123456789abcdef"})

	acceptedSuggestion := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "accept", "anchor": map[string]any{"artifact": "spec", "quote": "before"}, "suggestion": map[string]string{"replace_with": "after"}, "actor": sessionActor(),
	}, "")
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+acceptedSuggestion.ID+"/accept", nil, "alice")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	assertResolution("accept", decodeBody[model.Comment](t, accepted), model.Actor{Kind: "user", ID: "alice"})

	rejectedSuggestion := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "reject", "suggestion": map[string]string{"replace_with": "ignored"}, "actor": sessionActor(),
	}, "")
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+rejectedSuggestion.ID+"/reject", nil, "bob")
	if rejected.Code != http.StatusOK {
		t.Fatalf("reject suggestion: status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	assertResolution("reject", decodeBody[model.Comment](t, rejected), model.Actor{Kind: "user", ID: "bob"})
}
