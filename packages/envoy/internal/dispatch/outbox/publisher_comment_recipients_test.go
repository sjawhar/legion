package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedThreadComment inserts a comment author wrote on issueKey's issue and returns its id. The
// comment is anchored on the document anchoredOn names when that is not empty, and replies to
// replyTo when replyTo is set.
func seedThreadComment(t *testing.T, database *store.Store, issueKey, anchoredOn string, author model.Actor, replyTo *string) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode comment author: %v", err)
	}
	var anchor []byte
	if anchoredOn != "" {
		if anchor, err = json.Marshal(anchorOn(anchoredOn)); err != nil {
			t.Fatalf("encode comment anchor: %v", err)
		}
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, anchor, reply_to) values ($1, $2, 'In the thread.', $3, $4)
		returning id::text
	`, issueKey, authorJSON, anchor, replyTo).Scan(&id); err != nil {
		t.Fatalf("create thread comment: %v", err)
	}
	return id
}

// appendCommentEvent appends a comment event on T-1 with the payload the API writes for it.
func appendCommentEvent(t *testing.T, database *store.Store, broker *events.Broker, eventType string, actor model.Actor, comment model.Comment, mentioned ...string) model.Event {
	t.Helper()
	return appendIssueCommentEvent(t, database, broker, "T-1", eventType, actor, comment, mentioned...)
}

// appendIssueCommentEvent appends a comment event on issueKey's issue with the payload the API
// writes for it: the comment's mentions resolved to each session of mentioned, which the outbox's
// session routes read (payloadMentionedSessions).
func appendIssueCommentEvent(t *testing.T, database *store.Store, broker *events.Broker, issueKey, eventType string, actor model.Actor, comment model.Comment, mentioned ...string) model.Event {
	t.Helper()
	comment.IssueKey = new(issueKey)
	comment.Author = actor
	for _, sessionID := range mentioned {
		comment.Mentions = append(comment.Mentions, mentionOf(sessionID)...)
	}
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new(issueKey),
		Type:     eventType,
		Actor:    actor,
		Payload:  model.CommentEventPayload{Comment: comment},
	})
}

// makePrimary makes the document the issue's primary document, as its spec is.
func makePrimary(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update artifacts set is_primary = true where id = $1`, artifactID); err != nil {
		t.Fatalf("make document %s primary: %v", artifactID, err)
	}
}

