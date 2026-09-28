package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// directConversation is a human's direct message to s1 on a fresh handler whose fake listener
// delivers it, plus a way for s1 to answer that message's first delivery attempt the way
// dispatch_message does, asking to follow up once it has answered.
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
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply?follow_up=true", map[string]any{
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

// replyResponse is the reply route's answer: the message, and whether it was already there.
type replyResponse struct {
	model.Message
	Duplicate bool `json:"duplicate"`
}

// A follow-up is something the session asks for. Without ?follow_up=true an answered attempt
// answers with its stored reply and posts nothing, as the host's automatic BTW answer relies on,
// so a frame handed to the session twice cannot post a second answer. Whenever the route posts
// nothing and hands back a message the conversation already holds, it says so.
func TestAnsweredDeliveryPostsAFollowUpOnlyWhenAskedAndSaysWhenItPostedNothing(t *testing.T) {
	handler, root, reply, _ := directConversation(t)
	unflagged := func(body string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply", map[string]any{
			"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": body,
		})
	}
	answered := unflagged("Automatic answer.")
	if answered.Code != http.StatusCreated {
		t.Fatalf("first answer: status=%d body=%s", answered.Code, answered.Body.String())
	}
	first := decodeBody[replyResponse](t, answered)
	if first.Duplicate {
		t.Fatalf("first answer = %#v, want a fresh post", first)
	}

	again := unflagged("A second automatic answer to the same frame.")
	if got := decodeBody[replyResponse](t, again); again.Code != http.StatusOK || got.ID != first.ID || !got.Duplicate {
		t.Fatalf("unflagged second answer: status=%d %#v, want 200 with the stored answer marked duplicate", again.Code, got)
	}

	followed := reply("And a follow-up.")
	followUp := decodeBody[replyResponse](t, followed)
	if followed.Code != http.StatusCreated || followUp.Duplicate || followUp.InReplyTo == nil || *followUp.InReplyTo != first.ID {
		t.Fatalf("flagged follow-up: status=%d %#v, want 201 under %s", followed.Code, followUp, first.ID)
	}
	for body, want := range map[string]string{"Automatic answer.": first.ID, "And a follow-up.": followUp.ID} {
		resent := reply(body)
		if got := decodeBody[replyResponse](t, resent); resent.Code != http.StatusOK || got.ID != want || !got.Duplicate {
			t.Fatalf("resending %q: status=%d %#v, want 200 with %s marked duplicate", body, resent.Code, got, want)
		}
	}

	conversation := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	if replies := conversation[0].Replies; len(replies) != 2 || replies[0].ID != first.ID || replies[1].ID != followUp.ID {
		t.Fatalf("replies = %#v, want only the first answer and the one follow-up", replies)
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
		read := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+id+"?session=s1", nil)
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
	if missing := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/5a660655-04ad-4ce0-8a9b-93dd03c412b7?session=s1", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("missing message: status=%d body=%s", missing.Code, missing.Body.String())
	}
	if malformed := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/7430fab3?session=s1", nil); malformed.Code != http.StatusBadRequest ||
		!strings.Contains(malformed.Body.String(), `"code":"MESSAGE_ID_INPUT"`) {
		t.Fatalf("malformed message id: status=%d body=%s", malformed.Code, malformed.Body.String())
	}
}

// A direct message is between a human and one session: a bearer reads an issue-less conversation
// only as the session its root targets (or one that replied in it), so a session cannot read
// another session's direct conversation by mistake. The session is the caller's own claim, like
// `actor`, so this guards against accidents rather than authorizing. An issue thread follows the
// issue's rule, the same as GET /issues/{key}/messages/{id}.
func TestBearerReadsADirectConversationOnlyAsItsSessionAndAnIssueThreadAsTheIssueAllows(t *testing.T) {
	handler, root, reply, _ := directConversation(t)
	first := decodeBody[model.Message](t, reply("On it."))

	for _, id := range []string{root.ID, first.ID} {
		read := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+id+"?session=s2", nil)
		if read.Code != http.StatusForbidden || !strings.Contains(read.Body.String(), `"code":"THREAD_FORBIDDEN"`) ||
			strings.Contains(read.Body.String(), "Where is the dashboard?") || strings.Contains(read.Body.String(), "On it.") {
			t.Fatalf("s2 reading s1's direct conversation by %s: status=%d body=%s, want 403 THREAD_FORBIDDEN with no text", id, read.Code, read.Body.String())
		}
	}
	if unnamed := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+root.ID, nil); unnamed.Code != http.StatusBadRequest ||
		!strings.Contains(unnamed.Body.String(), `"code":"SESSION_REQUIRED"`) {
		t.Fatalf("bearer read naming no session: status=%d body=%s, want 400 SESSION_REQUIRED", unnamed.Code, unnamed.Body.String())
	}
	if human := dispatchRequest(t, handler, http.MethodGet, "/api/v1/messages/"+first.ID, nil, "bob"); human.Code != http.StatusOK ||
		decodeBody[messageRead](t, human).Message.ID != root.ID {
		t.Fatalf("a human reading the conversation: status=%d body=%s, want 200", human.Code, human.Body.String())
	}

	issue := createInteractionIssue(t, handler, "TEST", "Issue thread", "before")
	onIssue := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Who owns this?"}, "alice")
	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "I do.", "in_reply_to": onIssue.ID, "actor": map[string]any{"kind": "session", "id": "s2"},
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("s2 replying on the issue: status=%d body=%s", answered.Code, answered.Body.String())
	}
	viaIssue := bearerRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+onIssue.ID, nil)
	if viaIssue.Code != http.StatusOK {
		t.Fatalf("bearer reading the issue thread through the issue: status=%d body=%s", viaIssue.Code, viaIssue.Body.String())
	}
	reply2 := decodeBody[model.Message](t, answered)
	for _, query := range []string{"?session=s1", "?session=s2", ""} {
		read := bearerRequest(t, handler, http.MethodGet, "/api/v1/messages/"+reply2.ID+query, nil)
		if read.Code != http.StatusOK {
			t.Fatalf("bearer reading the issue thread by id with %q: status=%d body=%s, want 200 as the issue route answers", query, read.Code, read.Body.String())
		}
		if thread := decodeBody[messageRead](t, read); thread.Message.ID != onIssue.ID || len(thread.Replies) != 1 {
			t.Fatalf("bearer reading the issue thread by id with %q = %#v, want the issue thread with s2's reply", query, thread)
		}
	}
}

