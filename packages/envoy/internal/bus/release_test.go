package bus_test

import (
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// releaseDurable adds the push durable name for a test to bind, as a delivery intake binds its own,
// and returns name.
func releaseDurable(t *testing.T, client *bus.Client, name string) string {
	t.Helper()
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        name,
		DeliverSubject: natsgo.NewInbox(),
		AckPolicy:      natsgo.AckExplicitPolicy,
		FilterSubjects: bus.StreamSubjects(),
	}); err != nil {
		t.Fatalf("add the durable %s: %v", name, err)
	}
	return name
}

// bindCounting binds client's JetStream subscription to consumer with a handler that counts and
// acknowledges what it is handed.
func bindCounting(t *testing.T, client *bus.Client, consumer string, count *atomic.Int32) *natsgo.Subscription {
	t.Helper()
	sub, err := client.Subscribe("", func(msg *natsgo.Msg) {
		count.Add(1)
		_ = msg.Ack()
	}, natsgo.Bind(bus.Stream, consumer), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("bind the durable: %v", err)
	}
	return sub
}

// drainSubscription drains sub and waits for nats.go to end the drain.
func drainSubscription(t *testing.T, sub *natsgo.Subscription) {
	t.Helper()
	if err := sub.Drain(); err != nil {
		t.Fatalf("drain the subscription: %v", err)
	}
	waitFor(t, 10*time.Second, "the drain to end", func() bool { return !sub.IsValid() })
}

// awaitHook waits for the reconnect hook to report a run, which comes after the client's restore of
// its subscriptions on that connection event.
func awaitHook(t *testing.T, hooked <-chan struct{}) {
	t.Helper()
	select {
	case <-hooked:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect hooks never ran")
	}
}

func publishReleaseProbe(t *testing.T, client *bus.Client, subject string) {
	t.Helper()
	if _, err := client.JS().Publish(subject, []byte(`{}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// A subscription its caller has drained and released stays unbound across a reconnect, in place or
// onto a connection the client dials to replace a closed one: without the release the client binds
// the drained registration again, and its stopped handler takes the durable's deliveries. A later
// Subscribe on the same client binds the durable afresh and is handed what was published meanwhile.
func TestAReleasedSubscriptionIsNotBoundAgainByAReconnect(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reconnect func(*bus.Client) error
	}{
		{name: "in place", reconnect: func(client *bus.Client) error { return client.Conn.ForceReconnect() }},
		{name: "replaced connection", reconnect: func(client *bus.Client) error {
			client.Conn.Close()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, uri := startNATS(t)
			client, err := bus.ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			consumer := releaseDurable(t, client, "released")
			hooked := make(chan struct{}, 8)
			client.AddReconnectHook(func(*natsgo.Conn) error {
				hooked <- struct{}{}
				return nil
			})

			var stopped atomic.Int32
			sub := bindCounting(t, client, consumer, &stopped)
			drainSubscription(t, sub)
			client.Release(sub)

			if err := tc.reconnect(client); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			awaitHook(t, hooked)
			publishReleaseProbe(t, client, "notifications.github.test.released")
			// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a
			// rebind would show as push-bound, and be handed the message, within moments.
			time.Sleep(time.Second)
			if client.SubOK() {
				t.Fatal("SubOK reports a subscription bound after it was released")
			}
			info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
			if err != nil {
				t.Fatalf("read the durable: %v", err)
			}
			if info.PushBound {
				t.Fatal("the reconnect bound the released subscription to the durable again")
			}
			if got := stopped.Load(); got != 0 {
				t.Fatalf("the released subscription's handler was handed %d message(s) after the reconnect", got)
			}

			var fresh atomic.Int32
			bindCounting(t, client, consumer, &fresh)
			waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
			if !client.SubOK() {
				t.Fatal("SubOK reports the fresh subscription unbound")
			}
			if got := stopped.Load(); got != 0 {
				t.Fatalf("the released subscription's handler was handed %d message(s)", got)
			}
		})
	}
}

// A reconnect that lands after a subscription's drain ended and before its caller released it binds
// the drained registration again; the release then gives up the registration and unsubscribes that
// rebound handle, so the durable is left free and the stopped handler is handed nothing more.
func TestReleaseUnsubscribesWhatARestoreBoundAfterTheSubscriptionEnded(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	consumer := releaseDurable(t, client, "released")
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	var stopped atomic.Int32
	sub := bindCounting(t, client, consumer, &stopped)
	drainSubscription(t, sub)
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	awaitHook(t, hooked)
	if !client.SubOK() {
		t.Fatal("the reconnect did not bind the drained registration again; the test proves nothing")
	}

	client.Release(sub)
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was released")
	}
	waitFor(t, 10*time.Second, "the durable to lose its subscriber", func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && !info.PushBound
	})
	publishReleaseProbe(t, client, "notifications.github.test.released")
	time.Sleep(time.Second)
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the released subscription's handler was handed %d message(s)", got)
	}
}

// Release of a handle a later Subscribe replaced gives up nothing: the replacement stays the
// client's subscription, and a recovery onto a replacement connection binds it again.
func TestReleaseOfAReplacedSubscriptionKeepsTheReplacement(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	// Two durables, so the replacement's bind does not race the server's release of the binding the
	// replaced handle held.
	replacedDurable := releaseDurable(t, client, "replaced")
	replacementDurable := releaseDurable(t, client, "replacement")

	var first, second atomic.Int32
	replaced := bindCounting(t, client, replacedDurable, &first)
	bindCounting(t, client, replacementDurable, &second)
	client.Release(replaced)
	if !client.SubOK() {
		t.Fatal("releasing a replaced handle gave up its replacement")
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "the recovery to bind the replacement again", client.SubOK)
	publishReleaseProbe(t, client, "notifications.github.test.replaced")
	waitFor(t, 10*time.Second, "the replacement to be handed the message", func() bool { return second.Load() == 1 })
	if got := first.Load(); got != 0 {
		t.Fatalf("the replaced subscription's handler was handed %d message(s)", got)
	}
}
