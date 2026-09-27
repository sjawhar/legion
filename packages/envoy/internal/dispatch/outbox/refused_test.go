package outbox

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// refusingPublisher refuses one topic as NATS refuses an envelope it cannot take, and publishes
// the rest, counting every attempt by topic.
type refusingPublisher struct {
	mu       sync.Mutex
	refuse   string
	attempts map[string]int
	items    []contracts.Envelope
}

func (p *refusingPublisher) Publish(item contracts.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts[item.Topic]++
	if item.Topic == p.refuse {
		return fmt.Errorf("publish: %w", bus.ErrTooLarge)
	}
	p.items = append(p.items, item)
	return nil
}

func (p *refusingPublisher) snapshot() (map[string]int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	attempts := make(map[string]int, len(p.attempts))
	for topic, n := range p.attempts {
		attempts[topic] = n
	}
	return attempts, topicsOf(p.items)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A destination NATS refuses is refused the same way however often the outbox retries it, so it
// is done: the refusal is logged once, naming the event, the event's other destinations are
// published, and the event is marked published instead of backing off forever.
func TestRunTreatsADestinationNATSRefusesAsDone(t *testing.T) {
	var logged lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "role:reviewer"
	seedIssue(t, database, "T-1", &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Too large for NATS"},
	})
	refused := "notifications.dispatch.issue.T-1.message.created"
	publisher := &refusingPublisher{refuse: refused, attempts: map[string]int{}}
	stop := run(t, database, publisher, broker)
	defer stop()

	waitFor(t, 7*time.Second, "the event marked published past its refused destination", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	attempts, published := publisher.snapshot()
	if attempts[refused] != 1 {
		t.Fatalf("the refused destination was attempted %d times, want once", attempts[refused])
	}
	if strings.Join(published, ",") != "notifications.role.reviewer" {
		t.Fatalf("published %v, want the role route past the refused destination", published)
	}
	lines := 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: destination refused") {
			lines++
			if !strings.Contains(line, fmt.Sprintf("event_id=%d", event.ID)) || !strings.Contains(line, refused) {
				t.Fatalf("refusal line %q does not name the event and its topic", line)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d refusal lines, want one:\n%s", lines, logged.String())
	}
}
