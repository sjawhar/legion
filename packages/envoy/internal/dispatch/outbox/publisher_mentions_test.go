package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
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
// two minutes ago (its sender died with the send in flight; the API's delivery sweep resends it),
// "queued" for a pending attempt created just now that no sender has claimed yet, and
// "backlogged" or "abandoned" for one created past model.MentionSweepWindow ago that no sender
// ever claimed, which the delivery sweep will never send. ""
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
		state, age, claimed = "pending", model.MentionSweepWindow+time.Minute, false
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
			"mentions": mentionsOf("mentioned"),
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
					"id":               commentID,
					"mentions":         []any{map[string]any{"target": "role:reviewer", "delivery": "steer", "session_id": nil}},
					"suppress_route":   true,
					"suppressed_route": "role:reviewer",
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
// the attempt is that sender's: it stays pending under the fresh claim and no receipt is written,
// and the outbox, which waits on no attempt past the lease whoever holds it
// (model.MentionResumedSQL), publishes the event and keeps the route copy as a fallback row for
// that sender's send to decide. An attempt a sender claimed whose claim lapsed (its sender died
// with the send in flight) is the delivery sweep's to resend: the outbox leaves it pending, writes
// no receipt and holds its copy the same way, without waiting on it.
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
		// wantHeld is the destination of each fallback row the publish leaves behind.
		wantHeld  []string
		wantErr   error
		wantState string
	}{
		{name: "the outbox acts first", seeded: "abandoned", wantTopics: []string{issueTopic, routeTopic}, wantState: "failed"},
		{
			name: "a claim commits first", seeded: "abandoned", claimFirst: true,
			wantTopics: []string{issueTopic}, wantHeld: []string{routeTopic}, wantState: "pending",
		},
		{
			name: "a claim lapsed with its sender", seeded: "resumed",
			wantTopics: []string{issueTopic}, wantHeld: []string{routeTopic}, wantState: "pending",
		},
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
			heldRows, err := database.Pool.Query(ctx, `
				select destination from comment_fallback_copies where comment_id = $1 order by destination
			`, commentID)
			if err != nil {
				t.Fatalf("read the fallback copies: %v", err)
			}
			held, err := pgx.CollectRows(heldRows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("decode the fallback copies: %v", err)
			}
			if !slices.Equal(held, tc.wantHeld) {
				t.Fatalf("the fallback copies hold %#v, want %#v", held, tc.wantHeld)
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

// TestAResumedMentionsHeldCopyFollowsItsResend: a comment.created whose mention the delivery sweep
// resends is published without that session's copy and marked published like any other event.
// When the resend fails, the event's own publish still never sends the copy, and the fallback
// pass publishes it once, to the session's own topic only, under the mention's dedupe key,
// changing nothing of the event's row; a second pass sends nothing. When the resend is sent
// instead, the pass publishes no copy and settles what it held.
func TestAResumedMentionsHeldCopyFollowsItsResend(t *testing.T) {
	const (
		issueTopic   = "notifications.dispatch.issue.T-1.comment.created"
		sessionTopic = "notifications.agent.s1"
	)
	for _, tc := range []struct {
		name     string
		resend   string
		wantCopy bool
	}{
		{name: "the resend fails", resend: "failed", wantCopy: true},
		{name: "the resend is sent", resend: "sent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			broker := events.NewBroker()
			route := "session:s1"
			seedIssue(t, database, "T-1", &route)
			alice := model.Actor{Kind: "user", ID: "alice"}
			commentID := seedComment(t, database, "T-1", alice, "@s1 are you there?", nil)
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{
					Comment: model.Comment{
						ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf("s1"),
					},
					SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new("s1"),
				},
			})
			seedMentionDelivery(t, database, commentID, "s1", "resumed")
			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher, Broker: broker}

			scan(ctx, deps)
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, []string{issueTopic}) {
				t.Fatalf("the scan published %#v, want the issue topic alone: s1's copy is the resend's", got)
			}
			publishedBefore := publishedAt(t, database, event.ID)
			if publishedBefore == nil {
				t.Fatal("published_at is null after the scan left s1's copy to the resend")
			}
			destinationsBefore, attemptsBefore := publishedDestinations(t, database, event.ID)

			if _, err := database.Pool.Exec(ctx, `update comment_deliveries set state = $2 where comment_id = $1`,
				commentID, tc.resend); err != nil {
				t.Fatalf("settle the resend %s: %v", tc.resend, err)
			}
			// The event's own publish, as a retry of it runs, reads the resend's outcome now and
			// still leaves s1's topic to what the outbox held for it.
			var decoded model.Event
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("encode the event: %v", err)
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("decode the event: %v", err)
			}
			if err := publish(ctx, deps, decoded, "", routing{route: &route}, publishedDestinationSet(destinationsBefore)); err != nil {
				t.Fatalf("publish the event again: %v", err)
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, []string{issueTopic}) {
				t.Fatalf("published %#v in all, want the issue topic alone: the event's publish never sends a held copy", got)
			}

			publishFallbackCopies(ctx, deps)
			publishFallbackCopies(ctx, deps)
			copies := publisher.all()[1:]
			if tc.wantCopy {
				wantKey := contracts.AgentSendDedupeKey("s1", model.CommentMentionIdempotencyKey(commentID, "session:s1", "steer"))
				if len(copies) != 1 || copies[0].Topic != sessionTopic || copies[0].DedupeKey != wantKey ||
					copies[0].EventID != fmt.Sprintf("dispatch-%d", event.ID) {
					t.Fatalf("the fallback passes published %#v, want one copy to %s keyed %q", copies, sessionTopic, wantKey)
				}
			} else if len(copies) != 0 {
				t.Fatalf("the fallback passes published %#v, want nothing: the resend delivered the comment", copies)
			}

			if at := publishedAt(t, database, event.ID); at == nil || !at.Equal(*publishedBefore) {
				t.Fatalf("published_at = %v after the fallback pass, want it as the scan left it: %v", at, *publishedBefore)
			}
			destinations, attempts := publishedDestinations(t, database, event.ID)
			if !reflect.DeepEqual(destinations, destinationsBefore) || attempts != attemptsBefore {
				t.Fatalf("published_destinations = %#v and attempt_count = %d after the fallback pass, want %#v and %d",
					destinations, attempts, destinationsBefore, attemptsBefore)
			}
			var settled, published bool
			if err := database.Pool.QueryRow(ctx, `
				select settled_at is not null, published from comment_fallback_copies where comment_id = $1 and destination = $2
			`, commentID, sessionTopic).Scan(&settled, &published); err != nil {
				t.Fatalf("read the held copy: %v", err)
			}
			if !settled || published != tc.wantCopy {
				t.Fatalf("held copy settled=%v published=%v, want settled and published=%v", settled, published, tc.wantCopy)
			}
		})
	}
}

