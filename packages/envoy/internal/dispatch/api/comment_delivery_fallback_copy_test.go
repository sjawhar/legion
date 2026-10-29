package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
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

// A mention whose send fails, to a session no copy of the comment reaches - a reply in a thread a
// person started, on an issue with no route - adds no copy: the comment's comment.created payload
// is unchanged, and once the outbox has looked at it again it still reached no topic of s2's.
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
	again := eventDeliveries(t, database, deps.Docs, "comment.created", created.ID)
	if len(again) != 1 || slices.Contains(again[0].Destinations, contracts.AgentSubject("s2")) {
		t.Fatalf("comment.created deliveries after the mention failed = %#v, want none of them to s2", again)
	}
	if after := commentCreatedRow(t, database, created.ID); after.payload != before.payload || after.publishedAt == nil {
		t.Fatalf("comment.created after the mention failed = %+v, want its payload as before and published: %+v", after, before)
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

// The claimant route holds its copy back for a mention of the claimant as the issue route does,
// here on a reply in a thread no session wrote in. A process killed after it claimed that
// mention's attempt, before its send reached the listener, leaves the attempt to the delivery
// sweep; when that resend fails too, the claimant still hears the comment, from the claimant copy
// the outbox held for it.
func TestAClaimantWhoseResentMentionFailsStillHearsTheComment(t *testing.T) {
	listener, state := newMentionListener(t)
	stopping, stop := context.WithCancel(t.Context())
	handler, database, deps, served := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: time.Second, lifetime: stopping,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A claimed issue", "before")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/claim", map[string]any{
		"actor": claimActorBody("s1", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("claim the issue as s1: status=%d body=%s", response.Code, response.Body.String())
	}
	// A reply in a thread a person started: no session wrote in it, so only the claimant route
	// holds s1's copy back for the mention.
	root := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{"body": "Where should this go?"}))
	stop()
	claimed := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "For the claimant.", "reply_to": root.ID, "mentions": []map[string]any{{"target": "session:s1"}},
	}))
	if _, err := database.Pool.Exec(context.Background(), `
		update comment_deliveries set claimed_at = now() - `+model.DeliveryClaimLapseInterval+` - interval '1 second'
		where comment_id = $1
	`, claimed.ID); err != nil {
		t.Fatalf("lapse the killed process's claim: %v", err)
	}
	ageAttempts(t, database, claimed.ID)
	_ = state.holdLookups("/v1/registry/s1")

	published := eventDeliveries(t, database, deps.Docs, "comment.created", claimed.ID)
	if len(published) != 1 || slices.Contains(published[0].Destinations, contracts.AgentSubject("s1")) {
		t.Fatalf("comment.created deliveries = %#v, want the one event's, its claimant copy left to the resend", published)
	}

	if err := served.resumeOwedDeliveries(context.Background()); err != nil {
		t.Fatalf("resume the owed deliveries: %v", err)
	}
	settled := waitForCommentDeliveries(t, database, claimed.ID, "the resend settled", func(attempts []model.CommentDelivery) bool {
		return len(attempts) == 1 && attempts[0].State != "pending"
	})
	if settled[0].State != "failed" {
		t.Fatalf("the resend to a listener that answers nothing = %#v, want it failed", settled)
	}

	if copies := publishedFallbackCopies(t, deps, issue.Key, claimed.ID); !slices.Contains(copies, contracts.AgentSubject("s1")) {
		t.Fatalf("fallback copies published after the resend failed = %#v, want s1's topic among them", copies)
	}
	if sends := state.sent(); len(sends) != 0 {
		t.Fatalf("the listener took sends %#v, want none", sends)
	}
}

// publishedFallbackCopies runs the outbox until every event of issueKey is published and every
// copy of commentID it held back for a resent mention is settled, and returns the destinations of
// the held copies it published.
func publishedFallbackCopies(t *testing.T, deps Deps, issueKey, commentID string) []string {
	t.Helper()
	runOutboxUntilSettled(t, deps, acceptingPublisher{}, issueKey, commentID)
	rows, err := deps.Store.Pool.Query(context.Background(), `
		select destination from comment_fallback_copies where comment_id = $1 and published order by destination
	`, commentID)
	if err != nil {
		t.Fatalf("read the published fallback copies of %s: %v", commentID, err)
	}
	copies, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("decode the published fallback copies of %s: %v", commentID, err)
	}
	return copies
}

