package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// describeReplyBlock is a reply_block as a failure message prints it.
func describeReplyBlock(block *model.ReplyBlock) string {
	if block == nil {
		return "null"
	}
	return fmt.Sprintf("%+v", *block)
}

// requireReplyBlock fails unless got is want (nil: the reply names no block).
func requireReplyBlock(t *testing.T, step string, got, want *model.ReplyBlock) {
	t.Helper()
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Fatalf("%s: reply_block = %s, want %s", step, describeReplyBlock(got), describeReplyBlock(want))
	}
}

// requireRefusal fails unless the response is a 400 with code whose message holds message.
func requireRefusal(t *testing.T, step string, response *httptest.ResponseRecorder, code, message string) {
	t.Helper()
	body := response.Body.String()
	if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"`+code+`"`) || !strings.Contains(body, message) {
		t.Fatalf("%s: status=%d body=%s, want 400 %s saying %q", step, response.Code, body, code, message)
	}
}

// eventReplyBlock is the reply_block and reply_body of the issue event of eventType about item id.
func eventReplyBlock(t *testing.T, handler http.Handler, issueKey, eventType, id string) (*model.ReplyBlock, string) {
	t.Helper()
	for _, event := range issueEvents(t, handler, issueKey) {
		if event.Type != eventType {
			continue
		}
		var payload struct {
			ID         string            `json:"id"`
			ReplyBody  string            `json:"reply_body"`
			ReplyBlock *model.ReplyBlock `json:"reply_block"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode the %s payload: %v", eventType, err)
		}
		if payload.ID != id {
			continue
		}
		if !strings.Contains(string(event.Payload), `"reply_block":`) {
			t.Fatalf("the %s payload of %s names no reply_block: %s", eventType, id, event.Payload)
		}
		return payload.ReplyBlock, payload.ReplyBody
	}
	t.Fatalf("no %s event about %s on %s", eventType, id, issueKey)
	return nil, ""
}

// commentThread is GET /api/v1/comments/{id}'s answer.
type commentThread struct {
	Comment model.Comment   `json:"comment"`
	Replies []model.Comment `json:"replies"`
}

func readCommentThread(t *testing.T, handler http.Handler, id string) commentThread {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read comment %s: status=%d body=%s", id, response.Code, response.Body.String())
	}
	return decodeBody[commentThread](t, response)
}

