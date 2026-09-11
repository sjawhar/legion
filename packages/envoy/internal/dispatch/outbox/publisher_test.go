package outbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type recordingPublisher struct {
	mu        sync.Mutex
	items     []contracts.Envelope
	failures  int
	failTopic string
	attempt   chan struct{}
}

func (p *recordingPublisher) Publish(item contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failures > 0 || item.Topic == p.failTopic {
		if p.failures > 0 {
			p.failures--
		}
		select {
		case p.attempt <- struct{}{}:
		default:
		}
		return errors.New("publisher unavailable")
	}
	p.items = append(p.items, item)
	return nil
}

func (p *recordingPublisher) all() []contracts.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.Envelope(nil), p.items...)
}

func TestRunPublishesAskAnswerEnvelope(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	askID := "5a660655-04ad-4ce0-8a9b-93dd03c412b7"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "ask.answered",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Ask{
			ID:       askID,
			IssueKey: new("T-1"),
			Question: "Should the dispatcher publish this answer?",
			Urgency:  "blocking",
			State:    "answered",
		},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "ask answer publication", func() bool {
		return len(publisher.all()) == 1 && publishedAt(t, database, event.ID) != nil
	})
	item := publisher.all()[0]
	if item.EventID != fmt.Sprintf("dispatch-%d", event.ID) || item.SourceEventID != fmt.Sprint(event.ID) {
		t.Fatalf("event identity = (%q, %q)", item.EventID, item.SourceEventID)
	}
	if item.Source != "dispatch" || item.Topic != "notifications.dispatch.issue.T-1.ask.answered" {
		t.Fatalf("source/topic = (%q, %q)", item.Source, item.Topic)
	}
	if item.InReplyTo != askID || item.Urgency != "blocking" {
		t.Fatalf("answer metadata = (%q, %q)", item.InReplyTo, item.Urgency)
	}
	var payload model.Event
	if err := json.Unmarshal([]byte(item.Payload), &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.ID != event.ID || payload.Seq != event.Seq || payload.Type != event.Type || payload.Actor != event.Actor {
		t.Fatalf("payload event = %#v, want id=%d seq=%d type=%q actor=%#v", payload, event.ID, event.Seq, event.Type, event.Actor)
	}
	if item.PayloadSummary != "T-1 ask answered: Should the dispatcher publish this answer?" {
		t.Fatalf("payload summary = %q", item.PayloadSummary)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("published envelope does not validate: %v", err)
	}
}

func TestRunRoutesDocumentEventsToTheDocumentTopicAndTheirAuthor(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('TT', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("create unlinked artifact: %v", err)
	}
	var askID string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (artifact_id, author, question)
		values ($1, '{"kind":"session","id":"session-asker"}', 'Ship it?')
		returning id::text
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("create document ask: %v", err)
	}
	event := appendEvent(t, database, broker, model.Event{
		ArtifactID: new(artifactID),
		Type:       "comment.created",
		Actor:      model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID:    "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b",
			AskID: &askID,
			Body:  "Ship it.",
		}},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	// Project documents have no route, so the ask author route is the only way the
	// asking session ever learns about this human reply.
	waitFor(t, time.Second, "document event publication", func() bool {
		return len(publisher.all()) == 2 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[0].Topic != "notifications.dispatch.document.TT.notes-md.comment.created" {
		t.Fatalf("document topic = %q", items[0].Topic)
	}
	if !strings.HasPrefix(items[0].PayloadSummary, "TT/notes-md comment created") {
		t.Fatalf("document payload summary = %q", items[0].PayloadSummary)
	}
	var payload model.Event
	if err := json.Unmarshal([]byte(items[0].Payload), &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.IssueKey != nil || payload.ArtifactID == nil || *payload.ArtifactID != artifactID || payload.Project != "TT" {
		t.Fatalf("document event payload owner = issue %v artifact %v project %q, want nil issue %q artifact and TT project", payload.IssueKey, payload.ArtifactID, payload.Project, artifactID)
	}
	if items[1].Topic != "notifications.agent.session-asker" {
		t.Fatalf("document ask author route topic = %q, want %q", items[1].Topic, "notifications.agent.session-asker")
	}
	if items[1].InReplyTo != askID {
		t.Fatalf("document ask author route in_reply_to = %q, want %q", items[1].InReplyTo, askID)
	}
}

func TestRunSummarizesCommentBody(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID:       "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b",
				IssueKey: new("T-1"),
				Body:     "Tighten this paragraph",
			},
			ArtifactName: "spec.md",
		},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "comment publication", func() bool { return len(publisher.all()) == 1 })
	if summary := publisher.all()[0].PayloadSummary; summary != "T-1 comment created: Tighten this paragraph" {
		t.Fatalf("payload summary = %q", summary)
	}
}

