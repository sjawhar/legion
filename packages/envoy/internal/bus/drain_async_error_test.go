package bus_test

import (
	"bytes"
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

	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	var reports []string
	deadline := time.Now().Add(5 * time.Second)
	for len(reports) == 0 && time.Now().Before(deadline) {
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
	if got, want := strings.Join(reports, ", "), "WARN nats: consumer not found"; got != want {
		t.Fatalf("the drain's async error reports = %q, want %q", got, want)
	}
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
