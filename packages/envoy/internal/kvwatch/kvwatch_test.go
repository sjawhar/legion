package kvwatch_test

import (
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// seen records the keys a watcher delivered.
type seen struct {
	mu   sync.Mutex
	keys map[string]bool
}

func (s *seen) apply(entry natsgo.KeyValueEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[entry.Key()] = true
}

func (s *seen) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[key]
}

func newWatcher(name string, into *seen) *kvwatch.Watcher {
	return &kvwatch.Watcher{Name: name, Apply: into.apply, Ready: func() {}}
}

// bucket opens the test bucket on its own connection, which the test can close to end a watcher
// the way a lost connection does.
func bucket(t *testing.T, uri string) (*natsgo.Conn, natsgo.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.KeyValue("kvwatch-test")
	if err != nil {
		kv, err = js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "kvwatch-test"})
	}
	if err != nil {
		t.Fatalf("bucket: %v", err)
	}
	return conn, kv
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A watcher that ends on its own - its connection closed - leaves the cache frozen, and Err says so
// until a Watch replaces it.
func TestAWatcherThatEndsOnItsOwnRecordsItsError(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	into := &seen{keys: map[string]bool{}}
	w := newWatcher("test cache", into)
	if err := w.Watch(kv); err != nil {
		t.Fatalf("watch: %v", err)
	}
	conn.Close()
	eventually(t, "the ended watcher's error", func() bool { return w.Err() != nil })

	_, live := bucket(t, uri)
	if err := w.Watch(live); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("a replacement watcher kept the old one's error: %v", err)
	}
	if _, err := live.Put("after-rewatch", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	eventually(t, "the replacement watcher's delivery", func() bool { return into.has("after-rewatch") })
}

// A watcher Watch replaced ends without recording anything: only the current watcher's end is the
// cache's.
func TestAReplacedWatcherEndingRecordsNothing(t *testing.T) {
	_, uri := testnats.Start(t)
	firstConn, first := bucket(t, uri)
	_, second := bucket(t, uri)
	w := newWatcher("test cache", &seen{keys: map[string]bool{}})
	if err := w.Watch(first); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := w.Watch(second); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	firstConn.Close()
	time.Sleep(500 * time.Millisecond)
	if err := w.Err(); err != nil {
		t.Fatalf("the replaced watcher's end was recorded: %v", err)
	}
}

// After Stop, the current watcher's end records nothing and a later Watch arms nothing, so a
// reconnect hook still running at shutdown neither reports an error nor starts a watcher.
func TestStopMakesTheEndSilentAndWatchANoOp(t *testing.T) {
	_, uri := testnats.Start(t)
	conn, kv := bucket(t, uri)
	into := &seen{keys: map[string]bool{}}
	w := newWatcher("test cache", into)
	if err := w.Watch(kv); err != nil {
		t.Fatalf("watch: %v", err)
	}
	w.Stop()
	conn.Close()
	time.Sleep(500 * time.Millisecond)
	if err := w.Err(); err != nil {
		t.Fatalf("a stopped watcher's end was recorded: %v", err)
	}

	_, live := bucket(t, uri)
	if err := w.Watch(live); err != nil {
		t.Fatalf("a Watch after Stop returned an error: %v", err)
	}
	if _, err := live.Put("after-stop", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if into.has("after-stop") {
		t.Fatal("a Watch after Stop armed a watcher")
	}
}
