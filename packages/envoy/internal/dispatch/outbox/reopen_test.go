package outbox

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// reopeningPublisher accepts every envelope and records its topic, and runs reopen once, as its
// first envelope is sent: a writer reopening the event while its batch is publishing it.
type reopeningPublisher struct {
	mu     sync.Mutex
	topics []string
	reopen func()
	once   sync.Once
}

func (p *reopeningPublisher) Window() bus.Sender { return p }

func (p *reopeningPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	p.once.Do(p.reopen)
	p.mu.Lock()
	p.topics = append(p.topics, item.Topic)
	p.mu.Unlock()
	return answered(nil)
}

func (p *reopeningPublisher) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.topics)
}

// A comment's comment.created reopened while a batch is publishing it - its mention failed, so
// the route copy its creation suppressed for the mentioned session is owed after all - is not
// marked published by that batch, which sent the payload as it read it, without that copy. The
// batch records the destination it reached, and the next scan sends the reopened row to the
// route alone.
func TestAnEventReopenedWhileItsBatchPublishesItIsPublishedAgainToWhatItHasNotReached(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", new("session:s1"))
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "comment.created",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{
			"id":               "00000000-0000-0000-0000-000000000001",
			"suppress_route":   true,
			"suppressed_route": "session:s1",
		},
	})
	owner := "notifications.dispatch.issue.T-1.comment.created"
	publisher := &reopeningPublisher{reopen: func() {
		if _, err := database.Pool.Exec(ctx, `
			update events
			set payload = (payload - 'suppressed_route') || '{"suppress_route": false}',
			    published_at = null, next_attempt_at = null
			where id = $1
		`, event.ID); err != nil {
			t.Errorf("reopen the event: %v", err)
		}
	}}
	deps := Deps{Store: database, Publisher: publisher}

	if _, _, err := scanBatch(ctx, deps); err != nil {
		t.Fatalf("the batch the reopen lands in: %v", err)
	}
	if got := publisher.sent(); !slices.Equal(got, []string{owner}) {
		t.Fatalf("the first batch sent %v, want the owner topic alone: it read the route suppressed", got)
	}
	if publishedAt(t, database, event.ID) != nil {
		t.Fatal("the batch that published the payload as it was marked the reopened event published")
	}
	if destinations, _ := publishedDestinations(t, database, event.ID); !slices.Equal(destinations, []string{owner}) {
		t.Fatalf("published_destinations = %v after the first batch, want the owner topic it reached", destinations)
	}

	if _, _, err := scanBatch(ctx, deps); err != nil {
		t.Fatalf("the next batch: %v", err)
	}
	if got := publisher.sent(); !slices.Equal(got, []string{owner, contracts.AgentSubject("s1")}) {
		t.Fatalf("the two batches sent %v, want the owner topic and then s1's route copy alone", got)
	}
	if publishedAt(t, database, event.ID) == nil {
		t.Fatal("the reopened event is still unpublished after the batch that sent it its route copy")
	}
	if destinations, _ := publishedDestinations(t, database, event.ID); !slices.Equal(destinations,
		[]string{owner, contracts.AgentSubject("s1")}) {
		t.Fatalf("published_destinations = %v, want the owner topic and s1's topic", destinations)
	}
}
