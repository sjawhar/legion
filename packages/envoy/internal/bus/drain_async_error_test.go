package bus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A drain ends each subscription whose consumer nats.go created and then deletes that consumer. At
// shutdown the listener leaves a KV watcher the last reconnect has not yet replaced for the drain
// to end, and after a NATS restart with a slow reconnect the server has already dropped that
// watcher's ephemeral consumer, so the delete answers "consumer not found". The consumer is gone,
// which is what the delete was for, so the report is a warning: TestListenerFollowsTheInterest-
// RegistryAcrossNATSRestarts refuses any ERROR from a restart or the shutdown after it (LEGION-278).
func TestADrainThatFindsItsConsumerGoneWarns(t *testing.T) {
	_, uri := testnats.Start(t)
	var records lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&records, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	watchBucket(t, client, "drain_async_error")
	if deleted := deleteConsumers(t, uri, "KV_drain_async_error"); deleted != 1 {
		t.Fatalf("deleted %d consumers, want the watcher's one", deleted)
	}

	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got, want := awaitAsyncErrorReports(&records, 5*time.Second), "WARN nats: consumer not found"; got != want {
		t.Fatalf("the drain's async error reports = %q, want %q", got, want)
	}
}

// nats.go runs every async callback on one goroutine, the bus's reconnect hooks included (they run
// inside ReconnectedCB), and closing the connection does not wait for that goroutine. So when a
// shutdown drain finds a watcher's consumer gone while a reconnect hook is still running -- the
// case the test above names: the last reconnect has not yet replaced that watcher -- the drain's
// report runs only after the hook returns, on a connection the drain has already closed. It is
// the same report and stays a warning.
func TestADrainWhoseReportRunsAfterTheConnectionClosedStillWarns(t *testing.T) {
	ctr, uri := testnats.StartRestartable(t)
	var records lockedBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&records, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client, err := bus.Connect([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var enter, unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(release) }) })
	client.AddReconnectHook(func(*natsgo.Conn) error {
		enter.Do(func() { close(entered) })
		<-release
		return nil
	})
	watchBucket(t, client, "drain_after_close")

	testnats.Stop(t, ctr)
	if err := ctr.Start(context.Background()); err != nil {
		t.Fatalf("restart NATS: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the client never reconnected")
	}
	// The restart can already have lost the watcher's ephemeral consumer; whatever is left goes,
	// so the drain's delete finds none.
	deleteConsumers(t, uri, "KV_drain_after_close")

	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !client.Conn.IsClosed() {
		t.Fatal("the drain returned with the connection still open")
	}
	if got := awaitAsyncErrorReports(&records, time.Second); got != "" {
		t.Fatalf("an async error was reported while the reconnect hook still ran: %q", got)
	}
	unblock.Do(func() { close(release) })
	if got, want := awaitAsyncErrorReports(&records, 5*time.Second), "WARN nats: consumer not found"; got != want {
		t.Fatalf("the drain's async error reports = %q, want %q", got, want)
	}
}

// watchBucket creates a KV bucket on the client's connection and watches it, as the listener's
// caches do, draining the watcher's updates once its initial scan is done.
func watchBucket(t *testing.T, client *bus.Client, bucket string) {
	t.Helper()
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket})
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
}

// deleteConsumers deletes every consumer of stream from a separate connection, the way the server
// drops an ephemeral consumer nothing has shown interest in, and returns how many it deleted.
func deleteConsumers(t *testing.T, uri, stream string) int {
	t.Helper()
	admin := testnats.Connect(t, uri)
	adminJS, err := admin.JetStream()
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	deleted := 0
	for name := range adminJS.ConsumerNames(stream) {
		if err := adminJS.DeleteConsumer(stream, name); err != nil {
			t.Fatalf("delete consumer %s: %v", name, err)
		}
		deleted++
	}
	return deleted
}

// awaitAsyncErrorReports returns the bus's async error reports logged so far, as "LEVEL error"
// joined by ", ", once there is at least one or `within` has passed.
func awaitAsyncErrorReports(records *lockedBuffer, within time.Duration) string {
	var reports []string
	for deadline := time.Now().Add(within); len(reports) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		for _, line := range strings.Split(strings.TrimSpace(records.String()), "\n") {
			var record struct {
				Level string `json:"level"`
				Msg   string `json:"msg"`
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(line), &record) == nil && record.Msg == "envoy nats async error" {
				reports = append(reports, record.Level+" "+record.Error)
			}
		}
	}
	return strings.Join(reports, ", ")
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