// runOutboxUntilSettled runs the outbox through publisher until every event of issueKey is
// published and every copy of commentID it held back for a resent mention is settled, for at most
// 10 s.
func runOutboxUntilSettled(t *testing.T, deps Deps, publisher outbox.Publisher, issueKey, commentID string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		outbox.Run(ctx, outbox.Deps{Store: deps.Store, Publisher: publisher, Broker: events.NewBroker(), Docs: deps.Docs})
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var unpublished, unsettled int
		if err := deps.Store.Pool.QueryRow(context.Background(), `
			select (select count(*) from events where issue_key = $1 and published_at is null),
			       (select count(*) from comment_fallback_copies where comment_id = $2 and settled_at is null)
		`, issueKey, commentID).Scan(&unpublished, &unsettled); err != nil {
			t.Fatalf("count the outbox's work on %s: %v", issueKey, err)
		}
		if unpublished == 0 && unsettled == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left %d events of %s unpublished and %d copies of %s unsettled", unpublished, issueKey, unsettled, commentID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordingOutboxPublisher takes every envelope the outbox publishes and records it.
type recordingOutboxPublisher struct {
	mu    sync.Mutex
	items []contracts.Envelope
}

func (p *recordingOutboxPublisher) Window() bus.Sender { return p }

func (p *recordingOutboxPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	p.mu.Lock()
	p.items = append(p.items, item)
	p.mu.Unlock()
	accepted := make(chan error, 1)
	accepted <- nil
	return accepted
}

// topicsAbout returns, in the order they were published, the topics of the envelopes whose event
// is about commentID (its comment.created, a comment.delivery, any other).
func (p *recordingOutboxPublisher) topicsAbout(t *testing.T, commentID string) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	var topics []string
	for _, item := range p.items {
		var event struct {
			Payload struct {
				ID        string `json:"id"`
				CommentID string `json:"comment_id"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(item.Payload), &event); err != nil {
			t.Fatalf("decode the envelope published to %s: %v", item.Topic, err)
		}
		if event.Payload.ID == commentID || event.Payload.CommentID == commentID {
			topics = append(topics, item.Topic)
		}
	}
	return topics
}

// A mentioned session whose mention failed was left by the outbox's publish of the comment to the
// route it read then. Once the issue's route has moved, a person's Retry of that mention that
// fails too reaches no new destination: the outbox publishes nothing of the comment to the new
// route, and the comment.created row keeps the publication its first publish recorded.
func TestAFailedRetryOfAMentionSendsTheCommentToNoNewDestination(t *testing.T) {
	listener, state := newMentionListener(t)
	// Never released: every send to s2 waits until its caller gives up, and its attempt fails.
	_ = state.hold("s2")
	handler, database, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL, envoyTimeout: time.Second})
	issue := createInteractionIssue(t, handler, "TEST", "A moved route", "before")
	route := func(to string) {
		t.Helper()
		if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
			"route": to,
		}, "alice"); patched.Code != http.StatusOK {
			t.Fatalf("route the issue to %s: status=%d body=%s", to, patched.Code, patched.Body.String())
		}
	}
	route("role:planner")
	// A reply in a thread a person started: no route but the issue's reaches s2.
	root := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{"body": "Where should this go?"}))
	created := decodeMentionedComment(t, postMentionedComment(t, handler, issue.Key, map[string]any{
		"body": "Are you there?", "reply_to": root.ID, "mentions": []map[string]any{{"target": "session:s2"}},
	}))
	if settled := settledCommentDeliveries(t, database, created.ID); len(settled) != 1 || settled[0].State != "failed" {
		t.Fatalf("the mention whose send never answered = %#v, want its one attempt failed", settled)
	}
	published := eventDeliveries(t, database, deps.Docs, "comment.created", created.ID)
	if len(published) != 1 || !slices.Contains(published[0].Destinations, contracts.RoleTopicPrefix+"planner") {
		t.Fatalf("comment.created deliveries = %#v, want the one event's, the planner route among them", published)
	}
	before := commentCreatedRow(t, database, created.ID)

	route("role:implementer")
	retry := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+created.ID+"/deliveries", map[string]any{
		"delivery": "steer",
	}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.State != "failed" {
		t.Fatalf("the retried mention = %#v, want it failed", attempt)
	}

	publisher := &recordingOutboxPublisher{}
	runOutboxUntilSettled(t, deps, publisher, issue.Key, created.ID)
	if topics := publisher.topicsAbout(t, created.ID); slices.Contains(topics, contracts.RoleTopicPrefix+"implementer") {
		t.Fatalf("the outbox published %#v about the comment once its retry failed, want nothing to the new route", topics)
	}
	after := commentCreatedRow(t, database, created.ID)
	if after.publishedAt == nil || !after.publishedAt.Equal(*before.publishedAt) || after.payload != before.payload {
		t.Fatalf("comment.created after the failed retry = %+v, want it as its first publish left it: %+v", after, before)
	}
	again := eventDeliveries(t, database, deps.Docs, "comment.created", created.ID)
	if len(again) != 1 || !slices.Equal(again[0].Destinations, published[0].Destinations) {
		t.Fatalf("comment.created deliveries after the failed retry = %#v, want them as they were: %#v", again, published)
	}
}
