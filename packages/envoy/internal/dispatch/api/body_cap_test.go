package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// paragraphBody is count paragraphs of width characters each, the shape a body splits into one
// block a paragraph (itemblocks.Split).
func paragraphBody(count, width int) string {
	return strings.TrimSuffix(strings.Repeat(strings.Repeat("a", width)+"\n\n", count), "\n\n")
}

// A body is held to one bound on each of its blocks and a count of its blocks, never to a bound
// on the whole of it: a comment of six 20,000-unit paragraphs - 120,010 units, far past the
// single cap of 32,000 that stood before - is accepted, read back byte for byte and listed as
// its six blocks. A comment whose second paragraph alone passes contracts.DispatchBlockUnits is
// refused naming that block, and the issue's comments are as they were.
func TestACommentIsBoundedBlockByBlockRatherThanAsAWhole(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "BLK", "Block bounds", "before")

	long := paragraphBody(6, 20_000)
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": long}, "alice")
	if comment.Body != long {
		t.Fatalf("the comment written back is %d units, want the %d sent", itemblocks.Units16(comment.Body), itemblocks.Units16(long))
	}
	listed := decodeBody[model.ItemBlocks](t, dispatchRequest(t, handler,
		http.MethodGet, "/api/v1/comments/"+comment.ID+"/blocks", nil, "alice"))
	if !listed.Indexed || len(listed.Blocks) != 6 {
		t.Fatalf("a comment of six 20,000-unit paragraphs lists %d blocks (indexed=%v), want six", len(listed.Blocks), listed.Indexed)
	}

	before := countCommentRows(t, handler, issue.Key)
	overInBlockTwo := strings.Repeat("a", 10) + "\n\n" + strings.Repeat("b", contracts.DispatchBlockUnits+1)
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": overInBlockTwo}, "alice")
	assertBlockCapExceeded(t, "a second paragraph one unit over", refused, "body", 2, "paragraph", contracts.DispatchBlockUnits+1)
	if after := countCommentRows(t, handler, issue.Key); after != before {
		t.Fatalf("the issue holds %d comments after the refusal, want the %d it held", after, before)
	}
}

// Every route that stores a body holds it to the same two bounds, by the same wording: a body of
// two 20,000-unit paragraphs is accepted and written back whole, one paragraph of
// contracts.DispatchBlockUnits+1 units is refused naming the block, and a body of one block more
// than contracts.DispatchItemBlocks is refused for how many blocks it holds (INFRA-370).
func TestBodiesAreBoundedOnEveryWriteRoute(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "CAP", "Body bounds", "before")
	edited := createThreadComment(t, handler, issue.Key, map[string]any{"body": "before the edit"}, "alice")
	accepted := paragraphBody(2, 20_000)
	overCap := overCapCases(contracts.DispatchBlockUnits)
	tooManyBlocks := paragraphBody(contracts.DispatchItemBlocks+1, 1)

	for name, route := range map[string]struct {
		accepted int
		send     func(body string) *httptest.ResponseRecorder
		// settle waits out whatever the accepted write left running, so the test's teardown
		// never cuts it off; nil when the write finished inside its request.
		settle func(t *testing.T, accepted *httptest.ResponseRecorder)
	}{
		"comment create": {accepted: http.StatusCreated, send: func(body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": body}, "alice")
		}},
		"comment edit": {accepted: http.StatusOK, send: func(body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+edited.ID, map[string]any{"body": body}, "alice")
		}},
		"message create": {accepted: http.StatusCreated, send: func(body string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{"body": body}, "alice")
		}},
		"broadcast create": {accepted: http.StatusCreated, send: func(body string) *httptest.ResponseRecorder {
			return postBroadcast(t, handler, map[string]any{
				"body": body, "delivery": "btw", "session_ids": []string{"planner"},
			}, "alice")
		}, settle: func(t *testing.T, accepted *httptest.ResponseRecorder) {
			// The send delivers behind its 201; a delivery still writing its body to the fake
			// listener when the listener closes fails the test.
			awaitBroadcastDeliveries(t, handler, decodeBody[broadcastResponse](t, accepted).ID)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			sent := route.send(accepted)
			if sent.Code != route.accepted {
				t.Fatalf("a body of two 20,000-unit paragraphs: status=%d body=%.300s, want %d", sent.Code, sent.Body.String(), route.accepted)
			}
			written := decodeBody[struct {
				Body string `json:"body"`
			}](t, sent).Body
			if written != accepted {
				t.Fatalf("the body written back is %d units, want the %d sent", itemblocks.Units16(written), itemblocks.Units16(accepted))
			}
			if route.settle != nil {
				route.settle(t, sent)
			}
			for caseName, test := range overCap {
				t.Run(caseName, func(t *testing.T) {
					assertBlockCapExceeded(t, name, route.send(test.text), "body", 1, "paragraph", test.length)
				})
			}
			t.Run("one block over the count", func(t *testing.T) {
				assertBlockCountExceeded(t, name, route.send(tooManyBlocks), "body", contracts.DispatchItemBlocks+1)
			})
		})
	}
}
