package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// mentionsOf is a payload's mentions as the scan decodes them from JSON, each resolved to its
// session.
func mentionsOf(sessionIDs ...string) []any {
	mentions := make([]any, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		mentions = append(mentions, map[string]any{
			"target": "session:" + sessionID, "delivery": "steer", "session_id": sessionID,
		})
	}
	return mentions
}

// seedMentionDelivery records an attempt of commentID's mention of sessionID, pinned to it
// (seedDelivery).
func seedMentionDelivery(t *testing.T, database *store.Store, commentID, sessionID, outcome string) {
	t.Helper()
	seedDelivery(t, database, commentID, "session:"+sessionID, sessionID, outcome)
}

// seedDelivery records an attempt of commentID's mention of target, as the comment handler and
// its sender leave it, inserting the comment first when no test seeded it. sessionID is the
// session the attempt names, "" for none: an unanswered mention's attempt names none until its
// sender resolves it. outcome is the attempt's state - "sent", "failed", or "pending" for a send
// its sender claimed just now - or "resumed" for a pending attempt whose sender's claim lapsed
// two minutes ago (its sender died with the send in flight; the API's stranded pass resends it),
// "queued" for a pending attempt created just now that no sender has claimed yet, and
// "backlogged" or "abandoned" for one created two minutes ago that no sender ever claimed. ""
// records no attempt.
func seedDelivery(t *testing.T, database *store.Store, commentID, target, sessionID, outcome string) {
	t.Helper()
	if outcome == "" {
		return
	}
	state, age, claimed := outcome, time.Duration(0), true
	switch outcome {
	case "resumed":
		state, age = "pending", 2*time.Minute
	case "queued":
		state, claimed = "pending", false
	case "backlogged", "abandoned":
		state, age, claimed = "pending", 2*time.Minute, false
	}
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (id, issue_key, author, body) values ($1, 'T-1', '{"kind":"user","id":"alice"}', 'A mention.')
		on conflict (id) do nothing
	`, commentID); err != nil {
		t.Fatalf("seed mentioning comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, created_at, claimed_at)
		values (
			$1, $2,
			(select coalesce(max(attempt), 0) + 1 from comment_deliveries where comment_id = $1 and target = $2),
			'steer', nullif($3, ''), $4, now() - $5::interval, case when $6 then now() - $5::interval end
		)
	`, commentID, target, sessionID, state, age, claimed); err != nil {
		t.Fatalf("seed %s delivery of the mention of %s: %v", outcome, target, err)
	}
}

