package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// oweCommentIndex marks the comment's blocks owed, as rebuild-refs and an older build's write
// leave them: the reply that names one of its blocks has to index it before it can answer.
func oweCommentIndex(t *testing.T, database *store.Store, commentID string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		update comment_block_state set owed_at = now() where comment_id = $1
	`, commentID)
	if err != nil {
		t.Fatalf("owe comment %s its index: %v", commentID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("owe comment %s its index: %d rows, want the comment's block state", commentID, tag.RowsAffected())
	}
}

// oweMessageIndex is oweCommentIndex for a message.
func oweMessageIndex(t *testing.T, database *store.Store, messageID string) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(), `
		update message_block_state set owed_at = now() where message_id = $1
	`, messageID)
	if err != nil {
		t.Fatalf("owe message %s its index: %v", messageID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("owe message %s its index: %d rows, want the message's block state", messageID, tag.RowsAffected())
	}
}

// requireItemIndexed fails unless the item's block state is settled at body: owed_at cleared and
// the body the index was read from the stored one, which is what the repair leaves behind.
func requireItemIndexed(t *testing.T, database *store.Store, step, query, itemID, body string) {
	t.Helper()
	var sum string
	var settled bool
	if err := database.Pool.QueryRow(context.Background(), query, itemID).Scan(&sum, &settled); err != nil {
		t.Fatalf("%s: read the block state of %s: %v", step, itemID, err)
	}
	if sum != itemblocks.BodySHA256(body) || !settled {
		t.Fatalf("%s: block state = %q settled %v, want the stored body indexed and owed_at cleared", step, sum, settled)
	}
}

// The comment delivery's callback writes as it goes and joins no document ledger, so a repair it
// needs has no write phase to wait for: the index the comment it answers is owed runs at once.
// The callback names a block of a comment whose block state is owed, which is how rebuild-refs
// and an older build's write leave it, and it answers as it does for any other comment: the reply
// carries the block, and the comment is indexed by the same transaction.
func TestACommentDeliveryReplyNamesABlockOfAnOwedComment(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "A callback reply to an owed comment", "before")
	body := "Can this ship?\n\nOr should it wait?"
	mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": body, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	oweCommentIndex(t, database, mentioned.ID)

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
		"actor":          map[string]any{"kind": "session", "id": "s1"},
		"attempt":        1,
		"body":           "It should wait.",
		"reply_to_block": "b2",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("the callback reply to an owed comment: status=%d body=%s", answered.Code, answered.Body.String())
	}
	reply := decodeBody[model.Comment](t, answered)
	if reply.ReplyTo == nil || *reply.ReplyTo != mentioned.ID {
		t.Fatalf("the callback reply's reply_to = %v, want %s", reply.ReplyTo, mentioned.ID)
	}
	ref := "dispatch://" + issue.Key + "/comment/" + mentioned.ID
	wait := &model.ReplyBlock{Node: "comment/" + mentioned.ID + "#b2", Ref: ref + "#b-b2", BlockID: "b2", Text: "Or should it wait?"}
	requireReplyBlock(t, "the callback's answer", reply.ReplyBlock, wait)
	requireReplyBlock(t, "the stored reply", readCommentThread(t, handler, reply.ID).Comment.ReplyBlock, wait)
	requireItemIndexed(t, database, "the owed comment after the callback", `
		select body_sha256, owed_at is null from comment_block_state where comment_id = $1
	`, mentioned.ID, body)
}

// A session's reply to a message delivery writes as it goes and joins no document ledger either,
// so the index the message it answers is owed runs at once there too. The reply names a block of
// a message whose block state is owed and is answered as any other reply is: it carries the
// block, and the message is indexed by the same transaction.
func TestAMessageDeliveryReplyNamesABlockOfAnOwedMessage(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "A session reply to an owed message", "before")
	body := "Ship tonight?\n\nOr tomorrow?"
	targeted := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": body, "target": "session:s1", "delivery": "btw",
	}, "alice")
	oweMessageIndex(t, database, targeted.ID)

	answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+targeted.ID+"/reply", map[string]any{
		"actor":             map[string]any{"kind": "session", "id": "s1"},
		"attempt":           1,
		"body":              "Tomorrow.",
		"in_reply_to_block": "b2",
	})
	if answered.Code != http.StatusCreated {
		t.Fatalf("the session's reply to an owed message: status=%d body=%s", answered.Code, answered.Body.String())
	}
	reply := decodeBody[model.Message](t, answered)
	if reply.InReplyTo == nil || *reply.InReplyTo != targeted.ID {
		t.Fatalf("the session reply's in_reply_to = %v, want %s", reply.InReplyTo, targeted.ID)
	}
	ref := "dispatch://" + issue.Key + "/message/" + targeted.ID
	tomorrow := &model.ReplyBlock{Node: "message/" + targeted.ID + "#b2", Ref: ref + "#b-b2", BlockID: "b2", Text: "Or tomorrow?"}
	requireReplyBlock(t, "the reply route's answer", reply.ReplyBlock, tomorrow)
	event, replyBody := eventReplyBlock(t, handler, issue.Key, "message.answered", reply.ID)
	requireReplyBlock(t, "the session reply's message.answered payload", event, tomorrow)
	if replyBody != "Or tomorrow?" {
		t.Fatalf("the session reply's reply_body = %q, want the block's text", replyBody)
	}
	requireItemIndexed(t, database, "the owed message after the reply", `
		select body_sha256, owed_at is null from message_block_state where message_id = $1
	`, targeted.ID, body)
}
