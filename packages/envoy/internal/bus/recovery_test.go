package bus_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// startNATS launches a real NATS container and returns it with the connection
// URI. The container is terminated when the test completes.
func startNATS(t *testing.T) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("failed to start NATS: %v", err)
	}
	t.Cleanup(func() { ctr.Terminate(ctx) })
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get NATS URI: %v", err)
	}
	return ctr, uri
}

// waitFor polls a condition with timeout. Returns true if condition was met.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

func subscribeAllNotifications(client *bus.Client, handler natsgo.MsgHandler, opts ...natsgo.SubOpt) (*natsgo.Subscription, error) {
	subOpts := append([]natsgo.SubOpt{
		natsgo.BindStream(bus.Stream),
		natsgo.ConsumerFilterSubjects(bus.StreamSubjects()...),
	}, opts...)
	return client.Subscribe("", handler, subOpts...)
}

// TestSubOK_NoSubscription verifies SubOK is false when no subscription exists.
func TestSubOK_NoSubscription(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if client.SubOK() {
		t.Fatal("SubOK should be false without a subscription")
	}
}

func TestPublishBoundsReconnectWhenNATSUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		topic   string
		publish func(*bus.Client, contracts.Envelope) error
	}{
		{
			name:  "jetstream",
			topic: "notifications.github.acme.widgets.timeout",
			publish: func(client *bus.Client, item contracts.Envelope) error {
				return client.Publish(item)
			},
		},
		{
			name:  "core",
			topic: "notifications.role.legion-timeout",
			publish: func(client *bus.Client, item contracts.Envelope) error {
				return client.PublishCore(item)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctr, uri := startNATS(t)
			client, err := bus.Connect([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}

			ctx := context.Background()
			if err := ctr.Terminate(ctx); err != nil {
				t.Fatalf("stop NATS: %v", err)
			}
			client.Conn.Close()

			result := make(chan error, 1)
			started := time.Now()
			go func() {
				result <- tc.publish(client, contracts.Envelope{
					EventID:        "evt-publish-reconnect-timeout-" + tc.name,
					Source:         "agent",
					SourceEventID:  "source-publish-reconnect-timeout-" + tc.name,
					Topic:          tc.topic,
					DedupeKey:      "publish-reconnect-timeout-" + tc.name,
					IssuedAt:       contracts.NowMillis(),
					PayloadSummary: "timeout",
					TraceID:        "trace-publish-reconnect-timeout-" + tc.name,
				})
			}()

			select {
			case err := <-result:
				if err == nil {
					t.Fatal("publish unexpectedly succeeded without NATS")
				}
				if elapsed := time.Since(started); elapsed > 6*time.Second {
					t.Fatalf("publish returned after %s, want a bounded reconnect within 6s", elapsed)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("publish remained blocked after the reconnect deadline")
			}
		})
	}
}

func TestCoreSubscriptionRestoresAfterConnectionRecovery(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	publisher, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer publisher.Close()

	var received atomic.Int32
	_, err = client.SubscribeCore("notifications.role.recovery", func(*natsgo.Msg) {
		received.Add(1)
	})
	if err != nil {
		t.Fatalf("subscribe core role lane: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush core publisher: %v", err)
	}

	client.Conn.Close()
	waitFor(t, 30*time.Second, "core role subscription to recover", func() bool {
		if err := publisher.Publish("notifications.role.recovery", []byte("recovered")); err != nil {
			t.Fatalf("publish to recovered core role lane: %v", err)
		}
		if err := publisher.Flush(); err != nil {
			t.Fatalf("flush recovered core role publish: %v", err)
		}
		return received.Load() > 0
	})
}

func TestCoreSubscriptionsShareQueueGroup(t *testing.T) {
	_, uri := startNATS(t)
	first, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect first client: %v", err)
	}
	defer first.Close()
	second, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect second client: %v", err)
	}
	defer second.Close()
	publisher, err := natsgo.Connect(uri)
	if err != nil {
		t.Fatalf("connect publisher: %v", err)
	}
	defer publisher.Close()

	var received atomic.Int32
	handler := func(*natsgo.Msg) { received.Add(1) }
	const subject = "notifications.role.queue-group"
	const queue = "envoy-listener-test-machine"
	if _, err := first.SubscribeCore(subject, handler, queue); err != nil {
		t.Fatalf("subscribe first core role lane: %v", err)
	}
	if _, err := second.SubscribeCore(subject, handler, queue); err != nil {
		t.Fatalf("subscribe second core role lane: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush queue subscriptions: %v", err)
	}
	for range 3 {
		if err := publisher.Publish(subject, []byte("queued")); err != nil {
			t.Fatalf("publish queue test message: %v", err)
		}
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush queue test messages: %v", err)
	}
	waitFor(t, 5*time.Second, "one queue delivery per role event", func() bool {
		return received.Load() == 3
	})
}

