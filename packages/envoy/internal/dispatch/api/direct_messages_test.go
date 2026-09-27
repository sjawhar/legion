package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// directConversation is a human's direct message to s1 on a fresh handler whose fake listener
// delivers it, plus a way for s1 to answer that message's first delivery attempt.
func directConversation(t *testing.T) (http.Handler, model.Message, func(body string) *httptest.ResponseRecorder, *[]map[string]any) {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Where is the dashboard?", "delivery": "aside",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	reply := func(body string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply", map[string]any{
			"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": body,
		})
	}
	return handler, root, reply, &sent
}

// A session that answers a human's direct message and then has more to say posts that follow-up
// into the same conversation, threaded under its first reply, instead of being refused. A retry
// of either reply with the same text still posts nothing.
func TestSessionFollowUpToADirectMessageThreadsUnderItsFirstReply(t *testing.T) {
	handler, root, reply, sent := directConversation(t)
	sendsAfterRoot := len(*sent)

	firstResponse := reply("On it.")
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first reply: status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	first := decodeBody[model.Message](t, firstResponse)

	secondResponse := reply("Done: the dashboard is at /dash.")
	if secondResponse.Code != http.StatusCreated {
		t.Fatalf("follow-up reply: status=%d body=%s, want 201 with a new message", secondResponse.Code, secondResponse.Body.String())
	}
	second := decodeBody[model.Message](t, secondResponse)
	if second.ID == first.ID || second.Body != "Done: the dashboard is at /dash." ||
		second.InReplyTo == nil || *second.InReplyTo != first.ID ||
		second.IssueKey != nil || second.Target == nil || *second.Target != "session:s1" {
		t.Fatalf("follow-up = %#v, want an issue-less reply to %s in session:s1's conversation", second, first.ID)
	}

	if retried := reply("Done: the dashboard is at /dash."); retried.Code != http.StatusOK ||
		decodeBody[model.Message](t, retried).ID != second.ID {
		t.Fatalf("retried follow-up: status=%d body=%s, want 200 with %s", retried.Code, retried.Body.String(), second.ID)
	}
	if retried := reply("On it."); retried.Code != http.StatusOK || decodeBody[model.Message](t, retried).ID != first.ID {
		t.Fatalf("retried first reply: status=%d body=%s, want 200 with %s", retried.Code, retried.Body.String(), first.ID)
	}
	if len(*sent) != sendsAfterRoot {
		t.Fatalf("a session's own replies were delivered: %#v", (*sent)[sendsAfterRoot:])
	}

	conversation := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	if len(conversation) != 1 || conversation[0].Message.ID != root.ID {
		t.Fatalf("conversation = %#v, want the one direct message", conversation)
	}
	replies := conversation[0].Replies
	if len(replies) != 2 || replies[0].ID != first.ID || replies[1].ID != second.ID {
		t.Fatalf("replies = %#v, want [%s %s]", replies, first.ID, second.ID)
	}
	if attempt := conversation[0].Message.Deliveries[0]; attempt.ReplyID == nil || *attempt.ReplyID != first.ID {
		t.Fatalf("answered attempt = %#v, want it to keep naming the first reply", attempt)
	}
}

