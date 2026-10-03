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

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// jsonLogger is a JSON logger over a buffer, the shape of the one the listener hands a watcher, and
// a reader that decodes its records back, so a test asserts a line's fields rather than its
// rendering. Nothing reaches it through slog.Default(): a line the watcher writes anywhere but its
// own logger is missing here.
func jsonLogger(t *testing.T) (*slog.Logger, func() []map[string]any) {
	t.Helper()
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, func() []map[string]any {
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

// A cache's warm-up says what it cost and how it ended: an interest bucket can carry tens of
// thousands of delete markers behind a few live keys, every warm-up streams them all, and the
// restart's log has to say where that time went (LEGION-374). The two counts are disjoint, like
// the role restore line's: entries are the live keys the scan delivered, delete_markers what it
// streamed past to find them.
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

	logger, records := jsonLogger(t)
	into := newSeen()
	w := kvwatch.New("interest registry", kv, into.apply, into.reset, kvwatch.WithLogger(logger))
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
	if _, ok := line["elapsed_ms"].(float64); !ok {
		t.Fatalf("warm-up line elapsed_ms=%v, want the milliseconds the scan took", line["elapsed_ms"])
	}
}

// A warm-up whose initial scan timed out says so, at WARN, and is not the watcher's error: the
// watcher is still running and its cache still follows the bucket, so Err() stays nil and nothing
// rebuilds it or answers 503 (row F2 of LEGION-374's plan). nats.go signals the timeout by putting
// ErrKeyWatcherTimeout on the watcher's single buffered Error() channel before it sends the same
// nil marker a complete scan ends with, so a reader that only watches the marker cannot tell them
// apart (kvwatchtest.TimingOut stands in for the timer).
func TestATimedOutWarmUpSaysSoAndLeavesTheWatcherHealthy(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	for _, key := range []string{"one", "two", "three"} {
		if _, err := kv.Put(key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	logger, records := jsonLogger(t)
	into := newSeen()
	w := kvwatch.New("interest registry", bus.KeyValue{KeyValue: kvwatchtest.TimingOut(kv, 1)}, into.apply, into.reset, kvwatch.WithLogger(logger))
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
