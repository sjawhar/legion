package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A closed issue still takes a conversation: comments and messages, from a person or an agent,
// with their mention deliveries, retries and the agent's replies. Every other write a closed
// issue refused - an ask, a suggestion, a document edit, an upload, a claim, an answer, a
// resolve - it still refuses, until the issue is reopened.

// closedIssueListener is an Envoy listener with one live session, s1, that takes every send
// and counts them.
func closedIssueListener(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	const sessions = `[{"session_id":"s1","title":"planner","capabilities":["aside","btw","steer"]}]`
	var sends atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/s1":
			writeSessionRecord(t, w, sessions, "s1")
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte(sessions))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			sends.Add(1)
			_, _ = w.Write([]byte(`{"event_id":"closed-envelope","recipient":"s1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(listener.Close)
	return listener, &sends
}

// closeIssue moves the issue to done through the route a person uses.
func closeIssue(t *testing.T, handler http.Handler, key string) {
	t.Helper()
	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{"status": "done"}, "alice")
	if closed.Code != http.StatusOK || !strings.Contains(closed.Body.String(), `"status":"done"`) {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}
}

func assertCreatedOnClosedIssue(t *testing.T, name string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s on a closed issue: status=%d body=%s", name, response.Code, response.Body.String())
	}
	// The write's advice still reports the issue as it is: closed, by its done status.
	if !strings.Contains(response.Body.String(), `"issue_status":"done"`) {
		t.Fatalf("%s on a closed issue: advice does not report the done status: %s", name, response.Body.String())
	}
}

func assertRefusedOnClosedIssue(t *testing.T, name string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"ISSUE_CLOSED"`) {
		t.Fatalf("%s on a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED", name, response.Code, response.Body.String())
	}
}

func TestClosedIssueTakesCommentsAndMessagesFromPeopleAndAgents(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closed conversation", "before")
	closeIssue(t, handler, issue.Key)

	person := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A person's comment after the close.",
	}, "alice")
	assertCreatedOnClosedIssue(t, "a person's comment", person)
	agent := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "An agent's comment after the close.", "actor": sessionActor(),
	})
	assertCreatedOnClosedIssue(t, "an agent's comment", agent)
	reply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "An agent's reply in the person's thread.", "reply_to": decodeBody[model.Comment](t, person).ID,
		"actor": sessionActor(),
	})
	assertCreatedOnClosedIssue(t, "an agent's thread reply", reply)

	personMessage := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "A person's message after the close.",
	}, "alice")
	assertCreatedOnClosedIssue(t, "a person's message", personMessage)
	agentMessage := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "An agent's message after the close.", "actor": sessionActor(),
	})
	assertCreatedOnClosedIssue(t, "an agent's message", agentMessage)
	threadReply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "A person's reply in the agent's thread.", "in_reply_to": decodeBody[model.Message](t, agentMessage).ID,
	}, "alice")
	assertCreatedOnClosedIssue(t, "a person's message reply", threadReply)

	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	body := events.Body.String()
	if events.Code != http.StatusOK || strings.Count(body, `"type":"comment.created"`) != 3 ||
		strings.Count(body, `"type":"message.created"`) != 2 || strings.Count(body, `"type":"message.answered"`) != 1 {
		t.Fatalf("closed issue's events: status=%d body=%s, want three comments, two messages and a reply", events.Code, body)
	}
}

func TestClosedIssueDeliversACommentMentionItsRetryAndTheAgentsReply(t *testing.T) {
	listener, sends := closedIssueListener(t)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Closed mention", "before")
	closeIssue(t, handler, issue.Key)

	created := postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Look at this after the close.", "mentions": []map[string]any{{"target": "session:s1"}},
	})
	assertCreatedOnClosedIssue(t, "a mentioning comment", created)
	comment := decodeMentionedComment(t, created)
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].State != "sent" || comment.Deliveries[0].Attempt != 1 ||
		comment.Deliveries[0].SessionID == nil || *comment.Deliveries[0].SessionID != "s1" {
		t.Fatalf("closed issue's mention delivery = %#v, want attempt 1 sent to s1", comment.Deliveries)
	}
	if got := commentAttemptRow(t, database, comment.ID, "session:s1", 1); !strings.HasPrefix(got, "sent/") {
		t.Fatalf("closed issue's mention attempt row = %q, want it recorded as sent", got)
	}

	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry a mention on a closed issue: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 2 || attempt.State != "sent" {
		t.Fatalf("closed issue's mention retry = %#v, want attempt 2 sent", attempt)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("listener sends = %d, want the first attempt and the retry", got)
	}

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "target": "session:s1",
		"body": "Answered on the closed issue.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("agent's reply to a mention on a closed issue: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if reply := decodeBody[model.Comment](t, answered); reply.ReplyTo == nil || *reply.ReplyTo != comment.ID {
		t.Fatalf("agent's reply = %#v, want it in the mentioning comment's thread", reply)
	}
	if got := commentAttemptRow(t, database, comment.ID, "session:s1", 2); !strings.HasPrefix(got, "sent/") {
		t.Fatalf("answered retry attempt row = %q, want it sent and answered", got)
	}
}