// seedSecondIssue inserts a second issue, with no route, in the project seedIssue made.
func seedSecondIssue(t *testing.T, database *store.Store, key string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, 'TT', 2, 'Second issue', '{"kind":"user","id":"alice"}', 'V')
	`, key); err != nil {
		t.Fatalf("create issue %s: %v", key, err)
	}
}

// recipientCheck is one event of a case and the sessions whose own topics it must reach, in order.
type recipientCheck struct {
	event model.Event
	want  []string
}

// TestAPersonsCommentReachesTheClaimantAndTheThreadsSessions appends comment events as the API
// writes them, runs one batch of the outbox over them, and reads which sessions' own topics each
// event reached. The claim and the route come from the issue's row, as the outbox reads them. The
// claimant's copy is #2225's (addClaimantRoute); the thread's sessions are #2366's.
func TestAPersonsCommentReachesTheClaimantAndTheThreadsSessions(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	replier := model.Actor{Kind: "session", ID: "session-replier"}
	for _, tc := range []struct {
		name    string
		route   *string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck
	}{
		{
			// The reported case: a person comments on a passage of the issue's spec, which another session
			// wrote; the claimant replies and resolves; the person replies, which reopens the thread.
			name: "a person's reply on a claimed unrouted issue reaches the claimant and the document's writer once each",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer}, []model.Actor{claimant})
				root := seedThreadComment(t, database, "T-1", spec, sami, nil)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", sami, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{
					{event: reopened, want: []string{claimant.ID}},
					{event: created, want: []string{claimant.ID, writer.ID}},
				}
			},
		},
		{
			name: "a claimant who also wrote in the thread gets one copy of a person's reply",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", claimant, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{claimant.ID, replier.ID}}}
			},
		},
		{
			name: "a reply reaches the session that started the thread once and every session that replied",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				seedThreadComment(t, database, "T-1", "", rooter, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{rooter.ID, replier.ID}}}
			},
		},
		{
			name:  "a reply on a routed claimed issue reaches the route's holder, the claimant and the thread's sessions",
			route: new("session:session-holder"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{"session-holder", claimant.ID, replier.ID}}}
			},
		},
		{
			name: "a thread session the reply mentions is left to the mention once it delivered, and published to when it failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				seedThreadComment(t, database, "T-1", "", rooter, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				seedMentionDelivery(t, database, reply, claimant.ID, "sent")
				seedMentionDelivery(t, database, reply, replier.ID, "sent")
				seedMentionDelivery(t, database, reply, rooter.ID, "failed")
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root},
					claimant.ID, replier.ID, rooter.ID)
				return []recipientCheck{{event: created, want: []string{rooter.ID}}}
			},
		},
		{
			name: "a person's claim adds no recipient",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", model.Actor{Kind: "user", ID: "bob"})
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{replier.ID}}}
			},
		},
		{
			name: "the reopening a reply makes reaches the claimant and the thread's root author, and the reply reaches everyone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, rooter, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", alice, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{
					{event: reopened, want: []string{claimant.ID, rooter.ID}},
					{event: created, want: []string{claimant.ID, rooter.ID, replier.ID, writer.ID}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			checkSessionRecipients(t, database, tc.arrange(t, database, broker))
		})
	}
}

// checkSessionRecipients runs one batch of the outbox and fails unless each check's event was
// published to the own topics of its sessions, in that order, and of no other session. The batch
// sends each destination's chain beside the others (planChains), so an event's destinations reach
// the publisher in no order of the event's: the order is the one the batch records them in
// (published_destinations), and the publisher must have been sent exactly those.
func checkSessionRecipients(t *testing.T, database *store.Store, checks []recipientCheck) {
	t.Helper()
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	byEvent := map[string][]string{}
	for _, item := range publisher.all() {
		byEvent[item.SourceEventID] = append(byEvent[item.SourceEventID], item.Topic)
	}
	for _, check := range checks {
		row := readEventRow(t, database, check.event.ID)
		if row.publishedAt == nil {
			t.Fatalf("%s event %d was not published", check.event.Type, check.event.ID)
		}
		if sent := byEvent[fmt.Sprint(check.event.ID)]; !slices.Equal(slices.Sorted(slices.Values(sent)), slices.Sorted(slices.Values(row.destinations))) {
			t.Fatalf("%s event %d was sent to %v and recorded %v", check.event.Type, check.event.ID, sent, row.destinations)
		}
		want := make([]string, 0, len(check.want))
		for _, sessionID := range check.want {
			want = append(want, contracts.AgentSubject(sessionID))
		}
		topics := row.destinations
		if got := agentTopics(topics); !slices.Equal(got, want) {
			t.Fatalf("%s event %d reached sessions %v, want %v (every topic: %v)", check.event.Type, check.event.ID, got, want, topics)
		}
	}
}

// seedSpec inserts issueKey's primary document, which creator made, with one version per entry of
// versions crediting that entry's authors, and returns its id.
func seedSpec(t *testing.T, database *store.Store, issueKey string, creator model.Actor, versions ...[]model.Actor) string {
	t.Helper()
	spec := seedDocument(t, database, new(issueKey), "spec", creator, versions...)
	makePrimary(t, database, spec)
	return spec
}

// TestAPersonsCommentOnAnIssueNobodyRoutesOrClaimsReachesItsSpecsSessions covers #2366: a
// person's comment on an issue with no route and no session claim reaches the sessions the issue's
// primary document credits, where a session claimant would get #2225's copy. The spec of each case
// credits session-spec (its creator) and session-editor, and also a person and a session credit
// with no id, which have no topic.
func TestAPersonsCommentOnAnIssueNobodyRoutesOrClaimsReachesItsSpecsSessions(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	specWriter := model.Actor{Kind: "session", ID: "session-spec"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	replier := model.Actor{Kind: "session", ID: "session-replier"}
	specSessions := []string{specWriter.ID, editor.ID}
	for _, tc := range []struct {
		name    string
		route   *string
		arrange func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck
	}{
		{
			// A spec written without a claim: the session that wrote every version of the spec held
			// no claim.
			name: "a person's conversation comment and its edit reach the spec's sessions, and a reopening does not",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				edited := appendCommentEvent(t, database, broker, "comment.edited", sami, model.Comment{ID: comment})
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", sami, model.Comment{ID: root})
				return []recipientCheck{
					{event: created, want: specSessions},
					{event: edited, want: specSessions},
					{event: reopened, want: []string{}},
				}
			},
		},
		{
			name: "a person's reply in a conversation thread with no session reaches the spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: specSessions}}
			},
		},
		{
			name: "a person's reply reaches the spec's sessions, then the thread's",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: append(slices.Clone(specSessions), replier.ID)}}
			},
		},
		{
			name: "a person's comment on a passage of the spec reaches each of its sessions once",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				comment := seedThreadComment(t, database, "T-1", spec, sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, Anchor: &model.Anchor{ArtifactID: spec}})
				return []recipientCheck{{event: created, want: specSessions}}
			},
		},
		{
			name: "a person's comment in an ask's thread reaches the spec's sessions and the followers",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Ship it?")
				created := appendCommentEvent(t, database, broker, "comment.created", sami,
					model.Comment{ID: "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", AskID: &askID})
				return []recipientCheck{{event: created, want: append(slices.Clone(specSessions), "session-asker")}}
			},
		},
		{
			name: "on an issue a person claimed, a person's comment reaches the spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				claimIssue(t, database, "T-1", model.Actor{Kind: "user", ID: "bob"})
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: specSessions}}
			},
		},
		{
			name: "on an issue a session claimed, a person's comment reaches the claimant and not the spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			name:  "on a routed issue, a person's comment reaches the route and not the spec's sessions",
			route: new("session:session-holder"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: []string{"session-holder"}}}
			},
		},
		{
			name: "a spec session the comment mentions is left to the mention once it delivered, and published to when it failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				delivered := seedThreadComment(t, database, "T-1", "", sami, nil)
				seedMentionDelivery(t, database, delivered, editor.ID, "sent")
				failed := seedThreadComment(t, database, "T-1", "", sami, nil)
				seedMentionDelivery(t, database, failed, editor.ID, "failed")
				onDelivered := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: delivered}, editor.ID)
				onFailed := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: failed}, editor.ID)
				return []recipientCheck{
					{event: onDelivered, want: []string{specWriter.ID}},
					{event: onFailed, want: specSessions},
				}
			},
		},
		{
			name: "in one batch, a comment on each of two issues reaches that issue's spec's sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker, spec string) []recipientCheck {
				seedSecondIssue(t, database, "T-2")
				seedSpec(t, database, "T-2", model.Actor{Kind: "session", ID: "session-other"})
				first := seedThreadComment(t, database, "T-1", "", sami, nil)
				second := seedThreadComment(t, database, "T-2", "", sami, nil)
				onFirst := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: first})
				onSecond := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: second})
				return []recipientCheck{
					{event: onFirst, want: specSessions},
					{event: onSecond, want: []string{"session-other"}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			spec := seedSpec(t, database, "T-1", specWriter,
				[]model.Actor{specWriter}, []model.Actor{alice, {Kind: "session"}}, []model.Actor{editor, specWriter})
			checkSessionRecipients(t, database, tc.arrange(t, database, broker, spec))
		})
	}
}

// breakDocumentCredits makes every version of the document artifactID names list its authors as a
// JSON string, which documentCreditsSQL's jsonb_array_elements refuses: every recipient query that
// reads the document's credits then fails, as a query failing in the store does.
func breakDocumentCredits(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update artifact_versions set authors = '"not a list"'::jsonb where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("break the credits of document %s: %v", artifactID, err)
	}
}

// eventRow is what a batch left on an event's row.
type eventRow struct {
	destinations []string
	attempts     int
	nextAttempt  *time.Time
	publishedAt  *time.Time
}

func readEventRow(t *testing.T, database *store.Store, eventID int64) eventRow {
	t.Helper()
	var row eventRow
	if err := database.Pool.QueryRow(context.Background(), `
		select published_destinations, attempt_count, next_attempt_at, published_at from events where id = $1
	`, eventID).Scan(&row.destinations, &row.attempts, &row.nextAttempt, &row.publishedAt); err != nil {
		t.Fatalf("read event %d: %v", eventID, err)
	}
	return row
}

// appendIssueMessage appends a person's message on T-1, an event that takes no comment recipient.
func appendIssueMessage(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
	t.Helper()
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{ID: "message-1", IssueKey: new("T-1"), Body: "Next"}},
	})
}

// A recipient query that fails fails the comment event that needed it at the step that needed it,
// as the lookup did when it ran per event: the event's owner topic, its route and its claimant are
// published and recorded first, so the issue's subject still gets the event in id order and the
// retry sends only the rest. The batch's other events publish as they would have.
func TestAFailedRecipientQueryBacksOffOnlyTheEventsThatNeededItAfterTheirOwnerTopic(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	commentTopic := "notifications.dispatch.issue.T-1.comment.created"
	messageTopic := "notifications.dispatch.issue.T-1.message.created"
	for _, tc := range []struct {
		name  string
		route *string
		// arrange appends the comment event whose recipient query fails.
		arrange      func(t *testing.T, database *store.Store, broker *events.Broker) model.Event
		wantComment  []string
		wantMessages []string
	}{
		{
			name: "the primary document's sessions, on an issue nobody routes or claims",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
				breakDocumentCredits(t, database, seedSpec(t, database, "T-1", writer, []model.Actor{writer}))
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				return appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
			},
			wantComment:  []string{commentTopic},
			wantMessages: []string{messageTopic},
		},
		{
			name:  "a reply's thread sessions, on a routed claimed issue",
			route: new("role:reviewer"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
				claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				breakDocumentCredits(t, database, spec)
				root := seedThreadComment(t, database, "T-1", spec, alice, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				return appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
			},
			wantComment:  []string{commentTopic, roleTopic, contracts.AgentSubject("session-claimant")},
			wantMessages: []string{messageTopic, roleTopic, contracts.AgentSubject("session-claimant")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			comment := tc.arrange(t, database, broker)
			message := appendIssueMessage(t, database, broker)

			deps := withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}})
			if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			got := readEventRow(t, database, comment.ID)
			if got.publishedAt != nil || got.attempts != 1 || got.nextAttempt == nil {
				t.Fatalf("the comment whose recipient query failed has published_at %v, attempt_count %d and next_attempt_at %v, want it backed off: none, 1 and a retry",
					got.publishedAt, got.attempts, got.nextAttempt)
			}
			if !slices.Equal(got.destinations, tc.wantComment) {
				t.Fatalf("the comment whose recipient query failed recorded published_destinations %v, want %v", got.destinations, tc.wantComment)
			}
			other := readEventRow(t, database, message.ID)
			if other.publishedAt == nil || other.attempts != 0 || !slices.Equal(other.destinations, tc.wantMessages) {
				t.Fatalf("the batch's message has published_at %v, attempt_count %d and published_destinations %v, want it published to %v",
					other.publishedAt, other.attempts, other.destinations, tc.wantMessages)
			}
		})
	}
}

// A reply whose reply_to is not a uuid replies under no comment, so its thread reaches no session:
// the reply is published to its owner topic, its route and its claimant, with no attempt counted,
// and holds nothing back.
func TestAReplyToAnIDThatIsNoUUIDReachesNoThreadSessionAndPublishes(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("role:reviewer"))
	claimIssue(t, database, "T-1", claimant)
	comment := seedThreadComment(t, database, "T-1", "", sami, nil)
	replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, ReplyTo: new("not-a-uuid")})
	message := appendIssueMessage(t, database, broker)

	deps := withHeldRoles(Deps{Store: database, Publisher: &recordingPublisher{}})
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	got := readEventRow(t, database, replied.ID)
	want := []string{"notifications.dispatch.issue.T-1.comment.created", roleTopic, contracts.AgentSubject(claimant.ID)}
	if got.publishedAt == nil || got.attempts != 0 || got.nextAttempt != nil || !slices.Equal(got.destinations, want) {
		t.Fatalf("the reply to a non-uuid id has published_at %v, attempt_count %d, next_attempt_at %v and published_destinations %v, want it published to %v with no attempt counted",
			got.publishedAt, got.attempts, got.nextAttempt, got.destinations, want)
	}
	if other := readEventRow(t, database, message.ID); other.publishedAt == nil || other.attempts != 0 {
		t.Fatalf("the issue's later message has published_at %v and attempt_count %d, want it published", other.publishedAt, other.attempts)
	}
}

// repairDocumentCredits gives every version of the document artifactID names an empty credit list,
// which every recipient query reads again: the document then credits its creator alone.
func repairDocumentCredits(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update artifact_versions set authors = '[]'::jsonb where artifact_id = $1`, artifactID); err != nil {
		t.Fatalf("repair the credits of document %s: %v", artifactID, err)
	}
}

