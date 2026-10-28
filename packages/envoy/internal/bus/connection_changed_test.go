package bus

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A publish that waits for NATS to reconnect is woken by the reconnect itself: it returns within
// 100 ms of the connection becoming connected again, and its wait loop passes a bounded number of
// times over a 2 s outage instead of polling the connection's status every few milliseconds. The
// wait also reads the status again every reconnectRecheck without an event, so the bound is one
// pass for the first wait, one for each reconnectRecheck the publish spent waiting, and one for a
// connection event that woke it before the connection was connected: about 5 over the outage and
// nats.go's reconnect wait of up to 1.1 s, where a 10 ms poll would pass some 300 times.
func TestAPublishWaitingForAReconnectWakesOnTheReconnect(t *testing.T) {
	const outage = 2 * time.Second
	server, url := testnats.Start(t)
	client, err := Connect([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	client.mu.Lock()
	conn := client.Conn
	client.mu.Unlock()

	server.Stop(t)
	deadline := time.Now().Add(5 * time.Second)
	for client.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("the client still reads connected 5s after its server stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	reconnected := conn.StatusChanged(nats.CONNECTED)
	waitsBefore := client.reconnectWaits.Load()

	type outcome struct {
		err error
		at  time.Time
	}
	published := make(chan outcome, 1)
	started := time.Now()
	go func() {
		err := client.PublishCore(contracts.Envelope{EventID: "evt-reconnect-wake", Topic: "notifications.role.reconnect-wake"})
		published <- outcome{err: err, at: time.Now()}
	}()
	time.Sleep(outage)
	server.Start(t)

	var reconnectedAt time.Time
	select {
	case <-reconnected:
		reconnectedAt = time.Now()
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not reconnect within 10s of its server starting again")
	}
	var result outcome
	select {
	case result = <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("the publish did not return within 5s of the reconnect")
	}
	if result.err != nil {
		t.Fatalf("the publish waiting through the reconnect answered %v, want nil", result.err)
	}
	lag := result.at.Sub(reconnectedAt)
	waits := client.reconnectWaits.Load() - waitsBefore
	t.Logf("the publish returned %s after the reconnect; its wait loop passed %d times", lag, waits)
	if lag > 100*time.Millisecond {
		t.Fatalf("the publish returned %s after the reconnect, want within 100ms", lag)
	}
	if allowed := 2 + uint64(result.at.Sub(started)/reconnectRecheck); waits > allowed {
		t.Fatalf("the publish's wait loop passed %d times over %s of waiting, want at most %d", waits, result.at.Sub(started), allowed)
	}
}

// nats.go runs a connection's callbacks one after another on one goroutine, so a reconnect's event
// can wait behind a slow callback queued before it, here the disconnect's, held until the test
// ends. A publish waiting for the reconnect still returns once the connection is connected: its
// wait reads the connection's own status again every reconnectRecheck, with the event still queued.
func TestAPublishWaitingForAReconnectWhoseEventIsDelayedStillReturns(t *testing.T) {
	server, url := testnats.Start(t)
	client, err := Connect([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	client.mu.Lock()
	conn := client.Conn
	client.mu.Unlock()
	release := make(chan struct{})
	disconnected := conn.Opts.DisconnectedErrCB
	conn.SetDisconnectErrHandler(func(nc *nats.Conn, err error) {
		<-release
		disconnected(nc, err)
	})
	// Runs before client.Close, so the held callback lets the close's own callbacks run.
	t.Cleanup(func() { close(release) })

	server.Stop(t)
	deadline := time.Now().Add(5 * time.Second)
	for client.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("the client still reads connected 5s after its server stopped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	reconnected := conn.StatusChanged(nats.CONNECTED)
	published := make(chan error, 1)
	go func() {
		published <- client.PublishCore(contracts.Envelope{EventID: "evt-reconnect-delayed", Topic: "notifications.role.reconnect-delayed"})
	}()
	server.Start(t)

	var reconnectedAt time.Time
	select {
	case <-reconnected:
		reconnectedAt = time.Now()
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not reconnect within 10s of its server starting again")
	}
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("the publish waiting through the reconnect answered %v, want nil", err)
		}
		if lag := time.Since(reconnectedAt); lag > reconnectRecheck+500*time.Millisecond {
			t.Fatalf("the publish returned %s after the reconnect, want within %s", lag, reconnectRecheck+500*time.Millisecond)
		}
	case <-time.After(reconnectRecheck + 2*time.Second):
		t.Fatalf("the publish had not returned %s after the reconnect whose event was held", reconnectRecheck+2*time.Second)
	}
}
