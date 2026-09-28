package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// countingPublisher records every topic it is asked to publish, answering each with its entry in
// failTopics, or else with fail (nil publishes).
type countingPublisher struct {
	mu         sync.Mutex
	fail       error
	failTopics map[string]error
	topics     []string
}

func (p *countingPublisher) Publish(item contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.topics = append(p.topics, item.Topic)
	if err, ok := p.failTopics[item.Topic]; ok {
		return err
	}
	return p.fail
}

func (p *countingPublisher) attempted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.topics...)
}

// seedThreeDestinationEvent appends a human reply to a session's comment on an issue routed to a
// role: its issue topic, its role route and its author route.
func seedThreeDestinationEvent(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
	t.Helper()
	route := "role:reviewer"
	seedIssue(t, database, "T-1", &route)
	root := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "writer"}, "Draft", nil)
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: "reply", IssueKey: new("T-1"), Body: "Reviewed", ReplyTo: &root,
		}},
	})
}

// firstAttempt waits for the outbox's first attempt at event to be backed off.
func firstAttempt(t *testing.T, database *store.Store, event model.Event) {
	t.Helper()
	waitFor(t, 5*time.Second, "the first attempt backed off", func() bool {
		_, attempts := publishedDestinations(t, database, event.ID)
		return attempts > 0
	})
}

// A publish that fails for the connection (NATS unreachable, so each publish waits out its window)
// ends the attempt at its first destination: the event's other destinations would each wait out
// the same window, stalling every event queued behind it.
func TestAConnectionFailureEndsTheAttemptAtItsFirstDestination(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	publisher := &countingPublisher{fail: errors.New("bus: waiting for NATS to reconnect: context deadline exceeded")}
	stop := run(t, database, publisher, broker)
	defer stop()

	firstAttempt(t, database, event)
	if topics := publisher.attempted(); len(topics) != 1 {
		t.Fatalf("one attempt published %v while NATS was unreachable, want only its first destination", topics)
	}
}

// A destination whose record write fails ends the attempt: the store is failing, and every
// destination published after it would go unrecorded and be published again on each retry.
func TestAFailedRecordWriteEndsTheAttempt(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	if _, err := database.Pool.Exec(context.Background(), `
		create function refuse_destination() returns trigger language plpgsql as $$
		begin raise exception 'recording destinations is failing'; end $$;
		create trigger refuse_destination before update of published_destinations on events
		for each row execute function refuse_destination();
	`); err != nil {
		t.Fatalf("make the record write fail: %v", err)
	}
	publisher := &countingPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	firstAttempt(t, database, event)
	if topics := publisher.attempted(); len(topics) != 1 {
		t.Fatalf("one attempt published %v with the record write failing, want only its first destination", topics)
	}
	if destinations, _ := publishedDestinations(t, database, event.ID); len(destinations) != 0 {
		t.Fatalf("published_destinations = %v, though every record write fails", destinations)
	}
}

// An attempt that goes on past a denial and then ends at a connection failure returns both: the
// denial is not lost behind the error that ended the attempt.
func TestAnAttemptEndedAfterADenialReturnsBoth(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	unreachable := errors.New("bus: waiting for NATS to reconnect: context deadline exceeded")
	publisher := &countingPublisher{failTopics: map[string]error{
		roleTopic:                    fmt.Errorf("bus: %w to %q", bus.ErrPublishDenied, roleTopic),
		"notifications.agent.writer": unreachable,
	}}
	route := "role:reviewer"
	// publish reads the payload as the scan decodes it from the row, a JSON object.
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	event.Payload = nil
	if err := json.Unmarshal(raw, &event.Payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	err = publish(context.Background(), Deps{Store: database, Publisher: publisher}, event, "", &route, map[string]struct{}{})
	if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, unreachable) {
		t.Fatalf("publish returned %v, want the role route's denial and the author route's connection failure", err)
	}
}