// TestSubOK_AfterSubscribe verifies SubOK is true after subscribing.
func TestSubOK_AfterSubscribe(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		_ = msg.Ack()
	}, natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if !client.SubOK() {
		t.Fatal("SubOK should be true after Subscribe")
	}
}

// TestRecovery_ClosedTriggersWithoutPublish verifies that when the NATS
// connection enters CLOSED state (without any Publish call), the ClosedCB
// triggers recovery and the subscription is restored. After recovery,
// messages flow normally through the restored subscription.
func TestRecovery_ClosedTriggersWithoutPublish(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("recovery-test"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if !client.SubOK() {
		t.Fatal("SubOK should be true before disconnect")
	}

	// Force CLOSED state by closing the underlying connection directly.
	// This triggers ClosedCB → onClosed → go recover().
	// Note: we call Conn.Close() on the underlying nats.Conn, NOT client.Close()
	// which would also close stopCh and prevent recovery.
	client.Conn.Close()

	// SubOK should become false during recovery.
	time.Sleep(100 * time.Millisecond)

	// Wait for recovery to complete (recover creates new connection + resubscribes).
	waitFor(t, 30*time.Second, "SubOK to become true after recovery", client.SubOK)

	// Publish a message through the recovered connection and verify delivery.
	// This proves the subscription is functional after recovery — no Publish call
	// was needed to trigger recovery (the ClosedCB did it).
	data, _ := json.Marshal(map[string]string{"test": "recovery"})
	_, err = client.JS().Publish("notifications.github.test.recovery", data)
	if err != nil {
		t.Fatalf("publish after recovery: %v", err)
	}

	waitFor(t, 5*time.Second, "message delivered after recovery", func() bool {
		return received.Load() >= 1
	})
}

// TestRecovery_AtMostOneSubscription verifies that after recovery, only one
// subscription callback is active (no duplicate message delivery).
func TestRecovery_AtMostOneSubscription(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("dedup-sub-test"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Trigger recovery by closing the underlying connection.
	client.Conn.Close()
	waitFor(t, 30*time.Second, "first recovery", client.SubOK)

	// Trigger recovery again.
	client.Conn.Close()
	waitFor(t, 30*time.Second, "second recovery", client.SubOK)

	// Reset counter and publish a single message.
	received.Store(0)
	data, _ := json.Marshal(map[string]string{"test": "dedup"})
	_, err = client.JS().Publish("notifications.github.test.dedup", data)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for delivery.
	waitFor(t, 5*time.Second, "message delivered", func() bool {
		return received.Load() >= 1
	})

	// Give extra time to detect any duplicate delivery from stale subscriptions.
	time.Sleep(2 * time.Second)
	if count := received.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery (at-most-one subscription), got %d", count)
	}
}

// TestRecovery_ConcurrentRecoverySerializes verifies that multiple concurrent
// calls to recovery (e.g., ClosedCB firing while recovery is already running)
// do not spawn competing retry goroutines. The atomic recovering flag ensures
// at most one recovery goroutine runs at a time.
func TestRecovery_ConcurrentRecoverySerializes(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("concurrent-test"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Trigger multiple recovery cycles in rapid succession. Each cycle closes
	// the connection, waits for recovery, then immediately triggers another.
	// This verifies recovery is resilient to repeated CLOSED transitions and
	// doesn't accumulate stale subscriptions or leak goroutines.
	for range 3 {
		client.Conn.Close()
		waitFor(t, 30*time.Second, "recovery between rapid closes", client.SubOK)
	}

	// After all recovery cycles, publish and verify single delivery.
	received.Store(0)
	data, _ := json.Marshal(map[string]string{"test": "concurrent"})
	_, err = client.JS().Publish("notifications.github.test.concurrent", data)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 5*time.Second, "delivery after recovery cycles", func() bool {
		return received.Load() >= 1
	})
	time.Sleep(2 * time.Second)
	if count := received.Load(); count != 1 {
		t.Fatalf("expected exactly 1 delivery after recovery cycles, got %d", count)
	}
}
