package bus_test

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A caller that ends its JetStream subscription for good and gives it up with Release leaves the
// client holding nothing: the client's recovery onto a connection it dials to replace a closed one
// does not bind the stopped subscription again, and a later Subscribe on another subject registers
// rather than being refused with ErrSubscriptionHeld. Without the release, the recovery would hand
// the stopped handler the durable's deliveries again.
func TestAReleasedSubscriptionIsNotBoundAgainAndHoldsNothing(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	const durable, subject = "released", "notifications.github.>"
	if _, err := client.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        durable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  subject,
		AckPolicy:      natsgo.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create the durable: %v", err)
	}
	pushBound := func() bool {
		info, err := client.JS().ConsumerInfo(bus.Stream, durable)
		return err == nil && info.PushBound
	}
	sub, err := client.Subscribe(subject, func(*natsgo.Msg) {}, natsgo.Bind(bus.Stream, durable), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitFor(t, 10*time.Second, "the durable to be push-bound", pushBound)

	if err := sub.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	waitFor(t, 10*time.Second, "the drained subscription to end", func() bool { return !sub.IsValid() && !pushBound() })
	client.Release(sub)

	client.Conn.Close()
	waitFor(t, 30*time.Second, "the client to replace its closed connection", client.Connected)
	// Time for a recovery that binds the released subscription again to reach the server.
	time.Sleep(2 * time.Second)
	if pushBound() {
		t.Fatal("the client's recovery bound the released subscription again")
	}

	other, err := client.Subscribe("notifications.slack.>", func(*natsgo.Msg) {}, natsgo.BindStream(bus.Stream))
	if err != nil {
		t.Fatalf("subscribe on another subject after the release: %v", err)
	}
	t.Cleanup(func() { _ = other.Unsubscribe() })
}
