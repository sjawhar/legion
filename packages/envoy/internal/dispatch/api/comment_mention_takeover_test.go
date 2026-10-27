package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// A person's comment on an issue routed to session s1 that also mentions s1 reaches s1 once:
// either the mention steers it there and the route fan-out stands aside (suppress_route), or the
// mention does not reach s1 and the route fan-out does. The listener being replaced when the
// comment is written - its registry read answered 503 once - must not make it both: the
// creating transaction decides suppress_route from the mention's pre-commit resolution, so a
// mention that only resolves on the post-commit take-over retry steers s1 a comment the route
// fan-out also delivers, under a different key the listener cannot recognise as a repeat.
func TestACommentDuringAListenerTakeOverReachesTheRouteHolderOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	issue := createInteractionIssue(t, handler, "TEST", "Routed take-over", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"route": "session:s1"}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("route the issue to s1: status=%d body=%s", patched.Code, patched.Body.String())
	}

	// The listener is starting when the comment's mention is resolved, and answers from then on.
	listener.play(takeOverAnswer{takeOverRegistry, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)})
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "@s1, please look at this.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))

	var suppressRoute bool
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce((payload->>'suppress_route')::boolean, false)
		from events where type = 'comment.created' and payload->>'id' = $1
	`, created.ID).Scan(&suppressRoute); err != nil {
		t.Fatalf("read the comment's suppress_route: %v", err)
	}
	mentionSends := 0
	for _, send := range listener.published() {
		if send.target == "s1" {
			mentionSends++
		}
	}
	deliveries := eventDeliveries(t, database, deps.Docs, "comment.created", created.ID)
	if len(deliveries) != 1 {
		t.Fatalf("comment.created events for %s = %#v, want one", created.ID, deliveries)
	}
	routeSends := 0
	for _, topic := range deliveries[0].Destinations {
		if topic == contracts.AgentSubject("s1") {
			routeSends++
		}
	}

	attempts := make([]string, 0, len(created.Deliveries))
	for _, attempt := range created.Deliveries {
		session := "<none>"
		if attempt.SessionID != nil {
			session = *attempt.SessionID
		}
		attempts = append(attempts, fmt.Sprintf("%s attempt %d %s to %s", attempt.Target, attempt.Attempt, attempt.State, session))
	}
	if heard := mentionSends + routeSends; heard != 1 {
		t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d route fan-out(s); "+
			"comment.created suppress_route=%t while the mention's delivery is [%s] - the mention retried through the "+
			"listener take-over after commit and reached s1, but the route suppression was decided from its failed "+
			"pre-commit resolution, so the route holder also gets it",
			heard, mentionSends, routeSends, suppressRoute, strings.Join(attempts, "; "))
	}
}

// Each mention of a session the listener holds no record of waits out the listener take-over
// window before it settles "no live session", and the creating request sends its mentions before
// it answers. Those waits must not stack per mention: a comment mentioning several ended
// sessions answers within one take-over window, well inside the dispatch CLI's 60 s request
// abort, which would otherwise fire after the comment committed and make its retry post the
// comment a second time.
func TestACommentMentioningSeveralEndedSessionsAnswersWithinOneTakeOverWindow(t *testing.T) {
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/") {
			writeSessionRecord(t, w, `[]`, strings.TrimPrefix(r.URL.Path, "/v1/registry/"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer listener.Close()
	handler, _ := newTargetedMessageHandler(t, listener.URL)
	issue := createInteractionIssue(t, handler, "TEST", "Ended sessions", "before")

	ended := []string{"ended-1", "ended-2"}
	mentions := make([]map[string]any, 0, len(ended))
	for _, session := range ended {
		mentions = append(mentions, map[string]any{"target": "session:" + session})
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()
	go func() {
		responses <- postMentionedComment(t, handler, issue.Key, map[string]any{
			"body": "Did either of you finish this?", "mentions": mentions,
		})
	}()
	// The dispatch CLI aborts the request at 60 s (packages/envoy-client/src/dispatch-http.ts).
	const clientAbort = 60 * time.Second
	var response *httptest.ResponseRecorder
	select {
	case response = <-responses:
	case <-time.After(clientAbort):
		t.Fatalf("the comment mentioning %d ended sessions had not answered after %v, when the dispatch CLI aborts it", len(ended), clientAbort)
	}
	took := time.Since(started)
	created := decodeMentionedComment(t, response)
	if len(created.Deliveries) != len(ended) {
		t.Fatalf("deliveries = %#v, want one per mention", created.Deliveries)
	}
	for _, attempt := range created.Deliveries {
		if attempt.State != "failed" {
			t.Fatalf("delivery = %#v, want failed: the mentioned session is ended", attempt)
		}
	}
	if limit := listenerTakeoverWindow + 2*time.Second; took > limit {
		t.Fatalf("the comment mentioning %d ended sessions answered after %v, want within %v: each mention waited out "+
			"its own %v take-over window one after another before the response, so the wait grows with every "+
			"mention and the dispatch CLI's %v abort fires after the comment committed",
			len(ended), took.Round(100*time.Millisecond), limit, listenerTakeoverWindow, clientAbort)
	}
}