func unreadReplies(t *testing.T, handler http.Handler, login, sessionID string) userAgentState {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("%s agent state: status=%d body=%s", login, response.Code, response.Body.String())
	}
	return decodeBody[map[string]userAgentState](t, response)[sessionID]
}

// A session's reply to a human's direct message is unread for that human until they read it or
// clear the conversation: the count is theirs alone, their own messages never count, marking a
// conversation read never moves backwards, and a Clear keeps the read mark beside it.
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

	third := decodeBody[model.Message](t, reply("Also: the old URL redirects."))
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
		t.Fatalf("after a third reply: %#v, want one unread reply", got)
	}
	if cleared := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"cleared_before": third.CreatedAt,
	}, "alice"); cleared.Code != http.StatusOK {
		t.Fatalf("clear: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 || got.ClearedBefore == nil ||
		!parseTimestamp(t, *got.ClearedBefore).Equal(third.CreatedAt) || got.ReadThrough == nil ||
		!parseTimestamp(t, *got.ReadThrough).Equal(second.CreatedAt) {
		t.Fatalf("after a Clear: %#v, want nothing unread, cleared before %s, still read through %s", got, third.CreatedAt, second.CreatedAt)
	}
}

// A browser clock running fast stamps a read mark or a Clear slightly in the future. The route
// still accepts it, but stores it as no later than the server's now, so a reply that arrives in
// that gap still counts as unread instead of being hidden by a mark the viewer never read past.
func TestAFastBrowserClockNeitherHidesNorUncountsTheNextReply(t *testing.T) {
	for _, field := range []string{"read_through", "cleared_before"} {
		t.Run(field, func(t *testing.T) {
			handler, _, reply, _ := directConversation(t)
			ahead := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339Nano)
			marked := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{field: ahead}, "alice")
			if marked.Code != http.StatusOK {
				t.Fatalf("mark 30s ahead: status=%d body=%s", marked.Code, marked.Body.String())
			}
			reply("Arrived inside the gap.")
			if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
				t.Fatalf("after a reply inside the gap: %#v, want it counted unread", got)
			}
		})
	}
}

// A read mark or Clear is announced on the event stream with the login it belongs to, so the
// viewer's other open tabs and devices refresh their badge instead of waiting for a focus.
func TestAgentStateWriteAnnouncesItselfToTheViewersOtherDevices(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	marked := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": time.Now().UTC().Format(time.RFC3339Nano),
	}, "alice")
	if marked.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", marked.Code, marked.Body.String())
	}
	var login, session string
	if err := database.Pool.QueryRow(context.Background(), `
		select payload->>'login', payload->>'session_id' from events where type = 'user_agent_state.updated'
	`).Scan(&login, &session); err != nil {
		t.Fatalf("read the user_agent_state.updated event: %v", err)
	}
	if login != "alice" || session != "s1" {
		t.Fatalf("event names %s/%s, want alice/s1", login, session)
	}
}

// A session's conversations list newest activity first: a reply to an older direct message moves
// that conversation ahead of a newer one nobody has answered, so the one that last moved is the
// one the Agents page opens on and is never the one left outside the list's window.
func TestAgentConversationsListByLatestActivity(t *testing.T) {
	handler, root, reply, _ := directConversation(t)
	later := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "A newer question nobody has answered.", "delivery": "aside",
	}, "alice")
	if later.Code != http.StatusCreated {
		t.Fatalf("second direct message: status=%d body=%s", later.Code, later.Body.String())
	}
	newer := decodeBody[model.Message](t, later)
	reply("An answer to the older question.")

	conversation := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	if len(conversation) != 2 || conversation[0].Message.ID != root.ID || conversation[1].Message.ID != newer.ID {
		ids := []string{}
		for _, read := range conversation {
			ids = append(ids, read.Message.ID)
		}
		t.Fatalf("conversations = %v, want [%s (answered last) %s]", ids, root.ID, newer.ID)
	}
}
