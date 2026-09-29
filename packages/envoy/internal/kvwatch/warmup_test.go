package kvwatch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// captureRecords routes slog.Default() into a buffer for the test and decodes the JSON records
// back, so a test asserts a line's fields rather than its rendering.
func captureRecords(t *testing.T) func() []map[string]any {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return func() []map[string]any {
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if line == "" {
				continue
			}
			record := map[string]any{}
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("decode a log record: %v (%s)", err, line)
			}
			records = append(records, record)
		}
		return records
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// record returns the one record whose msg is msg, failing when there is none or more than one.
func record(t *testing.T, records []map[string]any, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, r := range records {
		if r["msg"] == msg {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %q record, got %d of them in:\n%v", msg, len(found), records)
	}
	return found[0]
}

// A cache's warm-up says what it cost and how it ended. A listener restart is 17-25 s of no
// deliveries on the on-prem machines (LEGION-374) and nothing in its log said where that went: the
// interest bucket carries ~41,833 delete markers behind 40 live keys, and every warm-up streams
// them all. The two counts are disjoint, like the role restore line's: entries are the keys the
// cache holds afterwards, delete_markers is what the scan streamed past to find them.
func TestAWarmUpLogsWhatItStreamedAndThatItCompleted(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	const (
		live    = 3
		markers = 4
	)
	for i := range live {
		if _, err := kv.Put(fmt.Sprintf("live-%d", i), []byte("1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	for i := range markers {
		key := fmt.Sprintf("gone-%d", i)
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		if err := kv.Delete(key); err != nil {
			t.Fatalf("delete %s: %v", key, err)
		}
	}

	records := captureRecords(t)
	into := newSeen()
	w := kvwatch.New("interest registry", kv, into.apply, into.reset)
	t.Cleanup(w.Stop)
	w.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	line := record(t, records(), "interest registry cache warm-up")
	if line["level"] != "INFO" || line["outcome"] != "completed" {
		t.Fatalf("a completed warm-up logged level=%v outcome=%v, want INFO completed: %v", line["level"], line["outcome"], line)
	}
	if line["entries"] != float64(live) || line["delete_markers"] != float64(markers) {
		t.Fatalf("warm-up line entries=%v delete_markers=%v, want %d and %d: %v",
			line["entries"], line["delete_markers"], live, markers, line)
	}
	if line["bucket"] != kv.Bucket() {
		t.Fatalf("warm-up line bucket=%v, want %q", line["bucket"], kv.Bucket())
	}
	elapsed, ok := line["elapsed_ms"].(float64)
	if !ok || elapsed < 0 {
		t.Fatalf("warm-up line elapsed_ms=%v, want the milliseconds the scan took", line["elapsed_ms"])
	}
	if got := w.WarmUp(); !got.Done || got.TimedOut || got.Entries != live || got.DeleteMarkers != markers {
		t.Fatalf("WarmUp() = %+v, want a completed scan of %d entries and %d markers", got, live, markers)
	}
}

// timingOutKV hands out a watcher over the real bucket that ends its initial scan the way nats.go's
// idle timer does (nats.go v1.50.0 kv.go:1145-1156): after `after` entries it puts
// ErrKeyWatcherTimeout on Error() and sends the nil marker a complete scan also sends. The watcher
// stays open afterwards, as nats.go's does — the timer gives up on the initial values, it does not
// end the subscription.
type timingOutKV struct {
	natsgo.KeyValue

	after int
}

func (k timingOutKV) Watch(keys string, opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(keys, opts...)
	if err != nil {
		return nil, err
	}
	timed := &timingOutWatcher{KeyWatcher: watcher, updates: make(chan natsgo.KeyValueEntry), faults: make(chan error, 1)}
	go func() {
		defer close(timed.updates)
		for range k.after {
			entry, ok := <-watcher.Updates()
			if !ok || entry == nil {
				return
			}
			timed.updates <- entry
		}
		timed.faults <- natsgo.ErrKeyWatcherTimeout
		timed.updates <- nil
		// The watcher lives on: forward whatever the bucket delivers next, as nats.go does.
		for entry := range watcher.Updates() {
			timed.updates <- entry
		}
	}()
	return timed, nil
}

func (k timingOutKV) WatchAll(opts ...natsgo.WatchOpt) (natsgo.KeyWatcher, error) {
	return k.Watch(natsgo.AllKeys, opts...)
}

type timingOutWatcher struct {
	natsgo.KeyWatcher

	updates chan natsgo.KeyValueEntry
	faults  chan error
}

func (w *timingOutWatcher) Updates() <-chan natsgo.KeyValueEntry { return w.updates }

func (w *timingOutWatcher) Error() <-chan error { return w.faults }

// A warm-up whose initial scan timed out says so, at WARN, and is not the watcher's error: the
// watcher is still running and its cache still follows the bucket, so Err() stays nil and nothing
// rebuilds it or answers 503 (row F2 of LEGION-374's plan). nats.go signals the timeout by putting
// ErrKeyWatcherTimeout on the watcher's single buffered Error() channel before it sends the same
// nil marker a complete scan ends with, so a reader that only watches the marker cannot tell them
// apart.
func TestATimedOutWarmUpSaysSoAndLeavesTheWatcherHealthy(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	for _, key := range []string{"one", "two", "three"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	records := captureRecords(t)
	into := newSeen()
	w := kvwatch.New("interest registry", bus.KeyValue{KeyValue: timingOutKV{KeyValue: kv, after: 1}}, into.apply, into.reset)
	t.Cleanup(w.Stop)
	w.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	line := record(t, records(), "interest registry cache warm-up")
	if line["level"] != "WARN" || line["outcome"] != "timed out" {
		t.Fatalf("a timed-out warm-up logged level=%v outcome=%v, want WARN \"timed out\": %v", line["level"], line["outcome"], line)
	}
	if got := w.WarmUp(); !got.Done || !got.TimedOut {
		t.Fatalf("WarmUp() = %+v, want a scan that timed out", got)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("Err() = %v after a timed-out warm-up; a live watcher's cache must not read as dead", err)
	}
	// The watcher is live: a key written after the timeout still reaches the cache.
	if _, err := kv.Put("after-the-timeout", []byte("1")); err != nil {
		t.Fatalf("put after-the-timeout: %v", err)
	}
	eventually(t, "a key written after the timed-out scan", func() bool { return into.has("after-the-timeout") })
	if err := w.Err(); err != nil {
		t.Fatalf("Err() = %v while the watcher is still delivering", err)
	}
}
