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
		IssueKey: "T-1",
		Type:     "ask.answered",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Ask{
			ID:       askID,
			IssueKey: "T-1",
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
				IssueKey: "T-1", Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
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

func TestRunSkipsNonNotifyingEvents(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	ignored := appendEvent(t, database, broker, model.Event{
		IssueKey: "T-1", Type: "message.created",
		Actor:   model.Actor{Kind: "session", ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7"},
		Payload: model.Message{ID: "d7657c0d-71b9-43d5-8783-a5d98f7812e0", IssueKey: "T-1", Body: "Agent-only update"},
	})
	notifying := appendEvent(t, database, broker, model.Event{
		IssueKey: "T-1", Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: "T-1", Body: "Notify listeners"},
	})
	publisher := &recordingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, time.Second, "notifying event publication", func() bool {
		return len(publisher.all()) == 1 && publishedAt(t, database, notifying.ID) != nil
	})
	if publishedAt(t, database, ignored.ID) != nil {
		t.Fatal("non-notifying event was marked published")
	}
}

func TestRunMarksEventPublishedAfterRouteFailure(t *testing.T) {
	database := openTestStore(t)
	broker := events.NewBroker()
	route := "role:legion-controller-x"
	seedIssue(t, database, "T-1", &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: "T-1", Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
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
		IssueKey: "T-1", Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
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