func TestRunCorrelatesAskReplyCommentToItsAsk(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	askID := "5a660655-04ad-4ce0-8a9b-93dd03c412b7"
	commentID := "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID:       commentID,
				IssueKey: new("T-1"),
				Body:     "I'd go with option A.",
				AskID:    &askID,
			},
			ArtifactName: "spec.md",
		},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "ask reply publication", func() bool {
		return len(publisher.all()) == 1 && publishedAt(t, database, event.ID) != nil
	})
	item := publisher.all()[0]
	if !event.Notify {
		t.Fatalf("human ask reply must notify")
	}
	if item.InReplyTo != askID {
		t.Fatalf("in_reply_to = %q, want %q", item.InReplyTo, askID)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("published envelope does not validate: %v", err)
	}
}

func TestRunRoutesAskReplyToAuthorEvenWhenIssueRoutedElsewhere(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	route := "role:legion-controller-x"
	seedIssue(t, database, "T-1", &route)
	askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Ship it?")
	commentID := "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID:       commentID,
				IssueKey: new("T-1"),
				Body:     "Ship it.",
				AskID:    &askID,
			},
			ArtifactName: "spec.md",
		},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "ask author route publication", func() bool {
		return len(publisher.all()) == 3 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[0].Topic != "notifications.dispatch.issue.T-1.comment.created" {
		t.Fatalf("issue topic = %q", items[0].Topic)
	}
	if items[1].Topic != "notifications.role.legion-controller-x" {
		t.Fatalf("issue route topic = %q, want the issue's own route", items[1].Topic)
	}
	if items[2].Topic != "notifications.agent.session-asker" {
		t.Fatalf("routed copy in-reply-to = %q, want %q", items[2].Topic, "notifications.agent.session-asker")
	}
	if items[2].InReplyTo != askID {
		t.Fatalf("ask author route in_reply_to = %q, want %q", items[2].InReplyTo, askID)
	}
}

