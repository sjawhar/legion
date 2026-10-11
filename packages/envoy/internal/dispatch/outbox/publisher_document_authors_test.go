package outbox

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seedDocument inserts a document creator made, owned by issueKey's issue when it is set and by
// the project TT otherwise, with one version per entry of versions crediting that entry's
// authors, and returns its id.
func seedDocument(t *testing.T, database *store.Store, issueKey *string, slug string, creator model.Actor, versions ...[]model.Actor) string {
	t.Helper()
	ctx := context.Background()
	creatorJSON, err := json.Marshal(creator)
	if err != nil {
		t.Fatalf("encode document creator: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ($1, 'TT', $2, $2 || '.md', 'doc', $3)
		returning id::text
	`, issueKey, slug, creatorJSON).Scan(&id); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	for index, authors := range versions {
		encoded, err := json.Marshal(authors)
		if err != nil {
			t.Fatalf("encode version authors: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, 'Text.', $3)
		`, id, index+1, encoded); err != nil {
			t.Fatalf("create version %d of %s: %v", index+1, slug, err)
		}
	}
	return id
}

// seedDocumentComment inserts a comment author wrote on a project document, and returns its id.
func seedDocumentComment(t *testing.T, database *store.Store, artifactID string, author model.Actor) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode comment author: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (artifact_id, author, body) values ($1, $2, 'On the document.') returning id::text
	`, artifactID, authorJSON).Scan(&id); err != nil {
		t.Fatalf("create document comment: %v", err)
	}
	return id
}

// agentTopics is every notifications.agent.<id> topic among topics, in order.
func agentTopics(topics []string) []string {
	agents := []string{}
	for _, topic := range topics {
		if strings.HasPrefix(topic, contracts.AgentTopicPrefix) {
			agents = append(agents, topic)
		}
	}
	return agents
}

func anchorOn(artifactID string) map[string]any {
	return map[string]any{"artifact_id": artifactID, "mark_id": "mark-1", "quote": "Text", "version": 1}
}

// TestACommentOnADocumentReachesTheSessionsThatWroteIt publishes comment events whose Notify the
// broker's own rule decides, as the outbox does, and reads which sessions' own topics they reached.
func TestACommentOnADocumentReachesTheSessionsThatWroteIt(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	for _, tc := range []struct {
		name string
		// arrange seeds what the event is about and returns it with the slug of its document
		// owner, empty for an issue's event.
		arrange    func(t *testing.T, database *store.Store) (model.Event, string)
		wantAgents []string
	}{
		{
			name: "a person's comment on a passage of an issue's document reaches the session that wrote it",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000001", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "a person's suggestion on an issue's document reaches the session that wrote it",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000002", "anchor": anchorOn(doc),
					"suggestion": map[string]any{"replace_with": "Other text."},
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "a person's comment on a whole project document reaches the session that wrote it",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000003",
				}}, "plan"
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "a comment on a project image reaches no document author",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				creator, err := json.Marshal(writer)
				if err != nil {
					t.Fatalf("encode image creator: %v", err)
				}
				var image string
				if err := database.Pool.QueryRow(context.Background(), `
					insert into artifacts (project_key, slug, name, kind, created_by)
					values ('TT', 'diagram', 'diagram.png', 'image', $1)
					returning id::text
				`, creator).Scan(&image); err != nil {
					t.Fatalf("create image: %v", err)
				}
				return model.Event{ArtifactID: new(image), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000014",
				}}, "diagram"
			},
			wantAgents: []string{},
		},
		{
			name: "every session the document credits is reached once, the creator first",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer,
					[]model.Actor{writer}, []model.Actor{editor, bob}, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000004", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer", "notifications.agent.session-editor"},
		},
		{
			name: "a document only people wrote reaches no session",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", alice, []model.Actor{alice}, []model.Actor{bob})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "carol"}, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000005", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{},
		},
		{
			name: "a version whose authors are JSON null credits no one and stops nothing",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, nil, []model.Actor{editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000013", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer", "notifications.agent.session-editor"},
		},
		{
			name: "a person's edit of their comment on a passage reaches the document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.edited", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000006", "anchor": anchorOn(doc), "suppressed_authors": nil,
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-writer"},
		},
		{
			name: "an author the comment mentions is left to the mention, the other authors reached",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000007", "anchor": anchorOn(doc),
					"suppressed_authors": []any{"session-writer"},
				}}, ""
			},
			wantAgents: []string{"notifications.agent.session-editor"},
		},
		{
			name: "a reply on a project document reaches its thread's author, not the document's",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				root := seedDocumentComment(t, database, doc, model.Actor{Kind: "session", ID: "session-root"})
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000008", "reply_to": root,
				}}, "plan"
			},
			wantAgents: []string{"notifications.agent.session-root"},
		},
		{
			name: "a reply to an ask on a project document reaches the ask's followers, not the document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, nil, "plan", writer, []model.Actor{writer})
				var askID string
				if err := database.Pool.QueryRow(context.Background(), `
					insert into asks (artifact_id, author, question) values ($1, '{"kind":"session","id":"session-asker"}', 'Ship it?')
					returning id::text
				`, doc).Scan(&askID); err != nil {
					t.Fatalf("create document ask: %v", err)
				}
				seedFollower(t, database, askID, "session-asker")
				return model.Event{ArtifactID: new(doc), Project: "TT", Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000009", "ask_id": askID,
				}}, "plan"
			},
			wantAgents: []string{"notifications.agent.session-asker"},
		},
		{
			name: "a comment on the issue's conversation, which names no document, reaches no document's writer",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: alice, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000010",
				}}, ""
			},
			wantAgents: []string{},
		},
		{
			// An agent's comment does not notify (events.Broker.Notify: only a person's event
			// does), so no author route runs for it: the writer commenting on its own document
			// is not told, and neither is the document's other writer.
			name: "an agent's comment reaches no author, the commenter included",
			arrange: func(t *testing.T, database *store.Store) (model.Event, string) {
				doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
				return model.Event{IssueKey: new("T-1"), Type: "comment.created", Actor: writer, Payload: map[string]any{
					"id": "00000000-0000-0000-0000-000000000011", "anchor": anchorOn(doc),
				}}, ""
			},
			wantAgents: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := storetest.Open(t)
			seedIssue(t, database, "T-1", nil)
			event, slug := tc.arrange(t, database)
			event.ID = 100
			event.CreatedAt = time.Now()
			event.Notify = events.NewBroker().Notify(event)
			publisher := &recordingPublisher{}
			if err := publish(context.Background(), Deps{Store: database, Publisher: publisher}, event, slug, nil, map[string]struct{}{}); err != nil {
				t.Fatalf("publish %s: %v", event.Type, err)
			}
			topics := topicsOf(publisher.all())
			if got := agentTopics(topics); !slices.Equal(got, tc.wantAgents) {
				t.Fatalf("sessions reached = %v, want %v (every topic: %v)", got, tc.wantAgents, topics)
			}
		})
	}
}

// The author routes skip the event's own actor wherever they find it, so a writer is never told
// of its own comment even should an agent's comment notify.
func TestPublishAuthorRoutesSkipsTheCommenterAmongTheDocumentsWriters(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	doc := seedDocument(t, database, new("T-1"), "notes", writer, []model.Actor{writer, editor})
	publisher := &recordingPublisher{}
	event := model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    writer,
		Payload:  map[string]any{"id": "00000000-0000-0000-0000-000000000012", "anchor": anchorOn(doc)},
	}
	item := contracts.Envelope{EventID: "dispatch-1", Topic: "notifications.dispatch.issue.T-1.comment.created"}
	if err := publishAuthorRoutes(context.Background(), Deps{Store: database, Publisher: publisher}, 0, item, event, &attempt{delivered: map[string]struct{}{}}); err != nil {
		t.Fatalf("publish author routes: %v", err)
	}
	if got := topicsOf(publisher.all()); !slices.Equal(got, []string{"notifications.agent.session-editor"}) {
		t.Fatalf("author routes = %v, want the document's other writer alone", got)
	}
}