// The session that sent a reply to a human's direct message can read that conversation back by
// any message id in it, the same way a human reads it on the Agents page: the root and every
// reply, oldest first.
func TestGetMessageReadsTheConversationAMessageBelongsTo(t *testing.T) {
	handler, root, reply, _ := directConversation(t)
	first := decodeBody[model.Message](t, reply("On it."))
	followUp := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Thanks.", "delivery": "aside", "in_reply_to": first.ID,
	}, "alice")
	if followUp.Code != http.StatusCreated {
		t.Fatalf("human follow-up: status=%d body=%s", followUp.Code, followUp.Body.String())
	}
	human := decodeBody[model.Message](t, followUp)

	for _, id := range []string{root.ID, first.ID, human.ID} {
		read := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+id, nil)
		if read.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/messages/%s: status=%d body=%s", id, read.Code, read.Body.String())
		}
		thread := decodeBody[messageRead](t, read)
		if thread.Message.ID != root.ID || thread.Message.IssueKey != nil || len(thread.Message.Deliveries) != 1 {
			t.Fatalf("thread of %s starts at %#v, want the direct message %s with its delivery", id, thread.Message, root.ID)
		}
		if len(thread.Replies) != 2 || thread.Replies[0].ID != first.ID || thread.Replies[1].ID != human.ID ||
			len(thread.Replies[1].Deliveries) != 1 {
			t.Fatalf("thread of %s replies = %#v, want [%s %s] with the follow-up's delivery", id, thread.Replies, first.ID, human.ID)
		}
	}

	issue := createInteractionIssue(t, handler, "TEST", "Issue thread", "before")
	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "On the issue"}, "alice")
	if read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/"+onIssue.ID, nil, "alice"); read.Code != http.StatusOK ||
		decodeBody[messageRead](t, read).Message.ID != onIssue.ID {
		t.Fatalf("issue message by id: status=%d body=%s", read.Code, read.Body.String())
	}
	if missing := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/5a660655-04ad-4ce0-8a9b-93dd03c412b7", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("missing message: status=%d body=%s", missing.Code, missing.Body.String())
	}
	if malformed := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/7430fab3", nil); malformed.Code != http.StatusBadRequest ||
		!strings.Contains(malformed.Body.String(), `"code":"MESSAGE_ID_INPUT"`) {
		t.Fatalf("malformed message id: status=%d body=%s", malformed.Code, malformed.Body.String())
	}
}

// unreadAgentState is the part of GET /api/v1/me/agents/state a viewer's unread signal reads.
type unreadAgentState struct {
	ReadThrough   *string `json:"read_through"`
	UnreadReplies int     `json:"unread_replies"`
}

func unreadReplies(t *testing.T, handler http.Handler, login, sessionID string) unreadAgentState {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("%s agent state: status=%d body=%s", login, response.Code, response.Body.String())
	}
	return decodeBody[map[string]unreadAgentState](t, response)[sessionID]
}

// A session's reply to a human's direct message is unread for that human until they read it:
// the count is theirs alone, their own messages never count, and marking a conversation read
// never moves backwards.
func TestAgentRepliesToADirectMessageAreUnreadForItsSenderUntilRead(t *testing.T) {
	handler, _, reply, _ := directConversation(t)
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("before any reply: %#v, want nothing unread", got)
	}

	first := decodeBody[model.Message](t, reply("On it."))
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
		t.Fatalf("after the reply: %#v, want one unread reply", got)
	}
	if got := unreadReplies(t, handler, "bob", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("bob sees %#v for alice's conversation, want nothing unread", got)
	}

	read := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": first.CreatedAt,
	}, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", read.Code, read.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 || got.ReadThrough == nil ||
		!parseTimestamp(t, *got.ReadThrough).Equal(first.CreatedAt) {
		t.Fatalf("after reading: %#v, want nothing unread, read through %s", got, first.CreatedAt)
	}

	followUp := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Thanks.", "delivery": "aside", "in_reply_to": first.ID,
	}, "alice")
	if followUp.Code != http.StatusCreated {
		t.Fatalf("human follow-up: status=%d body=%s", followUp.Code, followUp.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("after alice's own follow-up: %#v, want nothing unread", got)
	}

	second := decodeBody[model.Message](t, reply("Done."))
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
		t.Fatalf("after the follow-up reply: %#v, want one unread reply", got)
	}
	dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{"read_through": second.CreatedAt}, "alice")
	stale := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{"read_through": first.CreatedAt}, "alice")
	if stale.Code != http.StatusOK {
		t.Fatalf("stale mark read: status=%d body=%s", stale.Code, stale.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 || got.ReadThrough == nil ||
		!parseTimestamp(t, *got.ReadThrough).Equal(second.CreatedAt) {
		t.Fatalf("after a stale mark: %#v, want it still read through %s", got, second.CreatedAt)
	}
}