// sendFrame is the message event a listener send carries, with the fields a reply block moves.
func sendFrame(t *testing.T, send map[string]any) (string, *model.ReplyBlock) {
	t.Helper()
	payload, ok := send["payload"].(string)
	if !ok {
		t.Fatalf("listener payload = %#v, want a JSON string", send)
	}
	var frame struct {
		Event struct {
			Payload struct {
				ReplyBody  string            `json:"reply_body"`
				ReplyBlock *model.ReplyBlock `json:"reply_block"`
			} `json:"payload"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("decode delivery frame %s: %v", payload, err)
	}
	return frame.Event.Payload.ReplyBody, frame.Event.Payload.ReplyBlock
}

// A reply names a block of the comment it answers: the block is stored with the reply while
// reply_to holds the thread's root, every read and the reply's event carry it, and the reply's
// graph node belongs to the block. A reply in an ask's thread, a block the comment does not hold
// and a block with no comment are refused; a comment an older build wrote is indexed first; an
// edit that removes the block leaves the reply reading it removed, with its last text.
func TestAReplyNamesTheBlockItAnswers(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Reply blocks", "before")
	x := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	xRef := "dispatch://" + issue.Key + "/comment/" + x.ID
	beta := &model.ReplyBlock{Node: "comment/" + x.ID + "#b2", Ref: xRef + "#b-b2", BlockID: "b2", Text: "Beta."}

	reply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On beta.", "reply_to": x.ID, "reply_to_block": "b2",
	}, "bob")
	if reply.ReplyTo == nil || *reply.ReplyTo != x.ID {
		t.Fatalf("the reply's reply_to = %v, want %s", reply.ReplyTo, x.ID)
	}
	requireReplyBlock(t, "the write's answer", reply.ReplyBlock, beta)
	var stored *string
	if err := database.Pool.QueryRow(context.Background(), `select replies_to_block from comments where id = $1`, reply.ID).Scan(&stored); err != nil ||
		stored == nil || *stored != beta.Node {
		t.Fatalf("replies_to_block = %v (%v), want %s", stored, err, beta.Node)
	}

	read := readCommentThread(t, handler, reply.ID)
	requireReplyBlock(t, "GET the reply", read.Comment.ReplyBlock, beta)
	parent := readCommentThread(t, handler, x.ID)
	requireReplyBlock(t, "GET the comment it answers", parent.Comment.ReplyBlock, nil)
	if len(parent.Replies) != 1 || parent.Replies[0].ID != reply.ID {
		t.Fatalf("the comment's replies = %#v, want the reply", parent.Replies)
	}
	requireReplyBlock(t, "the comment's replies", parent.Replies[0].ReplyBlock, beta)
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
	found := false
	for _, comment := range decodeBody[[]model.Comment](t, listed) {
		switch comment.ID {
		case reply.ID:
			found = true
			requireReplyBlock(t, "the issue's comment list", comment.ReplyBlock, beta)
		case x.ID:
			requireReplyBlock(t, "the root in the issue's comment list", comment.ReplyBlock, nil)
		}
	}
	if !found {
		t.Fatalf("the issue's comments omit the reply: %s", listed.Body.String())
	}
	created, _ := eventReplyBlock(t, handler, issue.Key, "comment.created", reply.ID)
	requireReplyBlock(t, "the comment.created payload", created, beta)
	rootCreated, _ := eventReplyBlock(t, handler, issue.Key, "comment.created", x.ID)
	requireReplyBlock(t, "the root's comment.created payload", rootCreated, nil)

	// The reply's node belongs to the block it names (R12), and ?to= the block lists it.
	replyRef := "dispatch://" + issue.Key + "/comment/" + reply.ID
	if node := readGraph(t, handler, url.Values{"from": {replyRef}}).Node; node.Parent == nil ||
		*node.Parent != (model.NodeParent{Kind: "block", ID: beta.Node, Ref: beta.Ref}) {
		t.Fatalf("the reply's node %#v, want its parent block b2 of comment %s", node, x.ID)
	}
	if edges := readGraph(t, handler, url.Values{"to": {beta.Ref}, "kind": {"replies_to"}}).Edges; len(edges) != 1 || edges[0].Node.ID != reply.ID {
		t.Fatalf("?to= block b2 lists the replies %#v, want the reply", edges)
	}

	// A reply naming a block of a nested reply stores that block, while reply_to climbs to the root.
	nested := createThreadComment(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo.", "reply_to": x.ID}, "bob")
	deep := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "On two.", "reply_to": nested.ID, "reply_to_block": "b2",
	}, "alice")
	if deep.ReplyTo == nil || *deep.ReplyTo != x.ID {
		t.Fatalf("the deep reply's reply_to = %v, want the root %s", deep.ReplyTo, x.ID)
	}
	nestedRef := "dispatch://" + issue.Key + "/comment/" + nested.ID
	requireReplyBlock(t, "a reply naming a nested reply's block", deep.ReplyBlock,
		&model.ReplyBlock{Node: "comment/" + nested.ID + "#b2", Ref: nestedRef + "#b-b2", BlockID: "b2", Text: "Two."})

	post := func(input map[string]any) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", input, "alice")
	}
	requireRefusal(t, "a block the comment does not hold", post(map[string]any{"body": "On b9.", "reply_to": x.ID, "reply_to_block": "b9"}),
		"INVALID_COMMENT", "reply_to_block b9 is not a block of comment "+x.ID)
	requireRefusal(t, "a block with no reply_to", post(map[string]any{"body": "On b2.", "reply_to_block": "b2"}),
		"INVALID_COMMENT", "reply_to_block requires reply_to")
	askID := openAskAs(t, handler, issue.Key, "s1", "Ship it?")
	requireRefusal(t, "a reply to an ask naming a block", post(map[string]any{"body": "Yes.", "ask_id": askID, "reply_to_block": "b1"}),
		"INVALID_COMMENT", replyBlockInAskThread)
	underAsk := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Yes.\n\nTonight.", "ask_id": askID}, "alice")
	requireRefusal(t, "a reply under an ask naming a block", post(map[string]any{"body": "Tonight?", "reply_to": underAsk.ID, "reply_to_block": "b2"}),
		"INVALID_COMMENT", replyBlockInAskThread)

	// A comment an older build wrote is owed its index; the reply's transaction indexes it first.
	var old string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, issue.Key, "Old one.\n\nOld two.").Scan(&old); err != nil {
		t.Fatalf("insert a comment as an older build: %v", err)
	}
	onOld := createThreadComment(t, handler, issue.Key, map[string]any{"body": "On old two.", "reply_to": old, "reply_to_block": "b2"}, "bob")
	if onOld.ReplyBlock == nil || onOld.ReplyBlock.Text != "Old two." || onOld.ReplyBlock.Node != "comment/"+old+"#b2" {
		t.Fatalf("a reply to an owed comment reads %s, want its b2 \"Old two.\"", describeReplyBlock(onOld.ReplyBlock))
	}
	requireCommentBlocks(t, database, old, "the owed comment after the reply", "b1@1 b2@1", 1)

	// Alice's edit removes b2: the reply still names it, removed, with the text it last held.
	requireEdit(t, "remove beta", editCommentAs(t, handler, x.ID, "Alpha.", nil, nil))
	removed := *beta
	removed.Removed = true
	requireReplyBlock(t, "GET the reply after the edit removed b2", readCommentThread(t, handler, reply.ID).Comment.ReplyBlock, &removed)
	requireReplyBlock(t, "the root's replies after the edit", readCommentThread(t, handler, x.ID).Replies[0].ReplyBlock, &removed)
	requireRefusal(t, "a removed block", post(map[string]any{"body": "On beta.", "reply_to": x.ID, "reply_to_block": "b2"}),
		"INVALID_COMMENT", "reply_to_block b2 is not a block of comment "+x.ID)
	requireEdit(t, "restore beta", editCommentAs(t, handler, x.ID, "Alpha.\n\nBeta.", nil, nil))
	requireReplyBlock(t, "GET the reply after the edit restored b2", readCommentThread(t, handler, reply.ID).Comment.ReplyBlock, beta)

	// The delivery callback names a block of the comment that mentioned the session.
	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Can this ship?\n\nOr should it wait?", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	callback := func(input map[string]any) *httptest.ResponseRecorder {
		input["actor"] = map[string]any{"kind": "session", "id": "s1"}
		input["attempt"] = 1
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", input)
	}
	requireRefusal(t, "a callback naming a block the comment does not hold", callback(map[string]any{"body": "Wait.", "reply_to_block": "b9"}),
		"INVALID_COMMENT", "reply_to_block b9 is not a block of comment "+mentioned.ID)
	requireRefusal(t, "a callback error naming a block", callback(map[string]any{"error": "No model.", "reply_to_block": "b2"}),
		"INVALID_COMMENT", "reply_to_block")
	mentionedRef := "dispatch://" + issue.Key + "/comment/" + mentioned.ID
	wait := &model.ReplyBlock{Node: "comment/" + mentioned.ID + "#b2", Ref: mentionedRef + "#b-b2", BlockID: "b2", Text: "Or should it wait?"}
	answered := callback(map[string]any{"body": "It should wait.", "reply_to_block": "b2"})
	if answered.Code != http.StatusCreated {
		t.Fatalf("the callback reply: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Comment](t, answered)
	if answer.ReplyTo == nil || *answer.ReplyTo != mentioned.ID {
		t.Fatalf("the callback reply's reply_to = %v, want %s", answer.ReplyTo, mentioned.ID)
	}
	requireReplyBlock(t, "the callback's answer", answer.ReplyBlock, wait)
	answeredEvent, _ := eventReplyBlock(t, handler, issue.Key, "comment.answered", answer.ID)
	requireReplyBlock(t, "the comment.answered payload", answeredEvent, wait)
	replayed := callback(map[string]any{"body": "It should wait.", "reply_to_block": "b2"})
	if replayed.Code != http.StatusOK {
		t.Fatalf("the replayed callback: status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	requireReplyBlock(t, "the replayed callback", decodeBody[model.Comment](t, replayed).ReplyBlock, wait)
}

// A message reply names a block of the message it answers on every message route: an issue
// message, a session's reply to a delivery, a direct message and a person's reply to a session's
// answer. Its reply_body previews the block's text, on the event, the delivery frame and a retried
// delivery; a follow-up names no block; and a message whose stored body differs from the one its
// blocks were indexed from is indexed first.
func TestAMessageReplyNamesTheBlockItAnswers(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Message reply blocks", "before")

	root := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo."}, "alice")
	rootRef := "dispatch://" + issue.Key + "/message/" + root.ID
	two := &model.ReplyBlock{Node: "message/" + root.ID + "#b2", Ref: rootRef + "#b-b2", BlockID: "b2", Text: "Two."}
	reply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "On two.", "in_reply_to": root.ID, "in_reply_to_block": "b2",
	}, "bob")
	if reply.InReplyTo == nil || *reply.InReplyTo != root.ID {
		t.Fatalf("the reply's in_reply_to = %v, want %s", reply.InReplyTo, root.ID)
	}
	requireReplyBlock(t, "the issue message route's answer", reply.ReplyBlock, two)
	event, replyBody := eventReplyBlock(t, handler, issue.Key, "message.answered", reply.ID)
	requireReplyBlock(t, "the message.answered payload", event, two)
	if replyBody != "Two." {
		t.Fatalf("the message.answered reply_body = %q, want the block's text", replyBody)
	}
	rootEvent, _ := eventReplyBlock(t, handler, issue.Key, "message.created", root.ID)
	requireReplyBlock(t, "the root's message.created payload", rootEvent, nil)
	thread := decodeBody[messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+root.ID, nil, "alice"))
	requireReplyBlock(t, "the thread's root", thread.Message.ReplyBlock, nil)
	if len(thread.Replies) != 1 {
		t.Fatalf("the thread's replies = %#v, want the reply", thread.Replies)
	}
	requireReplyBlock(t, "the thread's reply", thread.Replies[0].ReplyBlock, two)
	byID := decodeBody[messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/messages/"+reply.ID, nil, "alice"))
	requireReplyBlock(t, "the reply read by its own id", byID.Message.ReplyBlock, two)

	post := func(input map[string]any) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", input, "alice")
	}
	requireRefusal(t, "a block the message does not hold", post(map[string]any{"body": "On b9.", "in_reply_to": root.ID, "in_reply_to_block": "b9"}),
		"INVALID_MESSAGE", "in_reply_to_block b9 is not a block of message "+root.ID)
	requireRefusal(t, "a block with no in_reply_to", post(map[string]any{"body": "On b2.", "in_reply_to_block": "b2"}),
		"INVALID_MESSAGE", "in_reply_to_block requires in_reply_to")

	// A session's reply to its delivery names a block of the message delivered.
	targeted := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "Ship tonight?\n\nOr tomorrow?", "target": "session:s1", "delivery": "btw",
	}, "alice")
	targetedRef := "dispatch://" + issue.Key + "/message/" + targeted.ID
	tomorrow := &model.ReplyBlock{Node: "message/" + targeted.ID + "#b2", Ref: targetedRef + "#b-b2", BlockID: "b2", Text: "Or tomorrow?"}
	replyTo := func(id, query string, input map[string]any) *httptest.ResponseRecorder {
		input["actor"] = map[string]any{"kind": "session", "id": "s1"}
		input["attempt"] = 1
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+id+"/reply"+query, input)
	}
	requireRefusal(t, "a delivery reply naming a block the message does not hold", replyTo(targeted.ID, "", map[string]any{"body": "Tomorrow.", "in_reply_to_block": "b9"}),
		"INVALID_MESSAGE", "in_reply_to_block b9 is not a block of message "+targeted.ID)
	answered := replyTo(targeted.ID, "", map[string]any{"body": "Tomorrow.", "in_reply_to_block": "b2"})
	if answered.Code != http.StatusCreated {
		t.Fatalf("the session's reply: status=%d body=%s", answered.Code, answered.Body.String())
	}
	answer := decodeBody[model.Message](t, answered)
	requireReplyBlock(t, "the reply route's answer", answer.ReplyBlock, tomorrow)
	answerEvent, answerBody := eventReplyBlock(t, handler, issue.Key, "message.answered", answer.ID)
	requireReplyBlock(t, "the session reply's message.answered payload", answerEvent, tomorrow)
	if answerBody != "Or tomorrow?" {
		t.Fatalf("the session reply's reply_body = %q, want the block's text", answerBody)
	}
	requireRefusal(t, "a follow-up naming a block", replyTo(targeted.ID, "?follow_up=true", map[string]any{"body": "And Friday.", "in_reply_to_block": "b1"}),
		"INVALID_MESSAGE", "a follow-up is filed under the session's first reply")
	retried := replyTo(targeted.ID, "", map[string]any{"body": "Tomorrow.", "in_reply_to_block": "b2"})
	if retried.Code != http.StatusOK {
		t.Fatalf("the retried reply: status=%d body=%s", retried.Code, retried.Body.String())
	}
	requireReplyBlock(t, "the retried reply, the stored answer", decodeBody[model.Message](t, retried).ReplyBlock, tomorrow)

	// A direct message to a session, which has no address: the session's reply names its block,
	// and the person's reply on the agent route names a block of the session's answer.
	direct := decodeBody[model.Message](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Direct one.\n\nDirect two.", "delivery": "btw",
	}, "alice"))
	sessionAnswered := replyTo(direct.ID, "", map[string]any{"body": "On direct two.\n\nAnd more.", "in_reply_to_block": "b2"})
	if sessionAnswered.Code != http.StatusCreated {
		t.Fatalf("the session's reply to the direct message: status=%d body=%s", sessionAnswered.Code, sessionAnswered.Body.String())
	}
	sessionAnswer := decodeBody[model.Message](t, sessionAnswered)
	requireReplyBlock(t, "the session's reply to a direct message", sessionAnswer.ReplyBlock,
		&model.ReplyBlock{Node: "message/" + direct.ID + "#b2", BlockID: "b2", Text: "Direct two."})
	more := &model.ReplyBlock{Node: "message/" + sessionAnswer.ID + "#b2", BlockID: "b2", Text: "And more."}
	sendsBefore := len(sent)
	personReplied := dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/s1/messages", map[string]any{
		"body": "Which more?", "delivery": "btw", "in_reply_to": sessionAnswer.ID, "in_reply_to_block": "b2",
	}, "alice")
	if personReplied.Code != http.StatusCreated {
		t.Fatalf("the person's direct reply: status=%d body=%s", personReplied.Code, personReplied.Body.String())
	}
	personReply := decodeBody[model.Message](t, personReplied)
	requireReplyBlock(t, "the agent route's answer", personReply.ReplyBlock, more)
	requireFrame := func(step string, sendsBefore int) {
		t.Helper()
		if len(sent) != sendsBefore+1 {
			t.Fatalf("%s: listener sends = %d, want %d", step, len(sent), sendsBefore+1)
		}
		body, block := sendFrame(t, sent[sendsBefore])
		if body != "And more." {
			t.Fatalf("%s: the frame's reply_body = %q, want the block's text", step, body)
		}
		requireReplyBlock(t, step, block, more)
	}
	requireFrame("the person's reply's frame", sendsBefore)
	sendsBefore = len(sent)
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+personReply.ID+"/deliveries", map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the person's reply: status=%d body=%s", retry.Code, retry.Body.String())
	}
	requireFrame("the retried frame", sendsBefore)
	conversations := decodeBody[[]messageRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/agents/s1/messages", nil, "alice"))
	var conversation *messageRead
	for index := range conversations {
		if conversations[index].Message.ID == direct.ID {
			conversation = &conversations[index]
		}
	}
	if conversation == nil || len(conversation.Replies) != 2 {
		t.Fatalf("the agent's conversations = %#v, want the direct thread with two replies", conversations)
	}
	requireReplyBlock(t, "the conversation's session reply", conversation.Replies[0].ReplyBlock, sessionAnswer.ReplyBlock)
	requireReplyBlock(t, "the conversation's person reply", conversation.Replies[1].ReplyBlock, more)

	// A message whose stored body is not the one its blocks were indexed from is indexed first.
	owed := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	rewritten := "Alpha.\n\nBeta.\n\nGamma."
	if _, err := database.Pool.Exec(context.Background(), `update messages set body = $2 where id = $1`, owed.ID, rewritten); err != nil {
		t.Fatalf("rewrite the message as an older build: %v", err)
	}
	onGamma := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "On gamma.", "in_reply_to": owed.ID, "in_reply_to_block": "b3",
	}, "bob")
	owedRef := "dispatch://" + issue.Key + "/message/" + owed.ID
	requireReplyBlock(t, "a reply to a message owed its index", onGamma.ReplyBlock,
		&model.ReplyBlock{Node: "message/" + owed.ID + "#b3", Ref: owedRef + "#b-b3", BlockID: "b3", Text: "Gamma."})
	var sum string
	var settled bool
	if err := database.Pool.QueryRow(context.Background(), `
		select body_sha256, owed_at is null from message_block_state where message_id = $1
	`, owed.ID).Scan(&sum, &settled); err != nil || sum != itemblocks.BodySHA256(rewritten) || !settled {
		t.Fatalf("the owed message's state = %q settled %v (%v), want the rewritten body indexed", sum, settled, err)
	}
}

// A7's last sentence: a reply an older build wrote, with no block state, still shows after the
// reconciliation indexes it: under the comment it answers and under the ask, on the comment read,
// the ask read and ?to=, each naming no block.
func TestRepliesWrittenBeforeThisChangeStillShow(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Older replies", "before")
	x := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	askID := openAskAs(t, handler, issue.Key, "s1", "Ship it?")
	insert := func(column, parent, body string) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(context.Background(), `
			insert into comments (issue_key, author, body, `+column+`)
			values ($1, '{"kind":"user","id":"alice"}', $2, $3) returning id::text
		`, issue.Key, body, parent).Scan(&id); err != nil {
			t.Fatalf("insert a reply as an older build: %v", err)
		}
		// A build without block state wrote no state row at all.
		if _, err := database.Pool.Exec(context.Background(), `delete from comment_block_state where comment_id = $1`, id); err != nil {
			t.Fatalf("drop the reply's block state: %v", err)
		}
		return id
	}
	reply := insert("reply_to", x.ID, "On the comment.")
	askReply := insert("ask_id", askID, "On the ask.")
	for _, id := range []string{reply, askReply} {
		if err := refs.ReindexOwedItem(context.Background(), database.Pool, refs.OwedItem{Kind: "comment", ID: id, EnqueuedAt: time.Now()}, "https://dispatch.example"); err != nil {
			t.Fatalf("reindex comment %s: %v", id, err)
		}
	}

	commentRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+x.ID, nil, "alice")
	thread := decodeBody[commentThread](t, commentRead)
	if len(thread.Replies) != 1 || thread.Replies[0].ID != reply || !strings.Contains(commentRead.Body.String(), `"reply_block":null`) {
		t.Fatalf("GET the comment = %s, want the older reply naming no block", commentRead.Body.String())
	}
	requireReplyBlock(t, "the older comment reply", thread.Replies[0].ReplyBlock, nil)

	askRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
	if askRead.Code != http.StatusOK {
		t.Fatalf("GET the ask: status=%d body=%s", askRead.Code, askRead.Body.String())
	}
	askThread := decodeBody[struct {
		Replies []model.Comment `json:"replies"`
	}](t, askRead)
	if len(askThread.Replies) != 1 || askThread.Replies[0].ID != askReply || !strings.Contains(askRead.Body.String(), `"reply_block":null`) {
		t.Fatalf("GET the ask = %s, want the older ask reply naming no block", askRead.Body.String())
	}
	requireReplyBlock(t, "the older ask reply", askThread.Replies[0].ReplyBlock, nil)

	xRef := "dispatch://" + issue.Key + "/comment/" + x.ID
	if edges := readGraph(t, handler, url.Values{"to": {xRef}, "kind": {"replies_to"}}).Edges; len(edges) != 1 || edges[0].Node.ID != reply || edges[0].Block != nil {
		t.Fatalf("?to= the comment lists the replies %#v, want the older reply under the comment", edges)
	}
	askRef := "dispatch://" + issue.Key + "/ask/" + askID
	if edges := readGraph(t, handler, url.Values{"to": {askRef}, "kind": {"replies_to"}}).Edges; len(edges) != 1 || edges[0].Node.ID != askReply || edges[0].Block != nil {
		t.Fatalf("?to= the ask lists the replies %#v, want the older ask reply", edges)
	}
}
