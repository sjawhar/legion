package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL, takeoverWindow: listenerTakeoverWindow})
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

// A comment on an issue routed to s1 that mentions s1, written while the listener is replaced
// for longer than the take-over window, decides its route on the mention's failed resolution: the
// mention reached nobody, so suppress_route stays off and the route fan-out carries the comment to
// s1. A sender that dies before it settles that mention leaves attempt 1 pending, holding the
// resolve error the decision was made on. Whoever finishes the attempt later - the delivery
// sweep, or a person's Retry of the pending attempt - must settle it with that resolution: s1
// answers by then, and a mention resolved afresh steers s1 the comment under the mention's own
// key, beside the route's frame the listener cannot recognise it as a repeat of.
func TestAResumedMentionOfARoutedCommentReachesTheRouteHolderOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	direct := directServer(deps)
	issue := createInteractionIssue(t, handler, "TEST", "Routed resume", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key,
		map[string]any{"route": "session:s1"}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("route the issue to s1: status=%d body=%s", patched.Code, patched.Body.String())
	}

	// Both comments are written during one take-over that outlasts the window, so each mention's
	// resolution runs the window out before its comment commits.
	listener.refuse(true)
	responses := make([]*httptest.ResponseRecorder, 2)
	var writes sync.WaitGroup
	for index := range responses {
		writes.Go(func() {
			responses[index] = postMentionedComment(t, handler, issue.Key, map[string]any{
				"body":     fmt.Sprintf("@s1, look at this (%d).", index),
				"mentions": []map[string]any{{"target": "session:s1"}},
			})
		})
	}
	writes.Wait()
	listener.refuse(false)
	comments := make([]mentionedCommentRead, len(responses))
	for index, response := range responses {
		comments[index] = decodeMentionedComment(t, response)
	}

	// heard is how many times s1 has received comment: the mention sends the listener published
	// under the comment's key since the last play, and the route fan-out of its comment.created.
	heard := func(comment mentionedCommentRead) (mentionSends, routeSends int) {
		t.Helper()
		for _, send := range listener.published() {
			if send.target == "s1" && strings.HasPrefix(send.key, comment.ID+":") {
				mentionSends++
			}
		}
		deliveries := eventDeliveries(t, database, deps.Docs, "comment.created", comment.ID)
		if len(deliveries) != 1 {
			t.Fatalf("comment.created events for %s = %#v, want one", comment.ID, deliveries)
		}
		for _, topic := range deliveries[0].Destinations {
			if topic == contracts.AgentSubject("s1") {
				routeSends++
			}
		}
		return mentionSends, routeSends
	}
	for _, comment := range comments {
		if mentionSends, routeSends := heard(comment); mentionSends != 0 || routeSends != 1 {
			t.Fatalf("comment %s reached s1 by %d mention send(s) and %d route fan-out(s), want the route alone: "+
				"its mention's resolution ran the take-over window out before the commit", comment.ID, mentionSends, routeSends)
		}
	}

	finish := []struct {
		name    string
		comment mentionedCommentRead
		run     func(t *testing.T)
	}{
		{"the delivery sweep", comments[0], func(t *testing.T) {
			// strandStoredAttempt leaves the attempt as main's sweep requires an owed one: created
			// over a minute ago, under a claim long lapsed.
			if err := direct.resumeOwedDeliveries(context.Background()); err != nil {
				t.Fatalf("delivery sweep pass: %v", err)
			}
		}},
		{"a person's Retry", comments[1], func(t *testing.T) {
			retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comments[1].ID+"/deliveries",
				map[string]any{"delivery": "steer"}, "alice")
			if retry.Code != http.StatusCreated {
				t.Fatalf("retry the pending attempt: status=%d body=%s", retry.Code, retry.Body.String())
			}
		}},
	}
	for _, tc := range finish {
		t.Run(tc.name, func(t *testing.T) {
			// The comment's sender died before it settled attempt 1, its claim long lapsed.
			strandStoredAttempt(t, database, "comment_deliveries", "comment_id", tc.comment.ID, "'5 minutes'", "'5 minutes'")
			listener.play()
			tc.run(t)

			mentionSends, routeSends := heard(tc.comment)
			states := attemptStates(t, database, "comment_deliveries", "comment_id", tc.comment.ID)
			if heardTimes := mentionSends + routeSends; heardTimes != 1 {
				t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d route "+
					"fan-out(s), attempts %v - the finished attempt resolved the mention afresh after the route was "+
					"decided on its failed resolution", heardTimes, mentionSends, routeSends, states)
			}
			if !slices.Equal(states, []string{"1:failed"}) {
				t.Fatalf("attempts = %v, want attempt 1 alone, settled failed with the resolution the route was decided on", states)
			}
		})
	}
}

