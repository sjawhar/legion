package bus_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A drain ends each subscription whose consumer nats.go created and then deletes that consumer. At
// shutdown the listener leaves a KV watcher the last reconnect has not yet replaced for the drain
// to end, and that watcher's ordered consumer can already be gone: a NATS restart loses it
// outright, since nats.go creates it with memory storage, and a disconnect longer than its
// inactive threshold lets the server delete it. Then the delete answers "consumer not found". The
// consumer is gone, which is what the delete was for, so the report is a warning:
// TestListenerFollowsTheInterestRegistryAcrossNATSRestarts refuses any ERROR from a restart or
// the shutdown after it (LEGION-278).
//
// A delivery held in its handler keeps the connection's drain open until the delete has reported.
// nats.go removes the watcher's subscription before it sends the delete (checkDrained), and
// drains its request subscription at its next 10 ms check once no other subscription is left
// (drainConnection), without waiting for the delete's answer. An answer that comes back after
// that has no subscription to arrive on, so the close ends the delete with "connection closed",
// and nats.go queues that report after it has closed its async callback queue, so it never runs;
// a delete sent once the drain reaches the publishes is refused as "connection draining", which
// the bus logs at ERROR. Without the held delivery the test reads that scheduling race rather
// than the report. Both functions behave so at the pinned nats.go (v1.50.0) and through v1.54.0;
// re-check on a nats.go bump.
func TestADrainThatFindsItsConsumerGoneWarns(t *testing.T) {
	_, uri := testnats.Start(t)
	logs := captureBusLogs(t)

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "drain_async_error"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	select {
	case <-watcher.Updates():
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never finished its initial scan")
	}
	go func() {
		for range watcher.Updates() {
		}
	}()

	// Delete the watcher's consumer from a second connection, as the server does when it loses it.
	admin := testnats.Connect(t, uri)
	adminJS, err := admin.JetStream()
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	deleted := 0
	for name := range adminJS.ConsumerNames("KV_drain_async_error") {
		if err := adminJS.DeleteConsumer("KV_drain_async_error", name); err != nil {
			t.Fatalf("delete the watcher's consumer %s: %v", name, err)
		}
		deleted++
	}
	if deleted != 1 {
		t.Fatalf("deleted %d consumers, want the watcher's one", deleted)
	}

	held := make(chan struct{})
	if _, err := client.Conn.Subscribe("drain_async_error.held", func(*natsgo.Msg) {
		close(held)
		for deadline := time.Now().Add(5 * time.Second); len(asyncErrorReports(logs)) == 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}); err != nil {
		t.Fatalf("subscribe the held delivery: %v", err)
	}
	if err := client.Conn.Publish("drain_async_error.held", nil); err != nil {
		t.Fatalf("publish the held delivery: %v", err)
	}
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the held delivery never reached its handler")
	}

	// The drain's deadline outlasts the held delivery's 5 s, so a delete that never reports fails
	// on its reports rather than on the drain.
	if err := client.Drain(10 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got, want := strings.Join(asyncErrorReports(logs), ", "), "WARN nats: consumer not found"; got != want {
		t.Fatalf("the drain's async error reports = %q, want %q", got, want)
	}
}

// asyncErrorReports is every async error the bus has logged so far, each as its level and error.
func asyncErrorReports(logs *busLogs) []string {
	var reports []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Msg == "envoy nats async error" {
			reports = append(reports, record.Level+" "+record.Error)
		}
	}
	return reports
}
