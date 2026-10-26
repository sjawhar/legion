package outbox

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// seedMentionDelivery records an attempt of commentID's mention of sessionID, as the comment
// handler leaves it, inserting the comment first when no test seeded it. outcome is the attempt's
// state - "sent", "failed", or "pending" for a send its sender claimed just now - or "abandoned"
// for a pending attempt whose sender's claim lapsed two minutes ago. "" records no attempt.
func seedMentionDelivery(t *testing.T, database *store.Store, commentID, sessionID, outcome string) {
	t.Helper()
	if outcome == "" {
		return
	}
	state, age := outcome, time.Duration(0)
	if outcome == "abandoned" {
		state, age = "pending", 2*time.Minute
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
			$1, 'session:' || $2,
			(select coalesce(max(attempt), 0) + 1 from comment_deliveries where comment_id = $1 and target = 'session:' || $2),
			'steer', $2, $3, now() - $4::interval, now() - $4::interval
		)
	`, commentID, sessionID, state, age); err != nil {
		t.Fatalf("seed %s delivery of the mention of %s: %v", outcome, sessionID, err)
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