// makeDue makes each event's back-off fall due now, as the passing of time would.
func makeDue(t *testing.T, database *store.Store, events ...model.Event) {
	t.Helper()
	ids := make([]int64, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update events set next_attempt_at = now() where id = any($1) and next_attempt_at is not null`, ids); err != nil {
		t.Fatalf("make events %v due: %v", ids, err)
	}
}

// An event whose recipient read failed lists its owner topic, route and claimant and nothing
// after, so the sessions it is still bound for are unknown. A later event of its owner that is
// bound for such a session - the thread root's author, told of the thread's resolution - is held
// back from it until the failed event is tried again, and reaches it after the failed event does;
// the destinations the failed event did list, the claimant here, chain as usual. The cases are
// the three recipient reads: a reply's thread sessions, the sessions of the primary document of an
// issue nobody routes or claims, and the sessions of the document a comment is anchored on.
func TestALaterEventReachesNoSessionTheFailedRecipientReadOfAnEarlierOneMissedBeforeIt(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	for _, tc := range []struct {
		name string
		// arrange returns the event whose recipient read fails, the later event that reaches
		// rooter, and the document whose credits break and are then repaired.
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) (failed, later model.Event, document string)
		// wantFirst is the later event's sessions in the first batch: those the failed event
		// listed.
		wantFirst []string
	}{
		{
			name: "a reply's thread sessions",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event, string) {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, rooter, nil)
				reply := seedThreadComment(t, database, "T-1", "", sami, &root)
				replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				return replied, resolved, spec
			},
			wantFirst: []string{claimant.ID},
		},
		{
			name: "the primary document's sessions, on an issue nobody routes or claims",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event, string) {
				spec := seedSpec(t, database, "T-1", rooter, []model.Actor{rooter})
				comment := seedThreadComment(t, database, "T-1", "", sami, nil)
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root})
				return commented, resolved, spec
			},
			wantFirst: []string{},
		},
		{
			name: "the sessions of the document a comment is anchored on",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event, string) {
				claimIssue(t, database, "T-1", claimant)
				notes := seedDocument(t, database, new("T-1"), "notes", rooter, []model.Actor{rooter})
				comment := seedThreadComment(t, database, "T-1", notes, sami, nil)
				root := seedThreadComment(t, database, "T-1", "", rooter, nil)
				commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, Anchor: &model.Anchor{ArtifactID: notes}})
				resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root})
				return commented, resolved, notes
			},
			wantFirst: []string{claimant.ID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", nil)
			failed, later, document := tc.arrange(t, database, broker)
			breakDocumentCredits(t, database, document)

			publisher := &recordingPublisher{}
			deps := Deps{Store: database, Publisher: publisher}
			holder := holding(t, deps).instance
			scan := func(t *testing.T) {
				t.Helper()
				if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
					t.Fatalf("scan batch: %v", err)
				}
			}
			scan(t)
			first, held := readEventRow(t, database, failed.ID), readEventRow(t, database, later.ID)
			if first.attempts != 1 || first.nextAttempt == nil {
				t.Fatalf("the event whose recipient read failed has attempt_count %d and next_attempt_at %v, want it backed off", first.attempts, first.nextAttempt)
			}
			if held.publishedAt != nil || held.attempts != 0 || held.nextAttempt == nil || held.nextAttempt.After(*first.nextAttempt) {
				t.Fatalf("the later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back no later than the failed event's retry at %v",
					held.publishedAt, held.attempts, held.nextAttempt, *first.nextAttempt)
			}
			want := []string{}
			for _, sessionID := range tc.wantFirst {
				want = append(want, contracts.AgentSubject(sessionID))
			}
			if got := agentTopics(held.destinations); !slices.Equal(got, want) {
				t.Fatalf("the later event reached sessions %v in the first batch, want %v, the ones the failed event listed", got, want)
			}

			repairDocumentCredits(t, database, document)
			makeDue(t, database, failed, later)
			scan(t)
			for _, event := range []model.Event{failed, later} {
				if row := readEventRow(t, database, event.ID); row.publishedAt == nil {
					t.Fatalf("%s event %d was not published once the credits were repaired: %+v", event.Type, event.ID, row)
				}
			}
			var order []string
			for _, item := range publisher.all() {
				if item.Topic == contracts.AgentSubject(rooter.ID) {
					order = append(order, item.SourceEventID)
				}
			}
			if want := []string{fmt.Sprint(failed.ID), fmt.Sprint(later.ID)}; !slices.Equal(order, want) {
				t.Fatalf("%s received events %v, want %v: the failed event first, then the later one", rooter.ID, order, want)
			}
		})
	}
}

// A document whose credit list no read can take backs off the comment anchored on it, and holds
// back nothing but the later session copies of that comment's issue: the batch is recorded, the
// issue's later event reaches the claimant its failed event listed and waits for the failed
// event's retry for the thread root's author, and another issue's events in the same batch
// publish - among them a comment anchored on a healthy document, whose credits the batch reads in
// the same statement as the broken document's (loadRecipients runs that statement again in halves,
// here each id alone, once Postgres refuses it).
func TestABrokenCreditListBacksOffItsEventAndHoldsOnlyItsOwnersLaterSessionCopies(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	claimant := model.Actor{Kind: "session", ID: "session-claimant"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	other := model.Actor{Kind: "session", ID: "session-other"}
	credited := model.Actor{Kind: "session", ID: "session-credited"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	seedSecondIssue(t, database, "T-2")
	claimIssue(t, database, "T-1", claimant)
	notes := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
	breakDocumentCredits(t, database, notes)
	comment := seedThreadComment(t, database, "T-1", notes, sami, nil)
	root := seedThreadComment(t, database, "T-1", "", rooter, nil)
	otherRoot := seedThreadComment(t, database, "T-2", "", other, nil)
	otherReply := seedThreadComment(t, database, "T-2", "", sami, &otherRoot)
	healthy := seedDocument(t, database, new("T-2"), "plan", credited, []model.Actor{credited})
	healthyComment := seedThreadComment(t, database, "T-2", healthy, sami, nil)
	commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, Anchor: &model.Anchor{ArtifactID: notes}})
	resolved := appendCommentEvent(t, database, broker, "comment.resolved", sami, model.Comment{ID: root})
	otherReplied := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: otherReply, ReplyTo: &otherRoot})
	otherResolved := appendIssueCommentEvent(t, database, broker, "T-2", "comment.resolved", sami, model.Comment{ID: otherRoot})
	anchored := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: healthyComment, Anchor: &model.Anchor{ArtifactID: healthy}})

	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	failed := readEventRow(t, database, commented.ID)
	if failed.publishedAt != nil || failed.attempts != 1 || failed.nextAttempt == nil {
		t.Fatalf("the comment on the broken document has published_at %v, attempt_count %d and next_attempt_at %v, want it backed off",
			failed.publishedAt, failed.attempts, failed.nextAttempt)
	}
	if want := []string{"notifications.dispatch.issue.T-1.comment.created", contracts.AgentSubject(claimant.ID)}; !slices.Equal(failed.destinations, want) {
		t.Fatalf("the comment on the broken document recorded published_destinations %v, want %v", failed.destinations, want)
	}
	held := readEventRow(t, database, resolved.ID)
	if want := []string{"notifications.dispatch.issue.T-1.comment.resolved", contracts.AgentSubject(claimant.ID)}; !slices.Equal(held.destinations, want) {
		t.Fatalf("the issue's later event recorded published_destinations %v, want %v: its owner topic and the claimant the failed event listed", held.destinations, want)
	}
	if held.publishedAt != nil || held.attempts != 0 || held.nextAttempt == nil || held.nextAttempt.After(*failed.nextAttempt) {
		t.Fatalf("the issue's later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back no later than the failed event's retry at %v",
			held.publishedAt, held.attempts, held.nextAttempt, *failed.nextAttempt)
	}
	for _, item := range publisher.all() {
		if item.SourceEventID == fmt.Sprint(resolved.ID) && item.Topic == contracts.AgentSubject(rooter.ID) {
			t.Fatalf("the issue's later event reached %s, a session the failed event may yet reach first", rooter.ID)
		}
	}
	for _, event := range []model.Event{otherReplied, otherResolved} {
		row := readEventRow(t, database, event.ID)
		if row.publishedAt == nil || row.attempts != 0 || !slices.Equal(agentTopics(row.destinations), []string{contracts.AgentSubject(other.ID)}) {
			t.Fatalf("the other issue's %s event has published_at %v, attempt_count %d and published_destinations %v, want it published to %s",
				event.Type, row.publishedAt, row.attempts, row.destinations, other.ID)
		}
	}
	row := readEventRow(t, database, anchored.ID)
	if want := []string{"notifications.dispatch.issue.T-2.comment.created", contracts.AgentSubject(credited.ID)}; row.publishedAt == nil || row.attempts != 0 || !slices.Equal(row.destinations, want) {
		t.Fatalf("the other issue's comment on a healthy document has published_at %v, attempt_count %d and published_destinations %v, want it published to %v",
			row.publishedAt, row.attempts, row.destinations, want)
	}
}

// An event that is not due still reads its recipients, since they are what it holds its owner's
// later events back from (resolveDestinations). One backed off on a thread whose document's credit
// list no read can take puts that row in every batch's statement beside another issue's reply, and
// the batch runs the refused statement again in halves (loadRecipients): the other issue's reply
// publishes, and so does the broken reply's issue's message, bound for the claimant the broken
// reply names.
func TestABrokenRowOfAnEventThatIsNotDueFailsNoOtherOwnersRecipientRead(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	other := model.Actor{Kind: "session", ID: "session-other"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	seedSecondIssue(t, database, "T-2")
	claimIssue(t, database, "T-1", model.Actor{Kind: "session", ID: "session-claimant"})
	spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
	breakDocumentCredits(t, database, spec)
	root := seedThreadComment(t, database, "T-1", spec, writer, nil)
	reply := seedThreadComment(t, database, "T-1", "", sami, &root)
	broken := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})

	deps := Deps{Store: database, Publisher: &recordingPublisher{}}
	holder := holding(t, deps).instance
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the broken reply's batch: %v", err)
	}
	if row := readEventRow(t, database, broken.ID); row.attempts != 1 {
		t.Fatalf("the reply on the broken thread has attempt_count %d after its batch, want 1", row.attempts)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update events set next_attempt_at = now() + interval '1 hour' where id = $1`, broken.ID); err != nil {
		t.Fatalf("move the broken reply's back-off out: %v", err)
	}
	message := appendIssueMessage(t, database, broker)
	otherRoot := seedThreadComment(t, database, "T-2", "", other, nil)
	otherReply := seedThreadComment(t, database, "T-2", "", sami, &otherRoot)
	otherReplied := appendIssueCommentEvent(t, database, broker, "T-2", "comment.created", sami, model.Comment{ID: otherReply, ReplyTo: &otherRoot})

	if _, err := scanBatch(context.Background(), deps, unfenced{}, holder); err != nil {
		t.Fatalf("scan the batch beside the broken reply: %v", err)
	}
	row := readEventRow(t, database, otherReplied.ID)
	if row.publishedAt == nil || row.attempts != 0 || !slices.Equal(agentTopics(row.destinations), []string{contracts.AgentSubject(other.ID)}) {
		t.Fatalf("the other issue's reply has published_at %v, attempt_count %d and published_destinations %v, want it published to %s",
			row.publishedAt, row.attempts, row.destinations, other.ID)
	}
	if row := readEventRow(t, database, message.ID); row.publishedAt == nil {
		t.Fatalf("the broken reply's issue's message was not published: %+v", row)
	}
	if row := readEventRow(t, database, broken.ID); row.attempts != 1 || row.publishedAt != nil {
		t.Fatalf("the reply on the broken thread, not due, has attempt_count %d and published_at %v, want it left as it was", row.attempts, row.publishedAt)
	}
}

