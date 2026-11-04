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
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// refusingPublisher refuses one topic as NATS refuses an envelope it cannot take, and publishes
// the rest, counting every attempt by topic.
type refusingPublisher struct {
	publishOnly
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

func (p *refusingPublisher) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *refusingPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(p.Publish(item))
}

// Request takes a role forward by its envelope's topic, the role's, as Publish takes a publish.
func (p *refusingPublisher) Request(_ string, item contracts.Envelope, _ bus.RequestOptions) <-chan error {
	return answered(p.Publish(item))
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

// A destination NATS refuses is refused the same way however often the outbox retries it, so
// the event goes out to it once more with its payload left out (standInEnvelope). This
// publisher refuses that destination whatever it carries, so the stand-in is refused too and
// the destination is done: the event's refusal is counted once and logged once, the stand-in's
// own refusal says it was refused too and is counted no second time, no line says the stand-in
// went out because it never reached NATS, the event's other destinations are published, and the
// event is marked published instead of backing off forever. A stand-in the bus takes is in
// event_share_nats_test.go, against a real NATS server, and it is that send that logs the line
// saying a stand-in went out.
func TestRunSendsAStandInForARefusedDestinationAndIsDoneOnceThatIsRefusedToo(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

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
	registry := metrics.New()
	deps := withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: broker, Listen: database.Pool.Listener})
	deps.PublishFailures = registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
		"Destinations of an outbox publish attempt that did not reach NATS")
	stop := runDeps(t, deps, time.Second)
	defer stop()

	waitFor(t, 7*time.Second, "the event marked published past its refused destination", func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	attempts, published := publisher.snapshot()
	if attempts[refused] != 2 {
		t.Fatalf("the refused destination was attempted %d times, want twice: the event, then its stand-in", attempts[refused])
	}
	if strings.Join(published, ",") != "notifications.role.reviewer" {
		t.Fatalf("published %v, want the role route past the refused destination", published)
	}
	refusals, standInRefusals, standInsOut := 0, 0, 0
	for _, line := range strings.Split(logged.String(), "\n") {
		if !strings.Contains(line, fmt.Sprintf("event_id=%d", event.ID)) || !strings.Contains(line, refused) {
			continue
		}
		if strings.Contains(line, "dispatch outbox: destination refused") {
			refusals++
		}
		if strings.Contains(line, "dispatch outbox: the stand-in was refused too") {
			standInRefusals++
		}
		if strings.Contains(line, "dispatch outbox: a stand-in went out with the payload left out") {
			standInsOut++
		}
	}
	if refusals != 1 || standInRefusals != 1 || standInsOut != 0 {
		t.Fatalf("logged %d refusal lines, %d stand-in refusal lines and %d stand-in-went-out lines naming the event and its topic, want one, one and none:\n%s",
			refusals, standInRefusals, standInsOut, logged.String())
	}
	// One event, one refusal: the stand-in's own refusal is the same event's, and counting it
	// again would read as two destinations NATS would not take.
	if body := failureReasons(t, registry); !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="refused"} 1`) {
		t.Fatalf("/metrics body = %q, want the refused destination counted once", body)
	}
}
