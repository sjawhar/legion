package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// directConversation is a human's direct message to s1 on a fresh handler whose fake listener
// delivers it, plus a way for s1 to answer that message's first delivery attempt the way
// dispatch_message does, asking to follow up once it has answered.
func directConversation(t *testing.T) (http.Handler, model.Message, func(body string) *httptest.ResponseRecorder, *[]map[string]any) {
	t.Helper()
	handler, _, root, reply, sent := directConversationFrom(t, "alice")
	return handler, root, reply, sent
}

// directConversationFrom is directConversation with the message sent by login, spelled as the
// identity source spells it.
func directConversationFrom(t *testing.T, login string) (http.Handler, *store.Store, model.Message, func(body string) *httptest.ResponseRecorder, *[]map[string]any) {
	t.Helper()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Where is the dashboard?", "delivery": "aside",
	}, login)
	if created.Code != http.StatusCreated {
		t.Fatalf("direct message: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[model.Message](t, created)
	reply := func(body string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply?follow_up=true", map[string]any{
			"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": body,
		})
	}
	return handler, database, root, reply, &sent
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

// follow_up is true, false or absent. Anything else is refused rather than read as a retry, so a
// caller's typo is not answered as "already answered" with nothing posted.
func TestReplyFollowUpIsTrueFalseOrAbsent(t *testing.T) {
	handler, root, reply, _ := directConversation(t)
	first := decodeBody[replyResponse](t, reply("On it."))
	answer := func(query string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+root.ID+"/reply"+query, map[string]any{
			"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 1, "body": "More to say.",
		})
	}
	for _, value := range []string{"1", "TRUE", "yes", ""} {
		if got := answer("?follow_up=" + value); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "follow_up") {
			t.Fatalf("follow_up=%q: status=%d body=%s, want 400 naming follow_up", value, got.Code, got.Body.String())
		}
	}
	retried := answer("?follow_up=false")
	if got := decodeBody[replyResponse](t, retried); retried.Code != http.StatusOK || got.ID != first.ID || !got.Duplicate {
		t.Fatalf("follow_up=false: status=%d %#v, want the stored answer marked duplicate", retried.Code, got)
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

// A viewer is one person however their identity source spells their login: the replies to the
// direct messages they sent count unread under any casing of it, and a read mark written under
// one casing clears them under another.
func TestAViewersRepliesCountAndClearWhateverTheCasingOfTheirLogin(t *testing.T) {
	handler, _, _, reply, _ := directConversationFrom(t, "Alice")
	first := decodeBody[model.Message](t, reply("On it."))
	for _, login := range []string{"Alice", "alice", "ALICE"} {
		if got := unreadReplies(t, handler, login, "s1"); got.UnreadReplies != 1 {
			t.Fatalf("%s reads %#v, want the reply to Alice's message unread", login, got)
		}
	}
	marked := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": first.CreatedAt,
	}, "aLiCe")
	if marked.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", marked.Code, marked.Body.String())
	}
	for _, login := range []string{"Alice", "alice"} {
		if got := unreadReplies(t, handler, login, "s1"); got.UnreadReplies != 0 || got.ReadThrough == nil {
			t.Fatalf("%s after aLiCe read it: %#v, want nothing unread and the read mark", login, got)
		}
	}
}

// A session id no route can name is refused as input, never answered 500 after the write.
func TestAgentStateRefusesASessionIDNoRouteNames(t *testing.T) {
	handler := newTestHandler(t)
	for _, field := range []string{"read_through", "cleared_before"} {
		put := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/odd.session/state", map[string]any{
			field: time.Now().UTC().Format(time.RFC3339Nano),
		}, "alice")
		if put.Code != http.StatusBadRequest || !strings.Contains(put.Body.String(), "odd.session") {
			t.Fatalf("%s for odd.session: status=%d body=%s, want 400 naming the session", field, put.Code, put.Body.String())
		}
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