// TestCommentPayloadSuppressesResolvedRouteAndAuthorSessions: the route and an author a new
// comment's mentions resolved to are left to the mention only once it reached them.
func TestCommentPayloadSuppressesResolvedRouteAndAuthorSessions(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "mentioned"}, "Root comment", nil)
	routeSuppression := func(id string) map[string]any {
		return map[string]any{
			"id":                          id,
			"mentions":                    mentionsOf("mentioned"),
			"suppress_route":              true,
			"suppressed_route":            "session:mentioned",
			"suppressed_route_session_id": "mentioned",
		}
	}
	authorSuppression := func(id string) map[string]any {
		return map[string]any{
			"id": id, "reply_to": rootID,
			"mentions":           mentionsOf("mentioned"),
			"suppressed_authors": []string{"mentioned"},
		}
	}
	for _, tc := range []struct {
		name    string
		route   *string
		payload map[string]any
		// delivery is the outcome of the mention of "mentioned" (seedMentionDelivery).
		delivery   string
		wantTopics []string
		wantErr    error
	}{
		{
			name:       "suppressed owner route",
			route:      new("session:mentioned"),
			payload:    routeSuppression("00000000-0000-0000-0000-000000000001"),
			delivery:   "sent",
			wantTopics: []string{"notifications.dispatch.issue.T-1.comment.created"},
		},
		{
			name:     "owner route whose mention failed",
			route:    new("session:mentioned"),
			payload:  routeSuppression("00000000-0000-0000-0000-000000000006"),
			delivery: "failed",
			wantTopics: []string{
				"notifications.dispatch.issue.T-1.comment.created",
				"notifications.agent.mentioned",
			},
		},
		{
			name:       "owner route whose mention is still being sent",
			route:      new("session:mentioned"),
			payload:    routeSuppression("00000000-0000-0000-0000-000000000007"),
			delivery:   "pending",
			wantTopics: []string{"notifications.dispatch.issue.T-1.comment.created"},
			wantErr:    errMentionPending,
		},
		{
			name:       "suppressed author route",
			route:      new("session:someone-else"),
			payload:    authorSuppression("00000000-0000-0000-0000-000000000002"),
			delivery:   "sent",
			wantTopics: []string{"notifications.dispatch.issue.T-1.comment.created", "notifications.agent.someone-else"},
		},
		{
			name:     "author route whose mention failed",
			route:    new("session:someone-else"),
			payload:  authorSuppression("00000000-0000-0000-0000-000000000008"),
			delivery: "failed",
			wantTopics: []string{
				"notifications.dispatch.issue.T-1.comment.created",
				"notifications.agent.someone-else",
				"notifications.agent.mentioned",
			},
		},
		{
			name:  "unmentioned owner still receives route",
			route: new("session:someone-else"),
			payload: map[string]any{
				"id": "00000000-0000-0000-0000-000000000003", "reply_to": rootID,
			},
			wantTopics: []string{
				"notifications.dispatch.issue.T-1.comment.created",
				"notifications.agent.someone-else",
				"notifications.agent.mentioned",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedMentionDelivery(t, database, tc.payload["id"].(string), "mentioned", tc.delivery)
			publisher := &recordingPublisher{}
			event := model.Event{
				ID:        100,
				IssueKey:  new("T-1"),
				Type:      "comment.created",
				Actor:     model.Actor{Kind: "user", ID: "alice"},
				Payload:   tc.payload,
				Notify:    true,
				CreatedAt: time.Now(),
			}
			err := publish(ctx, Deps{Store: database, Publisher: publisher}, event, "", routing{route: tc.route}, map[string]struct{}{})
			if tc.wantErr == nil && err != nil {
				t.Fatalf("publish comment with mention flags: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("publish comment with mention flags returned %v, want %v", err, tc.wantErr)
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.wantTopics) {
				t.Fatalf("published topics = %#v, want %#v", got, tc.wantTopics)
			}
		})
	}
}

func TestCommentRouteSuppressionAppliesOnlyToTheCreationRoute(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	publisher := &recordingPublisher{}
	event := model.Event{
		ID:        101,
		IssueKey:  new("T-1"),
		Type:      "comment.created",
		Actor:     model.Actor{Kind: "user", ID: "alice"},
		Notify:    true,
		CreatedAt: time.Now(),
		Payload: map[string]any{
			"id":                          "00000000-0000-0000-0000-000000000004",
			"mentions":                    mentionsOf("old-holder"),
			"suppress_route":              true,
			"suppressed_route":            "session:old-holder",
			"suppressed_route_session_id": "old-holder",
		},
	}
	seedMentionDelivery(t, database, "00000000-0000-0000-0000-000000000004", "old-holder", "sent")
	if err := publish(
		context.Background(),
		Deps{Store: database, Publisher: publisher},
		event,
		"",
		routing{route: new("session:new-holder")},
		map[string]struct{}{},
	); err != nil {
		t.Fatalf("publish route-changed comment: %v", err)
	}
	if got, want := topicsOf(publisher.all()), []string{
		"notifications.dispatch.issue.T-1.comment.created",
		"notifications.agent.new-holder",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("route-changed publications = %#v, want %#v", got, want)
	}
}

func TestCommentRouteSuppressionFollowsTheResolvedHolder(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	publisher := &recordingPublisher{}
	event := model.Event{
		ID:        102,
		IssueKey:  new("T-1"),
		Type:      "comment.created",
		Actor:     model.Actor{Kind: "user", ID: "alice"},
		Notify:    true,
		CreatedAt: time.Now(),
		Payload: map[string]any{
			"id":                          "00000000-0000-0000-0000-000000000005",
			"mentions":                    mentionsOf("s1"),
			"suppress_route":              true,
			"suppressed_route":            "role:reviewer",
			"suppressed_route_session_id": "s1",
		},
	}
	seedMentionDelivery(t, database, "00000000-0000-0000-0000-000000000005", "s1", "sent")
	if err := publish(
		context.Background(),
		Deps{Store: database, Publisher: publisher},
		event,
		"",
		routing{route: new("session:s1")},
		map[string]struct{}{},
	); err != nil {
		t.Fatalf("publish role-to-session route change: %v", err)
	}
	if got, want := topicsOf(publisher.all()), []string{"notifications.dispatch.issue.T-1.comment.created"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("role-to-session publications = %#v, want %#v", got, want)
	}
}