// failingPublisher is a NATS that answers every publish to topic with answer - a grant's denial,
// or NATS being unreachable - and accepts the rest.
type failingPublisher struct {
	mu       sync.Mutex
	topic    string
	answer   error
	accepted []contracts.Envelope
	failures int
}

func (p *failingPublisher) Window() bus.Sender { return p }

func (p *failingPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if item.Topic == p.topic {
		p.failures++
		return answered(fmt.Errorf("publish %s: %w", item.Topic, p.answer))
	}
	p.accepted = append(p.accepted, item)
	return answered(nil)
}

// allow lifts the failure: every later publish is accepted.
func (p *failingPublisher) allow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topic = ""
}

// seen is how many publishes to topic failed, and the topics of those accepted.
func (p *failingPublisher) seen() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failures, topicsOf(p.accepted)
}

// all is every envelope NATS accepted, in the order it took them.
func (p *failingPublisher) all() []contracts.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.Envelope(nil), p.accepted...)
}

// seedHeldFallbackCopy writes a comment on issue T-1, routed to session:s1, that mentions s1, whose
// attempt the delivery sweep resends; scans its comment.created event through deps, which holds
// s1's copy as a fallback row on s1's own topic; and fails the resend, so the next fallback pass
// publishes that copy. It returns the comment's id.
func seedHeldFallbackCopy(ctx context.Context, t *testing.T, deps Deps) string {
	t.Helper()
	route := "session:s1"
	seedIssue(t, deps.Store, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, deps.Store, "T-1", alice, "@s1 are you there?", nil)
	appendEvent(t, deps.Store, deps.Broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf("s1"),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new("s1"),
		},
	})
	seedMentionDelivery(t, deps.Store, commentID, "s1", "resumed")
	scan(ctx, deps)
	if _, err := deps.Store.Pool.Exec(ctx, `update comment_deliveries set state = 'failed' where comment_id = $1`, commentID); err != nil {
		t.Fatalf("fail the resend: %v", err)
	}
	return commentID
}

