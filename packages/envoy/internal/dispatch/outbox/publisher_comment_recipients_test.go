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

// claimIssue records claimant's claim on the issue key names.
func claimIssue(t *testing.T, database *store.Store, key string, claimant model.Actor) {
	t.Helper()
	claimJSON, err := json.Marshal(claimant)
	if err != nil {
		t.Fatalf("encode claimant: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update issues set claimed_by = $2, claimed_at = now() where key = $1
	`, key, claimJSON); err != nil {
		t.Fatalf("claim issue %s: %v", key, err)
	}
}

// appendCommentEvent appends a comment event on T-1 with the payload the API writes for it.
func appendCommentEvent(t *testing.T, database *store.Store, broker *events.Broker, eventType string, actor model.Actor, comment model.Comment, suppressedAuthors ...string) model.Event {
	t.Helper()
	comment.IssueKey = new("T-1")
	comment.Author = actor
	if suppressedAuthors == nil {
		suppressedAuthors = []string{}
	}
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     eventType,
		Actor:    actor,
		Payload:  model.CommentEventPayload{Comment: comment, SuppressedAuthors: suppressedAuthors},
	})
}

// recipientCheck is one event of a case and the sessions whose own topics it must reach, in order.
type recipientCheck struct {
	event model.Event
	want  []string
}

// TestAPersonsCommentReachesTheClaimantAndTheThreadsSessions appends comment events as the API
// writes them, runs one batch of the outbox over them, and reads which sessions' own topics each
// event reached. The claim and the route come from the issue's row, as the outbox reads them.
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
			// BET-15: a person comments on a passage of the issue's spec, which another session
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
					{event: reopened, want: []string{}},
					{event: created, want: []string{claimant.ID, writer.ID}},
				}
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
			name:  "a routed issue reaches the route's holder and the thread's sessions, not the claimant",
			route: new("session:session-holder"),
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{{event: created, want: []string{"session-holder", replier.ID}}}
			},
		},
		{
			name: "a claimant the reply mentions is left to the mention",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", claimant, &root)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root}, claimant.ID)
				return []recipientCheck{{event: created, want: []string{replier.ID}}}
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
			// An agent's comment does not notify, so nothing but its owner topic is published for it.
			// Its notify is forced on here to show that the claimant and author routes still skip the
			// actor, should such an event ever notify.
			name: "a session's own reply never reaches that session",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				root := seedThreadComment(t, database, "T-1", "", alice, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", claimant, &root)
				created := appendCommentEvent(t, database, broker, "comment.created", claimant, model.Comment{ID: reply, ReplyTo: &root})
				if _, err := database.Pool.Exec(context.Background(), `update events set notify = true where id = $1`, created.ID); err != nil {
					t.Fatalf("force notify: %v", err)
				}
				return []recipientCheck{{event: created, want: []string{replier.ID}}}
			},
		},
		{
			name: "the reopening a reply makes reaches the thread's root author only, and the reply reaches everyone",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				root := seedThreadComment(t, database, "T-1", spec, rooter, nil)
				seedThreadComment(t, database, "T-1", "", replier, &root)
				reply := seedThreadComment(t, database, "T-1", "", alice, &root)
				reopened := appendCommentEvent(t, database, broker, "comment.reopened", alice, model.Comment{ID: root, Anchor: &model.Anchor{ArtifactID: spec}})
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: reply, ReplyTo: &root})
				return []recipientCheck{
					{event: reopened, want: []string{rooter.ID}},
					{event: created, want: []string{claimant.ID, rooter.ID, replier.ID, writer.ID}},
				}
			},
		},
		{
			name: "a person's new comment on a claimed unrouted issue reaches the claimant",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				comment := seedThreadComment(t, database, "T-1", "", alice, nil)
				created := appendCommentEvent(t, database, broker, "comment.created", alice, model.Comment{ID: comment})
				return []recipientCheck{{event: created, want: []string{claimant.ID}}}
			},
		},
		{
			name: "a person's edit on a claimed unrouted issue reaches the claimant",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				comment := seedThreadComment(t, database, "T-1", "", alice, nil)
				edited := appendCommentEvent(t, database, broker, "comment.edited", alice, model.Comment{ID: comment})
				return []recipientCheck{{event: edited, want: []string{claimant.ID}}}
			},
		},
		{
			name: "a person's comment in an ask's thread on a claimed unrouted issue reaches the claimant and the followers",
			arrange: func(t *testing.T, database *store.Store, broker *events.Broker) []recipientCheck {
				claimIssue(t, database, "T-1", claimant)
				askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Ship it?")
				created := appendCommentEvent(t, database, broker, "comment.created", alice,
					model.Comment{ID: "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b", AskID: &askID})
				return []recipientCheck{{event: created, want: []string{claimant.ID, "session-asker"}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", tc.route)
			checks := tc.arrange(t, database, broker)
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
					t.Fatalf("%s reached sessions %v, want %v (every topic: %v)", check.event.Type, got, want, topics)
				}
			}
		})
	}
}

// One query reads the recipients of a whole batch's replies, keyed by the comment each replies
// under: two threads give two entries, a legacy nested reply finds its thread's root, a comment
// with no row and one that is not a uuid give none, and a cyclic reply_to chain ends at the depth
// bound rather than spinning, with the authors of the comments the walk visited.
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
			[]string{first, nested, cyclic, "00000000-0000-0000-0000-000000000099", "not-a-uuid"})
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