// TestAnUnansweredSessionMentionHoldsTheCopiesOfTheSessionItNames: a session:<id> mention whose
// lookup went unanswered is unpinned in the payload, and its attempt names no session until the
// sender resolves it. Its <id> is still the session it can reach, so the author and route copies
// its creation suppressed for that session follow its attempt as a pinned mention's do: held back
// while it is pending or queued, skipped once it was sent, published once it failed or its
// unclaimed attempt waited past the claim lease.
func TestAnUnansweredSessionMentionHoldsTheCopiesOfTheSessionItNames(t *testing.T) {
	const (
		issueTopic = "notifications.dispatch.issue.T-1.comment.created"
		s1Topic    = "notifications.agent.s1"
		routeTopic = "notifications.role.reviewer"
	)
	ctx := context.Background()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "s1"}, "Root comment", nil)
	unpinned := []any{map[string]any{"target": "session:s1", "delivery": "steer", "session_id": nil}}
	commentNumber := 0
	for _, tc := range []struct {
		copy     string
		delivery string
		want     []string
		wantErr  error
	}{
		{copy: "author", delivery: "pending", want: []string{issueTopic}, wantErr: errMentionPending},
		{copy: "author", delivery: "queued", want: []string{issueTopic}, wantErr: errMentionPending},
		{copy: "author", delivery: "sent", want: []string{issueTopic}},
		{copy: "author", delivery: "failed", want: []string{issueTopic, s1Topic}},
		{copy: "author", delivery: "backlogged", want: []string{issueTopic, s1Topic}},
		{copy: "route", delivery: "pending", want: []string{issueTopic}, wantErr: errMentionPending},
		{copy: "route", delivery: "sent", want: []string{issueTopic}},
		{copy: "route", delivery: "failed", want: []string{issueTopic, routeTopic}},
	} {
		t.Run(tc.copy+" "+tc.delivery, func(t *testing.T) {
			commentNumber++
			commentID := fmt.Sprintf("00000000-0000-0000-0001-%012d", commentNumber)
			payload := map[string]any{"id": commentID, "mentions": unpinned}
			var route *string
			if tc.copy == "author" {
				payload["reply_to"] = rootID
				payload["suppressed_authors"] = []any{"s1"}
			} else {
				// role:reviewer's own lookup found s1, which the unanswered mention names.
				route = new("role:reviewer")
				payload["suppress_route"] = true
				payload["suppressed_route"] = "role:reviewer"
				payload["suppressed_route_session_id"] = "s1"
			}
			seedDelivery(t, database, commentID, "session:s1", "", tc.delivery)
			publisher := &recordingPublisher{}
			err := publish(ctx, Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}, model.Event{
				ID: 103, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: payload, Notify: true, CreatedAt: time.Now(),
			}, "", routing{route: route}, map[string]struct{}{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("publish returned %v, want %v", err, tc.wantErr)
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("published topics = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestARouteSuppressedByAnUnansweredMentionsTargetFollowsThatMention: a route whose string is the
// target of a mention whose lookup went unanswered is suppressed by that string alone, with no
// session known. Its copy follows that target's attempt: held back while it is pending, skipped
// once it was sent, published once it failed. A route moved to another string since is
// published to whatever the mention did.
func TestARouteSuppressedByAnUnansweredMentionsTargetFollowsThatMention(t *testing.T) {
	const (
		issueTopic = "notifications.dispatch.issue.T-1.comment.created"
		routeTopic = "notifications.role.reviewer"
	)
	ctx := context.Background()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	commentNumber := 0
	for _, tc := range []struct {
		route    string
		delivery string
		want     []string
		wantErr  error
	}{
		{route: "role:reviewer", delivery: "pending", want: []string{issueTopic}, wantErr: errMentionPending},
		{route: "role:reviewer", delivery: "sent", want: []string{issueTopic}},
		{route: "role:reviewer", delivery: "failed", want: []string{issueTopic, routeTopic}},
		{route: "session:s1", delivery: "sent", want: []string{issueTopic, "notifications.agent.s1"}},
	} {
		t.Run(tc.route+" "+tc.delivery, func(t *testing.T) {
			commentNumber++
			commentID := fmt.Sprintf("00000000-0000-0000-0002-%012d", commentNumber)
			// The sender records the session it resolved the role to before it sends.
			sessionID := ""
			if tc.delivery != "pending" {
				sessionID = "s1"
			}
			seedDelivery(t, database, commentID, "role:reviewer", sessionID, tc.delivery)
			publisher := &recordingPublisher{}
			err := publish(ctx, Deps{Store: database, Publisher: publisher}, model.Event{
				ID: 104, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: map[string]any{
					"id":                 commentID,
					"mentions":           []any{map[string]any{"target": "role:reviewer", "delivery": "steer", "session_id": nil}},
					"suppress_route":     true,
					"suppressed_route":   "role:reviewer",
					"suppressed_authors": []any{},
				},
				Notify: true, CreatedAt: time.Now(),
			}, "", routing{route: new(tc.route)}, map[string]struct{}{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("publish returned %v, want %v", err, tc.wantErr)
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("published topics = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// nextAttempt reads an event's attempt count and when it is next due, nil when it is due now.
func nextAttempt(t *testing.T, database *store.Store, eventID int64) (attempts int, due *time.Time) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select attempt_count, next_attempt_at from events where id = $1
	`, eventID).Scan(&attempts, &due); err != nil {
		t.Fatalf("read the event's next attempt: %v", err)
	}
	return attempts, due
}

// TestAScanDefersAClaimantCopyWhileItsMentionIsBeingSent drives the scan itself: a new comment
// mentioning the claimant is published to its issue topic at once, and its claimant copy waits
// for the mention's send to settle - without counting a failed attempt - and then follows it: a
// failed send leaves the copy to the outbox, a sent one delivered it already.
func TestAScanDefersAClaimantCopyWhileItsMentionIsBeingSent(t *testing.T) {
	const (
		issueTopic    = "notifications.dispatch.issue.T-1.comment.created"
		claimantTopic = "notifications.agent.session-claimant"
	)
	for _, tc := range []struct {
		settled string
		want    []string
	}{
		{settled: "failed", want: []string{issueTopic, claimantTopic}},
		{settled: "sent", want: []string{issueTopic}},
	} {
		t.Run(tc.settled, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
			alice := model.Actor{Kind: "user", ID: "alice"}
			commentID := seedComment(t, database, "T-1", alice, "@session-claimant ship it.", nil)
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: commentID, IssueKey: new("T-1"), Body: "@session-claimant ship it.",
					Mentions: mentionOf("session-claimant"),
				}},
			})
			seedMentionDelivery(t, database, commentID, "session-claimant", "pending")
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher}

			scan(ctx, deps)
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, []string{issueTopic}) {
				t.Fatalf("topics while the mention is pending = %#v, want the issue topic alone", got)
			}
			if at := publishedAt(t, database, event.ID); at != nil {
				t.Fatalf("published_at = %v while the claimant copy waits on the mention", at)
			}
			attempts, due := nextAttempt(t, database, event.ID)
			if attempts != 0 {
				t.Fatalf("attempt_count = %d after a deferral, want 0", attempts)
			}
			if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
				t.Fatalf("next_attempt_at = %v, want about %v from now", due, mentionPendingDelay)
			}
			if destinations, _ := publishedDestinations(t, database, event.ID); !reflect.DeepEqual(destinations, []string{issueTopic}) {
				t.Fatalf("published_destinations = %#v, want the issue topic alone", destinations)
			}

			if _, err := database.Pool.Exec(ctx, `
				update comment_deliveries set state = $2 where comment_id = $1
			`, commentID, tc.settled); err != nil {
				t.Fatalf("settle the mention: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = null where id = $1`, event.ID); err != nil {
				t.Fatalf("make the event due: %v", err)
			}
			scan(ctx, deps)
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("topics after the mention was %s = %#v, want %#v", tc.settled, got, tc.want)
			}
			if publishedAt(t, database, event.ID) == nil {
				t.Fatalf("published_at is null after the mention was %s", tc.settled)
			}
			if attempts, _ := nextAttempt(t, database, event.ID); attempts != 0 {
				t.Fatalf("attempt_count = %d after the deferral resolved, want 0", attempts)
			}
		})
	}
}

// TestAHeldBackClaimantCopyDefersNoAttemptThatFailedElsewhere drives the scan itself: a new
// comment whose claimant copy is held back for its mention still being sent is deferred only when
// that is all that kept the attempt from finishing. When NATS denies another destination of the
// same event, the attempt is backed off like any denied one - counted, its retry.Delay waited, the
// destinations it reached recorded - rather than retried every mentionPendingDelay uncounted. When
// the connection fails, the event is released for the next scan as any event that met it is.
func TestAHeldBackClaimantCopyDefersNoAttemptThatFailedElsewhere(t *testing.T) {
	const (
		issueTopic  = "notifications.dispatch.issue.T-1.comment.created"
		authorTopic = "notifications.agent.writer"
	)
	for _, tc := range []struct {
		name       string
		failTopics map[string]error
		state      outcomeState
		reached    []string
	}{
		{
			name:       "a denied role route backs the event off",
			failTopics: map[string]error{roleTopic: denial(roleTopic)},
			state:      backedOff,
			reached:    []string{issueTopic, authorTopic},
		},
		{
			name:       "an unavailable connection releases the event",
			failTopics: map[string]error{authorTopic: fmt.Errorf("bus: %w", bus.ErrUnavailable)},
			state:      released,
			reached:    []string{issueTopic, roleTopic},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			broker := events.NewBroker()
			route := "role:reviewer"
			seedIssue(t, database, "T-1", &route)
			claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
			alice := model.Actor{Kind: "user", ID: "alice"}
			// A reply to a session's comment: its destinations are the issue topic, the role
			// route, the claimant route and then the author route.
			root := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "writer"}, "Draft", nil)
			commentID := seedComment(t, database, "T-1", alice, "@session-claimant ship it.", &root)
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: commentID, IssueKey: new("T-1"), Body: "@session-claimant ship it.", ReplyTo: &root,
					Mentions: mentionOf("session-claimant"),
				}},
			})
			seedMentionDelivery(t, database, commentID, "session-claimant", "pending")
			publisher := &countingPublisher{failTopics: tc.failTopics}

			before := time.Now()
			scan(ctx, Deps{Store: database, Publisher: publisher})
			after := time.Now()
			if got := publisher.attempted(); !reflect.DeepEqual(got, []string{issueTopic, roleTopic, authorTopic}) {
				t.Fatalf("topics sent = %#v, want every destination but the held-back claimant copy", got)
			}
			if at := publishedAt(t, database, event.ID); at != nil {
				t.Fatalf("published_at = %v, though the attempt did not finish", at)
			}
			if destinations, _ := publishedDestinations(t, database, event.ID); !reflect.DeepEqual(destinations, tc.reached) {
				t.Fatalf("published_destinations = %#v, want the destinations reached, %#v", destinations, tc.reached)
			}
			attempts, due := nextAttempt(t, database, event.ID)
			switch tc.state {
			case backedOff:
				if attempts != 1 {
					t.Fatalf("attempt_count = %d, want the one backed-off attempt, not a deferral's 0", attempts)
				}
				if due == nil || due.Before(before.Add(retry.Delay(1))) || due.After(after.Add(retry.Delay(1))) {
					t.Fatalf("next_attempt_at = %v, want retry.Delay(1) = %v from the scan, not mentionPendingDelay",
						due, retry.Delay(1))
				}
			case released:
				if attempts != 0 || due != nil {
					t.Fatalf("attempt_count = %d and next_attempt_at = %v, want the claim released: 0 and none", attempts, due)
				}
			}
		})
	}
}

