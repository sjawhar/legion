package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// Every body a caller writes is at most contracts.DispatchBodyMax UTF-16 units, on each of the
// four routes that take one: a comment, a comment's edit, a message and a broadcast. A body of
// exactly the cap is accepted and written back whole - the cap is not an approximation that
// truncates - and one unit over it is refused 400 CAP_EXCEEDED naming the count, by the same
// wording on every route (INFRA-45).
func TestBodiesAreCappedOnEveryWriteRoute(t *testing.T) {
	listener, _ := newBroadcastListener(t, broadcastSessions)
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "CAP", "Body cap", "before")
	edited := createThreadComment(t, handler, issue.Key, map[string]any{"body": "before the edit"}, "alice")
	atCap := strings.Repeat("a", contracts.DispatchBodyMax)
	overCap := overCapCases(contracts.DispatchBodyMax)

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
			accepted := route.send(atCap)
			if accepted.Code != route.accepted {
				t.Fatalf("a body of %d units: status=%d body=%.300s, want %d", len16(atCap), accepted.Code, accepted.Body.String(), route.accepted)
			}
			written := decodeBody[struct {
				Body string `json:"body"`
			}](t, accepted).Body
			if written != atCap {
				t.Fatalf("the body written back is %d units, want the %d sent", len16(written), contracts.DispatchBodyMax)
			}
			if route.settle != nil {
				route.settle(t, accepted)
			}
			for caseName, test := range overCap {
				t.Run(caseName, func(t *testing.T) {
					assertCapExceeded(t, name, route.send(test.text), "body", test.length, contracts.DispatchBodyMax, "")
				})
			}
		})
	}
}