// heldCopyRow is a comment_fallback_copies row as a test reads it.
type heldCopyRow struct {
	attempts  int
	settled   bool
	published bool
	due       *time.Time
}

// readHeldCopy reads commentID's fallback row at destination.
func readHeldCopy(t *testing.T, database *store.Store, commentID, destination string) heldCopyRow {
	t.Helper()
	var row heldCopyRow
	if err := database.Pool.QueryRow(context.Background(), `
		select attempt_count, settled_at is not null, published, next_attempt_at
		from comment_fallback_copies where comment_id = $1 and destination = $2
	`, commentID, destination).Scan(&row.attempts, &row.settled, &row.published, &row.due); err != nil {
		t.Fatalf("read the held copy: %v", err)
	}
	return row
}

// TestAFallbackCopyWhoseMentionIsStillBeingSentIsClaimedAgainOnlyOnceItsWaitHasPassed: a pass that
// finds the row's mention still in flight leaves the row to a later pass, counting no attempt, and
// the row waits mentionPendingDelay rather than falling due at once: a second pass that follows it
// claims nothing, so a row whose mention takes its time neither holds a place in every pass's
// batch nor is read again by each of them. Once that wait has passed the row is claimed again, and
// the pass that claims it publishes the copy when the mention has failed by then.
func TestAFallbackCopyWhoseMentionIsStillBeingSentIsClaimedAgainOnlyOnceItsWaitHasPassed(t *testing.T) {
	const (
		issueTopic   = "notifications.dispatch.issue.T-1.comment.created"
		sessionTopic = "notifications.agent.s1"
	)
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	// The mention is being sent again: a sender claimed its attempt just now, and that attempt was
	// created a lease ago, so the pass reads it as a send it waits no longer on
	// (model.MentionResumedSQL) and leaves the row to a later pass.
	if _, err := database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'pending', claimed_at = now() where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("put the mention back in flight: %v", err)
	}

	before := time.Now()
	publishFallbackCopies(ctx, deps)
	after := time.Now()
	row := readHeldCopy(t, database, commentID, sessionTopic)
	if row.attempts != 0 || row.settled || row.published || row.due == nil {
		t.Fatalf("the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, want no attempt "+
			"counted, unsettled, unpublished and due again", row.attempts, row.settled, row.published, row.due)
	}
	if row.due.Before(before.Add(mentionPendingDelay)) || row.due.After(after.Add(mentionPendingDelay)) {
		t.Fatalf("the held copy is due at %v, want mentionPendingDelay = %v from the pass, not at once",
			*row.due, mentionPendingDelay)
	}

	// A pass inside that wait claims the row again, and so reads it again, only if it is due: the
	// claim it would take is a next_attempt_at of its own.
	publishFallbackCopies(ctx, deps)
	if again := readHeldCopy(t, database, commentID, sessionTopic); again.attempts != 0 || again.due == nil ||
		!again.due.Equal(*row.due) {
		t.Fatalf("a pass inside the wait left the held copy attempts=%d next_attempt_at=%v, want 0 and %v: "+
			"a row that is not due is not claimed", again.attempts, again.due, *row.due)
	}
	if published := topicsOf(publisher.all()); !reflect.DeepEqual(published, []string{issueTopic}) {
		t.Fatalf("the passes published %#v, want the scan's issue topic alone while the mention is in flight", published)
	}

	// Once the mention has failed and the wait has passed, the next pass claims the row and
	// publishes the copy.
	if _, err := database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed' where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("fail the mention: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
		where comment_id = $1 and settled_at is null
	`, commentID); err != nil {
		t.Fatalf("pass the held copy's wait: %v", err)
	}
	publishFallbackCopies(ctx, deps)
	if settled := readHeldCopy(t, database, commentID, sessionTopic); settled.attempts != 0 || !settled.settled ||
		!settled.published {
		t.Fatalf("once the wait had passed the held copy has attempts=%d settled=%v published=%v, want no attempt "+
			"counted by the wait, settled and published", settled.attempts, settled.settled, settled.published)
	}
	if published := topicsOf(publisher.all()); !reflect.DeepEqual(published, []string{issueTopic, sessionTopic}) {
		t.Fatalf("the passes published %#v, want the scan's issue topic and s1's copy once", published)
	}
}

// TestAFallbackCopyNATSDeniesIsRetriedPastTheDeadLetterThresholdUntilItIsPublished: a held copy
// whose publish NATS denies counts each denial on its row and waits retry.Delay of its count before
// it is decided again, the schedule an event backs off by: a pass before that is due neither
// publishes nor counts it. The denial that brings the count to retry.DeadLetterAttempts, the
// threshold the event scan logs at, is logged at Error once, and the row is retried past it, never
// settled. Once NATS accepts the copy, the next pass publishes it, once, and settles the row.
func TestAFallbackCopyNATSDeniesIsRetriedPastTheDeadLetterThresholdUntilItIsPublished(t *testing.T) {
	const (
		issueTopic   = "notifications.dispatch.issue.T-1.comment.created"
		sessionTopic = "notifications.agent.s1"
		deadLetter   = `level=ERROR msg="dispatch outbox: fallback copy reached dead-letter threshold"`
	)
	var logged lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &failingPublisher{topic: sessionTopic, answer: bus.ErrPublishDenied}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	// pass runs a fallback pass once the held copy's backoff has passed.
	pass := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
			where comment_id = $1 and settled_at is null and next_attempt_at is not null
		`, commentID); err != nil {
			t.Fatalf("pass the held copy's backoff: %v", err)
		}
		publishFallbackCopies(ctx, deps)
	}

	denied := retry.DeadLetterAttempts + 2
	for want := 1; want <= denied; want++ {
		before := time.Now()
		if want == 1 {
			publishFallbackCopies(ctx, deps)
		} else {
			pass()
		}
		after := time.Now()
		row := readHeldCopy(t, database, commentID, sessionTopic)
		if row.attempts != want || row.settled || row.published || row.due == nil {
			t.Fatalf("after %d denied passes the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, "+
				"want %d attempts, unsettled and due again", want, row.attempts, row.settled, row.published, row.due, want)
		}
		if row.due.Before(before.Add(retry.Delay(want))) || row.due.After(after.Add(retry.Delay(want))) {
			t.Fatalf("after %d denied passes the held copy is due at %v, want retry.Delay(%d) = %v from the pass",
				want, *row.due, want, retry.Delay(want))
		}
		if want == 1 {
			// A pass before the backoff has passed leaves the row alone.
			publishFallbackCopies(ctx, deps)
			if again := readHeldCopy(t, database, commentID, sessionTopic); again.attempts != 1 || !again.due.Equal(*row.due) {
				t.Fatalf("a pass inside the backoff left the held copy attempts=%d next_attempt_at=%v, want 1 and %v",
					again.attempts, again.due, *row.due)
			}
		}
	}
	var lines []string
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, deadLetter) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "destination="+sessionTopic) ||
		!strings.Contains(lines[0], fmt.Sprintf("attempts=%d", retry.DeadLetterAttempts)) {
		t.Fatalf("the dead-letter lines = %q, want one naming %s at %d attempts; log:\n%s",
			lines, sessionTopic, retry.DeadLetterAttempts, logged.String())
	}
	if denials, accepted := publisher.seen(); denials != denied || !reflect.DeepEqual(accepted, []string{issueTopic}) {
		t.Fatalf("NATS saw %d denied publishes and accepted %#v, want %d denials and the issue topic alone",
			denials, accepted, denied)
	}

	publisher.allow()
	pass()
	pass()
	if row := readHeldCopy(t, database, commentID, sessionTopic); row.attempts != denied || !row.settled || !row.published {
		t.Fatalf("once NATS accepted it the held copy has attempts=%d settled=%v published=%v, want %d attempts, settled and published",
			row.attempts, row.settled, row.published, denied)
	}
	if denials, accepted := publisher.seen(); denials != denied || !reflect.DeepEqual(accepted, []string{issueTopic, sessionTopic}) {
		t.Fatalf("NATS saw %d denied publishes and accepted %#v, want %d denials and the issue topic, then the copy once",
			denials, accepted, denied)
	}
}