// TestARetryReachesTheClaimantTheScanReads: the claimant comes from the scan's read of the issue,
// so an attempt after the claim moved reaches the new holder, and a person's claim reaches no one.
func TestARetryReachesTheClaimantTheScanReads(t *testing.T) {
	const issueTopic = "notifications.dispatch.issue.T-1.message.created"
	for _, tc := range []struct {
		name  string
		moved model.Actor
		want  []string
	}{
		{
			name:  "the claim moved to another session",
			moved: model.Actor{Kind: "session", ID: "session-new"},
			want:  []string{issueTopic, "notifications.agent.session-new"},
		},
		{
			name:  "the claim moved to a person",
			moved: model.Actor{Kind: "user", ID: "bob"},
			want:  []string{issueTopic},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-old"})
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.MessageEventPayload{Message: model.Message{
					ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Ship it.",
				}},
			})
			// The first attempt fails at the issue topic, before any session is reached.
			publisher := &recordingPublisher{failTopic: issueTopic}
			deps := Deps{Store: database, Publisher: publisher}
			scan(ctx, deps)
			if attempts, _ := nextAttempt(t, database, event.ID); attempts != 1 {
				t.Fatalf("attempt_count = %d after the failed attempt, want 1", attempts)
			}

			claimIssue(t, database, "T-1", tc.moved)
			publisher.setFailTopic("")
			if _, err := database.Pool.Exec(ctx, `update events set next_attempt_at = null where id = $1`, event.ID); err != nil {
				t.Fatalf("make the retry due: %v", err)
			}
			scan(ctx, deps)
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("topics of the retry = %#v, want %#v", got, tc.want)
			}
			if publishedAt(t, database, event.ID) == nil {
				t.Fatal("published_at is null after the retry")
			}
		})
	}
}

