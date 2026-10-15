package bus_test

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestAnotherSubscriptionOnOneTransportIsRefused: a client holds one subscription per transport, so
// a registration on another subject, or the same subject in another queue group, than the one it
// holds is refused with ErrSubscriptionHeld and leaves the held subscription delivering. Replacing it
// silently is how two of Dispatch's delivery intakes on one client lost one of their durables
// (LEGION-690). A registration of the same subject and queue still replaces the held one, as the
// listener's self-health rebuild relies on.
func TestAnotherSubscriptionOnOneTransportIsRefused(t *testing.T) {
	uri := testnats.URL(t)
	publisher, err := nats.Connect(uri)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)

	const held = "notifications.role.held"
	for _, refused := range []struct {
		name         string
		queue        string
		otherSubject string
		otherQueue   string
	}{
		{name: "another subject", otherSubject: held + "-other"},
		{name: "another queue group", queue: "held-queue", otherSubject: held, otherQueue: "other-queue"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			client, err := bus.Connect([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			subscribe := func(subject, queue string, handler nats.MsgHandler) (*nats.Subscription, error) {
				if queue == "" {
					return client.SubscribeCore(subject, handler)
				}
				return client.SubscribeCore(subject, handler, queue)
			}

			received := make(chan string, 4)
			first, err := subscribe(held, refused.queue, func(msg *nats.Msg) { received <- string(msg.Data) })
			if err != nil {
				t.Fatalf("subscribe %s: %v", held, err)
			}
			// A SUB is asynchronous on the wire: flush it, so the server holds it before the
			// publisher's message arrives from another connection.
			if err := client.Conn.Flush(); err != nil {
				t.Fatalf("flush the subscription: %v", err)
			}
			if _, err := subscribe(refused.otherSubject, refused.otherQueue, func(*nats.Msg) {}); !errors.Is(err, bus.ErrSubscriptionHeld) {
				t.Fatalf("subscribe %s (queue %q) beside %s (queue %q) = %v, want ErrSubscriptionHeld",
					refused.otherSubject, refused.otherQueue, held, refused.queue, err)
			}
			if !first.IsValid() {
				t.Fatal("the refused registration unsubscribed the held subscription")
			}
			if err := publisher.Publish(held, []byte("still held")); err != nil {
				t.Fatalf("publish: %v", err)
			}
			select {
			case got := <-received:
				if got != "still held" {
					t.Fatalf("received %q, want the held subscription's message", got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the held subscription stopped delivering after the refused registration")
			}

			// The same subject and queue replace the held subscription, unsubscribing its handle.
			replacement, err := subscribe(held, refused.queue, func(*nats.Msg) {})
			if err != nil {
				t.Fatalf("subscribe %s again: %v", held, err)
			}
			if first.IsValid() {
				t.Fatal("re-registering the held subject left the previous handle subscribed")
			}
			if !replacement.IsValid() {
				t.Fatal("re-registering the held subject did not install the replacement")
			}
		})
	}
}