// TestAFallbackCopyOutlivesANATSOutageLongerThanTheDeadLetterThreshold: NATS being unreachable
// says nothing about a held copy, so a pass that cannot reach NATS counts no attempt on the row
// and releases its claim for retryInterval, the next tick's pass - however long the outage lasts.
// After more passes than retry.DeadLetterAttempts, the threshold a counted failure is logged at,
// the row is still unsettled, unpublished and at no attempts, and no pass has logged that line.
// The first pass once NATS is back publishes the copy, once and under the mention's dedupe key,
// and settles the row published; a pass after that publishes nothing.
func TestAFallbackCopyOutlivesANATSOutageLongerThanTheDeadLetterThreshold(t *testing.T) {
	const (
		issueTopic   = "notifications.dispatch.issue.T-1.comment.created"
		sessionTopic = "notifications.agent.s1"
		deadLetter   = `level=ERROR msg="dispatch outbox: fallback copy reached dead-letter threshold"`
	)
	var logged lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &failingPublisher{topic: sessionTopic, answer: bus.ErrUnavailable}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	// pass runs a fallback pass with whatever wait the held copy sits behind - the retryInterval a
	// pass that could not reach NATS releases it for - moved into the past rather than waited out.
	pass := func() {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
			where comment_id = $1 and settled_at is null and next_attempt_at is not null
		`, commentID); err != nil {
			t.Fatalf("pass the held copy's backoff: %v", err)
		}
		publishFallbackCopies(ctx, deps)
	}

	outage := retry.DeadLetterAttempts + 2
	for down := 1; down <= outage; down++ {
		before := time.Now()
		pass()
		after := time.Now()
		row := readHeldCopy(t, database, commentID, sessionTopic)
		if row.attempts != 0 || row.settled || row.published || row.due == nil {
			t.Fatalf("after %d passes with NATS unreachable the held copy has attempts=%d settled=%v published=%v "+
				"next_attempt_at=%v, want no attempt counted, unsettled, unpublished and due again",
				down, row.attempts, row.settled, row.published, row.due)
		}
		if row.due.Before(before.Add(retryInterval)) || row.due.After(after.Add(retryInterval)) {
			t.Fatalf("after %d passes with NATS unreachable the held copy is due at %v, want retryInterval = %v "+
				"from the pass, not at once", down, *row.due, retryInterval)
		}
	}
	if unreachable, accepted := publisher.seen(); unreachable != outage || !reflect.DeepEqual(accepted, []string{issueTopic}) {
		t.Fatalf("NATS could not take %d publishes and accepted %#v, want one try a pass, %d in all, and the issue topic alone",
			unreachable, accepted, outage)
	}
	if logged := logged.String(); strings.Contains(logged, deadLetter) {
		t.Fatalf("a pass logged the dead-letter line during the outage, want none: an unreachable NATS counts no attempt; log:\n%s",
			logged)
	}

	publisher.allow()
	pass()
	if row := readHeldCopy(t, database, commentID, sessionTopic); row.attempts != 0 || !row.settled || !row.published {
		t.Fatalf("once NATS was back the held copy has attempts=%d settled=%v published=%v, "+
			"want no attempt counted by the outage, settled and published", row.attempts, row.settled, row.published)
	}
	pass()
	wantKey := contracts.AgentSendDedupeKey("s1", model.CommentMentionIdempotencyKey(commentID, "session:s1", "steer"))
	copies := publisher.all()[1:]
	if len(copies) != 1 || copies[0].Topic != sessionTopic || copies[0].DedupeKey != wantKey {
		t.Fatalf("the passes after the outage published %#v, want s1's copy once, to %s keyed %q",
			copies, sessionTopic, wantKey)
	}
}

// takeoverPublisher is a NATS that answers every publish to topic with answer, after takeOver
// runs: a second process claims the row while the pass waits on NATS.
type takeoverPublisher struct {
	topic    string
	answer   error
	takeOver func()
}

func (p *takeoverPublisher) Window() bus.Sender { return p }

func (p *takeoverPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	if item.Topic != p.topic {
		return answered(nil)
	}
	p.takeOver()
	return answered(p.answer)
}

// TestAFallbackPassWritesNothingToARowClaimedSinceItsClaim: a pass that outlasts its claim on a
// held copy, which a second process's pass then claims, writes nothing of what it came to - a
// publish, a denial or a release - to that row: the row stays as the second claim left it.
func TestAFallbackPassWritesNothingToARowClaimedSinceItsClaim(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	for _, tc := range []struct {
		name   string
		answer error
	}{
		{name: "the copy is published", answer: nil},
		{name: "the copy is denied", answer: fmt.Errorf("publish %s: %w", sessionTopic, bus.ErrPublishDenied)},
		{name: "NATS is unreachable", answer: fmt.Errorf("publish %s: %w", sessionTopic, bus.ErrUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithTransactionTracking(context.Background())
			database := storetest.Open(t)
			publisher := &takeoverPublisher{topic: sessionTopic, answer: tc.answer}
			deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
			commentID := seedHeldFallbackCopy(ctx, t, deps)
			var secondClaim time.Time
			publisher.takeOver = func() {
				if err := database.Pool.QueryRow(ctx, `
					update comment_fallback_copies set next_attempt_at = now() + interval '1 hour'
					where comment_id = $1 and destination = $2
					returning next_attempt_at
				`, commentID, sessionTopic).Scan(&secondClaim); err != nil {
					t.Errorf("claim the held copy for a second process: %v", err)
				}
			}

			publishFallbackCopies(ctx, deps)
			if secondClaim.IsZero() {
				t.Fatal("the pass published no copy, so no second process claimed the row")
			}
			row := readHeldCopy(t, database, commentID, sessionTopic)
			if row.attempts != 0 || row.settled || row.published || row.due == nil || !row.due.Equal(secondClaim) {
				t.Fatalf("the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, "+
					"want it as the second claim left it: 0 attempts, unsettled, unpublished, due at %v",
					row.attempts, row.settled, row.published, row.due, secondClaim)
			}
		})
	}
}

// TestAFallbackPassThatIsStoppingClaimsNoRow: a pass that starts once its context has ended
// claims nothing at all. Every row it claimed would be handed straight back unread - the events
// behind them cannot be read on a context that is done, which would fail the pass's one read of
// them and log an Error - so the rows are left exactly as they were, due when they were due, for
// whatever process runs next. The row starts due on a wait that has passed, the shape a pass
// before this one released it in, so a claim this pass took would show as a wait pushed out by
// claimWindow.
func TestAFallbackPassThatIsStoppingClaimsNoRow(t *testing.T) {
	const sessionTopic = "notifications.agent.s1"
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	commentID := seedHeldFallbackCopy(ctx, t, deps)
	var due time.Time
	if err := database.Pool.QueryRow(ctx, `
		update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
		where comment_id = $1 and destination = $2
		returning next_attempt_at
	`, commentID, sessionTopic).Scan(&due); err != nil {
		t.Fatalf("let the held copy's wait pass: %v", err)
	}
	published := len(publisher.all())
	var logged lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	stopping, stop := context.WithCancel(ctx)
	stop()
	publishFallbackCopies(stopping, deps)

	row := readHeldCopy(t, database, commentID, sessionTopic)
	if row.attempts != 0 || row.settled || row.published || row.due == nil || !row.due.Equal(due) {
		t.Fatalf("the held copy has attempts=%d settled=%v published=%v next_attempt_at=%v, want it as the "+
			"stopping pass found it: no attempt counted, unsettled, unpublished and still due at %v",
			row.attempts, row.settled, row.published, row.due, due)
	}
	if now := len(publisher.all()); now != published {
		t.Fatalf("the stopping pass published %d envelopes, want none: it claimed no row", now-published)
	}
	if logs := logged.String(); strings.Contains(logs, "level=ERROR") {
		t.Fatalf("the stopping pass logged %q, want nothing at Error: it claimed no row, so it read no "+
			"event of one and gave none back", logs)
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
