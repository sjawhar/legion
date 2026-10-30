package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// INFRA-368. A reply names one block of the message it answers, and a repeat of that keyed write
// is answered from the key's recovery point (recoveredMessage) rather than run again. The repeat
// answers the same reply_block as the first press on both message routes: the issue message's,
// whose replay keeps the first answer's shape (replayIssueMessage), and the single-session send's,
// which answers the recovered message alone (replayAgentMessage). recoveredMessage reads the
// message row by itself, so without hydrateMessageReplyBlocks both answer reply_block null and a
// double press loses the block the reader pressed Reply on.
func TestARepeatOfAKeyedMessageReplyAnswersItsBlockAgain(t *testing.T) {
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createTestIssue(t, handler, "KEYB", "Keyed replies that name a block")

	t.Run("an issue message", func(t *testing.T) {
		root := createIssueMessage(t, handler, issue, map[string]any{"body": "One.\n\nTwo."}, "alice")
		two := &model.ReplyBlock{
			Node:    "message/" + root.ID + "#b2",
			Ref:     "dispatch://" + issue + "/message/" + root.ID + "#b-b2",
			BlockID: "b2",
			Text:    "Two.",
		}
		input := map[string]any{"body": "On two.", "in_reply_to": root.ID, "in_reply_to_block": "b2"}
		path := "/api/v1/issues/" + issue + "/messages"
		first := keyedRequest(t, handler, http.MethodPost, path, input, "bob", "issue-reply-1")
		if first.Code != http.StatusCreated || replayed(first) {
			t.Fatalf("the first press: status=%d replayed=%v body=%s", first.Code, replayed(first), first.Body.String())
		}
		requireReplyBlock(t, "the first press", decodeBody[model.Message](t, first).ReplyBlock, two)

		again := keyedRequest(t, handler, http.MethodPost, path, input, "bob", "issue-reply-1")
		if again.Code != http.StatusCreated || !replayed(again) {
			t.Fatalf("the repeat: status=%d replayed=%v body=%s", again.Code, replayed(again), again.Body.String())
		}
		requireReplyBlock(t, "the repeat", decodeBody[model.Message](t, again).ReplyBlock, two)
	})

	t.Run("a single-session send", func(t *testing.T) {
		direct := decodeBody[model.Message](t, postAgentMessage(t, handler, "s1", map[string]any{
			"body": "Direct one.\n\nDirect two.", "delivery": "btw",
		}, "alice"))
		// A direct message has no address, so the block it holds has a node id and no reference.
		two := &model.ReplyBlock{Node: "message/" + direct.ID + "#b2", BlockID: "b2", Text: "Direct two."}
		input := map[string]any{
			"body": "On direct two.", "delivery": "btw", "in_reply_to": direct.ID, "in_reply_to_block": "b2",
		}
		first := postAgentMessageKeyed(t, handler, "s1", input, "alice", "agent-reply-1")
		if first.Code != http.StatusCreated || replayed(first) {
			t.Fatalf("the first press: status=%d replayed=%v body=%s", first.Code, replayed(first), first.Body.String())
		}
		requireReplyBlock(t, "the first press", decodeBody[model.Message](t, first).ReplyBlock, two)

		again := postAgentMessageKeyed(t, handler, "s1", input, "alice", "agent-reply-1")
		if again.Code != http.StatusCreated || !replayed(again) {
			t.Fatalf("the repeat: status=%d replayed=%v body=%s", again.Code, replayed(again), again.Body.String())
		}
		requireReplyBlock(t, "the repeat", decodeBody[model.Message](t, again).ReplyBlock, two)
	})
}
