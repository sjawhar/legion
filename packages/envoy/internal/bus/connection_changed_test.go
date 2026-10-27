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
// times over a 2 s outage instead of polling the connection's status every few milliseconds.
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
	if waits > 3 {
		t.Fatalf("the publish's wait loop passed %d times over a %s outage, want at most 3", waits, outage)
	}
}
