package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// eventPayloads is the payload of every event of the issue of type, oldest first, as raw JSON.
func eventPayloads(t *testing.T, handler http.Handler, issueKey, eventType string) []map[string]any {
	t.Helper()
	rows := decodeBody[[]struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey+"/events", nil, "alice"))
	payloads := []map[string]any{}
	for _, row := range rows {
		if row.Type != eventType {
			continue
		}
		payload := map[string]any{}
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			t.Fatalf("decode a %s payload: %v", eventType, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// A body is bounded block by block, so it can be far longer than one event carries. The comment
// is stored and read back whole; its event carries the first contracts.DispatchEventBodyMax
// units of it, cut at the last blank line inside that, and says so with body_truncated. A body
// that fits rides whole and the event says nothing: no reader of an older build sees a key it
// does not know on an event that carries everything.
func TestACommentEventCarriesALongBodyInPartAndSaysSo(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "EVB", "Event body", "A spec")

	long := paragraphBody(3, 15_000)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": long}, "alice")
	if comment.Body != long {
		t.Fatalf("the stored comment is %d units, want the %d sent: the event's share is not the item's",
			itemblocks.Units16(comment.Body), itemblocks.Units16(long))
	}
	whole := createThreadComment(t, handler, issue.Key,
		map[string]any{"body": strings.Repeat("b", contracts.DispatchEventBodyMax)}, "alice")

	payloads := eventPayloads(t, handler, issue.Key, "comment.created")
	if len(payloads) != 2 {
		t.Fatalf("the issue carries %d comment.created events, want two", len(payloads))
	}
	cut, intact := payloads[0], payloads[1]
	if cut["id"] != comment.ID || intact["id"] != whole.ID {
		t.Fatalf("the events are not the two comments written, oldest first: %v, %v", cut["id"], intact["id"])
	}

	wantCut := paragraphBody(2, 15_000)
	if cut["body"] != wantCut {
		t.Fatalf("the event of a three-paragraph comment carries %d units, want its first two paragraphs (%d units)",
			itemblocks.Units16(cut["body"].(string)), itemblocks.Units16(wantCut))
	}
	if cut["body_truncated"] != true {
		t.Fatalf("the event of a cut comment body does not say it was cut: body_truncated=%v", cut["body_truncated"])
	}
	if intact["body"] != strings.Repeat("b", contracts.DispatchEventBodyMax) {
		t.Fatalf("the event of a comment at the event's share carries %d units, want all %d",
			itemblocks.Units16(intact["body"].(string)), contracts.DispatchEventBodyMax)
	}
	if _, said := intact["body_truncated"]; said {
		t.Fatalf("the event of a whole comment body carries body_truncated=%v, want no such key", intact["body_truncated"])
	}
}

// A message event carries its body the same way: the first part of a long one, all of a short.
func TestAMessageEventCarriesALongBodyInPartAndSaysSo(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "EVM", "Event body", "A spec")

	long := paragraphBody(3, 15_000)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": long}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("post the message: status=%d body=%.400s", created.Code, created.Body.String())
	}
	message := decodeBody[model.Message](t, created)
	if message.Body != long {
		t.Fatalf("the stored message is %d units, want the %d sent", itemblocks.Units16(message.Body), itemblocks.Units16(long))
	}

	payloads := eventPayloads(t, handler, issue.Key, "message.created")
	if len(payloads) != 1 {
		t.Fatalf("the issue carries %d message.created events, want one", len(payloads))
	}
	wantCut := paragraphBody(2, 15_000)
	if payloads[0]["body"] != wantCut || payloads[0]["body_truncated"] != true {
		t.Fatalf("the event of a three-paragraph message carries %d units (body_truncated=%v), want its first two paragraphs (%d units) and true",
			itemblocks.Units16(payloads[0]["body"].(string)), payloads[0]["body_truncated"], itemblocks.Units16(wantCut))
	}
}

// An anchor refresh rewrites the comment's event payload from the stored row, so it takes the
// same share: a long body refreshed carries its first part and says so, rather than the whole
// body no event can hold.
func TestAnAnchorRefreshCarriesTheCutBody(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "EVA", "Anchor refresh", "The quick brown fox")

	long := paragraphBody(3, 15_000)
	anchored := decodeBody[model.Comment](t, sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": long, "anchor": map[string]any{"artifact": "spec", "quote": "fox"},
			"actor": sessionActor(),
		}))
	competing := decodeBody[model.Comment](t, sessionRequest(t, handler, http.MethodPost,
		"/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Rename the animal.", "anchor": map[string]any{"artifact": "spec", "quote": "brown fox"},
			"suggestion": map[string]string{"replace_with": "brown dog"}, "actor": sessionActor(),
		}))
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+competing.ID+"/accept",
		map[string]any{}, "alice")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept the competing suggestion: status=%d body=%.400s", accepted.Code, accepted.Body.String())
	}

	payloads := eventPayloads(t, handler, issue.Key, "comment.anchor_refreshed")
	if len(payloads) != 1 {
		t.Fatalf("the issue carries %d comment.anchor_refreshed events, want one", len(payloads))
	}
	refreshed := payloads[0]
	if refreshed["id"] != anchored.ID {
		t.Fatalf("the refresh names comment %v, want the anchored one %s", refreshed["id"], anchored.ID)
	}
	wantCut := paragraphBody(2, 15_000)
	if refreshed["body"] != wantCut || refreshed["body_truncated"] != true {
		t.Fatalf("the refresh carries %d units (body_truncated=%v), want the first two paragraphs (%d units) and true",
			itemblocks.Units16(refreshed["body"].(string)), refreshed["body_truncated"], itemblocks.Units16(wantCut))
	}
}
