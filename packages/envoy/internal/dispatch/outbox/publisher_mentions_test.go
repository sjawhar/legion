package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestCommentPayloadSuppressesResolvedRouteAndAuthorSessions(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "mentioned"}, "Root comment", nil)
	for _, tc := range []struct {
		name       string
		route      *string
		payload    map[string]any
		wantTopics []string
	}{
		{
			name:  "suppressed owner route",
			route: new("session:mentioned"),
			payload: map[string]any{
				"id":                          "00000000-0000-0000-0000-000000000001",
				"suppress_route":              true,
				"suppressed_route":            "session:mentioned",
				"suppressed_route_session_id": "mentioned",
			},
			wantTopics: []string{"notifications.dispatch.issue.T-1.comment.created"},
		},
		{
			name:  "suppressed author route",
			route: new("session:someone-else"),
			payload: map[string]any{
				"id": "00000000-0000-0000-0000-000000000002", "reply_to": rootID,
				"suppressed_authors": []string{"mentioned"},
			},
			wantTopics: []string{"notifications.dispatch.issue.T-1.comment.created", "notifications.agent.someone-else"},
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
			if err := publish(ctx, Deps{Store: database, Publisher: publisher}, event, "", tc.route, map[string]struct{}{}); err != nil {
				t.Fatalf("publish comment with mention flags: %v", err)
			}
			if got := topicsOf(publisher.all()); len(got) != len(tc.wantTopics) {
				t.Fatalf("published topics = %#v, want %#v", got, tc.wantTopics)
			} else {
				for index, want := range tc.wantTopics {
					if got[index] != want {
						t.Fatalf("published topic %d = %q, want %q (all: %#v)", index, got[index], want, got)
					}
				}
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
			"suppress_route":              true,
			"suppressed_route":            "session:old-holder",
			"suppressed_route_session_id": "old-holder",
		},
	}
	if err := publish(
		context.Background(),
		Deps{Store: database, Publisher: publisher},
		event,
		"",
		new("session:new-holder"),
		map[string]struct{}{},
	); err != nil {
		t.Fatalf("publish route-changed comment: %v", err)
	}
	if got, want := topicsOf(publisher.all()), []string{
		"notifications.dispatch.issue.T-1.comment.created",
		"notifications.agent.new-holder",
	}; len(got) != len(want) {
		t.Fatalf("route-changed publications = %#v, want %#v", got, want)
	} else {
		for index, topic := range want {
			if got[index] != topic {
				t.Fatalf("route-changed publication %d = %q, want %q", index, got[index], topic)
			}
		}
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
			"suppress_route":              true,
			"suppressed_route":            "role:reviewer",
			"suppressed_route_session_id": "s1",
		},
	}
	if err := publish(
		context.Background(),
		Deps{Store: database, Publisher: publisher},
		event,
		"",
		new("session:s1"),
		map[string]struct{}{},
	); err != nil {
		t.Fatalf("publish role-to-session route change: %v", err)
	}
	if got, want := topicsOf(publisher.all()), []string{"notifications.dispatch.issue.T-1.comment.created"}; len(got) != len(want) {
		t.Fatalf("role-to-session publications = %#v, want %#v", got, want)
	} else if got[0] != want[0] {
		t.Fatalf("role-to-session publication = %q, want %q", got[0], want[0])
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
