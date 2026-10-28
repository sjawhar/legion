package api

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
)

// A listener that answers none of a session's lookups, its comment-time lookup or its send's
// recheck, delivers no mention copy: the mention's attempt fails. A session the comment's route or
// its author route reaches must then still hear the comment from that route's copy on its own
// topic, as it does when no mention names it, rather than from no copy at all.
func TestASilentListenerStillLeavesTheSessionOneCopyOfTheComment(t *testing.T) {
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
			// Never released: every lookup of s1 waits until its caller gives up.
			_ = state.holdLookups("/v1/registry/s1")
			handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
			issue := createInteractionIssue(t, handler, "TEST", "A silent listener", "before")
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
			if len(settled) != 1 || settled[0].State != "failed" {
				t.Fatalf("the mention to a listener that never answers = %#v, want its one attempt failed", settled)
			}
			if sends := state.sent(); len(sends) != 0 {
				t.Fatalf("the listener took sends %#v, want none: it answered nothing", sends)
			}
			if !publishedToSession(t, deps, issue.Key, created.ID, "s1") {
				t.Fatalf("no event of comment %s was published to %s, and its mention failed: s1 got no copy of the comment",
					created.ID, contracts.AgentSubject("s1"))
			}
		})
	}
}

// publishedToSession runs the outbox until every event of issueKey is published, then reports
// whether any event about commentID (its comment.created, a comment.delivery, any other) was
// published to the session's own topic.
func publishedToSession(t *testing.T, deps Deps, issueKey, commentID, sessionID string) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.Run(ctx, outbox.Deps{Store: deps.Store, Publisher: acceptingPublisher{}, Broker: events.NewBroker(), Docs: deps.Docs})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var unpublished int
		if err := deps.Store.Pool.QueryRow(context.Background(),
			`select count(*) from events where issue_key = $1 and published_at is null`, issueKey).Scan(&unpublished); err != nil {
			t.Fatalf("count unpublished events of %s: %v", issueKey, err)
		}
		if unpublished == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d events of %s unpublished", unpublished, issueKey)
		}
		time.Sleep(10 * time.Millisecond)
	}
	rows, err := deps.Store.Pool.Query(context.Background(), `
		select published_destinations from events
		where issue_key = $1 and (payload->>'id' = $2 or payload->>'comment_id' = $2)
	`, issueKey, commentID)
	if err != nil {
		t.Fatalf("read the destinations of comment %s's events: %v", commentID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var destinations []string
		if err := rows.Scan(&destinations); err != nil {
			t.Fatalf("scan destinations: %v", err)
		}
		if slices.Contains(destinations, contracts.AgentSubject(sessionID)) {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read destinations: %v", err)
	}
	return false
}

// A process stopped (or killed) with a comment's mention send queued leaves its attempt pending
// with no claim. Once it has waited a lease, the outbox counts it abandoned and publishes the route
// copy it had held back, and the next process's stranded pass may also send it. The two must be
// exclusive for one (comment, session): here the outbox acts first, and the session still ends up
// with exactly one copy, whichever of the two delivers it.
func TestAnAbandonedMentionIsDeliveredOnceByTheOutboxOrTheStrandedPassNotBoth(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL, lifetime: stopping})
	issue := createInteractionIssue(t, handler, "TEST", "A stopped process's mention", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s1",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}

	// The process is stopping: the comment commits and its send never starts.
	stop()
	stranded := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Accepted while stopping.", "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if !unclaimedPending(t, database, stranded.ID, "session:s1") {
		t.Fatal("the attempt of a comment accepted while the process stopped was taken")
	}
	ageAttempts(t, database, stranded.ID)

	// The outbox reaches the comment first.
	routeCopy := publishedToSession(t, deps, issue.Key, stranded.ID, "s1")

	// Then the next process's stranded pass runs.
	passes := make(chan struct{}, 1)
	lifetime, end := context.WithCancel(t.Context())
	next := newMentionSender(lifetime, 20*time.Millisecond, served.sendCommentMention, countedStranded(served, passes))
	next.start()
	awaitPasses(t, passes, 3)
	waitForCommentDeliveries(t, database, stranded.ID, "no attempt pending", func(attempts []model.CommentDelivery) bool {
		for _, attempt := range attempts {
			if attempt.State == "pending" {
				return false
			}
		}
		return true
	})
	end()
	next.wait()

	copies := 0
	if routeCopy {
		copies++
	}
	for _, send := range state.sent() {
		if send.session == "s1" {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("s1 received %d copies of the comment (route copy published: %v, mention sends: %#v), want exactly 1",
			copies, routeCopy, state.sent())
	}
}
