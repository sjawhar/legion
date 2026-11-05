package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func assertNoMentionFallback(t *testing.T, database *store.Store, commentID, sessionID string) {
	t.Helper()
	var destinations []string
	if err := database.Pool.QueryRow(context.Background(), `select published_destinations from events where type='comment.created' and payload->>'id'=$1`, commentID).Scan(&destinations); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(destinations, "notifications.agent."+sessionID) {
		t.Fatalf("explicit delivery was overridden by outbox destinations %v", destinations)
	}
}

func TestMentionCapabilityRefusalIsNotOverriddenByOutbox(t *testing.T) {
	listener, sends := newBroadcastListener(t, `[{"session_id":"s1","title":"Aside only","capabilities":["aside"]}]`)
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "CAP", "Capabilities", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{"body": "Requested steer", "delivery": "steer", "mentions": []map[string]string{{"target": "session:s1"}}}))
	if len(comment.Deliveries) != 1 || comment.Deliveries[0].State != "failed" || comment.Deliveries[0].SessionID == nil {
		t.Fatalf("capability refusal=%+v", comment.Deliveries)
	}
	(&answersFixture{database: database}).publishOutbox(t)
	assertNoMentionFallback(t, database, comment.ID, "s1")
	if keys := keysOf(sends); len(keys) != 0 {
		t.Fatalf("capability refusal sent %v", keys)
	}
}

func TestMentionOutboxLeavesLapsedAttemptToDeliverySweeper(t *testing.T) {
	listener, sends := newBroadcastListener(t, agentSessions)
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: listener.URL})
	issue := createInteractionIssue(t, handler, "RACE", "Sweeper race", "before")
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{"body": "Pending after sender died", "delivery": "steer", "mentions": []map[string]string{{"target": "session:s1"}}}))
	// The send landed, but a process died before settling its outcome. Its expired
	// claim permits the sweeper, not an independent outbox send with another key.
	if _, err := database.Pool.Exec(context.Background(), `update comment_deliveries set state='pending',envelope_id=null,error=null,claimed_at=now()-interval '2 minutes',created_at=now()-interval '2 minutes' where comment_id=$1`, comment.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.New(outbox.Deps{Store: database, Publisher: acceptingPublisher{}, Broker: events.NewBroker(), Listen: database.Pool.Listener, RoleClaims: unclaimedRoles{}, Sessions: unclaimedRoles{}}).Run(ctx)
	}()
	stop := func() { cancel(); <-stopped }
	t.Cleanup(cancel)
	deadline := time.Now().Add(10 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		var recorded bool
		if err := database.Pool.QueryRow(context.Background(), `select cardinality(published_destinations)>0 from events where type='comment.created' and payload->>'id'=$1`, comment.ID).Scan(&recorded); err != nil {
			stop()
			t.Fatal(err)
		}
		if recorded {
			observed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	if !observed {
		t.Fatal("outbox never recorded the creation event's owner topic")
	}
	assertNoMentionFallback(t, database, comment.ID, "s1")
	var published bool
	if err := database.Pool.QueryRow(context.Background(), `select published_at is not null from events where type='comment.created' and payload->>'id'=$1`, comment.ID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("outbox completed a creation event whose mention is still pending")
	}
	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	keys := keysOf(sends)
	want := comment.ID + ":session:s1:steer"
	if len(keys) != 2 || keys[0] != want || keys[1] != want {
		t.Fatalf("sender/sweeper keys=%v want two tries under %s", keys, want)
	}
	if _, err := database.Pool.Exec(context.Background(), `update events set next_attempt_at=null where type='comment.created' and payload->>'id'=$1`, comment.ID); err != nil {
		t.Fatal(err)
	}
	(&answersFixture{database: database}).publishOutbox(t)
	assertNoMentionFallback(t, database, comment.ID, "s1")
}
