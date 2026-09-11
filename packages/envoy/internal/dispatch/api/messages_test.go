package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createIssueMessage(t *testing.T, handler http.Handler, issueKey string, input map[string]any, login string) model.Message {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", input, login)
	if response.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Message](t, response)
}

func TestCreateMessageRejectsMalformedMissingAndCrossIssueReplyTo(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message threading", "before")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "before")
	otherMessage := createIssueMessage(t, handler, other.Key, map[string]any{"body": "on the other issue"}, "alice")

	malformed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "reply", "reply_to": "not-a-uuid",
	}, "alice")
	if malformed.Code != http.StatusBadRequest || !strings.Contains(malformed.Body.String(), `"code":"INVALID_MESSAGE"`) {
		t.Fatalf("malformed reply_to: status=%d body=%s", malformed.Code, malformed.Body.String())
	}

	crossIssue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "reply", "reply_to": otherMessage.ID,
	}, "alice")
	if crossIssue.Code != http.StatusBadRequest || !strings.Contains(crossIssue.Body.String(), `"code":"INVALID_MESSAGE"`) {
		t.Fatalf("cross-issue reply_to: status=%d body=%s", crossIssue.Code, crossIssue.Body.String())
	}

	missing := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "reply", "reply_to": "5a660655-04ad-4ce0-8a9b-93dd03c412b7",
	}, "alice")
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), `"code":"INVALID_MESSAGE"`) {
		t.Fatalf("nonexistent reply_to: status=%d body=%s", missing.Code, missing.Body.String())
	}

	if rows := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice"); strings.Count(rows.Body.String(), `"type":"message.created"`) != 0 {
		t.Fatalf("rejected replies must not append events: %s", rows.Body.String())
	}
}

func TestCreateMessageReplyCarriesReplyBodyAndGetMessageReturnsChain(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message threading", "before")
	root := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Ship the build tonight"}, "alice")
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Sounds good", "reply_to": root.ID}, "bob")
	if reply.ReplyTo == nil || *reply.ReplyTo != root.ID {
		t.Fatalf("reply.ReplyTo = %v, want %s", reply.ReplyTo, root.ID)
	}

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", events.Code, events.Body.String())
	}
	if !strings.Contains(events.Body.String(), `"reply_body":"Ship the build tonight"`) {
		t.Fatalf("message.created event missing reply_body preview: %s", events.Body.String())
	}

	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+root.ID, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("get message: status=%d body=%s", read.Code, read.Body.String())
	}
	decoded := decodeBody[struct {
		Message model.Message   `json:"message"`
		Replies []model.Message `json:"replies"`
	}](t, read)
	if decoded.Message.ID != root.ID {
		t.Fatalf("get message id = %s, want %s", decoded.Message.ID, root.ID)
	}
	if len(decoded.Replies) != 1 || decoded.Replies[0].ID != reply.ID {
		t.Fatalf("get message replies = %#v, want [%s]", decoded.Replies, reply.ID)
	}

	notFound := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/5a660655-04ad-4ce0-8a9b-93dd03c412b7", nil, "alice")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("get missing message: status=%d body=%s", notFound.Code, notFound.Body.String())
	}

	otherIssue := createInteractionIssue(t, handler, "OTHER", "Different issue", "before")
	wrongIssue := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+otherIssue.Key+"/messages/"+root.ID, nil, "alice")
	if wrongIssue.Code != http.StatusNotFound {
		t.Fatalf("get message via wrong issue: status=%d body=%s", wrongIssue.Code, wrongIssue.Body.String())
	}
}

func TestMessageReferencingAnotherMessageCreatesAToKindMessageRef(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Message refs", "before")
	target := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "The decision"}, "alice")
	citing := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "See dispatch://" + issue.Key + "/message/" + target.ID,
	}, "bob")

	var toKind, toID string
	if err := database.Pool.QueryRow(context.Background(), `
		select to_kind, to_id from refs where from_kind = 'message' and from_id = $1
	`, citing.ID).Scan(&toKind, &toID); err != nil {
		t.Fatalf("read stored reference: %v", err)
	}
	if toKind != "message" || toID != target.ID {
		t.Fatalf("reference = (%q, %q), want (message, %s)", toKind, toID, target.ID)
	}
}
