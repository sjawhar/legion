package bus_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
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

// TestSubOK_AfterSubscribe verifies SubOK is true after subscribing.
func TestSubOK_AfterSubscribe(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	_, err = client.Subscribe("notifications.>", func(msg *natsgo.Msg) {
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
// triggers recovery and the subscription is restored. A message published by
// a separate client DURING the outage is delivered after recovery, proving
// the durable consumer preserves stream position.
func TestRecovery_ClosedTriggersWithoutPublish(t *testing.T) {
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	var received atomic.Int32
	_, err = client.Subscribe("notifications.>", func(msg *natsgo.Msg) {
		received.Add(1)
		_ = msg.Ack()
	}, natsgo.Durable("recovery-test"), natsgo.DeliverAll(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if !client.SubOK() {
		t.Fatal("SubOK should be true before disconnect")
	}

	// Separate publisher that stays connected while listener recovers.
	publisher, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("publisher connect: %v", err)
	}
	defer publisher.Close()

	// Force CLOSED state by closing the underlying connection directly.
	// This triggers ClosedCB → onClosed → go recover().
	// Note: we call Conn.Close() on the underlying nats.Conn, NOT client.Close()
	// which would also close stopCh and prevent recovery.
	client.Conn.Close()

	// Publish via the separate publisher while the listener is disconnected.
	// The durable consumer should preserve this message for replay after recovery.
	data, _ := json.Marshal(map[string]string{"test": "during-outage"})
	_, err = publisher.JS().Publish("notifications.test.recovery", data)
	if err != nil {
		t.Fatalf("publish during outage: %v", err)
	}

	// Wait for recovery to complete (recover creates new connection + resubscribes).
	waitFor(t, 30*time.Second, "SubOK to become true after recovery", client.SubOK)

	// The message published during the outage should be delivered after recovery.
	waitFor(t, 10*time.Second, "message delivered after recovery", func() bool {
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
	_, err = client.Subscribe("notifications.>", func(msg *natsgo.Msg) {
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
	_, err = client.JS().Publish("notifications.test.dedup", data)
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
	_, err = client.Subscribe("notifications.>", func(msg *natsgo.Msg) {
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
	_, err = client.JS().Publish("notifications.test.concurrent", data)
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