// TestAnAbandonedMentionIsSettledByTheOutboxOrLeftToTheClaimThatTookItFirst: a mention attempt no
// sender ever claimed, pending past the claim lease, is settled by whichever of the outbox and a
// sender acts on its row first. When the outbox does, the attempt reads failed with its
// comment.delivery receipt and the route copy it held back is published; a sender that claims it
// afterwards finds it settled. When a sender's claim commits while the outbox waits to settle it,
// the attempt is that sender's: it stays pending under the fresh claim, no receipt is written, and
// the event defers on the send. An attempt a sender claimed whose claim lapsed (its sender died
// with the send in flight) is the stranded pass's to resend: the outbox leaves it pending, writes
// no receipt and publishes no copy, without waiting on it.
func TestAnAbandonedMentionIsSettledByTheOutboxOrLeftToTheClaimThatTookItFirst(t *testing.T) {
	const (
		issueTopic = "notifications.dispatch.issue.T-1.comment.created"
		routeTopic = "notifications.agent.s1"
	)
	for _, tc := range []struct {
		name string
		// seeded is the attempt's outcome (seedDelivery).
		seeded string
		// claimFirst takes the attempt's claim, as a sender's claim transaction does, and
		// commits it only once the outbox waits on the comment's owner lock to settle it.
		claimFirst bool
		wantTopics []string
		wantErr    error
		wantState  string
	}{
		{name: "the outbox acts first", seeded: "abandoned", wantTopics: []string{issueTopic, routeTopic}, wantState: "failed"},
		{
			name: "a claim commits first", seeded: "abandoned", claimFirst: true,
			wantTopics: []string{issueTopic}, wantErr: errMentionPending, wantState: "pending",
		},
		{name: "a claim lapsed with its sender", seeded: "resumed", wantTopics: []string{issueTopic}, wantState: "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			seedIssue(t, database, "T-1", nil)
			commentID := "00000000-0000-0000-0003-000000000001"
			seedMentionDelivery(t, database, commentID, "s1", tc.seeded)
			event := model.Event{
				ID: 105, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: map[string]any{
					"id":                          commentID,
					"mentions":                    mentionsOf("s1"),
					"suppress_route":              true,
					"suppressed_route":            "session:s1",
					"suppressed_route_session_id": "s1",
				},
				Notify: true, CreatedAt: time.Now(),
			}
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
			publishing := func() error {
				return publish(ctx, deps, event, "", routing{route: new("session:s1")}, map[string]struct{}{})
			}

			var err error
			if tc.claimFirst {
				claim, beginErr := database.Pool.Begin(context.Background())
				if beginErr != nil {
					t.Fatalf("begin the claim: %v", beginErr)
				}
				defer claim.Rollback(context.Background())
				if _, err := claim.Exec(context.Background(), `select 1 from issues where key = 'T-1' for no key update`); err != nil {
					t.Fatalf("lock the comment's owner: %v", err)
				}
				if _, err := claim.Exec(context.Background(), `
					update comment_deliveries set claimed_at = now() where comment_id = $1
				`, commentID); err != nil {
					t.Fatalf("claim the attempt: %v", err)
				}
				published := make(chan error, 1)
				go func() { published <- publishing() }()
				waitFor(t, 10*time.Second, "the outbox waiting on the owner lock to settle the attempt", func() bool {
					var waiting int
					if err := database.Pool.QueryRow(context.Background(), `
						select count(*) from pg_stat_activity
						where datname = current_database() and wait_event_type = 'Lock'
					`).Scan(&waiting); err != nil {
						t.Fatalf("read lock waits: %v", err)
					}
					return waiting > 0
				})
				if err := claim.Commit(context.Background()); err != nil {
					t.Fatalf("commit the claim: %v", err)
				}
				err = <-published
			} else {
				err = publishing()
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("publish returned %v, want %v", err, tc.wantErr)
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.wantTopics) {
				t.Fatalf("published topics = %#v, want %#v", got, tc.wantTopics)
			}

			var state string
			var failure *string
			if err := database.Pool.QueryRow(ctx, `
				select state, error from comment_deliveries where comment_id = $1
			`, commentID).Scan(&state, &failure); err != nil {
				t.Fatalf("read the attempt: %v", err)
			}
			if state != tc.wantState {
				t.Fatalf("attempt state = %q, want %q", state, tc.wantState)
			}
			rows, err := database.Pool.Query(ctx, `
				select actor, payload from events where type = 'comment.delivery' and payload ->> 'comment_id' = $1
			`, commentID)
			if err != nil {
				t.Fatalf("read the attempt's receipts: %v", err)
			}
			type receipt struct {
				actor   model.Actor
				payload model.CommentDeliveryEventPayload
			}
			receipts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (receipt, error) {
				var actor, payload []byte
				if err := row.Scan(&actor, &payload); err != nil {
					return receipt{}, err
				}
				var r receipt
				if err := json.Unmarshal(actor, &r.actor); err != nil {
					return receipt{}, err
				}
				return r, json.Unmarshal(payload, &r.payload)
			})
			if err != nil {
				t.Fatalf("decode the attempt's receipts: %v", err)
			}
			if tc.wantState == "failed" {
				if failure == nil || *failure != mentionAbandonedError {
					t.Fatalf("attempt error = %v, want %q", failure, mentionAbandonedError)
				}
				want := receipt{actor: event.Actor, payload: model.CommentDeliveryEventPayload{
					CommentID: commentID, Target: "session:s1", Attempt: 1, Delivery: "steer",
					SessionID: new("s1"), State: "failed", Error: mentionAbandonedError,
				}}
				if len(receipts) != 1 || !reflect.DeepEqual(receipts[0], want) {
					t.Fatalf("receipts = %#v, want %#v", receipts, want)
				}
				return
			}
			if failure != nil {
				t.Fatalf("attempt error = %q, want none: the sender that claimed it owns its outcome", *failure)
			}
			if len(receipts) != 0 {
				t.Fatalf("receipts = %#v, want none for an attempt the outbox did not settle", receipts)
			}
		})
	}
}