// waitOut makes event an earlier attempt's: not due for the next hour, one attempt counted, and
// reached already on each of reached.
func waitOut(t *testing.T, database *store.Store, event model.Event, reached ...string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update events set next_attempt_at = now() + interval '1 hour', attempt_count = 1, published_destinations = $2
		where id = $1
	`, event.ID, reached); err != nil {
		t.Fatalf("back event %d off: %v", event.ID, err)
	}
}

// A due event behind an earlier event of its issue that is waiting out a back-off is held back
// only on the keys the earlier event could still owe. A session the earlier event already reached
// is no such key, even when the earlier event's recipient read fails now; a session its failed
// read may still name is, until the earlier event's retry; and an earlier event whose reads answer
// holds back only the key it stalled on - a session whose send failed, or its role route - so the
// later event reaches every other session at once. Each later event replies in a thread of its
// own, so no due event asks the earlier event's recipient read.
func TestAnEventBehindAnotherThatIsNotDueIsHeldOnlyOnTheKeysTheOtherCouldStillOwe(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	reached := model.Actor{Kind: "session", ID: "session-reached"}
	unreached := model.Actor{Kind: "session", ID: "session-unreached"}
	stalled := model.Actor{Kind: "session", ID: "session-stalled"}
	topic := "notifications.dispatch.issue.T-1.comment.created"
	// reply appends sami's reply in a new thread on the document on names, or on none, whose root
	// author writes and each of members then replies in, and returns its event.
	reply := func(t *testing.T, database *store.Store, broker *events.Broker, on string, author model.Actor, members ...model.Actor) model.Event {
		t.Helper()
		root := seedThreadComment(t, database, "T-1", on, author, nil)
		for _, member := range members {
			seedThreadComment(t, database, "T-1", "", member, &root)
		}
		comment := seedThreadComment(t, database, "T-1", "", sami, &root)
		return appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment, ReplyTo: &root})
	}
	for _, tc := range []struct {
		name  string
		route *string
		// arrange appends the earlier event, backed off, and the later one.
		arrange func(t *testing.T, database *store.Store, broker *events.Broker) (earlier, later model.Event)
		// want is every subject the later event reaches in the batch, in order; the rest of its
		// destinations wait for the earlier event's retry.
		want []string
	}{
		{
			// The earlier event reached the session in an attempt before its document's credit
			// list broke; the later one is bound for it and for a session the earlier one never
			// reached.
			name: "the earlier event's recipient read fails",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
				spec := seedDocument(t, database, new("T-1"), "spec", reached, []model.Actor{reached})
				breakDocumentCredits(t, database, spec)
				earlier := reply(t, database, broker, spec, reached)
				waitOut(t, database, earlier, topic, contracts.AgentSubject(reached.ID))
				return earlier, reply(t, database, broker, "", reached, unreached)
			},
			want: []string{topic, contracts.AgentSubject(reached.ID)},
		},
		{
			name: "the earlier event's reads answer and its send to one session failed",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
				earlier := reply(t, database, broker, "", reached, stalled)
				waitOut(t, database, earlier, topic, contracts.AgentSubject(reached.ID))
				return earlier, reply(t, database, broker, "", reached, unreached, stalled)
			},
			want: []string{topic, contracts.AgentSubject(reached.ID), contracts.AgentSubject(unreached.ID)},
		},
		{
			name:  "the earlier event's reads answer and its role route is pending",
			route: new("role:reviewer"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) (model.Event, model.Event) {
				earlier := reply(t, database, broker, "", reached)
				waitOut(t, database, earlier, topic, contracts.AgentSubject(reached.ID))
				return earlier, reply(t, database, broker, "", reached, unreached)
			},
			want: []string{topic, contracts.AgentSubject(reached.ID), contracts.AgentSubject(unreached.ID)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			earlier, later := tc.arrange(t, database, broker)
			before := readEventRow(t, database, earlier.ID)

			publisher := &recordingPublisher{}
			deps := withHeldRoles(Deps{Store: database, Publisher: publisher})
			if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
				t.Fatalf("scan batch: %v", err)
			}
			var sent []string
			for _, item := range publisher.all() {
				if item.SourceEventID == fmt.Sprint(later.ID) {
					sent = append(sent, item.Topic)
				}
			}
			if !slices.Equal(sent, tc.want) {
				t.Fatalf("the later event was sent to %v, want %v", sent, tc.want)
			}
			got := readEventRow(t, database, later.ID)
			if !slices.Equal(got.destinations, tc.want) {
				t.Fatalf("the later event recorded published_destinations %v, want %v", got.destinations, tc.want)
			}
			if got.publishedAt != nil || got.attempts != 0 || got.nextAttempt == nil || !got.nextAttempt.Equal(*before.nextAttempt) {
				t.Fatalf("the later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back until the earlier event's retry at %v",
					got.publishedAt, got.attempts, got.nextAttempt, *before.nextAttempt)
			}
			if after := readEventRow(t, database, earlier.ID); !reflect.DeepEqual(after, before) {
				t.Fatalf("the earlier event, not due, has %+v after the batch, want it left as it was: %+v", after, before)
			}
		})
	}
}

// An event whose role route reached the role's holder of an attempt before is held back no more
// on that route: the role topic it recorded names the route reached for good, whoever holds the
// role now. When its recipient read fails now, the role's current holder is a session its failed
// read may still name - here the thread's document credits it - so a later event of its issue is
// held back from the current holder until the earlier event's retry, its role forward included,
// even though the earlier event's role route is resolved to that same holder in the batch.
func TestAnEarlierEventsReachedRoleRouteDoesNotNameTheRolesCurrentHolder(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	holder := model.Actor{Kind: "session", ID: roleHolder}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("role:reviewer"))
	spec := seedDocument(t, database, new("T-1"), "spec", holder, []model.Actor{holder})
	breakDocumentCredits(t, database, spec)
	root := seedThreadComment(t, database, "T-1", spec, sami, nil)
	reply := seedThreadComment(t, database, "T-1", "", sami, &root)
	earlier := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
	// The earlier event's role route reached the role's holder then, another session than
	// roleHolder, which holds the role now (withHeldRoles).
	waitOut(t, database, earlier, "notifications.dispatch.issue.T-1.comment.created", roleTopic)
	later := appendIssueMessage(t, database, broker)
	before := readEventRow(t, database, earlier.ID)

	publisher := &recordingPublisher{}
	deps := withHeldRoles(Deps{Store: database, Publisher: publisher})
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	want := []string{"notifications.dispatch.issue.T-1.message.created"}
	var sent []string
	for _, item := range publisher.all() {
		if item.SourceEventID == fmt.Sprint(later.ID) {
			sent = append(sent, item.Topic)
		}
	}
	if !slices.Equal(sent, want) {
		t.Fatalf("the later event was sent to %v, want %v: its role forward to %s waits for the earlier event's retry", sent, want, roleHolder)
	}
	got := readEventRow(t, database, later.ID)
	if !slices.Equal(got.destinations, want) {
		t.Fatalf("the later event recorded published_destinations %v, want %v", got.destinations, want)
	}
	if got.publishedAt != nil || got.attempts != 0 || got.nextAttempt == nil || got.nextAttempt.After(*before.nextAttempt) {
		t.Fatalf("the later event has published_at %v, attempt_count %d and next_attempt_at %v, want it held back no later than the earlier event's retry at %v",
			got.publishedAt, got.attempts, got.nextAttempt, *before.nextAttempt)
	}
	if after := readEventRow(t, database, earlier.ID); !reflect.DeepEqual(after, before) {
		t.Fatalf("the earlier event, not due, has %+v after the batch, want it left as it was: %+v", after, before)
	}
}

// A process that stops mid-batch releases the batch's events (settle), and an event whose recipient
// query failed is no exception: it is neither backed off nor left claimed, whether the stop is what
// failed the query or the query failed in the store and the stop came while the batch's answers
// were still due.
func TestAStoppingBatchReleasesTheEventsWhoseRecipientQueryFailed(t *testing.T) {
	sami := model.Actor{Kind: "user", ID: "sami"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	assertReleased := func(t *testing.T, database *store.Store, events ...model.Event) {
		t.Helper()
		for _, event := range events {
			if got := readEventRow(t, database, event.ID); got.publishedAt != nil || got.attempts != 0 || got.nextAttempt != nil {
				t.Errorf("%s event %d has published_at %v, attempt_count %d and next_attempt_at %v, want it released: none, 0 and none",
					event.Type, event.ID, got.publishedAt, got.attempts, got.nextAttempt)
			}
		}
	}

	t.Run("the stop cancels the recipient queries", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		seedSpec(t, database, "T-1", writer, []model.Actor{writer})
		root := seedThreadComment(t, database, "T-1", "", writer, nil)
		reply := seedThreadComment(t, database, "T-1", "", sami, &root)
		comment := seedThreadComment(t, database, "T-1", "", sami, nil)
		replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
		commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
		message := appendIssueMessage(t, database, broker)

		deps := Deps{Store: database, Publisher: &recordingPublisher{}}
		holder := holding(t, deps).instance
		pending, _, _, err := readBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("scan the batch: %v", err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		if _, err := publishBatch(stopped, deps, unfenced{}, holder, pending, false); err != nil {
			t.Fatalf("publish the batch: %v", err)
		}
		assertReleased(t, database, replied, commented, message)
	})

	// Every destination the event lists before its recipient read is reached already, so the
	// batch has no send of it to lose: the read the stop cancelled is what releases it
	// (batchEvent.resolveLost), rather than backing it off.
	t.Run("the stop cancels the recipient query of an event with nothing else to send", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		seedSpec(t, database, "T-1", writer, []model.Actor{writer})
		root := seedThreadComment(t, database, "T-1", "", writer, nil)
		reply := seedThreadComment(t, database, "T-1", "", sami, &root)
		replied := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: reply, ReplyTo: &root})
		if _, err := database.Pool.Exec(context.Background(),
			`update events set published_destinations = array['notifications.dispatch.issue.T-1.comment.created'] where id = $1`, replied.ID); err != nil {
			t.Fatalf("record the reply's owner topic reached: %v", err)
		}

		deps := Deps{Store: database, Publisher: &recordingPublisher{}}
		holder := holding(t, deps).instance
		pending, _, _, err := readBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("scan the batch: %v", err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		if _, err := publishBatch(stopped, deps, unfenced{}, holder, pending, false); err != nil {
			t.Fatalf("publish the batch: %v", err)
		}
		assertReleased(t, database, replied)
	})

	t.Run("the query fails in the store and the stop comes before the answers", func(t *testing.T) {
		database := storetest.Open(t)
		broker := events.NewBroker()
		seedIssue(t, database, "T-1", nil)
		breakDocumentCredits(t, database, seedSpec(t, database, "T-1", writer, []model.Actor{writer}))
		comment := seedThreadComment(t, database, "T-1", "", sami, nil)
		commented := appendCommentEvent(t, database, broker, "comment.created", sami, model.Comment{ID: comment})
		message := appendIssueMessage(t, database, broker)

		publisher := &heldPublisher{}
		stop := run(t, database, publisher, broker)
		// The batch's sends to the issue topic go one at a time (planChains), so the comment's,
		// which is held unanswered, is the one sent.
		waitFor(t, 5*time.Second, "the comment sent", func() bool {
			return slices.Contains(publisher.sent(), "notifications.dispatch.issue.T-1.comment.created")
		})
		stop()
		assertReleased(t, database, commented, message)
	})
}