// backdate moves a conversation and everything in it into the past, so newer traffic outranks it
// by activity.
func backdate(t *testing.T, database *store.Store, rootID string, by time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		with recursive thread as (
			select id from messages where id = $1::uuid
			union all
			select m.id from messages m join thread on m.in_reply_to = thread.id
		)
		update messages set created_at = created_at - $2::interval where id in (select id from thread)
	`, rootID, by.String()); err != nil {
		t.Fatalf("backdate %s: %v", rootID, err)
	}
}

// seedAnsweredConversations writes count conversations targeted at s1 and answered by it, as the
// traffic of a session that has been busy: issue-anchored when issueKey is not nil, direct
// otherwise.
func seedAnsweredConversations(t *testing.T, database *store.Store, count int, issueKey *string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		select $2, '{"kind":"user","id":"alice"}'::jsonb, 'Busier question ' || n, 'session:s1', null
		from generate_series(1, $1) as n
	`, count, issueKey); err != nil {
		t.Fatalf("seed roots: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		select issue_key, '{"kind":"session","id":"s1"}'::jsonb, 'Busier answer', target, id
		from messages
		where in_reply_to is null and body like 'Busier question %'
	`); err != nil {
		t.Fatalf("seed replies: %v", err)
	}
}

// windowHolds reports whether the conversation list a viewer reads holds rootID.
func windowHolds(t *testing.T, handler http.Handler, rootID string) (bool, int) {
	t.Helper()
	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	for _, read := range window {
		if read.Message.ID == rootID {
			return true, len(window)
		}
	}
	return false, len(window)
}

// The unread count is over every direct message the viewer sent the session, while the
// conversation list is a window: a reply the count counts has to be in the list, or opening the
// view marks it read through a watermark that only moves forward and nothing ever shows it.
func TestUnreadReplyOutsideTheActivityWindowIsStillListed(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	answer := decodeBody[model.Message](t, reply("Here is the answer you needed."))
	backdate(t, database, root.ID, 24*time.Hour)
	seedAnsweredConversations(t, database, 50, nil)

	// Every seeded conversation is a direct message this viewer sent, so all 51 replies count,
	// one more than the activity window holds.
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 51 {
		t.Fatalf("before opening the view: %#v, want all 51 replies counted unread", got)
	}
	held, size := windowHolds(t, handler, root.ID)
	if !held {
		t.Fatalf("window of %d conversations does not hold %s, whose reply %s is counted unread: opening the view would mark it read unseen", size, root.ID, answer.ID)
	}
}

// The count covers direct messages alone, while the window covers every conversation targeted at
// the session, so ordinary issue traffic is enough to push a direct conversation out of it.
func TestIssueTrafficDoesNotPushAnUnreadDirectReplyOutOfTheWindow(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	answer := decodeBody[model.Message](t, reply("No - it rolled back, here is why."))
	backdate(t, database, root.ID, 24*time.Hour)
	issue := createInteractionIssue(t, handler, "WINDOW", "Issue traffic", "seed")
	seedAnsweredConversations(t, database, 50, &issue.Key)

	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 1 {
		t.Fatalf("before opening the view: %#v, want the direct reply counted unread", got)
	}
	held, size := windowHolds(t, handler, root.ID)
	if !held {
		t.Fatalf("window of %d conversations, all issue traffic, does not hold the direct conversation %s, whose reply %s is counted unread", size, root.ID, answer.ID)
	}
	read := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": answer.CreatedAt,
	}, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", read.Code, read.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("after reading the reply the view showed: %#v, want nothing unread", got)
	}
}

// seedConversations writes count conversations targeted at s1, each answered by it, authored by
// login: issue-anchored when issueKey is not nil, direct otherwise. The bodies are tagged so a
// test can tell one batch from another.
func seedConversations(t *testing.T, database *store.Store, count int, issueKey *string, login, tag string) {
	t.Helper()
	author := fmt.Sprintf(`{"kind":"user","id":%q}`, login)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		select $2, $3::jsonb, $4 || ' question ' || n, 'session:s1', null
		from generate_series(1, $1) as n
	`, count, issueKey, author, tag); err != nil {
		t.Fatalf("seed %s roots: %v", tag, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		select issue_key, '{"kind":"session","id":"s1"}'::jsonb, $1 || ' answer', target, id
		from messages where in_reply_to is null and body like $1 || ' question %'
	`, tag); err != nil {
		t.Fatalf("seed %s replies: %v", tag, err)
	}
}

// The unread term of the window has no ceiling, because it is exactly what the count counts: 60
// unread conversations come back beside the 50 that moved last, and one read mark over the window
// clears the badge with every reply already shown.
func TestTheWindowReturnsEveryUnreadConversationBesideTheFiftyMostActive(t *testing.T) {
	handler, database, _, _, _ := directConversationFrom(t, "alice")
	seedConversations(t, database, 60, nil, "alice", "unread")
	issue := createInteractionIssue(t, handler, "WINDOW", "Issue traffic", "seed")
	seedConversations(t, database, 50, &issue.Key, "alice", "busier")

	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 60 {
		t.Fatalf("before opening the view: %#v, want 60 unread replies", got)
	}
	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	seen := map[string]bool{}
	unread, busier := 0, 0
	var newestReply string
	for _, read := range window {
		if seen[read.Message.ID] {
			t.Fatalf("conversation %s is in the window twice", read.Message.ID)
		}
		seen[read.Message.ID] = true
		switch {
		case strings.HasPrefix(read.Message.Body, "unread question"):
			unread++
		case strings.HasPrefix(read.Message.Body, "busier question"):
			busier++
		}
		for _, reply := range read.Replies {
			if reply.Author.Kind == "session" && (newestReply == "" || reply.CreatedAt.Format(time.RFC3339Nano) > newestReply) {
				newestReply = reply.CreatedAt.Format(time.RFC3339Nano)
			}
		}
	}
	// 110: every unread conversation, and the 50 that moved last. The fixture's own unanswered
	// question is neither - nothing in it is unread, and 110 conversations moved after it.
	if unread != 60 || busier != 50 || len(window) != 110 {
		t.Fatalf("window = %d conversations (%d unread, %d busier), want 110 (60 unread and the 50 most active)", len(window), unread, busier)
	}

	read := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": newestReply,
	}, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", read.Code, read.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("after one read mark over the window: %#v, want nothing unread", got)
	}
}

// The count and the window read one definition of an unread reply (unreadDirectRepliesCTE), so
// for every session the count equals the number of unread replies the window shows. The login is
// mixed-case on purpose: with a login whose raw and canonical forms are the same, a call site
// passing the raw one would pass this test while the window read zero roots.
func TestTheUnreadCountEqualsWhatTheWindowShowsForEverySession(t *testing.T) {
	const login = "Alice"
	if login == canonicalLogin(login) {
		t.Fatalf("the fixture login %q is already canonical, or this test stops testing the canonicalization it is named for", login)
	}
	handler, database, root, reply, _ := directConversationFrom(t, login)
	first := decodeBody[model.Message](t, reply("The first answer."))
	seedConversations(t, database, 2, nil, login, "direct")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		select null, '{"kind":"user","id":"Alice"}'::jsonb, 'To another session', 'session:s2', null
	`); err != nil {
		t.Fatalf("seed s2: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		select null, '{"kind":"session","id":"s2"}'::jsonb, 'An answer from s2', target, id
		from messages where body = 'To another session'
	`); err != nil {
		t.Fatalf("seed s2 reply: %v", err)
	}
	// Fifty busier conversations after the direct ones, so every unread direct conversation is
	// outside the fifty most active and comes back only through the window's unread term - the
	// term whose login the call site supplies.
	issue := createInteractionIssue(t, handler, "SHARED", "Issue traffic", "seed")
	seedConversations(t, database, 50, &issue.Key, login, "busier")
	// One conversation read, so a session's counted replies are the ones after its mark.
	marked := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s2/state", map[string]any{
		"read_through": time.Now().UTC().Format(time.RFC3339Nano),
	}, "alice")
	if marked.Code != http.StatusOK {
		t.Fatalf("mark s2 read: status=%d body=%s", marked.Code, marked.Body.String())
	}

	states := decodeBody[map[string]userAgentState](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "ALICE"))
	if len(states) == 0 {
		t.Fatalf("no per-session state for ALICE, want s1 and s2")
	}
	for _, session := range []string{"s1", "s2"} {
		counted := states[session].UnreadReplies
		shown := unreadRepliesShownInWindow(t, handler, session, states[session])
		if counted != shown {
			t.Fatalf("session %s: unread_replies=%d, window shows %d unread replies; the count and the window disagree", session, counted, shown)
		}
	}
	if states["s1"].UnreadReplies == 0 {
		t.Fatalf("session s1 counts nothing unread, so the equality above proves nothing; first reply %s", first.ID)
	}
	if root.Target == nil {
		t.Fatalf("the fixture's root has no target")
	}
}

// unreadRepliesShownInWindow counts, in the conversation list a viewer reads, the replies the
// server marked unread: the session's replies, after the viewer's read mark and Clear, in the
// conversations the response itself flags - what the Agents page and the live view show as new.
func unreadRepliesShownInWindow(t *testing.T, handler http.Handler, sessionID string, state userAgentState) int {
	t.Helper()
	watermark := time.Time{}
	for _, at := range []*string{state.ReadThrough, state.ClearedBefore} {
		if at == nil {
			continue
		}
		if parsed := parseTimestamp(t, *at); parsed.After(watermark) {
			watermark = parsed
		}
	}
	// Read as the identity source spells the login, as the state above is: a call site that
	// passed the raw form to the shared fragment would read a window over no roots at all.
	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/"+sessionID+"/messages", nil, "ALICE"))
	shown := 0
	for _, read := range window {
		// The conversation's own verdict, as the server sends it - no client-side re-derivation.
		if !read.Unread {
			continue
		}
		for _, reply := range read.Replies {
			if reply.Author.Kind == "session" && reply.Author.ID == sessionID && reply.CreatedAt.After(watermark) {
				shown++
			}
		}
	}
	return shown
}

// The window's top term is the 50 conversations that moved last, where moving is the newest
// message anywhere in the conversation, not the root's own age: an old question answered a moment
// ago is the one the Agents page opens on. Ordering or limiting `messages` before the reply walk
// would make it "the 50 most recently created" and drop this conversation, which is the same
// defect the unread union fixes, for a conversation the viewer has already read.
func TestAnOldConversationAnsweredNowIsInsideTheWindow(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	seedConversations(t, database, 50, nil, "bob", "quiet")
	if _, err := database.Pool.Exec(context.Background(), `
		delete from messages where body like 'quiet answer%'
	`); err != nil {
		t.Fatalf("drop the quiet answers: %v", err)
	}
	answer := decodeBody[model.Message](t, reply("Answered now, long after the question."))
	read := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": answer.CreatedAt,
	}, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", read.Code, read.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("%#v, want the reply read, so only the activity order can keep its conversation in the window", got)
	}

	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	if len(window) != 50 {
		t.Fatalf("window = %d conversations, want the 50 that moved last", len(window))
	}
	if window[0].Message.ID != root.ID {
		t.Fatalf("window opens on %s, want %s: the oldest question, answered last", window[0].Message.ID, root.ID)
	}
}

// A conversation moves on its newest message wherever it sits: a reply to a reply, deep in the
// chain, is what brings an old question back to the top. Ranking roots on their own created_at,
// or on their direct replies alone, would bury it under a newer question nobody has touched.
func TestAConversationMovesOnActivityDeepInItsChain(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	first := decodeBody[model.Message](t, reply("On it."))
	newer := decodeBody[model.Message](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "A newer question nobody has touched.", "delivery": "aside",
	}, "alice"))
	// A reply to the reply: the conversation's newest message is two levels down.
	deep := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "And here is the detail.", "delivery": "aside", "in_reply_to": first.ID,
	}, "alice")
	if deep.Code != http.StatusCreated {
		t.Fatalf("deep reply: status=%d body=%s", deep.Code, deep.Body.String())
	}
	deepest := decodeBody[model.Message](t, deep)
	read := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/agents/s1/state", map[string]any{
		"read_through": deepest.CreatedAt,
	}, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("mark read: status=%d body=%s", read.Code, read.Body.String())
	}
	if got := unreadReplies(t, handler, "alice", "s1"); got.UnreadReplies != 0 {
		t.Fatalf("%#v, want everything read, so only the activity order can rank these", got)
	}
	if _, err := database.Pool.Exec(context.Background(), `select 1`); err != nil {
		t.Fatal(err)
	}

	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	if len(window) != 2 || window[0].Message.ID != root.ID || window[1].Message.ID != newer.ID {
		ids := []string{}
		for _, read := range window {
			ids = append(ids, read.Message.ID)
		}
		t.Fatalf("window = %v, want [%s (answered deep in its chain) %s]", ids, root.ID, newer.ID)
	}
}

// A root the session reaches only through a delivery - targeted at a role rather than at the
// session - is a conversation the window lists by activity, and never an unread one: it is not a
// direct message this viewer sent that session. The count and the window agree on that, as they
// do on everything else.
func TestADeliveredOnlyConversationIsListedAndCountsNothingUnread(t *testing.T) {
	const login = "Alice"
	handler, database, _, _, _ := directConversationFrom(t, login)
	var rootID string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		values (null, $1::jsonb, 'A role question the session answered', 'role:planner', null)
		returning id::text
	`, `{"kind":"user","id":"`+login+`"}`).Scan(&rootID); err != nil {
		t.Fatalf("seed the role-targeted root: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1::uuid, 1, 'aside', 's1', 'sent')
	`, rootID); err != nil {
		t.Fatalf("seed its delivery to s1: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (issue_key, author, body, target, in_reply_to)
		values (null, '{"kind":"session","id":"s1"}'::jsonb, 'The session answered it.', 'role:planner', $1::uuid)
	`, rootID); err != nil {
		t.Fatalf("seed its reply: %v", err)
	}

	states := decodeBody[map[string]userAgentState](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/me/agents/state", nil, "ALICE"))
	if got := states["s1"].UnreadReplies; got != 0 {
		t.Fatalf("unread_replies = %d, want 0: a delivered-only root is not a direct message this viewer sent", got)
	}
	window := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "ALICE"))
	listed := false
	for _, read := range window {
		if read.Message.ID == rootID {
			listed = true
			if read.Unread {
				t.Fatalf("the delivered-only conversation %s is marked unread", rootID)
			}
		}
	}
	if !listed {
		t.Fatalf("window of %d conversations does not list the delivered-only conversation %s", len(window), rootID)
	}
	if shown := unreadRepliesShownInWindow(t, handler, "s1", states["s1"]); shown != states["s1"].UnreadReplies {
		t.Fatalf("session s1: unread_replies=%d, window shows %d", states["s1"].UnreadReplies, shown)
	}
}
