package bus_test

import (
	"errors"
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

// awaitPushBound waits for the durable consumer to report want for its push binding. nats-server
// applies a push subscriber's interest, and its release, on the consumer's own goroutine, after
// the client's bind or unsubscribe is answered, so only the consumer's own report tells a test
// that the server has applied one.
func awaitPushBound(t *testing.T, client *bus.Client, consumer string, want bool, what string) {
	t.Helper()
	waitFor(t, 10*time.Second, what, func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
		return err == nil && info.PushBound == want
	})
}

func publishReleaseProbe(t *testing.T, client *bus.Client, subject string) {
	t.Helper()
	if _, err := client.JS().Publish(subject, []byte(`{}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// A subscription its caller has ended with DrainSubscription stays unbound across a reconnect, in
// place or onto a connection the client dials to replace a closed one: DrainSubscription gives the
// registration up, without which the client would bind the drained registration again and its
// stopped handler take the durable's deliveries. A later Subscribe on the same client binds the
// durable afresh and is handed what was published meanwhile.
func TestADrainedSubscriptionIsNotBoundAgainByAReconnect(t *testing.T) {
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
			drained, err := client.DrainSubscription(sub)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			if drained != sub {
				t.Fatalf("DrainSubscription drained %p, want the handle Subscribe returned (%p)", drained, sub)
			}
			waitFor(t, 10*time.Second, "the drain to end", func() bool { return !drained.IsValid() })

			if err := tc.reconnect(client); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			awaitHook(t, hooked)
			publishReleaseProbe(t, client, "notifications.github.test.released")
			// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a
			// rebind would show as push-bound, and be handed the message, within moments.
			time.Sleep(time.Second)
			if client.SubOK() {
				t.Fatal("SubOK reports a subscription bound after it was drained")
			}
			info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
			if err != nil {
				t.Fatalf("read the durable: %v", err)
			}
			if info.PushBound {
				t.Fatal("the reconnect bound the drained subscription to the durable again")
			}
			if got := stopped.Load(); got != 0 {
				t.Fatalf("the drained subscription's handler was handed %d message(s) after the reconnect", got)
			}

			var fresh atomic.Int32
			bindCounting(t, client, consumer, &fresh)
			waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
			if !client.SubOK() {
				t.Fatal("SubOK reports the fresh subscription unbound")
			}
			if got := stopped.Load(); got != 0 {
				t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
			}
		})
	}
}

// A subscription that ended otherwise than by DrainSubscription -- its handle drained directly --
// is bound again by the next reconnect; DrainSubscription then drains that rebound handle and gives
// the registration up, so the durable is left free and the stopped handler is handed nothing more.
func TestDrainSubscriptionEndsWhatARestoreBoundAfterTheSubscriptionEnded(t *testing.T) {
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
	// Wait for the server to apply the restore's bind before draining it: the drain and the checks
	// below would otherwise pass on a binding the server applies afterwards.
	awaitPushBound(t, client, consumer, true, "the durable to take the restore's binding")

	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained == nil || drained == sub {
		t.Fatalf("DrainSubscription drained %p, want the handle the restore bound, not the one Subscribe returned (%p)", drained, sub)
	}
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was drained")
	}
	awaitPushBound(t, client, consumer, false, "the durable to lose its subscriber")
	publishReleaseProbe(t, client, "notifications.github.test.released")
	time.Sleep(time.Second)
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
	}
}

// DrainSubscription of a handle a later Subscribe replaced drains and gives up nothing: the
// replacement stays the client's subscription, and a recovery onto a replacement connection binds
// it again.
func TestDrainSubscriptionOfAReplacedSubscriptionKeepsTheReplacement(t *testing.T) {
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
	if drained, err := client.DrainSubscription(replaced); drained != nil || err != nil {
		t.Fatalf("DrainSubscription of a replaced handle = %p, %v; want nothing drained", drained, err)
	}
	if !client.SubOK() {
		t.Fatal("draining a replaced handle gave up its replacement")
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "the recovery to bind the replacement again", client.SubOK)
	publishReleaseProbe(t, client, "notifications.github.test.replaced")
	waitFor(t, 10*time.Second, "the replacement to be handed the message", func() bool { return second.Load() == 1 })
	if got := first.Load(); got != 0 {
		t.Fatalf("the replaced subscription's handler was handed %d message(s)", got)
	}
}

// subscribeAbsent subscribes client's JetStream subscription to the durable name before it exists,
// with a handler that counts what it is handed, and requires the bind to fail: the client keeps
// the registration, which a reconnect binds once the durable exists.
func subscribeAbsent(t *testing.T, client *bus.Client, name string, count *atomic.Int32) {
	t.Helper()
	sub, err := client.Subscribe("", func(msg *natsgo.Msg) {
		count.Add(1)
		_ = msg.Ack()
	}, natsgo.Bind(bus.Stream, name), natsgo.ManualAck())
	if err == nil {
		t.Fatalf("bound the durable %s before it existed (%v); the test proves nothing", name, sub)
	}
}

// A subscription whose bind failed, and whose caller gave up its registration, stays unbound
// across a reconnect, in place or onto a connection the client dials to replace a closed one, even
// once the durable it named exists: without the release the client binds the failed registration,
// and the handler its caller gave up takes the durable's deliveries. A later Subscribe on the same
// client binds the durable and is handed what was published meanwhile.
func TestAFailedSubscriptionReleasedIsNotBoundByAReconnect(t *testing.T) {
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
			hooked := make(chan struct{}, 8)
			client.AddReconnectHook(func(*natsgo.Conn) error {
				hooked <- struct{}{}
				return nil
			})

			var failed atomic.Int32
			subscribeAbsent(t, client, "failed", &failed)
			client.ReleaseFailed("")
			consumer := releaseDurable(t, client, "failed")

			if err := tc.reconnect(client); err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			awaitHook(t, hooked)
			publishReleaseProbe(t, client, "notifications.github.test.failed")
			// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a
			// bind would show as push-bound, and be handed the message, within moments.
			time.Sleep(time.Second)
			if client.SubOK() {
				t.Fatal("SubOK reports a subscription bound after its failed registration was released")
			}
			info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
			if err != nil {
				t.Fatalf("read the durable: %v", err)
			}
			if info.PushBound {
				t.Fatal("the reconnect bound the released failed registration to the durable")
			}
			if got := failed.Load(); got != 0 {
				t.Fatalf("the failed subscription's handler was handed %d message(s) after the reconnect", got)
			}

			var fresh atomic.Int32
			bindCounting(t, client, consumer, &fresh)
			waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
			if !client.SubOK() {
				t.Fatal("SubOK reports the fresh subscription unbound")
			}
			if got := failed.Load(); got != 0 {
				t.Fatalf("the failed subscription's handler was handed %d message(s)", got)
			}
		})
	}
}

// A reconnect that lands after a subscription's bind failed and before its caller gave up the
// registration binds it; ReleaseFailed then gives up the registration and unsubscribes that bound
// handle, so the durable is left free and the handler is handed nothing.
func TestReleaseFailedUnsubscribesWhatARestoreBound(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})

	var failed atomic.Int32
	subscribeAbsent(t, client, "failed", &failed)
	consumer := releaseDurable(t, client, "failed")
	if err := client.Conn.ForceReconnect(); err != nil {
		t.Fatalf("reconnect in place: %v", err)
	}
	awaitHook(t, hooked)
	if !client.SubOK() {
		t.Fatal("the reconnect did not bind the failed registration; the test proves nothing")
	}
	// Wait for the server to apply the restore's bind before releasing it: only once the durable
	// has reported the binding, and then its loss, is the server known to have applied both, so the
	// probe published next stays pending for the fresh bind rather than going to the released
	// handle's deliver inbox and waiting out the consumer's AckWait there.
	awaitPushBound(t, client, consumer, true, "the durable to take the restore's binding")

	client.ReleaseFailed("")
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after its failed registration was released")
	}
	awaitPushBound(t, client, consumer, false, "the durable to lose its subscriber")
	publishReleaseProbe(t, client, "notifications.github.test.failed")
	time.Sleep(time.Second)
	if got := failed.Load(); got != 0 {
		t.Fatalf("the failed subscription's handler was handed %d message(s)", got)
	}

	var fresh atomic.Int32
	bindCounting(t, client, consumer, &fresh)
	waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
}

// ReleaseFailed gives up only a registration whose Subscribe failed: not the one a later Subscribe
// on the subject made and bound, nor the one a Subscribe on another subject was refused beside
// (ErrSubscriptionHeld). The kept subscription is bound again by a recovery onto a replacement
// connection.
func TestReleaseFailedKeepsASubscriptionThatBound(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var failed, bound atomic.Int32
	subscribeAbsent(t, client, "later", &failed)
	consumer := releaseDurable(t, client, "later")
	bindCounting(t, client, consumer, &bound)
	client.ReleaseFailed("")
	if !client.SubOK() {
		t.Fatal("ReleaseFailed gave up a later Subscribe's bound subscription")
	}
	const other = "notifications.github.test.other"
	if _, err := client.Subscribe(other, func(*natsgo.Msg) {}); !errors.Is(err, bus.ErrSubscriptionHeld) {
		t.Fatalf("subscribe on another subject: %v, want %v", err, bus.ErrSubscriptionHeld)
	}
	client.ReleaseFailed(other)
	if !client.SubOK() {
		t.Fatal("ReleaseFailed of a refused subject gave up the held subscription")
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "the recovery to bind the kept subscription again", client.SubOK)
	publishReleaseProbe(t, client, "notifications.github.test.later")
	waitFor(t, 10*time.Second, "the kept subscription to be handed the message", func() bool { return bound.Load() == 1 })
	if got := failed.Load(); got != 0 {
		t.Fatalf("the failed subscription's handler was handed %d message(s)", got)
	}
}

// DrainSubscription after the client replaced its connection drains the handle the client bound
// on the new connection, the one delivering, rather than the handle Subscribe returned, which went
// with the closed connection: the drain answers no error and the durable loses its subscriber.
// No recovery binds the drained registration again, and a later Subscribe binds the durable afresh
// and is handed what was published meanwhile.
func TestDrainSubscriptionDrainsTheHandleARestoreBound(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	consumer := releaseDurable(t, client, "drained")
	hooked := make(chan struct{}, 8)
	client.AddReconnectHook(func(*natsgo.Conn) error {
		hooked <- struct{}{}
		return nil
	})
	var stopped atomic.Int32
	sub := bindCounting(t, client, consumer, &stopped)

	client.Conn.Close()
	awaitHook(t, hooked)
	waitFor(t, 30*time.Second, "the recovery to bind the registration again", client.SubOK)
	if sub.IsValid() {
		t.Fatal("the handle Subscribe returned is still valid after its connection closed; the test proves nothing")
	}
	// Wait for the server to apply the recovery's bind before draining it, so the drain's release
	// of the binding below is one the server has something to apply it to.
	awaitPushBound(t, client, consumer, true, "the durable to take the recovery's binding")

	drained, err := client.DrainSubscription(sub)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained == nil || drained == sub {
		t.Fatalf("DrainSubscription drained %p, want the handle the recovery bound, not the one Subscribe returned (%p)", drained, sub)
	}
	waitFor(t, 10*time.Second, "the drain to end", func() bool { return !drained.IsValid() })
	awaitPushBound(t, client, consumer, false, "the durable to lose its subscriber")

	client.Conn.Close()
	awaitHook(t, hooked)
	publishReleaseProbe(t, client, "notifications.github.test.drained")
	// nats-server applies a new subscriber's interest on the consumer's own goroutine, so a rebind
	// would show as push-bound, and be handed the message, within moments.
	time.Sleep(time.Second)
	if client.SubOK() {
		t.Fatal("SubOK reports a subscription bound after it was drained")
	}
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	if info.PushBound {
		t.Fatal("the recovery bound the drained registration to the durable again")
	}
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
	}

	var fresh atomic.Int32
	bindCounting(t, client, consumer, &fresh)
	waitFor(t, 10*time.Second, "the fresh subscription to be handed the message", func() bool { return fresh.Load() == 1 })
	if got := stopped.Load(); got != 0 {
		t.Fatalf("the drained subscription's handler was handed %d message(s)", got)
	}
}