// TestTheCopyOfAFailedMentionCarriesTheMentionsDedupeKey: the copy of a new comment on the own
// topic of a session whose mention failed carries the dedupe key that mention's send went under
// (agent.<session>.<comment>:<target>:<mode>), so a host the send reached before its attempt
// failed drops the copy; every other destination carries the event's own key, and every one the
// event's own id. Of several failed targets the lowest keys the copy, and of one target's failed
// attempts the latest one's mode; an abandoned attempt the outbox settles failed keys it too.
// The copy goes out as the route and as the claimant route alike.
func TestTheCopyOfAFailedMentionCarriesTheMentionsDedupeKey(t *testing.T) {
	const (
		issueTopic     = "notifications.dispatch.issue.T-1.comment.created"
		mentionedTopic = "notifications.agent.mentioned"
		otherTopic     = "notifications.agent.someone-else"
		eventKey       = "dispatch-106"
	)
	for _, tc := range []struct {
		name      string
		commentID string
		// viaRoute sends the mentioned session's copy as the route (its suppression held only
		// while the mention was not failed), the unmentioned session's as the claimant route;
		// otherwise the other way round.
		viaRoute bool
		seed     func(t *testing.T, database *store.Store, commentID string)
		wantKey  string
	}{
		{
			name:      "failed under two targets, as the route",
			commentID: "00000000-0000-0000-0004-000000000001",
			viaRoute:  true,
			seed: func(t *testing.T, database *store.Store, commentID string) {
				seedDelivery(t, database, commentID, "session:mentioned", "mentioned", "failed")
				seedDelivery(t, database, commentID, "role:reviewer", "mentioned", "failed")
			},
			wantKey: "agent.mentioned.00000000-0000-0000-0004-000000000001:role:reviewer:steer",
		},
		{
			name:      "failed under two targets, as the claimant route",
			commentID: "00000000-0000-0000-0004-000000000002",
			seed: func(t *testing.T, database *store.Store, commentID string) {
				seedDelivery(t, database, commentID, "session:mentioned", "mentioned", "failed")
				seedDelivery(t, database, commentID, "role:reviewer", "mentioned", "failed")
			},
			wantKey: "agent.mentioned.00000000-0000-0000-0004-000000000002:role:reviewer:steer",
		},
		{
			name:      "two failed attempts of one target in two modes",
			commentID: "00000000-0000-0000-0004-000000000003",
			seed: func(t *testing.T, database *store.Store, commentID string) {
				seedDelivery(t, database, commentID, "session:mentioned", "mentioned", "failed")
				seedDelivery(t, database, commentID, "session:mentioned", "mentioned", "failed")
				if _, err := database.Pool.Exec(context.Background(), `
					update comment_deliveries set delivery = 'btw' where comment_id = $1 and attempt = 2
				`, commentID); err != nil {
					t.Fatalf("retry the mention in another mode: %v", err)
				}
			},
			wantKey: "agent.mentioned.00000000-0000-0000-0004-000000000003:session:mentioned:btw",
		},
		{
			name:      "abandoned, settled failed by the outbox",
			commentID: "00000000-0000-0000-0004-000000000004",
			viaRoute:  true,
			seed: func(t *testing.T, database *store.Store, commentID string) {
				seedDelivery(t, database, commentID, "session:mentioned", "mentioned", "abandoned")
			},
			wantKey: "agent.mentioned.00000000-0000-0000-0004-000000000004:session:mentioned:steer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			seedIssue(t, database, "T-1", nil)
			tc.seed(t, database, tc.commentID)
			payload := map[string]any{
				"id": tc.commentID,
				"mentions": append(mentionsOf("mentioned"), map[string]any{
					"target": "role:reviewer", "delivery": "steer", "session_id": "mentioned",
				}),
			}
			route, claimant := "session:someone-else", "mentioned"
			if tc.viaRoute {
				route, claimant = "session:mentioned", "someone-else"
				payload["suppress_route"] = true
				payload["suppressed_route"] = "session:mentioned"
				payload["suppressed_route_session_id"] = "mentioned"
			}
			event := model.Event{
				ID: 106, IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: payload, Notify: true, CreatedAt: time.Now(),
			}
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
			if err := publish(ctx, deps, event, "", routing{route: &route, claimant: claimant}, map[string]struct{}{}); err != nil {
				t.Fatalf("publish the comment: %v", err)
			}
			keys := map[string]string{}
			for _, item := range publisher.all() {
				if item.EventID != eventKey {
					t.Errorf("%s: event_id = %q, want the event's own %q", item.Topic, item.EventID, eventKey)
				}
				if err := item.Validate(); err != nil {
					t.Errorf("%s: envelope invalid: %v", item.Topic, err)
				}
				if !contracts.DedupeKeyNamesTheUpstreamEvent(item) {
					t.Errorf("%s: dedupe key %q is not one a host claims", item.Topic, item.DedupeKey)
				}
				keys[item.Topic] = item.DedupeKey
			}
			want := map[string]string{issueTopic: eventKey, otherTopic: eventKey, mentionedTopic: tc.wantKey}
			if !reflect.DeepEqual(keys, want) {
				t.Fatalf("dedupe keys by topic = %#v, want %#v", keys, want)
			}
		})
	}
}