func TestRunRoutesHumanReplyToCommentAuthor(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Draft done.", nil)
	replyID := "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID:       replyID,
			IssueKey: new("T-1"),
			Body:     "Looks great, ship it.",
			ReplyTo:  &rootID,
		}},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	// The root author is not subscribed to the issue's route (there is none here), so
	// without a direct author route the session that wrote the root comment never
	// learns a human replied to it.
	waitFor(t, time.Second, "comment author route publication", func() bool {
		return len(publisher.all()) == 2 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[0].Topic != "notifications.dispatch.issue.T-1.comment.created" {
		t.Fatalf("issue topic = %q", items[0].Topic)
	}
	if items[1].Topic != "notifications.agent.session-writer" {
		t.Fatalf("comment author route topic = %q, want %q", items[1].Topic, "notifications.agent.session-writer")
	}
}

func TestRunRoutesHumanReplyToMessageAuthor(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	rootID := seedMessage(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Draft done.", nil)
	replyID := "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "message.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{
			Message:   model.Message{ID: replyID, IssueKey: "T-1", Body: "Looks great, ship it.", ReplyTo: &rootID},
			ReplyBody: "Draft done.",
		},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	// The root author is not subscribed to the issue's route (there is none here), so
	// without a direct author route the session that wrote the root message never
	// learns a human replied to it.
	waitFor(t, time.Second, "message author route publication", func() bool {
		return len(publisher.all()) == 2 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[0].InReplyTo != rootID {
		t.Fatalf("message reply in_reply_to = %q, want %q", items[0].InReplyTo, rootID)
	}
	if items[1].Topic != "notifications.agent.session-writer" {
		t.Fatalf("message author route topic = %q, want %q", items[1].Topic, "notifications.agent.session-writer")
	}
}

func TestRunRoutesReplyToReplyToBothTheRootAndParentAuthors(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Draft done.", nil)
	midID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-editor"}, "One nit.", &rootID)
	replyID := "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID:       replyID,
			IssueKey: new("T-1"),
			Body:     "Fixed the nit.",
			ReplyTo:  &midID,
		}},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	// A reply to a reply must still reach the thread root's author (the comment
	// humans reply under), not just the comment it directly targets.
	waitFor(t, time.Second, "nested reply author route publication", func() bool {
		return len(publisher.all()) == 3 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[1].Topic != "notifications.agent.session-writer" {
		t.Fatalf("root author route topic = %q, want %q", items[1].Topic, "notifications.agent.session-writer")
	}
	if items[2].Topic != "notifications.agent.session-editor" {
		t.Fatalf("parent author route topic = %q, want %q", items[2].Topic, "notifications.agent.session-editor")
	}
}

func TestRunRoutesCommentResolutionToRootAuthor(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Please confirm the approach.", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.resolved",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID:       rootID,
			IssueKey: new("T-1"),
			Body:     "Please confirm the approach.",
			Resolved: true,
		}},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	// A resolved root comment carries neither reply_to nor ask_id, so the root
	// author route must key off the resolved comment's own id.
	waitFor(t, time.Second, "comment resolution author route publication", func() bool {
		return len(publisher.all()) == 2 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[1].Topic != "notifications.agent.session-writer" {
		t.Fatalf("comment resolution author route topic = %q, want %q", items[1].Topic, "notifications.agent.session-writer")
	}
}

func TestPublishAuthorRoutesSkipsAgentReplyingToItself(t *testing.T) {
	database := openTestStore(t)
	seedIssue(t, database, "T-1", nil)
	rootID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Draft done.", nil)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher}
	item := contracts.Envelope{EventID: "dispatch-1", Topic: "notifications.dispatch.issue.T-1.comment.created"}
	event := model.Event{
		Type:    "comment.created",
		Actor:   model.Actor{Kind: "session", ID: "session-writer"},
		Payload: map[string]any{"reply_to": rootID},
	}
	publishAuthorRoutes(context.Background(), deps, item, event)

	if got := publisher.all(); len(got) != 0 {
		t.Fatalf("published %d author route(s) for an agent replying to itself, want 0: %#v", len(got), got)
	}
}

func TestLoadRootCommentAuthorTerminatesOnACycle(t *testing.T) {
	database := openTestStore(t)
	seedIssue(t, database, "T-1", nil)
	commentID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Draft done.", nil)
	// comments.reply_to carries no acyclicity constraint; a comment can end up pointing
	// at itself (or a longer cycle). The walk must terminate promptly rather than spin
	// the recursive query and stall the outbox scan.
	if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $1 where id = $1`, commentID); err != nil {
		t.Fatalf("create self-referential comment: %v", err)
	}

	done := make(chan struct{})
	var ok bool
	go func() {
		defer close(done)
		_, ok = loadRootCommentAuthor(context.Background(), Deps{Store: database}, commentID)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loadRootCommentAuthor did not terminate on a cyclic reply_to chain")
	}
	if ok {
		t.Fatal("loadRootCommentAuthor reported a root for a cyclic reply_to chain, want none")
	}
}

func TestRunStillPublishesWhenTheRootWalkHitsAReplyToCycle(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	commentID := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "session-writer"}, "Draft done.", nil)
	if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $1 where id = $1`, commentID); err != nil {
		t.Fatalf("create self-referential comment: %v", err)
	}
	replyID := "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID:       replyID,
			IssueKey: new("T-1"),
			Body:     "Ship it.",
			ReplyTo:  &commentID,
		}},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	// A malformed cycle must not stop the event from reaching its own issue topic or
	// resolving the direct parent's author (a plain, non-recursive lookup); only the
	// unresolvable thread-root walk is skipped.
	waitFor(t, 5*time.Second, "cyclic reply_to publication", func() bool {
		return len(publisher.all()) == 2 && publishedAt(t, database, event.ID) != nil
	})
	items := publisher.all()
	if items[1].Topic != "notifications.agent.session-writer" {
		t.Fatalf("parent author route topic = %q, want %q", items[1].Topic, "notifications.agent.session-writer")
	}
}

func TestRunRoutesHumanAskResolutionToAuthorOnly(t *testing.T) {
	for _, tc := range []struct {
		name            string
		actor           model.Actor
		wantTopics      []string
		wantRouteNotify bool
	}{
		{
			name:            "human resolution",
			actor:           model.Actor{Kind: "user", ID: "alice"},
			wantTopics:      []string{"notifications.dispatch.issue.T-1.ask.resolved", "notifications.role.legion-controller-x", "notifications.agent.session-asker"},
			wantRouteNotify: true,
		},
		{
			name:            "session resolution",
			actor:           model.Actor{Kind: "session", ID: "session-closer"},
			wantTopics:      []string{"notifications.dispatch.issue.T-1.ask.resolved"},
			wantRouteNotify: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestStore(t)
			broker := events.NewBroker()
			route := "role:legion-controller-x"
			seedIssue(t, database, "T-1", &route)
			askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "session-asker"}, "Ship it?")
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"),
				Type:     "ask.resolved",
				Actor:    tc.actor,
				Payload:  model.Ask{ID: askID, IssueKey: new("T-1"), Question: "Ship it?", State: "resolved"},
			})
			publisher := &recordingPublisher{}
			stop := run(t, database, publisher, broker)
			defer stop()

			waitFor(t, time.Second, "ask resolution publication", func() bool {
				return len(publisher.all()) == len(tc.wantTopics) && publishedAt(t, database, event.ID) != nil
			})
			items := publisher.all()
			for index, topic := range tc.wantTopics {
				if items[index].Topic != topic {
					t.Fatalf("publication %d topic = %q, want %q", index, items[index].Topic, topic)
				}
			}
			if items[0].InReplyTo != askID {
				t.Fatalf("issue event in_reply_to = %q, want %q", items[0].InReplyTo, askID)
			}
			if tc.wantRouteNotify && items[2].InReplyTo != askID {
				t.Fatalf("author route in_reply_to = %q, want %q", items[2].InReplyTo, askID)
			}
		})
	}
}