func TestClosedIssueDeliversAMessageItsRetryAndTheAgentsReply(t *testing.T) {
	listener, sends := closedIssueListener(t)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Closed message", "before")
	closeIssue(t, handler, issue.Key)

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Did this ship?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	assertCreatedOnClosedIssue(t, "a targeted message", created)
	message := decodeBody[model.Message](t, created)
	if len(message.Deliveries) != 1 || message.Deliveries[0].Attempt != 1 || message.Deliveries[0].State != "sent" ||
		message.Deliveries[0].SessionID != "s1" {
		t.Fatalf("closed issue's message delivery = %#v, want attempt 1 sent to s1", message.Deliveries)
	}

	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry a message on a closed issue: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[model.MessageDelivery](t, retry); attempt.Attempt != 2 || attempt.State != "sent" {
		t.Fatalf("closed issue's message retry = %#v, want attempt 2 sent", attempt)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("listener sends = %d, want the first attempt and the retry", got)
	}

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
		"actor": map[string]any{"kind": "session", "id": "s1"}, "attempt": 2, "body": "It shipped.",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("agent's reply to a message on a closed issue: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if reply := decodeBody[model.Message](t, answered); reply.InReplyTo == nil || *reply.InReplyTo != message.ID {
		t.Fatalf("agent's reply = %#v, want it in the message's thread", reply)
	}
}

func TestClosedIssueTakesAnAnchoredCommentAndItsThread(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closed anchors", "before the close")
	earlier := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Settled before the close.", "anchor": map[string]any{"artifact": "spec", "quote": "close"},
	}, "alice")
	if earlier.Code != http.StatusCreated {
		t.Fatalf("anchored comment before the close: status=%d body=%s", earlier.Code, earlier.Body.String())
	}
	resolvedRoot := decodeBody[model.Comment](t, earlier)
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+resolvedRoot.ID+"/resolve", map[string]any{}, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve before the close: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	closeIssue(t, handler, issue.Key)

	anchored := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "About this line, after the close.", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
	}, "alice")
	assertCreatedOnClosedIssue(t, "an anchored comment", anchored)
	root := decodeBody[model.Comment](t, anchored)
	if root.Anchor == nil || root.Anchor.ArtifactID != issue.PrimaryArtifactID || root.Anchor.Quote != "before" || root.Anchor.MarkID == "" {
		t.Fatalf("anchored comment's anchor = %#v, want a mark on the spec's quote", root.Anchor)
	}
	threadReply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "An agent's reply in the anchored thread.", "reply_to": root.ID, "actor": sessionActor(),
	})
	assertCreatedOnClosedIssue(t, "a reply in an anchored thread", threadReply)
	if reply := decodeBody[model.Comment](t, threadReply); reply.ReplyTo == nil || *reply.ReplyTo != root.ID {
		t.Fatalf("anchored thread reply = %#v, want it under the anchored comment", reply)
	}

	// A reply to a resolved root reopens it, on a closed issue as on an open one.
	reopening := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Not settled after all.", "reply_to": resolvedRoot.ID,
	}, "alice")
	assertCreatedOnClosedIssue(t, "a reply to a resolved anchored comment", reopening)
	read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+resolvedRoot.ID, nil, "alice")
	if read.Code != http.StatusOK || decodeBody[model.Comment](t, read).Resolved {
		t.Fatalf("resolved root after a reply on the closed issue: status=%d body=%s, want it reopened", read.Code, read.Body.String())
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if !strings.Contains(events.Body.String(), `"type":"comment.reopened"`) {
		t.Fatalf("closed issue's events = %s, want the reply's comment.reopened", events.Body.String())
	}
}

func TestClosedIssueStillRefusesEveryOtherWriteUntilReopened(t *testing.T) {
	listener, _ := closedIssueListener(t)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Closed for other writes", "before")
	ask := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Open ask", "actor": sessionActor(),
	})
	if ask.Code != http.StatusCreated {
		t.Fatalf("ask before the close: status=%d body=%s", ask.Code, ask.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, ask).ID
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Open comment",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("comment before the close: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID
	closeIssue(t, handler, issue.Key)

	assertRefusedOnClosedIssue(t, "a new ask", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Later ask", "actor": sessionActor(),
	}))
	assertRefusedOnClosedIssue(t, "a suggestion", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Replace before.", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
		"suggestion": map[string]string{"replace_with": "after"}, "actor": sessionActor(),
	}))
	assertRefusedOnClosedIssue(t, "a document edit", sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}}, "actor": sessionActor(),
	}))
	assertRefusedOnClosedIssue(t, "an upload", multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "later.md",
	}, "later.md", "text/markdown", []byte("later"), "alice"))
	assertRefusedOnClosedIssue(t, "a claim", bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{
		"actor": claimActorBody("s1", "Implementer"),
	}))
	assertRefusedOnClosedIssue(t, "an ask answer", dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{}, "alice"))
	assertRefusedOnClosedIssue(t, "a comment resolve", dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+commentID+"/resolve", map[string]any{}, "alice"))

	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"before\n"`) {
		t.Fatalf("closed issue's spec after the refused writes: status=%d body=%s, want it unchanged", text.Code, text.Body.String())
	}

	reopened := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice")
	if reopened.Code != http.StatusOK || !strings.Contains(reopened.Body.String(), `"closed_at":null`) {
		t.Fatalf("reopen the closed issue: status=%d body=%s", reopened.Code, reopened.Body.String())
	}
	if created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Reopened ask", "actor": sessionActor(),
	}); created.Code != http.StatusCreated {
		t.Fatalf("ask after reopen: status=%d body=%s", created.Code, created.Body.String())
	}
}
