package outbox

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// claimIssue gives issueKey's claim to holder, as a claim write leaves the row.
func claimIssue(t *testing.T, database *store.Store, issueKey string, holder model.Actor) {
	t.Helper()
	holderJSON, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("encode claim holder: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set claimed_by = $2, claimed_at = now() where key = $1
	`, issueKey, holderJSON); err != nil {
		t.Fatalf("claim issue %s: %v", issueKey, err)
	}
}

// mentionOf is a comment's mention of a session that resolved to it.
func mentionOf(sessionID string) []model.Mention {
	return []model.Mention{{Target: "session:" + sessionID, Delivery: "steer", SessionID: new(sessionID)}}
}

// TestClaimantRouteReachesTheSessionHoldingTheIssue publishes each event as the scan would - its
// Notify decided by the broker's own rule when it was written, its payload decoded from JSON - and
// reads every topic it reached, in order.
func TestClaimantRouteReachesTheSessionHoldingTheIssue(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	asker := model.Actor{Kind: "session", ID: "session-asker"}
	other := model.Actor{Kind: "session", ID: "session-other"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	messenger := model.Actor{Kind: "session", ID: "session-messenger"}
	const (
		issueTopic     = "notifications.dispatch.issue.T-1."
		claimantTopic  = "notifications.agent.session-claimant"
		askerTopic     = "notifications.agent.session-asker"
		writerTopic    = "notifications.agent.session-writer"
		messengerTopic = "notifications.agent.session-messenger"
		commentID      = "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
		messageID      = "5a660655-04ad-4ce0-8a9b-93dd03c412b7"
	)
	issue := new("T-1")
	askEvent := func(eventType string, actor model.Actor, askID string) model.Event {
		return model.Event{IssueKey: issue, Type: eventType, Actor: actor, Payload: model.Ask{
			ID: askID, IssueKey: issue, Question: "Ship it?", State: "answered",
		}}
	}
	commentEvent := func(eventType string, actor model.Actor, comment model.Comment, suppressedAuthors ...string) model.Event {
		comment.IssueKey = issue
		if comment.ID == "" {
			comment.ID = commentID
		}
		return model.Event{IssueKey: issue, Type: eventType, Actor: actor, Payload: model.CommentEventPayload{
			Comment: comment, SuppressedAuthors: suppressedAuthors,
		}}
	}
	messageEvent := func(eventType string, actor model.Actor, message model.Message) model.Event {
		message.ID = messageID
		message.IssueKey = issue
		return model.Event{IssueKey: issue, Type: eventType, Actor: actor, Payload: model.MessageEventPayload{Message: message}}
	}

	for _, tc := range []struct {
		name  string
		route *string
		// claim is the issue's claim holder, nil for an unclaimed issue.
		claim *model.Actor
		// arrange seeds what the event is about and returns it with the slug of its document
		// owner, empty for an issue's event.
		arrange func(t *testing.T, database *store.Store) (model.Event, string)
		// forceNotify marks the event notifying whatever the broker's rule says, for an
		// agent's event that notifies as child.* events do.
		forceNotify bool
		want        []string
	}{
		{
			name:  "a person's answer to an ask another session opened reaches the asker and the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return askEvent("ask.answered", alice, seedAsk(t, database, "T-1", asker, "Ship it?")), ""
			},
			want: []string{issueTopic + "ask.answered", claimantTopic, askerTopic},
		},
		{
			name:  "a person's reply in a person's thread on the issue's spec reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				seedDocument(t, database, issue, "spec", writer, []model.Actor{writer})
				root := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
				return commentEvent("comment.created", alice, model.Comment{Body: "And the retry?", ReplyTo: &root}), ""
			},
			want: []string{issueTopic + "comment.created", claimantTopic},
		},
		{
			name:  "a person's hand-back reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return askEvent("ask.handed_back", alice, seedAsk(t, database, "T-1", asker, "Ship it?")), ""
			},
			want: []string{issueTopic + "ask.handed_back", claimantTopic, askerTopic},
		},
		{
			name:  "a person's resolution of an ask reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return askEvent("ask.resolved", alice, seedAsk(t, database, "T-1", asker, "Ship it?")), ""
			},
			want: []string{issueTopic + "ask.resolved", claimantTopic, askerTopic},
		},
		{
			name:  "a person's edit of a comment reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.edited", alice, model.Comment{Body: "Ship it Tuesday."}), ""
			},
			want: []string{issueTopic + "comment.edited", claimantTopic},
		},
		{
			name:  "a person's resolution of a thread reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				root := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
				return commentEvent("comment.resolved", alice, model.Comment{ID: root, Resolved: true}), ""
			},
			want: []string{issueTopic + "comment.resolved", claimantTopic},
		},
		{
			name:  "a person's reopening of a thread reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				root := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
				return commentEvent("comment.reopened", alice, model.Comment{ID: root}), ""
			},
			want: []string{issueTopic + "comment.reopened", claimantTopic},
		},
		{
			name:  "a person's untargeted message reaches the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return messageEvent("message.created", alice, model.Message{Body: "Ship it."}), ""
			},
			want: []string{issueTopic + "message.created", claimantTopic},
		},
		{
			name:  "a person's untargeted reply to an agent's message reaches its author and the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				root := seedMessage(t, database, "T-1", messenger, "Status?", nil)
				return messageEvent("message.answered", alice, model.Message{Body: "Green.", InReplyTo: &root}), ""
			},
			want: []string{issueTopic + "message.answered", claimantTopic, messengerTopic},
		},
		{
			name:  "an agent's comment does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.created", other, model.Comment{Body: "Done."}), ""
			},
			want: []string{issueTopic + "comment.created"},
		},
		{
			name:  "an agent's message does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return messageEvent("message.created", other, model.Message{Body: "Done."}), ""
			},
			want: []string{issueTopic + "message.created"},
		},
		{
			name:  "an agent's resolution of its own ask does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return askEvent("ask.resolved", asker, seedAsk(t, database, "T-1", asker, "Ship it?")), ""
			},
			want: []string{issueTopic + "ask.resolved"},
		},
		{
			name:  "an agent's comment that notifies still does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.edited", other, model.Comment{Body: "Done, edited."}), ""
			},
			forceNotify: true,
			want:        []string{issueTopic + "comment.edited"},
		},
		{
			name:  "a person's targeted message does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return messageEvent("message.created", alice, model.Message{Body: "Ship it.", Target: new("session:session-other")}), ""
			},
			want: []string{issueTopic + "message.created"},
		},
		{
			name: "a person's comment on an unclaimed issue reaches no claimant",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.created", alice, model.Comment{Body: "Ship it."}), ""
			},
			want: []string{issueTopic + "comment.created"},
		},
		{
			name:  "a person's comment on an issue a person claimed reaches no claimant",
			claim: &model.Actor{Kind: "user", ID: "bob"},
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.created", alice, model.Comment{Body: "Ship it."}), ""
			},
			want: []string{issueTopic + "comment.created"},
		},
		{
			name:  "a claimant that is the issue's route is reached once",
			route: new("session:session-claimant"),
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.created", alice, model.Comment{Body: "Ship it."}), ""
			},
			want: []string{issueTopic + "comment.created", claimantTopic},
		},
		{
			name:  "a claimant that follows the ask is reached once",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return askEvent("ask.answered", alice, seedAsk(t, database, "T-1", claimant, "Ship it?")), ""
			},
			want: []string{issueTopic + "ask.answered", claimantTopic},
		},
		{
			name:  "a claimant that wrote the commented document is reached once",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, issue, "spec", claimant, []model.Actor{claimant})
				return model.Event{IssueKey: issue, Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": commentID, "anchor": anchorOn(doc),
				}}, ""
			},
			want: []string{issueTopic + "comment.created", claimantTopic},
		},
		{
			name:  "a person's new comment mentioning the claimant is left to the mention",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return commentEvent("comment.created", alice, model.Comment{
					Body: "@session-claimant ship it.", Mentions: mentionOf(claimant.ID),
				}, claimant.ID), ""
			},
			want: []string{issueTopic + "comment.created"},
		},
		{
			name:  "a person's thread reply mentioning the claimant is left to the mention",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				root := seedComment(t, database, "T-1", alice, "Is the order right?", nil)
				return commentEvent("comment.created", alice, model.Comment{
					Body: "@session-claimant is it?", ReplyTo: &root, Mentions: mentionOf(claimant.ID),
				}), ""
			},
			want: []string{issueTopic + "comment.created"},
		},
		{
			name:  "a person's ask reply mentioning the claimant is left to the mention",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				askID := seedAsk(t, database, "T-1", asker, "Ship it?")
				return commentEvent("comment.created", alice, model.Comment{
					Body: "@session-claimant yes.", AskID: &askID, Mentions: mentionOf(claimant.ID),
				}), ""
			},
			want: []string{issueTopic + "comment.created", askerTopic},
		},
		{
			name:  "a person's ask reply mentioning a claimant that follows the ask reaches only the other follower",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				askID := seedAsk(t, database, "T-1", asker, "Ship it?")
				seedFollower(t, database, askID, claimant.ID)
				return commentEvent("comment.created", alice, model.Comment{
					Body: "@session-claimant yes.", AskID: &askID, Mentions: mentionOf(claimant.ID),
				}), ""
			},
			want: []string{issueTopic + "comment.created", askerTopic},
		},
		{
			name:  "a person's edit of an ask reply mentioning the claimant reaches it",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				askID := seedAsk(t, database, "T-1", asker, "Ship it?")
				seedFollower(t, database, askID, claimant.ID)
				return commentEvent("comment.edited", alice, model.Comment{
					Body: "@session-claimant yes, Tuesday.", AskID: &askID, Mentions: mentionOf(claimant.ID),
				}), ""
			},
			want: []string{issueTopic + "comment.edited", claimantTopic, askerTopic},
		},
		{
			name:  "a person's issue update does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return model.Event{IssueKey: issue, Type: "issue.updated", Actor: alice, Payload: map[string]any{"title": "Renamed"}}, ""
			},
			want: []string{issueTopic + "issue.updated"},
		},
		{
			name:  "a person's named version of the issue's document does not reach the claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				return model.Event{IssueKey: issue, Type: "artifact.version", Actor: alice, Payload: map[string]any{
					"version": map[string]any{"named": true},
				}}, ""
			},
			want: []string{issueTopic + "artifact.version"},
		},
		{
			name:  "a person's comment on a project document reaches its writer and no claimant",
			claim: &claimant,
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": commentID,
				}}, "plan"
			},
			want: []string{"notifications.dispatch.document.TT.plan.comment.created", writerTopic},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			seedIssue(t, database, "T-1", tc.route)
			if tc.claim != nil {
				claimIssue(t, database, "T-1", *tc.claim)
			}
			event, slug := tc.arrange(t, database)
			event.ID = 100
			event.CreatedAt = time.Now()
			event.Notify = tc.forceNotify || events.NewBroker().Notify(event)
			raw, err := json.Marshal(event.Payload)
			if err != nil {
				t.Fatalf("encode payload: %v", err)
			}
			event.Payload = nil
			if err := json.Unmarshal(raw, &event.Payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			publisher := &recordingPublisher{}
			if err := publish(context.Background(), Deps{Store: database, Publisher: publisher}, event, slug, tc.route, map[string]struct{}{}); err != nil {
				t.Fatalf("publish %s: %v", event.Type, err)
			}
			if got := topicsOf(publisher.all()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("topics = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestRunRecordsTheClaimantRouteSoARetryDoesNotRepublishIt: the claimant's topic is one of the
// event's recorded destinations, so an attempt that fails at a later destination is retried
// without publishing to the claimant a second time.
func TestRunRecordsTheClaimantRouteSoARetryDoesNotRepublishIt(t *testing.T) {
	const (
		issueTopic    = "notifications.dispatch.issue.T-1.ask.answered"
		claimantTopic = "notifications.agent.session-claimant"
		askerTopic    = "notifications.agent.session-asker"
	)
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
	askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Ship it?")
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "ask.answered", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Ask{ID: askID, IssueKey: new("T-1"), Question: "Ship it?", State: "answered"},
	})
	// The asker's follower route comes after the claimant's, so failing it ends the first attempt
	// with the claimant already reached.
	publisher := &recordingPublisher{failTopic: askerTopic}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, 5*time.Second, "the first attempt backed off after reaching the claimant", func() bool {
		destinations, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0 && slices.Contains(destinations, claimantTopic)
	})
	if destinations, _ := publishedDestinations(t, database, event.ID); !reflect.DeepEqual(destinations, []string{issueTopic, claimantTopic}) {
		t.Fatalf("published_destinations after the first attempt = %#v, want the issue topic and the claimant", destinations)
	}
	publisher.setFailTopic("")
	waitFor(t, 7*time.Second, "the retry published the event", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	if got, want := topicsOf(publisher.all()), []string{issueTopic, claimantTopic, askerTopic}; !reflect.DeepEqual(got, want) {
		t.Fatalf("topics across both attempts = %#v, want each once: %#v", got, want)
	}
	if destinations, _ := publishedDestinations(t, database, event.ID); !reflect.DeepEqual(destinations, []string{issueTopic, claimantTopic, askerTopic}) {
		t.Fatalf("published_destinations after the retry = %#v", destinations)
	}
}