func TestRunAddsBoundRoutePublication(t *testing.T) {
	for _, tc := range []struct {
		name, route, wantTopic string
	}{
		{"role", "role:legion-controller-x", "notifications.role.legion-controller-x"},
		{"session", "session:5a660655-04ad-4ce0-8a9b-93dd03c412b7", "notifications.agent.5a660655-04ad-4ce0-8a9b-93dd03c412b7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestStore(t)
			broker := events.NewBroker()
			seedIssue(t, database, "T-1", &tc.route)
			event := appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: "T-1", Body: "Route this update"},
			})
			publisher := &recordingPublisher{}
			stop := run(t, database, publisher, broker)
			defer stop()

			waitFor(t, time.Second, "route publication", func() bool {
				return len(publisher.all()) == 2 && publishedAt(t, database, event.ID) != nil
			})
			items := publisher.all()
			if items[0].Topic != "notifications.dispatch.issue.T-1.message.created" {
				t.Fatalf("issue topic = %q", items[0].Topic)
			}
			if items[1].Topic != tc.wantTopic {
				t.Fatalf("route topic = %q, want %q", items[1].Topic, tc.wantTopic)
			}
		})
	}
}

func TestRunPublishesEveryEventButRoutesOnlyNotifyingEvents(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	route := "role:legion-controller-x"
	seedIssue(t, database, "T-1", &route)
	silent := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created",
		Actor:   model.Actor{Kind: "session", ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7"},
		Payload: model.Message{ID: "d7657c0d-71b9-43d5-8783-a5d98f7812e0", IssueKey: "T-1", Body: "Agent-only update"},
	})
	notifying := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: "T-1", Body: "Notify listeners"},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "all event publication", func() bool {
		return len(publisher.all()) == 3 && publishedAt(t, database, silent.ID) != nil && publishedAt(t, database, notifying.ID) != nil
	})
	items := publisher.all()
	wantTopics := []string{
		"notifications.dispatch.issue.T-1.message.created",
		"notifications.dispatch.issue.T-1.message.created",
		"notifications.role.legion-controller-x",
	}
	for index, want := range wantTopics {
		if items[index].Topic != want {
			t.Fatalf("publication %d topic = %q, want %q", index, items[index].Topic, want)
		}
	}
	for _, tc := range []struct {
		name string
		item contracts.Envelope
		want bool
	}{
		{name: "silent issue event", item: items[0], want: false},
		{name: "notifying issue event", item: items[1], want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload model.Event
			if err := json.Unmarshal([]byte(tc.item.Payload), &payload); err != nil {
				t.Fatalf("decode event payload: %v", err)
			}
			if payload.Notify != tc.want {
				t.Fatalf("payload notify = %t, want %t", payload.Notify, tc.want)
			}
		})
	}
}

