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

// heldCopy is one comment_fallback_copies row a test reads: where it would go, whether a pass has
// settled it and whether that pass published it.
type heldCopy struct {
	destination string
	settled     bool
	published   bool
}

// fallbackCopiesOf is every fallback row the outbox holds for the comment, by destination.
func fallbackCopiesOf(t *testing.T, database *store.Store, commentID string) []heldCopy {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select destination, settled_at is not null, published from comment_fallback_copies
		where comment_id = $1 order by destination
	`, commentID)
	if err != nil {
		t.Fatalf("read the fallback copies of %s: %v", commentID, err)
	}
	copies, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (heldCopy, error) {
		var held heldCopy
		return held, row.Scan(&held.destination, &held.settled, &held.published)
	})
	if err != nil {
		t.Fatalf("decode the fallback copies of %s: %v", commentID, err)
	}
	return copies
}

// awaitFallbackCopies waits, for at most 10 s, until the comment has a fallback row for every
// destination in want and each of them reads settled, and answers the rows then. want is the
// copies the outbox is to hold; settled says whether to wait for a pass to decide them.
func awaitFallbackCopies(
	t *testing.T, database *store.Store, commentID string, want []string, settled bool,
) []heldCopy {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		copies := fallbackCopiesOf(t, database, commentID)
		destinations := make([]string, 0, len(copies))
		decided := true
		for _, row := range copies {
			destinations = append(destinations, row.destination)
			decided = decided && row.settled == settled
		}
		if slices.Equal(destinations, want) && decided {
			return copies
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fallback copies of %s are %#v, want one per %v, settled=%v", commentID, copies, want, settled)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitPublishedCommentCreated waits, for at most 10 s, until the outbox has published the
// comment.created event of commentID, and answers the destinations it reached.
func awaitPublishedCommentCreated(t *testing.T, database *store.Store, commentID string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var destinations []string
		var publishedAt *time.Time
		if err := database.Pool.QueryRow(context.Background(), `
			select published_at, published_destinations from events
			where type = 'comment.created' and payload->>'id' = $1
		`, commentID).Scan(&publishedAt, &destinations); err != nil {
			t.Fatalf("read comment.created of %s: %v", commentID, err)
		}
		if publishedAt != nil {
			return destinations
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox left the comment.created of %s unpublished", commentID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// heldLaneComment is a comment whose mention of s1 waits on a lane held longer than a claim
// lease, the outbox running beside it: the issue it is on, what that outbox published, the
// comment, and the release that lets the queued send run.
type heldLaneComment struct {
	database  *store.Store
	issue     string
	published *recordingOutboxPublisher
	comment   string
	release   func()
	listener  *mentionListener
}

// queuedMention is a mention of s1 waiting on a lane a stalled first send holds: the server the
// comments were posted to, the issue they are on, the comment whose send is stalled at the
// listener, the comment queued behind it, the listener and the release that lets the queued send
// run. Its attempts are as young as the comments; a caller that wants the lane held past a claim
// lease ages them (ageAttempts).
type queuedMention struct {
	handler  http.Handler
	database *store.Store
	deps     Deps
	issue    string
	sending  string
	queued   string
	release  func()
	listener *mentionListener
}

// aMentionQueuedBehindAStalledSend posts two comments mentioning s1 on an issue routed to s1, the
// first send stalled at the listener so the second waits on s1's lane. The route is the
// destination that would reach s1, so it is the copy the outbox decides. The sender holds the
// queued attempt's claim throughout, writing it again every mentionClaimRefresh: it is the live
// claim the outbox bounds its wait by however long the lane is held.
func aMentionQueuedBehindAStalledSend(t *testing.T) queuedMention {
	t.Helper()
	listener, state := newMentionListener(t)
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{
		envoyURL: listener.URL, envoyTimeout: 30 * time.Second,
	})
	issue := createInteractionIssue(t, handler, "TEST", "A lane held past a lease", "before")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"route": "session:s1",
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("set the route: status=%d body=%s", patched.Code, patched.Body.String())
	}
	reached, release := state.stall("s1")
	t.Cleanup(release)
	sending := postMentionOfS1(t, handler, issue.Key, "first")
	<-reached
	queued := postMentionOfS1(t, handler, issue.Key, "second")
	if !claimedPending(t, database, queued, "session:s1") {
		t.Fatal("the attempt waiting on the held lane is not held by the sender")
	}
	return queuedMention{
		handler: handler, database: database, deps: deps, issue: issue.Key,
		sending: sending, queued: queued, release: release, listener: state,
	}
}

// aMentionQueuedPastALease is a mention queued behind a stalled send
// (aMentionQueuedBehindAStalledSend) with both attempts moved a claim lease into the past - the
// lane held that long - and the outbox run over them.
func aMentionQueuedPastALease(t *testing.T) heldLaneComment {
	t.Helper()
	lane := aMentionQueuedBehindAStalledSend(t)
	ageAttempts(t, lane.database, lane.sending, lane.queued)
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)
	return heldLaneComment{
		database: lane.database, issue: lane.issue, published: published, comment: lane.queued,
		release: lane.release, listener: lane.listener,
	}
}

// A mention waiting on a lane held longer than a claim lease does not hold its comment's event
// with it: the outbox publishes that event to every other destination and keeps the mentioned
// session's copy as a comment_fallback_copies row instead. When the queued send then lands, the
// row settles with nothing published, so the session hears the comment once, from the mention.
func TestAMentionQueuedPastALeaseHoldsItsCopyAndSettlesItWhenTheSendLands(t *testing.T) {
	held := aMentionQueuedPastALease(t)
	destinations := awaitPublishedCommentCreated(t, held.database, held.comment)
	if slices.Contains(destinations, contracts.AgentSubject("s1")) {
		t.Fatalf("comment.created reached %v, want s1's own topic left to the mention", destinations)
	}
	if !slices.Contains(destinations, "notifications.dispatch.issue."+held.issue+".comment.created") {
		t.Fatalf("comment.created reached %v, want the issue's own topic among them: only s1's copy waits",
			destinations)
	}
	awaitFallbackCopies(t, held.database, held.comment, []string{contracts.AgentSubject("s1")}, false)

	held.release()
	if settled := settledCommentDeliveries(t, held.database, held.comment); !allSent(settled) {
		t.Fatalf("the queued mention = %#v, want it sent once its lane came to it", settled)
	}
	copies := awaitFallbackCopies(t, held.database, held.comment, []string{contracts.AgentSubject("s1")}, true)
	if copies[0].published {
		t.Fatalf("the fallback copy = %#v, want it settled with nothing published: the mention delivered the comment", copies[0])
	}
	if topics := held.published.topicsAbout(t, held.comment); slices.Contains(topics, contracts.AgentSubject("s1")) {
		t.Fatalf("the outbox published %v about the comment, want nothing to s1's own topic", topics)
	}
}

// The copy held for a mention waiting on a held lane is the session's only one when that send
// fails: the fallback pass publishes it to that destination alone, once.
func TestAMentionQueuedPastALeasePublishesItsCopyWhenTheSendFails(t *testing.T) {
	held := aMentionQueuedPastALease(t)
	awaitPublishedCommentCreated(t, held.database, held.comment)
	awaitFallbackCopies(t, held.database, held.comment, []string{contracts.AgentSubject("s1")}, false)

	held.listener.refuseSends("s1")
	held.release()
	settled := waitForCommentDeliveries(t, held.database, held.comment, "the queued send failed",
		func(attempts []model.CommentDelivery) bool {
			return len(attempts) == 1 && attempts[0].State == "failed"
		})
	if settled[0].Error == nil {
		t.Fatalf("the refused send = %#v, want its cause recorded", settled[0])
	}
	copies := awaitFallbackCopies(t, held.database, held.comment, []string{contracts.AgentSubject("s1")}, true)
	if !copies[0].published {
		t.Fatalf("the fallback copy = %#v, want it published: the mention's send failed", copies[0])
	}
	topics := held.published.topicsAbout(t, held.comment)
	var copiesToS1 int
	for _, topic := range topics {
		if topic == contracts.AgentSubject("s1") {
			copiesToS1++
		}
	}
	if copiesToS1 != 1 {
		t.Fatalf("the outbox published %d copies to s1's own topic (%v), want the held copy alone", copiesToS1, topics)
	}
}

// A Retry of a mention that is waiting on a held lane opens an attempt of its own, which
// overtakes the queued one (claimCommentDeliveryAttempt); when that Retry fails, the comment's
// event is not held with the attempt it overtook. Once the overtaken attempt is past a claim
// lease the outbox counts it resumed whoever holds it (model.MentionResumedSQL), so it publishes
// the comment to every other destination at once and keeps s1's copy as a fallback row, which
// waits while that attempt is pending. The lane then settles the attempt superseded without
// sending it, and the pass publishes the copy: s1 hears the comment exactly once, from the
// outbox.
func TestARetryThatFailsDoesNotHoldTheCommentOfTheAttemptItOvertook(t *testing.T) {
	lane := aMentionQueuedBehindAStalledSend(t)
	lane.listener.refuseSends("s1")
	retry := dispatchRequest(t, lane.handler, http.MethodPost, "/api/v1/comments/"+lane.queued+"/deliveries",
		map[string]any{"delivery": "steer"}, "alice")
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry the queued mention: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if attempt := decodeBody[commentDeliveryRead](t, retry); attempt.Attempt != 2 || attempt.State != "failed" {
		t.Fatalf("the Retry = %#v, want an attempt 2 of its own, failed: attempt 1 waits on the held lane "+
			"and s1's host refused this send", attempt)
	}
	ageAttempts(t, lane.database, lane.sending, lane.queued)
	published := &recordingOutboxPublisher{}
	runOutboxBeside(t, lane.deps, published)

	destinations := awaitPublishedCommentCreated(t, lane.database, lane.queued)
	if slices.Contains(destinations, contracts.AgentSubject("s1")) {
		t.Fatalf("comment.created reached %v, want s1's own topic held as a fallback row", destinations)
	}
	if !slices.Contains(destinations, "notifications.dispatch.issue."+lane.issue+".comment.created") {
		t.Fatalf("comment.created reached %v, want the issue's own topic among them: the overtaken attempt "+
			"holds the event no longer", destinations)
	}
	awaitFallbackCopies(t, lane.database, lane.queued, []string{contracts.AgentSubject("s1")}, false)

	// The lane reaches the attempt the Retry overtook and settles it superseded, sending nothing.
	lane.release()
	settled := waitForCommentDeliveries(t, lane.database, lane.queued, "both attempts settled",
		func(attempts []model.CommentDelivery) bool {
			return len(attempts) == 2 && attempts[0].State != "pending" && attempts[1].State != "pending"
		})
	if settled[0].State != "failed" || settled[0].Error == nil || *settled[0].Error != mentionSupersededError {
		t.Fatalf("the overtaken attempt = %#v, want it settled superseded once its lane reached it", settled[0])
	}
	copies := awaitFallbackCopies(t, lane.database, lane.queued, []string{contracts.AgentSubject("s1")}, true)
	if !copies[0].published {
		t.Fatalf("the fallback copy = %#v, want it published: no attempt of the mention was sent", copies[0])
	}
	topics := published.topicsAbout(t, lane.queued)
	var copiesToS1 int
	for _, topic := range topics {
		if topic == contracts.AgentSubject("s1") {
			copiesToS1++
		}
	}
	if copiesToS1 != 1 {
		t.Fatalf("the outbox published %d copies to s1's own topic (%v), want the held copy alone", copiesToS1, topics)
	}
	var sendsOfTheComment int
	for _, send := range lane.listener.sent() {
		if send.body == "second" {
			sendsOfTheComment++
		}
	}
	if sendsOfTheComment != 1 {
		t.Fatalf("the listener took %d sends of the queued comment, want the refused Retry alone: the "+
			"overtaken attempt sends nothing", sendsOfTheComment)
	}
}