// receivedDuringTakeOver posts alice's comment on issueKey mentioning s1, with input's fields beside
// the body and the mention, while the listener is starting - its registry read answers 503 once,
// and it answers from then on - and returns how many times s1 received the comment: the mention
// steers the listener published to s1, and the comment.created event's fan-outs to s1's own topic.
// attempts describes the mention's delivery attempts, for a failure.
func receivedDuringTakeOver(
	t *testing.T, listener *takeOverListener, handler http.Handler, database *store.Store, documentService docs.API,
	issueKey string, input map[string]any,
) (mentionSends, fanOuts int, attempts string) {
	t.Helper()
	body := map[string]any{"body": "@s1, please look at this.", "mentions": []map[string]any{{"target": "session:s1"}}}
	for field, value := range input {
		body[field] = value
	}
	listener.play(takeOverAnswer{takeOverRegistry, answerJSON(http.StatusServiceUnavailable, `{"error":"service starting"}`)})
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issueKey, body))

	for _, send := range listener.published() {
		if send.target == "s1" {
			mentionSends++
		}
	}
	deliveries := eventDeliveries(t, database, documentService, "comment.created", created.ID)
	if len(deliveries) != 1 {
		t.Fatalf("comment.created events for %s = %#v, want one", created.ID, deliveries)
	}
	for _, topic := range deliveries[0].Destinations {
		if topic == contracts.AgentSubject("s1") {
			fanOuts++
		}
	}
	described := make([]string, 0, len(created.Deliveries))
	for _, attempt := range created.Deliveries {
		session := "<none>"
		if attempt.SessionID != nil {
			session = *attempt.SessionID
		}
		described = append(described, fmt.Sprintf("%s attempt %d %s to %s", attempt.Target, attempt.Attempt, attempt.State, session))
	}
	return mentionSends, fanOuts, strings.Join(described, "; ")
}

// A person's comment on an issue that session s1 claims and that has no route reaches s1 from the
// claimant route (the outbox's publishClaimantRoute) unless the comment's mention of s1 delivered
// it. A mention that only resolves on its post-commit take-over retry names no session in the
// comment.created payload, so the claimant route cannot leave s1's copy to it: the comment must
// resolve its mentions through the take-over before it commits, as a routed issue's comment does.
func TestACommentDuringAListenerTakeOverReachesTheClaimantOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	issue := createInteractionIssue(t, handler, "TEST", "Claimed take-over", "before")
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set claimed_by = '{"kind":"session","id":"s1"}'::jsonb, claimed_at = now() where key = $1
	`, issue.Key); err != nil {
		t.Fatalf("claim the issue for s1: %v", err)
	}

	mentionSends, fanOuts, attempts := receivedDuringTakeOver(t, listener, handler, database, deps.Docs, issue.Key, nil)
	if heard := mentionSends + fanOuts; heard != 1 {
		t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d claimant fan-out(s) "+
			"while the mention's delivery is [%s] - the mention committed unresolved and retried through the listener "+
			"take-over after commit, so the claimant route found no mentioned session to leave s1's copy to",
			heard, mentionSends, fanOuts, attempts)
	}
}

// A person's reply on an ask's thread reaches every session that follows the ask - its asker s1
// here - from the follower route (the outbox's publishFollowerRoutes) unless the reply's mention
// of s1 delivered it. The reply names the ask, not a comment to reply to, so it is in a thread with
// an agent even with no route, no reply_to, no anchor and no document: it must resolve its mentions
// through the take-over before it commits too.
func TestAnAskReplyDuringAListenerTakeOverReachesTheFollowerOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	issue := createInteractionIssue(t, handler, "TEST", "Ask thread take-over", "before")
	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Which approach?", "actor": map[string]any{"kind": "session", "id": "s1"},
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("s1 asks: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)

	mentionSends, fanOuts, attempts := receivedDuringTakeOver(t, listener, handler, database, deps.Docs, issue.Key,
		map[string]any{"ask_id": ask.ID})
	if heard := mentionSends + fanOuts; heard != 1 {
		t.Fatalf("session s1 received the reply %d times, want once: %d mention steer(s) and %d follower fan-out(s) "+
			"while the mention's delivery is [%s] - the mention committed unresolved and retried through the listener "+
			"take-over after commit, so the follower route found no mentioned session to leave s1's copy to",
			heard, mentionSends, fanOuts, attempts)
	}
}

// A person's comment on an issue with no route and no session claimant reaches every session the
// issue's primary document credits - s1, which wrote it, here - from the primary document route
// (the outbox's publishPrimaryDocumentAuthors) unless the comment's mention of s1 delivered it. A
// comment on the issue's conversation is on no document itself, so it must resolve its mentions
// through the take-over before it commits once the issue's primary document credits a session.
func TestACommentDuringAListenerTakeOverReachesThePrimaryDocumentAuthorOnce(t *testing.T) {
	listener := startTakeOverListener(t)
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.server.URL})
	issue := createInteractionIssue(t, handler, "TEST", "Spec author take-over", "before")
	if _, err := database.Pool.Exec(context.Background(), `
		update artifacts set created_by = '{"kind":"session","id":"s1"}'::jsonb where id = $1
	`, issue.PrimaryArtifactID); err != nil {
		t.Fatalf("credit s1 with the issue's primary document: %v", err)
	}

	mentionSends, fanOuts, attempts := receivedDuringTakeOver(t, listener, handler, database, deps.Docs, issue.Key, nil)
	if heard := mentionSends + fanOuts; heard != 1 {
		t.Fatalf("session s1 received the comment %d times, want once: %d mention steer(s) and %d primary document "+
			"author fan-out(s) while the mention's delivery is [%s] - the mention committed unresolved and retried "+
			"through the listener take-over after commit, so the author route found no mentioned session to leave "+
			"s1's copy to", heard, mentionSends, fanOuts, attempts)
	}
}
