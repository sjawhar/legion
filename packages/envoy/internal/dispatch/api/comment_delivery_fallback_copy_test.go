package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A listener that answers delivers the mention to the session the comment's route or its author
// route also reaches, and that session hears the comment once, from the mention: no event of the
// comment is published to its own topic, the copy the silent listener's failed mention gets
// (TestASilentListenerStillLeavesTheSessionOneCopyOfTheComment) included.
func TestALiveListenerLeavesTheMentionedSessionTheMentionsCopyAlone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		route     string
		replyToS1 bool
	}{
		{name: "the issue's route is the mentioned session", route: "session:s1"},
		{name: "the comment replies in the mentioned session's thread", replyToS1: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, state := newMentionListener(t)
			handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
			issue := createInteractionIssue(t, handler, "TEST", "A live listener", "before")
			if tc.route != "" {
				if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
					"route": tc.route,
				}, "alice"); patched.Code != http.StatusOK {
					t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
				}
			}
			input := map[string]any{"body": "Are you there?", "mentions": []map[string]any{{"target": "session:s1"}}}
			if tc.replyToS1 {
				root := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": "Where should this go?", "actor": map[string]any{"kind": "session", "id": "s1"},
				})
				input["reply_to"] = decodeMentionedComment(t, root).ID
			}
			created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, input))

			settled := settledCommentDeliveries(t, database, created.ID)
			if len(settled) != 1 || settled[0].State != "sent" || settled[0].SessionID == nil || *settled[0].SessionID != "s1" {
				t.Fatalf("the mention to a listener that answers = %#v, want its one attempt sent to s1", settled)
			}
			if sends := state.sent(); len(sends) != 1 || sends[0].session != "s1" {
				t.Fatalf("the listener took sends %#v, want the mention's one, to s1", sends)
			}
			if publishedToSession(t, deps, issue.Key, created.ID, "s1") {
				t.Fatalf("an event of comment %s was published to %s as well as the mention's send: s1 got two copies",
					created.ID, contracts.AgentSubject("s1"))
			}
		})
	}
}

// A mention whose send fails, to a session the comment's creation skipped on no route - a reply
// in a thread a person started, on an issue with no route - leaves the comment's comment.created
// event as it was: its payload unchanged, and its publication not reopened.
func TestAFailedMentionOfASessionNothingSkippedLeavesTheCommentCreatedEventAlone(t *testing.T) {
	listener, state := newMentionListener(t)
	// Never released: the send to s2 waits until its caller gives up, and the attempt fails.
	_ = state.hold("s2")
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	issue := createInteractionIssue(t, handler, "TEST", "A failed mention", "before")
	root := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{"body": "Where should this go?"}))
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "reply_to": root.ID, "mentions": []map[string]any{{"target": "session:s2"}},
	}))
	if session := state.awaitSend(t); session != "s2" {
		t.Fatalf("the mention's send went to %s, want s2", session)
	}
	// The send is still held, so its attempt is pending while comment.created is published.
	published := eventDeliveries(t, database, deps.Docs, "comment.created", created.ID)
	if len(published) != 1 || slices.Contains(published[0].Destinations, contracts.AgentSubject("s2")) {
		t.Fatalf("comment.created deliveries = %#v, want the one event's, none of it to s2", published)
	}
	before := commentCreatedRow(t, database, created.ID)
	if before.publishedAt == nil {
		t.Fatalf("comment.created of %s is unpublished after the outbox published it", created.ID)
	}

	settled := settledCommentDeliveries(t, database, created.ID)
	if len(settled) != 1 || settled[0].State != "failed" {
		t.Fatalf("the mention whose send never answered = %#v, want its one attempt failed", settled)
	}
	after := commentCreatedRow(t, database, created.ID)
	if after.payload != before.payload || after.publishedAt == nil || !after.publishedAt.Equal(*before.publishedAt) {
		t.Fatalf("comment.created after the mention failed = %+v, want it as it was before: %+v", after, before)
	}
}

// commentCreatedState is a comment.created row's payload, as text, and its published_at.
type commentCreatedState struct {
	payload     string
	publishedAt *time.Time
}

func commentCreatedRow(t *testing.T, database *store.Store, commentID string) commentCreatedState {
	t.Helper()
	var row commentCreatedState
	if err := database.Pool.QueryRow(context.Background(), `
		select payload::text, published_at from events where type = 'comment.created' and payload->>'id' = $1
	`, commentID).Scan(&row.payload, &row.publishedAt); err != nil {
		t.Fatalf("read comment.created of %s: %v", commentID, err)
	}
	return row
}
