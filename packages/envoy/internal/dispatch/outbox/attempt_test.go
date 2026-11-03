package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	publishOnly
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

func (p *countingPublisher) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *countingPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(p.Publish(item))
}

// Request takes a role forward by its envelope's topic, the role's, as Publish takes a publish.
func (p *countingPublisher) Request(_ string, item contracts.Envelope, _ bus.RequestOptions) <-chan error {
	return answered(p.Publish(item))
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

// A send that finds NATS unreachable (bus.ErrUnavailable) stops the batch: the sends already made
// are answered, nothing further is sent - not the next event of a chain, nor any later one - and
// the event is released with no attempt counted, to be sent again when a wake comes (a reconnect),
// rather than sent again and again while NATS stays unreachable.
func TestAnUnreachableNATSReleasesTheBatchAndSendsNothingAfterIt(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	next := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Next"},
	})
	unreachable := fmt.Errorf("bus: %w: waiting for NATS to reconnect: context deadline exceeded", bus.ErrUnavailable)
	publisher := &countingPublisher{fail: unreachable}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, 5*time.Second, "the first event's three destinations tried", func() bool { return len(publisher.attempted()) >= 3 })
	// The hold's first scan and the LISTEN connection's catch-up each wake one scan; after them
	// nothing does.
	time.Sleep(500 * time.Millisecond)
	settled := len(publisher.attempted())
	time.Sleep(time.Second)
	topics := publisher.attempted()
	if len(topics) != settled {
		t.Fatalf("sent %d more envelopes with no wake while NATS was unreachable", len(topics)-settled)
	}
	firstHeads := []string{"notifications.dispatch.issue.T-1.comment.created", roleTopic, "notifications.agent.writer"}
	for index, topic := range topics {
		if topic != firstHeads[index%len(firstHeads)] {
			t.Fatalf("sent %v while NATS was unreachable, want only the first event's three chain heads at each scan", topics)
		}
	}
	for _, id := range []int64{event.ID, next.ID} {
		destinations, attempts := publishedDestinations(t, database, id)
		if len(destinations) != 0 || attempts != 0 || publishedAt(t, database, id) != nil {
			t.Fatalf("event %d: published_destinations %v, attempt_count %d, published %v, want it released untouched",
				id, destinations, attempts, publishedAt(t, database, id) != nil)
		}
	}
}

// A batch whose record write fails is left as it was read: nothing about it is recorded, no
// attempt is counted, and once the store records again the event is published with every
// destination it reached.
func TestABatchWhoseRecordWriteFailsIsLeftAsItWasRead(t *testing.T) {
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

	waitFor(t, 5*time.Second, "the event's three destinations sent", func() bool { return len(publisher.attempted()) >= 3 })
	destinations, attempts := publishedDestinations(t, database, event.ID)
	if len(destinations) != 0 || attempts != 0 || publishedAt(t, database, event.ID) != nil {
		t.Fatalf("published_destinations = %v, attempt_count = %d, published = %v, though the record write failed",
			destinations, attempts, publishedAt(t, database, event.ID) != nil)
	}
	var nextAttempt *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select next_attempt_at from events where id = $1`, event.ID).Scan(&nextAttempt); err != nil {
		t.Fatalf("read the event's next attempt: %v", err)
	}
	if nextAttempt != nil {
		t.Fatalf("next_attempt_at = %v, though the record write failed", *nextAttempt)
	}

	if _, err := database.Pool.Exec(context.Background(), `drop trigger refuse_destination on events`); err != nil {
		t.Fatalf("let the record write succeed: %v", err)
	}
	broker.Publish(model.Event{})
	waitFor(t, 5*time.Second, "the event published once the store records again", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	if destinations, attempts := publishedDestinations(t, database, event.ID); len(destinations) != 3 || attempts != 0 {
		t.Fatalf("published_destinations = %v and attempt_count %d, want the three destinations and no attempt", destinations, attempts)
	}
}

// publishOnce runs one publish of event, routed to role:reviewer, as the scan would: with its
// payload decoded from JSON, as the scan decodes it from the row.
func publishOnce(t *testing.T, database *store.Store, publisher Publisher, event model.Event) error {
	t.Helper()
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	event.Payload = nil
	if err := json.Unmarshal(raw, &event.Payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	route := "role:reviewer"
	return publish(context.Background(), withHeldRoles(Deps{Store: database, Publisher: publisher}), event, "", routing{route: &route}, map[string]struct{}{})
}

func denial(subject string) error {
	return fmt.Errorf("bus: %w to %q", bus.ErrPublishDenied, subject)
}

// An attempt that goes on past a denial and then ends at a connection failure returns both: the
// denial is not lost behind the error that ended the attempt.
func TestAnAttemptEndedAfterADenialReturnsBoth(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	event := seedThreeDestinationEvent(t, database, broker)
	unreachable := errors.New("bus: waiting for NATS to reconnect: context deadline exceeded")
	publisher := &countingPublisher{failTopics: map[string]error{
		roleTopic:                    denial(roleTopic),
		"notifications.agent.writer": unreachable,
	}}

	err := publishOnce(t, database, publisher, event)
	if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, unreachable) {
		t.Fatalf("publish returned %v, want the role route's denial and the author route's connection failure", err)
	}
}

// Each denial names the destination it was for - the route, an author, a follower, the previous
// claimant - since the subject alone does not say which, and it is still a bus.ErrPublishDenied.
// One event cannot have all four: a reply to a session's comment reaches its author, a reply in an
// ask's thread reaches the ask's followers, and a claim reaches the previous claimant, each besides
// the issue's route.
func TestEveryDenialNamesItsDestination(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	reply := seedThreeDestinationEvent(t, database, broker)
	askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "asker"}, "Ship it?")
	seedFollower(t, database, askID, "follower")
	askReply := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: "answer", IssueKey: new("T-1"), Body: "Yes", AskID: &askID,
		}},
	})
	claim := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "issue.claimed", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{
			"key":            "T-1",
			"previous_claim": map[string]any{"actor": map[string]any{"kind": "session", "id": "claimant"}},
		},
	})
	failTopics := map[string]error{roleTopic: denial(roleTopic)}
	for _, session := range []string{"writer", "asker", "follower", "claimant"} {
		failTopics[contracts.AgentSubject(session)] = denial(contracts.AgentSubject(session))
	}
	publisher := &countingPublisher{failTopics: failTopics}

	for _, tc := range []struct {
		name   string
		event  model.Event
		labels []string
	}{
		{"reply to a session's comment", reply, []string{`publish route "role:reviewer"`, `publish author route to "writer"`}},
		{"reply in an ask's thread", askReply, []string{`publish route "role:reviewer"`, `publish follower route to "asker"`, `publish follower route to "follower"`}},
		{"claim", claim, []string{`publish route "role:reviewer"`, `publish claim change to "claimant"`}},
	} {
		err := publishOnce(t, database, publisher, tc.event)
		if !errors.Is(err, bus.ErrPublishDenied) {
			t.Fatalf("%s: publish returned %v, want a bus.ErrPublishDenied", tc.name, err)
		}
		for _, label := range tc.labels {
			if !strings.Contains(err.Error(), label+": bus: NATS denied the publish") {
				t.Fatalf("%s: publish returned %q, which does not name the denied destination as %s", tc.name, err, label)
			}
		}
	}
}
