package bus_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// connectWithHandler connects a client and subscribes a core handler that signals when a
// delivery reaches it and then holds it for hold.
func connectWithHandler(t *testing.T, hold time.Duration) (*bus.Client, <-chan struct{}, *atomic.Bool) {
	t.Helper()
	_, uri := startNATS(t)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	finished := &atomic.Bool{}
	if _, err := client.Conn.Subscribe("drain.in-flight", func(*natsgo.Msg) {
		close(started)
		time.Sleep(hold)
		finished.Store(true)
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Publish("drain.in-flight", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}
	return client, started, finished
}

// A drain lets a delivery already in its handler finish before the connection closes. nats.go's
// Drain only starts the drain, so returning when it returns would close under the handler.
func TestDrainLetsAnInFlightDeliveryFinish(t *testing.T) {
	client, _, finished := connectWithHandler(t, 300*time.Millisecond)
	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !finished.Load() {
		t.Fatal("Drain returned while a delivery was still in its handler")
	}
	if !client.Conn.IsClosed() {
		t.Fatal("Drain returned with the connection still open")
	}
}

// A drain that cannot finish closes the connection at its deadline, so a handler that never
// returns cannot keep a shutting-down process alive.
func TestDrainClosesTheConnectionAtItsDeadline(t *testing.T) {
	client, _, _ := connectWithHandler(t, 5*time.Second)
	started := time.Now()
	err := client.Drain(200 * time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Drain returned after %s, want about its 200ms deadline", elapsed)
	}
	if !client.Conn.IsClosed() {
		t.Fatal("Drain returned at its deadline with the connection still open")
	}
}

// The drain's close is a shutdown, not a lost connection: the client does not reconnect and
// re-subscribe afterwards, which would hand deliveries to a process that is about to exit.
func TestDrainDoesNotReconnect(t *testing.T) {
	client, _, _ := connectWithHandler(t, 0)
	if _, err := client.SubscribeCore("notifications.role.>", func(*natsgo.Msg) {}, "drain-test"); err != nil {
		t.Fatalf("subscribe role lane: %v", err)
	}
	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	time.Sleep(2 * time.Second)
	if client.Connected() {
		t.Fatal("the client reconnected after Drain closed its connection")
	}
}