func TestCommentEnvelopeCorrelatesRepliesUnlessTheyReplyToAnAsk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   model.Event
		wantRef string
	}{
		{
			name: "plain comment reply",
			event: model.Event{
				ID: 1, IssueKey: new("T-1"), Type: "comment.answered", CreatedAt: time.Now(),
				Payload: map[string]any{"reply_to": "00000000-0000-0000-0000-000000000010"},
			},
			wantRef: "00000000-0000-0000-0000-000000000010",
		},
		{
			name: "ask reply wins over comment parent",
			event: model.Event{
				ID: 2, IssueKey: new("T-1"), Type: "comment.created", CreatedAt: time.Now(),
				Payload: map[string]any{
					"reply_to": "00000000-0000-0000-0000-000000000010", "ask_id": "00000000-0000-0000-0000-000000000020",
				},
			},
			wantRef: "00000000-0000-0000-0000-000000000020",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item, err := envelope(tc.event, "")
			if err != nil {
				t.Fatalf("create comment envelope: %v", err)
			}
			if item.InReplyTo != tc.wantRef {
				t.Fatalf("comment envelope InReplyTo = %q, want %q", item.InReplyTo, tc.wantRef)
			}
			if err := item.Validate(); err != nil {
				t.Fatalf("comment envelope validation: %v", err)
			}
		})
	}
}