func TestUnpublishedEventScanUsesEventsUnpublishedIndex(t *testing.T) {
	database := openTestStore(t)
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at)
		select 'T-1', id, 'message.created', '{"kind":"session","id":"test"}', '{"body":"published"}', false, now()
		from generate_series(1, 2000) as id
	`); err != nil {
		t.Fatalf("seed published events: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ('T-1', 2001, 'message.created', '{"kind":"session","id":"test"}', '{"body":"unpublished"}', false)
	`); err != nil {
		t.Fatalf("seed unpublished event: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), "analyze events"); err != nil {
		t.Fatalf("analyze events: %v", err)
	}
	rows, err := database.Pool.Query(context.Background(), `
		explain (costs off)
		select e.id, e.issue_key, e.seq, e.type, e.actor, e.notify, e.created_at, e.payload, i.route
		from events e
		join issues i on i.key = e.issue_key
		where e.published_at is null
		order by e.id
		limit $1
	`, batchSize)
	if err != nil {
		t.Fatalf("explain unpublished event scan: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "Index Scan using events_unpublished") {
		t.Fatalf("unpublished event scan plan =\n%s\nwant Index Scan using events_unpublished", strings.Join(plan, "\n"))
	}
}

func TestRunMarksEventPublishedAfterRouteFailure(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	route := "role:legion-controller-x"
	seedIssue(t, database, "T-1", &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: "T-1", Body: "Role publication is best effort"},
	})
	publisher := &recordingPublisher{failTopic: "notifications.role.legion-controller-x"}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "issue topic publication", func() bool {
		return len(publisher.all()) == 1 && publishedAt(t, database, event.ID) != nil
	})
	if publishedAt(t, database, event.ID) == nil {
		t.Fatal("event was not marked published after a route publish failure")
	}
	time.Sleep(retryInterval + time.Second)
	published := publisher.all()
	if len(published) != 1 || published[0].Topic != "notifications.dispatch.issue.T-1.message.created" {
		t.Fatalf("published issue events after route failure = %#v, want exactly one issue-topic event", published)
	}
}

func TestRunRetriesFailedIssuePublication(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: "T-1", Body: "Retry me"},
	})
	publisher := &recordingPublisher{failures: 1, attempt: make(chan struct{}, 1)}
	stop := run(t, database, publisher, broker)
	defer stop()

	select {
	case <-publisher.attempt:
	case <-time.After(time.Second):
		t.Fatal("initial publication was not attempted")
	}
	if publishedAt(t, database, event.ID) != nil {
		t.Fatal("event was marked published after failed issue publication")
	}
	waitFor(t, 7*time.Second, "ticker retry publication", func() bool {
		return len(publisher.all()) == 1 && publishedAt(t, database, event.ID) != nil
	})
	if publishedAt(t, database, event.ID) == nil {
		t.Fatal("event remained unpublished after successful retry")
	}
}

func run(t *testing.T, database *store.Store, publisher Publisher, broker *events.Broker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Run(ctx, Deps{Store: database, Publisher: publisher, Broker: broker}); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("outbox did not stop")
		}
	}
}

func appendEvent(t *testing.T, database *store.Store, broker *events.Broker, event model.Event) model.Event {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin event transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	event, err = broker.Append(context.Background(), tx, event)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	return event
}

func seedIssue(t *testing.T, database *store.Store, key string, route *string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `insert into projects (key, name) values ('TT', 'Test')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, route, created_by)
		values ($1, 'TT', 1, 'Test issue', $2, '{"kind":"user","id":"alice"}')
	`, key, route); err != nil {
		t.Fatalf("create issue: %v", err)
	}
}

func seedAsk(t *testing.T, database *store.Store, issueKey string, author model.Actor, question string) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode ask author: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into asks (issue_key, author, question) values ($1, $2, $3) returning id
	`, issueKey, authorJSON, question).Scan(&id); err != nil {
		t.Fatalf("create ask: %v", err)
	}
	return id
}

func seedComment(t *testing.T, database *store.Store, issueKey string, author model.Actor, body string, replyTo *string) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode comment author: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body, reply_to) values ($1, $2, $3, $4) returning id::text
	`, issueKey, authorJSON, body, replyTo).Scan(&id); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	return id
}

func seedMessage(t *testing.T, database *store.Store, issueKey string, author model.Actor, body string, replyTo *string) string {
	t.Helper()
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode message author: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, reply_to) values ($1, $2, $3, $4) returning id::text
	`, issueKey, authorJSON, body, replyTo).Scan(&id); err != nil {
		t.Fatalf("create message: %v", err)
	}
	return id
}

func publishedAt(t *testing.T, database *store.Store, eventID int64) *time.Time {
	t.Helper()
	var value *time.Time
	if err := database.Pool.QueryRow(context.Background(), `select published_at from events where id = $1`, eventID).Scan(&value); err != nil {
		t.Fatalf("read publication timestamp: %v", err)
	}
	return value
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	baseURL := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run outbox tests")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	adminURL := *parsed
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(context.Background(), adminURL.String())
	if err != nil {
		t.Fatalf("open test database admin: %v", err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate database suffix: %v", err)
	}
	databaseName := "dispatch_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(context.Background(), "create database "+databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+databaseName+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})
	testURL := *parsed
	testURL.Path = "/" + databaseName
	database, err := store.Open(context.Background(), testURL.String())
	if err != nil {
		t.Fatalf("open isolated store: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate isolated store: %v", err)
	}
	return database
}

func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
