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
// claimant's copy is #2225's (publishClaimantRoute); the thread's sessions are #2366's.
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
// published to the own topics of its sessions, in that order, and of no other session.
func checkSessionRecipients(t *testing.T, database *store.Store, checks []recipientCheck) {
	t.Helper()
	publisher := &recordingPublisher{}
	if _, _, err := scanBatch(context.Background(), Deps{Store: database, Publisher: publisher}); err != nil {
		t.Fatalf("scan batch: %v", err)
	}
	byEvent := map[string][]string{}
	for _, item := range publisher.all() {
		byEvent[item.SourceEventID] = append(byEvent[item.SourceEventID], item.Topic)
	}
	for _, check := range checks {
		if publishedAt(t, database, check.event.ID) == nil {
			t.Fatalf("%s event %d was not published", check.event.Type, check.event.ID)
		}
		want := make([]string, 0, len(check.want))
		for _, sessionID := range check.want {
			want = append(want, contracts.AgentSubject(sessionID))
		}
		topics := byEvent[fmt.Sprint(check.event.ID)]
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

// One query reads the sessions the primary documents of a whole batch's issues credit, keyed by
// issue: a document that is not primary credits no session here, and an issue with no primary
// document, or with no row, has no entry.
func TestLoadPrimaryDocumentAuthorsReadsEveryIssueOfABatchInOneQuery(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	seedSecondIssue(t, database, "T-2")
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	seedSpec(t, database, "T-1", alice, []model.Actor{writer, alice}, []model.Actor{editor, writer})
	seedSpec(t, database, "T-2", editor)
	seedDocument(t, database, new("T-1"), "notes", model.Actor{Kind: "session", ID: "session-notes"})

	authors, err := loadPrimaryDocumentAuthors(context.Background(), Deps{Store: database}, []string{"T-1", "T-2", "T-9"})
	if err != nil {
		t.Fatalf("load primary document authors: %v", err)
	}
	want := map[string][]string{
		"T-1": {writer.ID, editor.ID},
		"T-2": {editor.ID},
	}
	if !reflect.DeepEqual(authors, want) {
		t.Fatalf("authors = %v, want %v", authors, want)
	}
}

// One query reads the recipients of a whole batch's replies, keyed by the comment each replies
// under: two threads give two entries, a legacy nested reply finds its thread's root, a comment
// with no row gives none, and a cyclic reply_to chain ends at the depth bound rather than
// spinning, with the authors of the comments the walk visited.
func TestLoadCommentRecipientsReadsEveryThreadOfABatchInOneQuery(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	replier := model.Actor{Kind: "session", ID: "session-replier"}
	spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer, alice}, nil)
	first := seedThreadComment(t, database, "T-1", spec, alice, nil)
	seedThreadComment(t, database, "T-1", "", replier, &first)
	second := seedThreadComment(t, database, "T-1", "", rooter, nil)
	nested := seedThreadComment(t, database, "T-1", "", replier, &second)
	seedThreadComment(t, database, "T-1", "", writer, &nested)
	cyclic := seedThreadComment(t, database, "T-1", "", rooter, nil)
	loop := seedThreadComment(t, database, "T-1", "", replier, &cyclic)
	if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $1 where id = $2`, loop, cyclic); err != nil {
		t.Fatalf("create a reply_to cycle: %v", err)
	}

	done := make(chan struct{})
	var recipients map[string][]string
	var err error
	go func() {
		defer close(done)
		recipients, err = loadCommentRecipients(context.Background(), Deps{Store: database},
			[]string{first, nested, cyclic, "00000000-0000-0000-0000-000000000099"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loadCommentRecipients did not end on a cyclic reply_to chain")
	}
	if err != nil {
		t.Fatalf("load comment recipients: %v", err)
	}
	want := map[string][]string{
		first:  {replier.ID, writer.ID},
		nested: {rooter.ID, replier.ID, writer.ID},
		cyclic: {rooter.ID, replier.ID},
	}
	if !reflect.DeepEqual(recipients, want) {
		t.Fatalf("recipients = %v, want %v", recipients, want)
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

			if _, _, err := scanBatch(context.Background(), Deps{Store: database, Publisher: &recordingPublisher{}}); err != nil {
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
		pending, _, err := scanPendingEvents(context.Background(), deps)
		if err != nil {
			t.Fatalf("scan the batch: %v", err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		if _, err := publishBatch(stopped, deps, pending); err != nil {
			t.Fatalf("publish the batch: %v", err)
		}
		assertReleased(t, database, replied, commented, message)
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
		waitFor(t, 5*time.Second, "the message sent", func() bool {
			return slices.Contains(publisher.sent(), "notifications.dispatch.issue.T-1.message.created")
		})
		stop()
		assertReleased(t, database, commented, message)
	})
}
